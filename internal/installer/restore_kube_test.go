package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRestoreNamespaceMetricsAreExcludedFromBothCleanupPasses(t *testing.T) {
	for _, issue := range []string{"none", "foreign pod", "foreign configmap", "foreign crd", "uidless pod",
		"uidless configmap", "other PodMetrics", "incomplete", "denied", "empty discovery", "metrics only",
		"truncated discovery", "oversized discovery", "too many resources", "discovery denied", "namespace wait"} {
		passes := []int{1, 2}
		if issue == "none" || issue == "namespace wait" {
			passes = []int{0}
		}
		for _, failPass := range passes {
			t.Run(fmt.Sprintf("%s/pass=%d", issue, failPass), func(t *testing.T) {
				c := testConfig(t)
				r := &restoreRunner{config: c, namespace: "space", actor: "actor", token: "token", poolExists: true}
				if issue == "namespace wait" {
					r.failures = map[string]error{" wait namespace ": errors.New("namespace deletion pending")}
				}
				discovery := 0
				metricsLists := 0
				var calls []string
				runner := prerequisiteRunner(func(ctx context.Context, cmd Command) ([]byte, error) {
					command := strings.Join(cmd.Args, " ")
					calls = append(calls, command)
					if strings.Contains(command, " publish restore-namespace-resources ") {
						discovery++
						if discovery == failPass {
							switch issue {
							case "empty discovery":
								return nil, nil
							case "metrics only":
								return []byte("pods.metrics.k8s.io\n"), nil
							case "truncated discovery":
								return []byte("pods"), nil
							case "oversized discovery":
								return []byte(strings.Repeat("x", restoreInventoryBytes+1)), nil
							case "too many resources":
								return []byte(strings.Repeat("pods\n", restoreNamespaceResources+1)), nil
							case "discovery denied":
								return nil, errors.New("discovery denied")
							}
						}
						return []byte("workerpools.ate.dev\npods\npods.metrics.k8s.io\nconfigmaps\nthings.example.test\npodmetrics.example.test\n"), nil
					}
					if strings.Contains(command, " get pods.metrics.k8s.io ") {
						metricsLists++
						return []byte(`{"kind":"PodMetricsList","items":[{"metadata":{"name":"pod","namespace":"space"}}]}`), nil
					}
					if strings.Contains(command, " get pods --namespace=") && discovery == failPass {
						switch issue {
						case "foreign pod":
							if r.poolExists {
								return []byte(`{"kind":"PodList","items":[{"kind":"Pod","metadata":{"name":"pod","namespace":"space","uid":"pod-id","ownerReferences":[{"uid":"pool-id"}]}},{"metadata":{"name":"foreign","namespace":"space","uid":"foreign"}}]}`), nil
							}
							return []byte(`{"kind":"PodList","items":[{"metadata":{"name":"foreign","namespace":"space","uid":"foreign"}}]}`), nil
						case "uidless pod":
							return []byte(`{"kind":"PodList","items":[{"metadata":{"name":"pod","namespace":"space"}}]}`), nil
						case "incomplete":
							return []byte(`{"kind":"PodList","metadata":{"continue":"next"},"items":[]}`), nil
						case "denied":
							return nil, errors.New("pod list denied")
						}
					}
					if strings.Contains(command, " get configmaps --namespace=") {
						if discovery == failPass && issue == "foreign configmap" {
							return []byte(`{"kind":"ConfigMapList","items":[{"metadata":{"name":"foreign","namespace":"space","uid":"foreign"}}]}`), nil
						}
						if discovery == failPass && issue == "uidless configmap" {
							return []byte(`{"kind":"ConfigMapList","items":[{"metadata":{"name":"foreign","namespace":"space"}}]}`), nil
						}
						return []byte(`{"kind":"ConfigMapList","items":[]}`), nil
					}
					if strings.Contains(command, " get things.example.test --namespace=") {
						if discovery == failPass && issue == "foreign crd" {
							return []byte(`{"kind":"ThingList","items":[{"metadata":{"name":"foreign","namespace":"space","uid":"foreign"}}]}`), nil
						}
						return []byte(`{"kind":"ThingList","items":[]}`), nil
					}
					if strings.Contains(command, " get podmetrics.example.test --namespace=") {
						if discovery == failPass && issue == "other PodMetrics" {
							return []byte(`{"kind":"PodMetricsList","items":[{"metadata":{"name":"pod","namespace":"space"}}]}`), nil
						}
						return []byte(`{"kind":"PodMetricsList","items":[]}`), nil
					}
					return r.Run(ctx, cmd)
				})
				state := &restoreState{namespace: "space", token: "token", client: "/client", helper: &restoreHelper{binary: "/fixture"},
					ns: restoreKubeIdentity{Name: "space", UID: "namespace-id"}, pool: restoreKubeIdentity{Name: restorePoolName, Namespace: "space", UID: "pool-id"},
					space: restoreAPIIdentity{Name: "space", UID: "space-id"}, actor: restoreAPIIdentity{Name: "actor", Atespace: "space", UID: "actor-id"},
					template: restoreAPIIdentity{Name: restorePoolName, Atespace: "space", UID: "template-id"}}
				err := (App{Config: c, Runner: runner}).restoreCleanup(t.Context(), state)
				if (err == nil) != (issue == "none") {
					t.Fatalf("issue=%s pass=%d error=%v", issue, failPass, err)
				}
				joined := strings.Join(calls, "\n")
				if metricsLists != 0 || strings.Contains(joined, " get pods.metrics.k8s.io ") {
					t.Fatal("listed the excluded projection")
				}
				if failPass == 1 && (strings.Contains(joined, "delete --raw") || !r.poolExists) {
					t.Fatalf("unsafe first census removed capacity: %s", joined)
				}
				if failPass == 2 && strings.Contains(joined, "delete --raw /api/v1/namespaces/") {
					t.Fatalf("unsafe final census removed namespace: %s", joined)
				}
				if issue == "none" && (discovery != 2 || !strings.Contains(joined, "wait namespace space --for=delete")) {
					t.Fatalf("cleanup did not verify both passes and deletion: %s", joined)
				}
				if issue == "namespace wait" && (err == nil || !strings.Contains(err.Error(), "namespace deletion pending")) {
					t.Fatalf("pending namespace deletion accepted: %v", err)
				}
			})
		}
	}
}

