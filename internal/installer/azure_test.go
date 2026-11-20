package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type reply struct {
	contains string
	output   string
	err      error
	effect   func(Command)
}

type scriptedRunner struct {
	t       *testing.T
	replies []reply
	calls   []Command
}

func (r *scriptedRunner) Run(_ context.Context, c Command) ([]byte, error) {
	r.t.Helper()
	r.calls = append(r.calls, c)
	if len(r.replies) == 0 {
		r.t.Fatalf("unexpected command: %v", c.Args)
	}
	step := r.replies[0]
	r.replies = r.replies[1:]
	if !strings.Contains(strings.Join(c.Args, " "), step.contains) {
		r.t.Fatalf("expected %q, got %v", step.contains, c.Args)
	}
	if step.effect != nil {
		step.effect(c)
	} else if step.err == nil && strings.Contains(strings.Join(c.Args, " "), "aks get-credentials") {
		var path, name string
		for i := 0; i+1 < len(c.Args); i++ {
			switch c.Args[i] {
			case "--file":
				path = c.Args[i+1]
			case "--context":
				name = c.Args[i+1]
			}
		}
		if err := os.WriteFile(path, kubeconfigFixture(name), 0600); err != nil {
			r.t.Fatal(err)
		}
	}
	return []byte(step.output), step.err
}

func script(t *testing.T, replies ...reply) *scriptedRunner {
	r := &scriptedRunner{t: t, replies: replies}
	t.Cleanup(func() {
		if len(r.replies) != 0 {
			t.Errorf("%d expected commands were not run", len(r.replies))
		}
	})
	return r
}

func accountReply(c Config) reply {
	return reply{contains: "account show", output: `{"id":"` + c.Azure.SubscriptionID + `"}`}
}

func ownerReply(c Config) reply {
	return reply{contains: "group show", output: `{"location":"` + c.Azure.Location + `","tags":{"` + ownershipTag + `":"` + c.Installation.ID + `"}}`}
}

func targetReply() reply {
	return reply{contains: "group list --tag", output: "[]"}
}

func resourceGroupJSON(c Config, name, managedBy string) string {
	manager := "null"
	if managedBy != "" {
		manager = fmt.Sprintf("%q", managedBy)
	}
	return fmt.Sprintf(`{"id":%q,"name":%q,"type":"Microsoft.Resources/resourceGroups","location":%q,"properties":{"provisioningState":"Succeeded"},"tags":{%q:%q},"managedBy":%s}`,
		"/subscriptions/"+c.Azure.SubscriptionID+"/resourceGroups/"+name,
		name, c.Azure.Location, ownershipTag, c.Installation.ID, manager)
}

func managedNodeGroupJSON(c Config) string {
	return resourceGroupJSON(c,
		"MC_"+c.Azure.ResourceGroup+"_"+c.AKS.ClusterName+"_"+c.Azure.Location,
		"/subscriptions/"+c.Azure.SubscriptionID+"/resourceGroups/"+c.Azure.ResourceGroup+"/providers/Microsoft.ContainerService/managedClusters/"+c.AKS.ClusterName)
}

