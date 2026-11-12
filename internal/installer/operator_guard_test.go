package installer

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clusterReply(c Config) reply {
	managed := c.AKS.Operator != nil
	return reply{contains: "aks list --resource-group " + c.Azure.ResourceGroup, output: fmt.Sprintf(
		`[{"name":%q,"disableLocalAccounts":%t,"aadProfile":{"managed":%t,"enableAzureRbac":%t},"agentPoolProfiles":[{"name":"system","vmSize":%q,"count":%d},{"name":"microvm","vmSize":%q,"count":1,"osDiskType":%q,"osDiskSizeGb":%d,"maxPods":%d}]}]`,
		c.AKS.ClusterName, managed, managed, managed, c.AKS.SystemPool.VMSize, c.AKS.SystemPool.Count, c.AKS.WorkerPool.VMSize,
		c.AKS.WorkerPool.osDiskType(), c.AKS.WorkerPool.osDiskSizeGB(), cmp.Or(c.AKS.WorkerPool.MaxPods, AKSDefaultMaxPods))}
}

func TestProvisionRejectsFixedSystemPoolChanges(t *testing.T) {
	for _, whatIf := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			change func(*SystemPoolConfig)
			ok     bool
		}{
			{"unchanged", func(*SystemPoolConfig) {}, true},
			{"count increase", func(s *SystemPoolConfig) { s.Count = 3 }, true},
			{"case insensitive", func(s *SystemPoolConfig) { s.VMSize = "Standard_d4s_v6" }, true},
			{"size change", func(s *SystemPoolConfig) { s.VMSize = "Standard_D8s_v6" }, false},
		} {
			t.Run(fmt.Sprintf("preview=%t/%s", whatIf, tc.name), func(t *testing.T) {
				original := testConfig(t)
				c := original
				tc.change(&c.AKS.SystemPool)
				replies := []reply{accountReply(c), targetReply(), {contains: "group exists", output: "true"}, ownerReply(c),
					clusterReply(original), operatorRecordReply(original)}
				if tc.ok {
					action := "create"
					if whatIf {
						action = "what-if"
					}
					replies = append(replies, reply{contains: "deployment sub " + action, output: `{"status":"Succeeded","changes":[]}`})
				}
				err := (App{Config: c, Runner: script(t, replies...)}).provision(t.Context(), whatIf, false)
				if tc.ok && err != nil || !tc.ok && (err == nil || !strings.Contains(err.Error(), "destroy and recreate")) {
					t.Fatalf("fixed system pool guard: %v", err)
				}
			})
		}
	}
}

func TestProvisionAllowsSystemPoolCountDecrease(t *testing.T) {
	for _, whatIf := range []bool{false, true} {
		t.Run(fmt.Sprintf("preview=%t", whatIf), func(t *testing.T) {
			original := testConfig(t)
			original.AKS.SystemPool.Count = 3
			c := original
			c.AKS.SystemPool.Count = 1
			action := "create"
			if whatIf {
				action = "what-if"
			}
			r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "true"},
				ownerReply(c), clusterReply(original), operatorRecordReply(original),
				reply{contains: "deployment sub " + action, output: `{"status":"Succeeded","changes":[]}`})
			if err := (App{Config: c, Runner: r}).Provision(t.Context(), whatIf); err != nil {
				t.Fatalf("count decrease rejected: %v", err)
			}
		})
	}
}

func operatorRecordReply(c Config) reply {
	id, kind := "", "User"
	if c.AKS.Operator != nil {
		id, kind = c.AKS.Operator.ObjectID, c.AKS.Operator.PrincipalType
	}
	return reply{contains: "deployment group show --resource-group " + c.Azure.ResourceGroup + " --name substrate-aks-" + c.Installation.ID,
		output: fmt.Sprintf(`{"properties":{"parameters":{"operatorObjectId":{"value":%q},"operatorPrincipalType":{"value":%q},"workerCount":{"value":1},"apiAuthorizedCidrs":{"value":["203.0.113.8/32"]}}}}`, id, kind)}
}