func TestRestoreRecoveryExcludesOnlyPodMetrics(t *testing.T) {
	c := testConfig(t)
	receipt := (App{Config: c}).restoreRecovery(&restoreState{namespace: "space", token: "token", client: "/client"}, "actor")
	if !strings.Contains(receipt, `case "$RESOURCE" in pods.metrics.k8s.io) continue ;; esac`) ||
		!strings.Contains(receipt, "not evidence of ownership or absence") ||
		!strings.Contains(receipt, "manual inventory may stop until metrics discovery is repaired") ||
		!strings.Contains(receipt, "bounded readiness GETs may retry without incrementing; the two counter POSTs are not retried") ||
		!strings.Contains(receipt, "A timed-out counter request may have executed") {
		t.Fatalf("incorrect recovery inventory: %s", receipt)
	}
}

func TestRestoreNamespaceInventoryFailsClosed(t *testing.T) {
	c := testConfig(t)
	for _, tc := range []struct {
		name string
		list string
		err  error
	}{
		{"unknown object", `{"kind":"SecretList","items":[{"metadata":{"namespace":"space","name":"foreign","uid":"uid"}}]}`, nil},
		{"incomplete", `{"kind":"SecretList","metadata":{"continue":"next"},"items":[]}`, nil},
		{"denied", "", errors.New("access denied")},
		{"null", `null`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := script(t,
				reply{contains: "publish restore-namespace-resources", output: "secrets\n"},
				reply{contains: "get secrets --namespace=space --output=json", output: tc.list, err: tc.err},
			)
			if _, err := (App{Config: c, Runner: r}).restoreNamespaceInventory(t.Context(), &restoreHelper{binary: "/fixture"}, "space", "pool-id", nil); err == nil {
				t.Fatal("unknown namespace inventory was accepted")
			}
		})
	}
}

func TestRestoreNamespaceInventoryRejectsExpiredDiscovery(t *testing.T) {
	c := testConfig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	runner := prerequisiteRunner(func(_ context.Context, cmd Command) ([]byte, error) {
		calls++
		if !strings.Contains(strings.Join(cmd.Args, " "), "publish restore-namespace-resources") {
			t.Fatal("listed resources after discovery expired")
		}
		cancel()
		return []byte("pods\n"), nil
	})
	_, err := (App{Config: c, Runner: runner}).restoreNamespaceInventory(ctx, &restoreHelper{binary: "/fixture"}, "space", "pool-id", nil)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("expired discovery accepted: %v calls=%d", err, calls)
	}
}