func TestDestroyRequiresConfirmationAndOwnership(t *testing.T) {
	c := testConfig(t)
	t.Run("confirmation before any cloud call", func(t *testing.T) {
		a := App{Config: c, Runner: script(t)}
		if err := a.Destroy(t.Context(), "wrong"); err == nil {
			t.Fatal("destroy accepted wrong confirmation")
		}
	})
	t.Run("cannot delete unowned group", func(t *testing.T) {
		r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "true"},
			reply{contains: "group show", output: `{"location":"eastus2","tags":{}}`})
		var steps []string
		a := App{Config: c, Runner: r, Progress: func(format string, args ...any) {
			steps = append(steps, fmt.Sprintf(format, args...))
		}}
		if err := a.Destroy(t.Context(), c.Azure.ResourceGroup); err == nil {
			t.Fatal("destroy accepted unowned resource group")
		}
		if strings.Contains(strings.Join(steps, "\n"), "Deleting") {
			t.Fatal("teardown claimed to delete resources after ownership check failed")
		}
	})
	t.Run("delete failure remains an error", func(t *testing.T) {
		wantErr := errors.New("Azure deletion failed")
		r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "true"}, ownerReply(c),
			reply{contains: "group delete", err: wantErr})
		var steps []string
		a := App{Config: c, Runner: r, Progress: func(format string, args ...any) {
			steps = append(steps, fmt.Sprintf(format, args...))
		}}
		err := a.Destroy(t.Context(), c.Azure.ResourceGroup)
		if !errors.Is(err, wantErr) {
			t.Fatalf("lost deletion error: %v", err)
		}
		if !strings.HasPrefix(steps[len(steps)-1], "Deleting") {
			t.Fatal("teardown did not report the long-running deletion step")
		}
		for _, call := range r.calls {
			if !strings.Contains(strings.Join(call.Args, " "), "--subscription "+c.Azure.SubscriptionID) {
				t.Fatalf("command lost explicit subscription: %v", call.Args)
			}
		}
	})
	t.Run("already deleted is safe", func(t *testing.T) {
		r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"})
		if err := (App{Config: c, Runner: r}).Destroy(t.Context(), c.Azure.ResourceGroup); err != nil {
			t.Fatal(err)
		}
	})
	// The CLI removes local state after success, so leftovers must fail.
	for name, group := range map[string]string{
		"orphaned node group": managedNodeGroupJSON(c),
		"retargeted record":   resourceGroupJSON(c, "original-group", ""),
	} {
		t.Run(name+" is not reported as deleted", func(t *testing.T) {
			r := script(t, accountReply(c), reply{contains: "group list --tag", output: "[" + group + "]"})
			if err := (App{Config: c, Runner: r}).Destroy(t.Context(), c.Azure.ResourceGroup); err == nil {
				t.Fatal("destroy succeeded while installation-tagged resource groups remain")
			}
		})
	}
}

func TestStatusDisplaysResultsButNotSetupCommands(t *testing.T) {
	c := testConfig(t)
	outputs, err := json.Marshal(testInfrastructure())
	if err != nil {
		t.Fatal(err)
	}
	r := script(t,
		reply{contains: "group exists", output: "true"}, ownerReply(c),
		reply{contains: "deployment sub list", output: string(outputs)},
		reply{contains: "aks get-credentials"},
		reply{contains: "get configmap substrate-aks-install", output: ""},
		reply{contains: "get pods,pvc", output: "pod status"},
	)
	if err := (App{Config: c, Runner: r}).Status(t.Context()); err != nil {
		t.Fatal(err)
	}
	for index, call := range r.calls {
		if call.ShowOutput != (index == len(r.calls)-1) {
			t.Fatalf("status hid its results or exposed setup command output: %+v", call)
		}
	}
}

func TestStatusReportsInstalledRevisionsFromTheRecord(t *testing.T) {
	c := testConfig(t)
	for _, tc := range []struct {
		name, record string
		want         []string
	}{
		{"current record",
			`{"data":{"repository":"` + DefaultRepository + `","tag":"v1.0.0","commit":"` + DefaultCommit + `","pluginRepository":"` + DefaultPluginRepository + `","pluginCommit":"` + DefaultPluginCommit + `","phase":"deployed"}}`,
			[]string{"v1.0.0 (" + DefaultCommit[:12] + ")", "plugin " + DefaultPluginCommit[:12], "phase deployed"}},
		{"record from the previous pin",
			`{"data":{"repository":"https://example.invalid/substrate.git","commit":"c3b44aa46312e073a6384f68723a7b14dffd5518","phase":"deployed"}}`,
			[]string{"c3b44aa46312 from https://example.invalid/substrate.git", "plugin not recorded"}},
		{"no installation", "", []string{"No Substrate installation is recorded"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			r := script(t, reply{contains: "get configmap substrate-aks-install", output: tc.record})
			a := App{Config: c, Runner: r, Progress: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }}
			if err := a.installedRevisions(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := strings.Join(lines, "\n")
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("status reported %q, missing %q", got, want)
				}
			}
			if tc.name == "record from the previous pin" && strings.Contains(got, DefaultPluginCommit[:12]) {
				t.Fatalf("status reported the configured plugin for an older installation: %q", got)
			}
		})
	}
}