func TestProvisionGuardsExistingOperator(t *testing.T) {
	for _, registryOnly := range []bool{false, true} {
		for _, whatIf := range []bool{false, true} {
			for _, change := range []string{"unchanged", "add", "remove", "identity", "type", "legacy"} {
				t.Run(fmt.Sprintf("registry=%v/preview=%v/%s", registryOnly, whatIf, change), func(t *testing.T) {
					original := testConfig(t)
					if change != "add" && change != "legacy" {
						original.AKS.Operator = &OperatorConfig{ObjectID: original.Azure.AssetPublisherObjectID, PrincipalType: "User"}
					}
					c := original
					switch change {
					case "add":
						c.AKS.Operator = &OperatorConfig{ObjectID: c.Azure.AssetPublisherObjectID, PrincipalType: "User"}
					case "remove":
						c.AKS.Operator = nil
					case "identity":
						c.AKS.Operator = &OperatorConfig{ObjectID: "44444444-4444-4444-8444-444444444444", PrincipalType: "User"}
					case "type":
						c.AKS.Operator = &OperatorConfig{ObjectID: c.Azure.AssetPublisherObjectID, PrincipalType: "ServicePrincipal"}
					}
					replies := []reply{accountReply(c), targetReply(), {contains: "group exists", output: "true"}, ownerReply(c), clusterReply(original)}
					if change != "add" && change != "remove" {
						replies = append(replies, operatorRecordReply(original))
					}
					ok := change == "unchanged" || change == "legacy"
					if ok {
						action := "create"
						if whatIf {
							action = "what-if"
						}
						replies = append(replies, reply{contains: "deployment sub " + action, output: `{"status":"Succeeded","changes":[]}`})
					}
					r := script(t, replies...)
					err := (App{Config: c, Runner: r}).provision(t.Context(), whatIf, registryOnly)
					if ok && err != nil || !ok && (err == nil || !strings.Contains(err.Error(), "restore")) {
						t.Fatalf("operator guard: %v", err)
					}
				})
			}
		}
	}
}

func TestProvisionRejectsFixedWorkerPoolChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*WorkerPoolConfig)
		ok     bool
	}{
		{"unchanged", func(*WorkerPoolConfig) {}, true},
		{"unset settings keep the existing pool", func(w *WorkerPoolConfig) { *w = WorkerPoolConfig{VMSize: w.VMSize, Count: 2} }, true},
		{"explicit existing pod limit", func(w *WorkerPoolConfig) { w.MaxPods = 250 }, true},
		{"disk type", func(w *WorkerPoolConfig) { w.OSDiskType = "Ephemeral" }, false},
		{"disk size", func(w *WorkerPoolConfig) { w.OSDiskSizeGB = 256 }, false},
		{"pod limit", func(w *WorkerPoolConfig) { w.MaxPods = 30 }, false},
		{"pod limit removed", func(w *WorkerPoolConfig) { w.MaxPods = 0 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := testConfig(t)
			if tc.name == "pod limit removed" {
				original.AKS.WorkerPool.MaxPods = 30
			}
			c := original
			tc.change(&c.AKS.WorkerPool)
			replies := []reply{accountReply(c), targetReply(), {contains: "group exists", output: "true"}, ownerReply(c),
				clusterReply(original), operatorRecordReply(original)}
			if tc.ok {
				replies = append(replies, reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`})
			}
			err := (App{Config: c, Runner: script(t, replies...)}).provision(t.Context(), true, false)
			if tc.ok && err != nil || !tc.ok && (err == nil || !strings.Contains(err.Error(), "destroy and recreate")) {
				t.Fatalf("fixed worker pool guard: %v", err)
			}
		})
	}
}

func TestOperatorGuardRejectsUnknownDeploymentState(t *testing.T) {
	c := testConfig(t)
	for _, output := range []string{`{}`, `{"properties":{"parameters":{}}}`, `{"properties":{"parameters":{"operatorObjectId":{"value":42},"operatorPrincipalType":{"value":"User"}}}}`, `{"properties":{"parameters":{"operatorObjectId":{"value":null},"operatorPrincipalType":{"value":"User"}}}}`} {
		t.Run(output, func(t *testing.T) {
			r := script(t, clusterReply(c), reply{contains: "deployment group show", output: output})
			if _, err := (App{Config: c, Runner: r}).guardOperator(t.Context()); err == nil {
				t.Fatal("accepted unverifiable operator state")
			}
		})
	}
	want := errors.New("deployment history unavailable")
	r := script(t, clusterReply(c), reply{contains: "deployment group show", err: want})
	if _, err := (App{Config: c, Runner: r}).guardOperator(t.Context()); !errors.Is(err, want) {
		t.Fatalf("lost deployment-history error: %v", err)
	}
}

func TestProvisionSerializesIdentityPrincipalTypes(t *testing.T) {
	for _, principalType := range []string{"User", "ServicePrincipal", "Group"} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit=%v", principalType, explicit), func(t *testing.T) {
				c := testConfig(t)
				c.AKS.Operator = &OperatorConfig{ObjectID: c.Azure.AssetPublisherObjectID, PrincipalType: principalType}
				c.Azure.SnapshotReaderObjectID = c.Azure.AssetPublisherObjectID
				if explicit {
					c.AKS.Operator = nil
					c.Azure.AssetPublisherPrincipalType = principalType
					c.Azure.SnapshotReaderPrincipalType = principalType
				}
				r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
					reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`})
				if err := (App{Config: c, Runner: r}).Provision(t.Context(), true); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(c.workspace(), "infra", "parameters.json"))
				if err != nil {
					t.Fatal(err)
				}
				var params struct {
					Parameters map[string]struct{ Value any }
				}
				if err := json.Unmarshal(data, &params); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"assetPublisherPrincipalType", "snapshotReaderPrincipalType"} {
					if params.Parameters[name].Value != principalType {
						t.Fatalf("%s lost %s: %s", name, principalType, data)
					}
				}
			})
		}
	}
}