func TestRestoreNamespaceRefusesUnexpectedObjects(t *testing.T) {
	var objects []restoreNamespaceObject
	input := `[{"kind":"WorkerPool","metadata":{"name":"counter-microvm","uid":"pool"}},{"kind":"Deployment","metadata":{"name":"counter-microvm","uid":"deployment","ownerReferences":[{"uid":"pool"}]}},{"kind":"Pod","metadata":{"name":"pod","uid":"pod","ownerReferences":[{"uid":"deployment"}]}},{"kind":"Event","metadata":{"name":"event","uid":"event"},"involvedObject":{"uid":"pod"}},{"kind":"ServiceAccount","metadata":{"name":"default","uid":"account"}}]`
	if err := json.Unmarshal([]byte(input), &objects); err != nil {
		t.Fatal(err)
	}
	owned, err := restoreNamespaceOwned(objects, "pool", nil)
	if err != nil || !owned["pod"] || !owned["event"] || !owned["account"] {
		t.Fatalf("owned inventory=%v err=%v", owned, err)
	}
	replacement := append([]restoreNamespaceObject(nil), objects...)
	replacement[len(replacement)-1].Metadata.UID = "replacement"
	if _, err := restoreNamespaceOwned(replacement, "pool", owned); err == nil {
		t.Fatal("replacement default account was accepted")
	}
	var foreign restoreNamespaceObject
	if err := json.Unmarshal([]byte(`{"kind":"Secret","metadata":{"name":"foreign","uid":"foreign"}}`), &foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreNamespaceOwned(append(objects, foreign), "pool", nil); err == nil {
		t.Fatal("foreign object accepted")
	}
}

func TestRestoreAPIDeleteRefusesIdentityReplacement(t *testing.T) {
	c := testConfig(t)
	space := restoreAPIIdentity{Name: "space", UID: "space-id"}
	actor := restoreAPIIdentity{Name: "restore-a", Atespace: space.Name, UID: "actor-id"}
	data := func(name, atespace, uid string) string {
		return fmt.Sprintf(`{"metadata":{"name":%q,"atespace":%q,"uid":%q}}`, name, atespace, uid)
	}
	for _, tc := range []struct {
		name, spaceID, actorID string
	}{
		{"space replaced", "other", ""},
		{"actor replaced", "space-id", "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replies := []reply{{contains: "get atespace space -o json", output: data(space.Name, "", tc.spaceID)}}
			if tc.actorID != "" {
				replies = append(replies, reply{contains: "get actor restore-a -a space -o json", output: data(actor.Name, actor.Atespace, tc.actorID)})
			}
			r := script(t, replies...)
			err := (App{Config: c, Runner: r}).restoreDeleteAPI(t.Context(), "/client", "actor", space, actor)
			if err == nil || !strings.Contains(err.Error(), "identity changed") {
				t.Fatalf("replacement accepted: %v", err)
			}
		})
	}
}

func TestRestoreKubernetesDeleteRequiresOriginalUID(t *testing.T) {
	c := testConfig(t)
	token := "00112233445566778899aabbccddeeff"
	output := func(uid string) string {
		return fmt.Sprintf(`{"kind":"WorkerPool","metadata":{"name":"counter-microvm","namespace":"test-space","uid":%q,"labels":{"substrate-aks.io/check":%q,"substrate-aks.io/installation":%q}}}`, uid, token, c.Installation.ID)
	}
	identity := restoreKubeIdentity{Name: "counter-microvm", Namespace: "test-space", UID: "original"}
	t.Run("replacement", func(t *testing.T) {
		r := script(t, reply{contains: "get workerpools", output: output("replacement")})
		err := (App{Config: c, Runner: r}).restoreDeleteKube(t.Context(), "workerpools", "/apis/ate.dev/v1/namespaces/test-space/workerpools/counter-microvm", "WorkerPool", identity, token)
		if err == nil || !strings.Contains(err.Error(), "identity changed") {
			t.Fatalf("replacement accepted: %v", err)
		}
	})
	t.Run("precondition", func(t *testing.T) {
		r := script(t, reply{contains: "get workerpools", output: output("original")}, reply{contains: "delete --raw", effect: func(cmd Command) {
			var body struct {
				Preconditions struct {
					UID string `json:"uid"`
				} `json:"preconditions"`
			}
			if err := json.Unmarshal(cmd.Input, &body); err != nil || body.Preconditions.UID != identity.UID {
				t.Fatalf("missing UID precondition: %s (%v)", cmd.Input, err)
			}
		}})
		err := (App{Config: c, Runner: r}).restoreDeleteKube(t.Context(), "workerpools", "/apis/ate.dev/v1/namespaces/test-space/workerpools/counter-microvm", "WorkerPool", identity, token)
		if err != nil {
			t.Fatal(err)
		}
	})
}