func TestProvisionWhatIfWritesTypedParameters(t *testing.T) {
	c := testConfig(t)
	r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
		reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[{"changeType":"Create","resourceId":"/subscriptions/test/resourceGroups/dev/providers/Microsoft.ContainerRegistry/registries/example"}]}`})
	var changes []ResourceChange
	a := App{Config: c, Runner: r, Preview: func(result []ResourceChange) error {
		changes = result
		return nil
	}}
	if err := a.Provision(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].ChangeType != "Create" ||
		changes[0].ResourceID != "/subscriptions/test/resourceGroups/dev/providers/Microsoft.ContainerRegistry/registries/example" {
		t.Fatalf("preview altered Azure resource IDs: %+v", changes)
	}
	if !strings.Contains(strings.Join(r.calls[len(r.calls)-1].Args, " "), "--no-pretty-print") {
		t.Fatal("preview did not request structured output")
	}
	data, err := os.ReadFile(filepath.Join(c.workspace(), "infra/parameters.json"))
	if err != nil {
		t.Fatal(err)
	}
	var params struct {
		Parameters map[string]struct {
			Value any `json:"value"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(data, &params); err != nil {
		t.Fatal(err)
	}
	if params.Parameters["workerCount"].Value != float64(1) ||
		params.Parameters["workerOsDiskType"].Value != "Managed" ||
		params.Parameters["workerOsDiskSizeGB"].Value != float64(128) ||
		params.Parameters["workerMaxPods"].Value != float64(0) ||
		params.Parameters["assetPublisherObjectId"].Value != c.Azure.AssetPublisherObjectID {
		t.Fatalf("incorrect typed parameters: %s", data)
	}
	cidrs, ok := params.Parameters["apiAuthorizedCidrs"].Value.([]any)
	if !ok || len(cidrs) != 1 || cidrs[0] != c.AKS.APIAuthorizedCIDRs[0] {
		t.Fatal("API ranges did not survive parameter serialization")
	}
}

func TestProvisionRejectsRetargetingBeforeDeployment(t *testing.T) {
	for _, registryOnly := range []bool{false, true} {
		for _, whatIf := range []bool{false, true} {
			t.Run(fmt.Sprintf("registry=%v/preview=%v", registryOnly, whatIf), func(t *testing.T) {
				c := testConfig(t)
				groups := "[" + resourceGroupJSON(c, "original-group", "") + "]"
				r := script(t, accountReply(c), reply{contains: "group list --tag " + ownershipTag + "=" + c.Installation.ID, output: groups})
				err := (App{Config: c, Runner: r}).provision(t.Context(), whatIf, registryOnly)
				if err == nil || !strings.Contains(err.Error(), "original-group") || !strings.Contains(err.Error(), "Restore azure.resourceGroup") {
					t.Fatalf("retargeting was not rejected with recovery guidance: %v", err)
				}
			})
		}
	}
}

