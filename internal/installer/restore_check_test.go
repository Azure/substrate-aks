package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRestorePoolExistenceProbe(t *testing.T) {
	empty := `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[]}`
	for _, tc := range []struct {
		name, input string
		busy, fail  bool
	}{
		{"empty", empty, false, false},
		{"empty metadata", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","metadata":{},"items":[]}`, false, false},
		{"extra Kubernetes fields", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","metadata":{"resourceVersion":"1"},"items":[],"extra":true}`, false, false},
		{"pool present", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[{"metadata":{"name":"x","namespace":"n","uid":"u"}}]}`, true, false},
		{"missing items", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList"}`, false, true},
		{"null items", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":null}`, false, true},
		{"wrong kind", `{"apiVersion":"ate.dev/v1","kind":"Other","items":[]}`, false, true},
		{"wrong API", `{"apiVersion":"ate.dev/v2","kind":"WorkerPoolList","items":[]}`, false, true},
		{"wrong key case", `{"apiVersion":"ate.dev/v1","Kind":"WorkerPoolList","items":[]}`, false, true},
		{"not object", `null`, false, true},
		{"trailing data", empty + `{}`, false, true},
		{"duplicate items", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[],"items":[]}`, false, true},
		{"null metadata", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","metadata":null,"items":[]}`, false, true},
		{"empty with continuation", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","metadata":{"continue":"next"},"items":[]}`, false, true},
		{"empty with remaining", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","metadata":{"remainingItemCount":1},"items":[]}`, false, true},
		{"item with continuation", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","metadata":{"continue":"next"},"items":[{"metadata":{"name":"x","namespace":"n","uid":"u"}}]}`, true, false},
		{"deleting pool", fmt.Sprintf(`{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[{"metadata":{"name":"x","namespace":"n","uid":"u","deletionTimestamp":%q}}]}`, time.Now().UTC().Format(time.RFC3339)), true, false},
		{"invalid pool scope", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[{"metadata":{"name":"x","uid":"u"}}]}`, false, true},
		{"duplicate pool namespace", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[{"metadata":{"name":"x","namespace":"","namespace":"n","uid":"u"}}]}`, false, true},
		{"duplicate pool status", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[{"metadata":{"name":"x","namespace":"n","uid":"u"},"status":{"state":"a","state":"b"}}]}`, false, true},
		{"too many items", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[{"metadata":{"name":"x","namespace":"n","uid":"u"}},{"metadata":{"name":"y","namespace":"n","uid":"v"}}]}`, false, true},
		{"invalid item with continuation", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","metadata":{"continue":"next"},"items":[{}]}`, false, true},
		{"negative remaining", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","metadata":{"remainingItemCount":-1},"items":[]}`, false, true},
		{"null continuation", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","metadata":{"continue":null},"items":[]}`, false, true},
		{"null remaining", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","metadata":{"remainingItemCount":null},"items":[]}`, false, true},
		{"duplicate continuation", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","metadata":{"continue":"next","continue":""},"items":[]}`, false, true},
		{"truncated", `{"apiVersion":"ate.dev/v1"`, false, true},
		{"limit", empty + strings.Repeat(" ", restoreInventoryBytes-len(empty)), false, false},
		{"over limit", empty + strings.Repeat(" ", restoreInventoryBytes-len(empty)+1), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			busy, err := workerPoolPage([]byte(tc.input))
			if busy != tc.busy || (err != nil) != tc.fail {
				t.Fatalf("busy=%v error=%v", busy, err)
			}
		})
	}
}

func TestRestoreInventoryResult(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		busy, fail  bool
	}{
		{"empty", `{"workers":false,"actors":false,"actorTemplates":false}`, false, false},
		{"busy", `{"workers":false,"actors":true,"actorTemplates":false}`, true, false},
		{"missing", `{"workers":false,"actors":false}`, false, true},
		{"duplicate", `{"workers":false,"actors":false,"actorTemplates":false,"workers":true}`, false, true},
		{"null", `{"workers":null,"actors":false,"actorTemplates":false}`, false, true},
		{"wrong type", `{"workers":"false","actors":false,"actorTemplates":false}`, false, true},
		{"unknown field", `{"workers":false,"actors":false,"actorTemplates":false,"other":false}`, false, true},
		{"wrong key case", `{"Workers":false,"actors":false,"actorTemplates":false}`, false, true},
		{"truncated", `{"workers":false`, false, true},
		{"trailing data", `{"workers":false,"actors":false,"actorTemplates":false}{}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			busy, err := restoreInventoryResult([]byte(tc.input))
			if busy != tc.busy || (err != nil) != tc.fail {
				t.Fatalf("busy=%v error=%v", busy, err)
			}
		})
	}
}

func TestRestoreBusyChecksEveryInventory(t *testing.T) {
	c := testConfig(t)
	for _, selected := range []int{-1, 0, 1, 2, 3} {
		t.Run(fmt.Sprint(selected), func(t *testing.T) {
			pool := `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[]}`
			report := `{"workers":false,"actors":false,"actorTemplates":false}`
			switch selected {
			case 0:
				pool = `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","metadata":{"continue":"next"},"items":[{"metadata":{"name":"x","namespace":"n","uid":"u"}}]}`
			case 1:
				report = `{"workers":true,"actors":false,"actorTemplates":false}`
			case 2:
				report = `{"workers":false,"actors":true,"actorTemplates":false}`
			case 3:
				report = `{"workers":false,"actors":false,"actorTemplates":true}`
			}
			r := &restoreCheckRunner{c: c, restore: &restoreRunner{config: c}, pools: []byte(pool), inventory: []byte(report)}
			busy, helper, err := (App{Config: c, Runner: r}).restoreBusy(context.Background(), "/client")
			if helper != nil {
				defer helper.close()
			}
			if err != nil || busy != (selected >= 0) {
				t.Fatalf("busy=%v err=%v", busy, err)
			}
			foundPool, foundAPI := false, false
			var binary, root, builtBinary, builtRoot string
			for _, call := range r.calls {
				if len(call.Args) >= 4 && strings.Join(call.Args[:3], " ") == "go build -o" {
					builtBinary, builtRoot = call.Args[3], call.Dir
				}
				if strings.Contains(strings.Join(call.Args, " "), "publish restore-worker-pools") {
					foundPool = true
					binary, root = call.Args[0], call.Dir
					if binary != builtBinary || root != builtRoot || !strings.Contains(strings.Join(call.Args, " "), (App{Config: c}).kubeconfig()+" "+c.AKS.ClusterName) || call.MaxOutput != restoreInventoryBytes {
						t.Fatalf("unexpected pool target or bound: %v", call)
					}
				}
				if strings.Contains(strings.Join(call.Args, " "), "publish restore-inventory") {
					foundAPI = true
					if !foundPool || call.Args[0] != binary || call.Dir != root || !strings.Contains(strings.Join(call.Args, " "), (App{Config: c}).kubeconfig()+" "+c.AKS.ClusterName) || call.MaxOutput != restoreInventoryBytes {
						t.Fatalf("unexpected target or bound: %v", call)
					}
				}
			}
			if !foundPool || !foundAPI {
				t.Fatal("serial inventory probes did not run")
			}
		})
	}
}

func TestRestoreBusyRefusesMissingBoundedHelper(t *testing.T) {
	c := testConfig(t)
	want := errors.New("build unavailable")
	r := &restoreCheckRunner{c: c, restore: &restoreRunner{config: c}, buildErr: want}
	busy, helper, err := (App{Config: c, Runner: r}).restoreBusy(t.Context(), "/client")
	if helper != nil {
		defer helper.close()
	}
	if busy || !errors.Is(err, want) || !strings.Contains(err.Error(), "PREREQUISITE") {
		t.Fatalf("busy=%v error=%v", busy, err)
	}
	for _, call := range r.calls {
		if strings.Contains(strings.Join(call.Args, " "), "publish restore-inventory") ||
			strings.Contains(strings.Join(call.Args, " "), "publish restore-worker-pools") {
			t.Fatal("attempted an unbounded inventory fallback")
		}
	}
}

func TestRestoreCheckCleanAbsenceAndRequiredMode(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(fmt.Sprint(required), func(t *testing.T) {
			c := testConfig(t)
			outputs, err := json.Marshal(testInfrastructure())
			if err != nil {
				t.Fatal(err)
			}
			r := script(t,
				reply{contains: "group exists", output: "true"}, ownerReply(c),
				reply{contains: "deployment sub show", output: string(outputs)},
				reply{contains: "aks get-credentials"},
				reply{contains: "get --raw /version", output: `{"gitVersion":"v1.37.1"}`},
				reply{contains: "api-resources", output: "podcertificaterequests.certificates.k8s.io\nclustertrustbundles.certificates.k8s.io\n"},
				reply{contains: "wait node"},
				reply{contains: "get nodes", output: `{"items":[{"metadata":{"labels":{"substrate-aks/runtime":"microvm"}},"status":{"nodeInfo":{"kubeletVersion":"v1.37.1","architecture":"amd64","operatingSystem":"linux"}}}]}`},
				reply{contains: "get namespace ate-system"},
			)
			var progress []string
			a := App{Config: c, Runner: r, Progress: func(format string, args ...any) { progress = append(progress, fmt.Sprintf(format, args...)) }}
			err = a.Check(t.Context(), required)
			if required {
				if err == nil || !strings.Contains(err.Error(), "PREREQUISITE") {
					t.Fatalf("required returned %v", err)
				}
			} else if err != nil || !strings.Contains(strings.Join(progress, "\n"), "RESTORE_SKIPPED_NOT_INSTALLED") {
				t.Fatalf("skip returned %v, progress %v", err, progress)
			}
		})
	}
}

func TestRestoreInstalledRequiresPositiveEvidence(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	ns := `{"metadata":{"name":"ate-system"}}`
	deployment := `{"metadata":{"name":"ate-api-server","namespace":"ate-system"}}`
	marker := fmt.Sprintf(`{"metadata":{"name":"substrate-aks-install","namespace":"ate-system"},"data":{"installationId":%q,"fingerprint":%q,"phase":"deployed"}}`,
		c.Installation.ID, fingerprint(c, i))
	failure := errors.New("read denied")
	for _, tc := range []struct {
		name      string
		replies   []reply
		installed bool
		err       bool
	}{
		{"absent namespace", []reply{{contains: "get namespace"}}, false, false},
		{"empty namespace", []reply{{contains: "get namespace", output: ns}, {contains: "get configmap"}, {contains: "get deployment"}}, false, false},
		{"installed", []reply{{contains: "get namespace", output: ns}, {contains: "get configmap", output: marker}, {contains: "get deployment", output: deployment}}, true, false},
		{"namespace denied", []reply{{contains: "get namespace", err: failure}}, false, true},
		{"malformed namespace", []reply{{contains: "get namespace", output: "{}"}}, false, true},
		{"record denied", []reply{{contains: "get namespace", output: ns}, {contains: "get configmap", err: failure}}, false, true},
		{"malformed record", []reply{{contains: "get namespace", output: ns}, {contains: "get configmap", output: "{}"}}, false, true},
		{"foreign record", []reply{{contains: "get namespace", output: ns}, {contains: "get configmap", output: `{"metadata":{"name":"substrate-aks-install","namespace":"ate-system"},"data":{"installationId":"foreign","fingerprint":"foreign","phase":"deployed"}}`}}, false, true},
		{"partial record", []reply{{contains: "get namespace", output: ns}, {contains: "get configmap", output: fmt.Sprintf(`{"metadata":{"name":"substrate-aks-install","namespace":"ate-system"},"data":{"installationId":%q,"fingerprint":%q,"phase":"installing:ate-system"}}`, c.Installation.ID, fingerprint(c, i))}}, false, true},
		{"deployment denied", []reply{{contains: "get namespace", output: ns}, {contains: "get configmap", output: marker}, {contains: "get deployment", err: failure}}, false, true},
		{"orphan record", []reply{{contains: "get namespace", output: ns}, {contains: "get configmap", output: marker}, {contains: "get deployment"}}, false, true},
		{"unowned deployment", []reply{{contains: "get namespace", output: ns}, {contains: "get configmap"}, {contains: "get deployment", output: deployment}}, false, true},
		{"malformed deployment", []reply{{contains: "get namespace", output: ns}, {contains: "get configmap", output: marker}, {contains: "get deployment", output: "{}"}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (App{Config: c, Runner: script(t, tc.replies...)}).restoreInstalled(t.Context(), i)
			if got != tc.installed || (err != nil) != tc.err {
				t.Fatalf("installed=%v, error=%v", got, err)
			}
		})
	}
}