func TestProvisionRetryWithAKSManagedNodeGroup(t *testing.T) {
	for _, registryOnly := range []bool{false, true} {
		for _, whatIf := range []bool{false, true} {
			t.Run(fmt.Sprintf("registry=%v/preview=%v", registryOnly, whatIf), func(t *testing.T) {
				c := testConfig(t)
				groups := "[" + managedNodeGroupJSON(c) + "," + resourceGroupJSON(c, c.Azure.ResourceGroup, "") + "]"
				action := "create"
				if whatIf {
					action = "what-if"
				}
				r := script(t, accountReply(c),
					reply{contains: "group list --tag " + ownershipTag + "=" + c.Installation.ID, output: groups},
					reply{contains: "group exists", output: "true"}, ownerReply(c),
					clusterReply(c), operatorRecordReply(c),
					reply{contains: "deployment sub " + action, output: `{"status":"Succeeded","changes":[]}`})
				if err := (App{Config: c, Runner: r}).provision(t.Context(), whatIf, registryOnly); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestTargetDiscoveryFailsClosedAndAllowsSameTargetRetry(t *testing.T) {
	c := testConfig(t)
	same := resourceGroupJSON(c, strings.ToUpper(c.Azure.ResourceGroup), "")
	other := resourceGroupJSON(c, "another-group", "")
	node := managedNodeGroupJSON(c)
	withNode := func(node string) string { return "[" + node + "," + same + "]" }
	foreignSubscription := c
	foreignSubscription.Azure.SubscriptionID = "33333333-3333-4333-8333-333333333333"
	foreignGroup := c
	foreignGroup.Azure.ResourceGroup = "another-group"
	foreignCluster := c
	foreignCluster.AKS.ClusterName = "another-cluster"
	accessErr := errors.New("subscription resource-group listing denied")
	for _, tc := range []struct {
		name, output string
		err          error
		ok           bool
	}{
		{"first install or fully torn down", `[]`, nil, true},
		{"same group with case-insensitive name", "[" + same + "]", nil, true},
		{"own AKS managed node group before main group", withNode(node), nil, true},
		{"case-insensitive managed resource ID", withNode(strings.ReplaceAll(node, "/subscriptions/", "/SUBSCRIPTIONS/")), nil, true},
		{"managed node group with custom name", withNode(strings.ReplaceAll(node, "MC_"+c.Azure.ResourceGroup+"_"+c.AKS.ClusterName+"_"+c.Azure.Location, "custom-node-group")), nil, true},
		{"unmanaged MC group", withNode(resourceGroupJSON(c, "MC_unrelated", "")), nil, false},
		{"node group without main group", "[" + node + "]", nil, false},
		{"node group managed by another subscription", withNode(managedNodeGroupJSON(foreignSubscription)), nil, false},
		{"node group managed by another resource group", withNode(managedNodeGroupJSON(foreignGroup)), nil, false},
		{"node group managed by another cluster", withNode(managedNodeGroupJSON(foreignCluster)), nil, false},
		{"managed resource ID suffix", withNode(strings.Replace(node, c.AKS.ClusterName+`"}`, c.AKS.ClusterName+`/child"}`, 1)), nil, false},
		{"node group plus duplicate installation", "[" + node + "," + same + "," + other + "]", nil, false},
		{"malformed managedBy", withNode(strings.Replace(node, `"managedBy":"`, `"managedBy":123,"unused":"`, 1)), nil, false},
		{"duplicate installation groups", "[" + same + "," + other + "]", nil, false},
		{"missing group name", "[" + resourceGroupJSON(c, "", "") + "]", nil, false},
		{"wrong tag", "[" + strings.Replace(other, c.Installation.ID, "other", 1) + "]", nil, false},
		{"missing tags", `[{"name":"original-group"}]`, nil, false},
		{"projected response without native tags", `[{"name":"original-group","installationId":"` + c.Installation.ID + `"}]`, nil, false},
		{"malformed tags", `[{"name":"original-group","tags":["wrong"]}]`, nil, false},
		{"invalid JSON", `broken`, nil, false},
		{"unknown list", `null`, nil, false},
		{"access failure", "", accessErr, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := script(t, reply{contains: "group list --tag " + ownershipTag + "=" + c.Installation.ID, output: tc.output, err: tc.err})
			err := (App{Config: c, Runner: r}).guardTarget(t.Context())
			if (err == nil) != tc.ok || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("target guard result = %v", err)
			}
			if !strings.Contains(strings.Join(r.calls[0].Args, " "), "--subscription "+c.Azure.SubscriptionID) {
				t.Fatal("target discovery used the wrong subscription")
			}
			for _, arg := range r.calls[0].Args {
				if arg == "--query" {
					t.Fatal("target discovery must consume native Azure resource-group JSON, not an untested projection")
				}
			}
		})
	}
}

func TestTargetGuardRecoveryGuidance(t *testing.T) {
	for _, orphaned := range []bool{false, true} {
		t.Run(fmt.Sprintf("orphaned=%v", orphaned), func(t *testing.T) {
			c := testConfig(t)
			groups := "[" + managedNodeGroupJSON(c)
			var want []string
			if orphaned {
				want = []string{"node resource group", "was not found", "Wait for any in-progress teardown", "incomplete teardown"}
			} else {
				groups += "," + resourceGroupJSON(c, c.Azure.ResourceGroup, "")
				c.AKS.ClusterName = "edited-cluster"
				want = []string{"does not match configured AKS cluster", "edited-cluster", "Restore aks.clusterName and azure.resourceGroup"}
			}
			groups += "]"
			r := script(t, reply{contains: "group list --tag", output: groups})
			err := (App{Config: c, Runner: r}).guardTarget(t.Context())
			if err == nil {
				t.Fatal("unsafe target accepted")
			}
			for _, text := range want {
				if !strings.Contains(err.Error(), text) {
					t.Fatalf("recovery guidance missing %q: %v", text, err)
				}
			}
			if strings.Contains(err.Error(), "Restore azure.resourceGroup for management") {
				t.Fatalf("recovery guidance incorrectly diagnoses a resource-group name change: %v", err)
			}
		})
	}
}

func TestProvisionRetainsPreviousDeploymentAttempts(t *testing.T) {
	c := testConfig(t)
	failure := errors.New("resource update failed")
	r := script(t,
		accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
		reply{contains: "deployment sub create"},
		accountReply(c), targetReply(), reply{contains: "group exists", output: "true"}, ownerReply(c),
		reply{contains: "aks list", output: "[]"},
		reply{contains: "deployment sub create", err: failure},
	)
	a := App{Config: c, Runner: r}
	if err := a.Provision(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if err := a.Provision(t.Context(), false); !errors.Is(err, failure) {
		t.Fatalf("failed reprovision lost its original error: %v", err)
	}
	names := map[string]bool{}
	for _, call := range r.calls {
		if !strings.Contains(strings.Join(call.Args, " "), "deployment sub create") {
			continue
		}
		for index, arg := range call.Args {
			if arg != "--name" {
				continue
			}
			name := call.Args[index+1]
			if !strings.HasPrefix(name, c.deploymentName()+"-run-") || len(name) > 64 || names[name] {
				t.Fatalf("a new attempt would overwrite deployment history or use an invalid name: %q", name)
			}
			names[name] = true
		}
	}
	if len(names) != 2 {
		t.Fatalf("expected two independent deployment attempts, got %v", names)
	}
}

func TestInfrastructureRejectsMissingOrInvalidSuccessfulOutputs(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
	}{
		{"empty", "", "no successful infrastructure deployment outputs"},
		{"null", "null\n", "no successful infrastructure deployment outputs"},
		{"invalid JSON", "{", "reading successful infrastructure deployment outputs"},
		{"empty object", "{}", "missing or inconsistent"},
		{"wrong shape", "[]", "reading successful infrastructure deployment outputs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			r := script(t, reply{contains: "group exists", output: "true"}, ownerReply(c),
				reply{contains: "deployment sub list", output: tc.output})
			if _, err := (App{Config: c, Runner: r}).infrastructure(t.Context()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("missing or invalid outputs were accepted or poorly diagnosed: %v", err)
			}
		})
	}
	t.Run("access failure is not missing state", func(t *testing.T) {
		c := testConfig(t)
		failure := errors.New("deployment history access denied")
		r := script(t, reply{contains: "group exists", output: "true"}, ownerReply(c),
			reply{contains: "deployment sub list", err: failure})
		if _, err := (App{Config: c, Runner: r}).infrastructure(t.Context()); !errors.Is(err, failure) {
			t.Fatalf("discovery concealed an Azure access failure: %v", err)
		}
	})
}

func TestProvisionUnrestrictedAPIAccessIsExplicitAndUsesAnEmptyArray(t *testing.T) {
	for _, unrestricted := range []bool{false, true} {
		t.Run(fmt.Sprintf("opt-in=%v", unrestricted), func(t *testing.T) {
			c := testConfig(t)
			c.AKS.APIAuthorizedCIDRs = nil
			c.AKS.AllowUnrestrictedAPI = unrestricted
			if !unrestricted {
				if err := (App{Config: c, Runner: script(t)}).Provision(t.Context(), true); err == nil {
					t.Fatal("missing allowlist reached Azure without explicit opt-in")
				}
				return
			}
			r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
				reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`})
			if err := (App{Config: c, Runner: r}).Provision(t.Context(), true); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(c.workspace(), "infra/parameters.json"))
			if err != nil {
				t.Fatal(err)
			}
			var params struct {
				Parameters map[string]struct {
					Value any `json:"value"`
				} `json:"parameters"`
			}
			if err := json.Unmarshal(data, &params); err != nil {
				t.Fatal(err)
			}
			cidrs, ok := params.Parameters["apiAuthorizedCidrs"].Value.([]any)
			if !ok || len(cidrs) != 0 {
				t.Fatalf("unrestricted access must deploy [] rather than null or a bogus range: %s", data)
			}
		})
	}
}

func TestPlanRejectsIncompleteResults(t *testing.T) {
	for _, input := range []string{
		`not json`, `{}`, `{"status":"Failed","changes":[]}`,
		`{"status":"Succeeded"}`, `{"status":"Succeeded","changes":[],"error":{"message":"preview failed"}}`,
	} {
		t.Run(input, func(t *testing.T) {
			presented := false
			a := App{Preview: func([]ResourceChange) error {
				presented = true
				return nil
			}}
			if err := a.showPlan([]byte(input)); err == nil {
				t.Fatal("invalid preview became success")
			}
			if presented {
				t.Fatal("invalid preview emitted a success-shaped result")
			}
		})
	}
	wantErr := errors.New("presentation failed")
	a := App{Preview: func([]ResourceChange) error { return wantErr }}
	if err := a.showPlan([]byte(`{"status":"Succeeded","changes":[]}`)); !errors.Is(err, wantErr) {
		t.Fatalf("preview swallowed a presentation failure: %v", err)
	}
}

func TestInstallGuard(t *testing.T) {
	c := testConfig(t)
	for _, tc := range []struct {
		name      string
		operation string
		replies   []reply
		wantError bool
	}{
		{"first install", "ate-system", []reply{{contains: "get namespace", output: ""}}, false},
		{"components before install", "atelet", []reply{{contains: "get namespace", output: ""}}, true},
		{"foreign installation", "ate-system", []reply{
			{contains: "get namespace", output: "{}"},
			{contains: "get configmap", output: ""},
			{contains: "get deployment", output: `{"metadata":{"name":"ate-api-server"}}`},
		}, true},
		{"changed images", "ate-system", []reply{
			{contains: "get namespace", output: "{}"},
			{contains: "get configmap", output: `{"data":{"installationId":"` + c.Installation.ID + `","fingerprint":"other"}}`},
		}, true},
		{"retry partial install", "ate-system", []reply{
			{contains: "get namespace", output: "{}"},
			{contains: "get configmap", output: `{"data":{"installationId":"` + c.Installation.ID + `","fingerprint":"same","phase":"installing:ate-system"}}`},
		}, false},
		{"no demo on partial install", "demo", []reply{
			{contains: "get namespace", output: "{}"},
			{contains: "get configmap", output: `{"data":{"installationId":"` + c.Installation.ID + `","fingerprint":"same","phase":"installing:atelet"}}`},
		}, true},
		{"retry interrupted component", "atelet", []reply{
			{contains: "get namespace", output: "{}"},
			{contains: "get configmap", output: `{"data":{"installationId":"` + c.Installation.ID + `","fingerprint":"same","phase":"installing:atelet"}}`},
		}, false},
		{"cannot skip interrupted component", "apiserver", []reply{
			{contains: "get namespace", output: "{}"},
			{contains: "get configmap", output: `{"data":{"installationId":"` + c.Installation.ID + `","fingerprint":"same","phase":"installing:atelet"}}`},
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := App{Config: c, Runner: script(t, tc.replies...)}
			err := a.guardInstall(t.Context(), "same", tc.operation)
			if (err != nil) != tc.wantError {
				t.Fatalf("guard error = %v", err)
			}
		})
	}
}

func TestKVMCleanupOnFailure(t *testing.T) {
	c := testConfig(t)
	wantErr := errors.New("KVM unavailable")
	r := script(t,
		reply{contains: "apply -f -"},
		reply{contains: "rollout status", err: wantErr},
		reply{contains: "describe pods"},
		reply{contains: "logs"},
		reply{contains: "delete daemonset"},
	)
	err := (App{Config: c, Runner: r}).checkKVM(t.Context())
	if !errors.Is(err, wantErr) {
		t.Fatalf("KVM failure was hidden: %v", err)
	}
	if !strings.Contains(string(r.calls[0].Input), "0xAE01") {
		t.Fatal("probe does not test KVM_CREATE_VM")
	}
}

func TestClusterRejectsMissingCertificateAPI(t *testing.T) {
	r := script(t,
		reply{contains: "get --raw /version", output: `{"gitVersion":"v1.37.1"}`},
		reply{contains: "api-resources", output: "certificatesigningrequests.certificates.k8s.io\n"},
	)
	err := (App{Config: testConfig(t), Runner: r}).checkCluster(t.Context())
	if err == nil || !strings.Contains(err.Error(), "podcertificaterequests") {
		t.Fatalf("cluster version was treated as sufficient evidence: %v", err)
	}
}

func TestClusterRejectsWrongRuntimeArchitecture(t *testing.T) {
	r := script(t,
		reply{contains: "get --raw /version", output: `{"gitVersion":"v1.37.1"}`},
		reply{contains: "api-resources", output: "podcertificaterequests.certificates.k8s.io\nclustertrustbundles.certificates.k8s.io\n"},
		reply{contains: "wait node"},
		reply{contains: "get nodes", output: `{"items":[{"metadata":{"labels":{"substrate-aks/runtime":"microvm"}},"status":{"nodeInfo":{"kubeletVersion":"v1.37.1","architecture":"arm64","operatingSystem":"linux"}}}]}`},
	)
	err := (App{Config: testConfig(t), Runner: r}).checkCluster(t.Context())
	if err == nil || !strings.Contains(err.Error(), "amd64") {
		t.Fatalf("accepted a node incompatible with the runtime assets: %v", err)
	}
}
