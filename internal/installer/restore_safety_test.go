package installer

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRestorePlacementRejectsCustomerWorker(t *testing.T) {
	c := testConfig(t)
	state := &restoreState{namespace: "check-space", token: "token", client: "/client",
		actor: restoreAPIIdentity{Name: "restore-a", Atespace: "check-space", UID: "actor-id"}}
	r := prerequisiteRunner(func(_ context.Context, cmd Command) ([]byte, error) {
		if !strings.Contains(strings.Join(cmd.Args, " "), "get actor restore-a") {
			t.Fatalf("unexpected command: %v", cmd.Args)
		}
		return []byte(fmt.Sprintf(`{"metadata":{"name":%q,"atespace":%q,"uid":%q},"status":{"workerAssignment":{"workerNamespace":"customer","workerPool":"counter-microvm","workerPod":"pod","workerPodUid":"pod-id","worker":{"name":"worker"}}}}`, state.actor.Name, state.actor.Atespace, state.actor.UID)), nil
	})
	if err := (App{Config: c, Runner: r}).restoreCheckPlacement(t.Context(), state); err == nil {
		t.Fatal("foreign worker placement was accepted")
	}
}

func TestRestoreCensusResultRejectsIncompleteOutput(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		ok         bool
	}{
		{"empty", `{"workers":[],"actors":false,"actorTemplates":false}`, true},
		{"additional worker fields", `{"workers":[{"metadata":{"name":"x","uid":"u","resourceVersion":"1"},"status":{"state":"ACTIVE","other":true},"other":true}],"actors":false,"actorTemplates":false}`, true},
		{"missing", `{"workers":[],"actors":false}`, false},
		{"duplicate", `{"workers":[],"actors":false,"actorTemplates":false,"actors":true}`, false},
		{"null", `{"workers":null,"actors":false,"actorTemplates":false}`, false},
		{"unknown", `{"workers":[],"actors":false,"actorTemplates":false,"other":true}`, false},
		{"wrong key case", `{"Workers":[],"actors":false,"actorTemplates":false}`, false},
		{"nested duplicate", `{"workers":[{"metadata":{"name":"x","name":"y"}}],"actors":false,"actorTemplates":false}`, false},
		{"wrong type", `{"workers":[],"actors":"false","actorTemplates":false}`, false},
		{"truncated", `{"workers":[]`, false},
		{"extra", `{"workers":[],"actors":false,"actorTemplates":false}{}`, false},
		{"oversized", strings.Repeat(" ", restoreInventoryBytes+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := restoreCensusResult([]byte(tc.data))
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}

func TestRestoreWorkersSafeRejectsForeignPlacementAndUnknownInventory(t *testing.T) {
	c := testConfig(t)
	token := "00112233445566778899aabbccddeeff"
	worker := func(namespace string, assigned int) string {
		return fmt.Sprintf(`{"metadata":{"name":"worker","uid":"uid"},"workerNamespace":%q,"workerPool":"counter-microvm","workerPod":"pod","workerPodUid":"pod-id","labels":{"substrate-aks.io/check":%q,"substrate-aks.io/installation":%q},"status":{"state":"WORKER_STATE_ACTIVE","allocated":{"actors":%d}}}`, namespace, token, c.Installation.ID, assigned)
	}
	for _, tc := range []struct {
		name, workers         string
		actors, templates, ok bool
	}{
		{"owned idle", worker("space", 0), false, false, true},
		{"foreign worker", worker("other", 0), false, false, false},
		{"assigned worker", worker("space", 1), false, false, false},
		{"unknown worker", `null`, false, false, false},
		{"unknown allocation", strings.Replace(worker("space", 0), `"allocated":{"actors":0}`, `"allocated":null`, 1), false, false, false},
		{"foreign actor", "", true, false, false},
		{"foreign template", "", false, true, false},
		{"allocated CPU", strings.Replace(worker("space", 0), `"actors":0`, `"actors":0,"cpuMillicores":"1"`, 1), false, false, false},
		{"allocated memory", strings.Replace(worker("space", 0), `"actors":0`, `"actors":0,"memoryBytes":"1"`, 1), false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workers := "[]"
			if tc.workers != "" {
				workers = "[" + tc.workers + "]"
			}
			output := fmt.Sprintf(`{"workers":%s,"actors":%t,"actorTemplates":%t}`, workers, tc.actors, tc.templates)
			state := &restoreState{namespace: "space", token: token, helper: &restoreHelper{binary: "/fixture"}}
			_, err := (App{Config: c, Runner: script(t, reply{contains: "publish restore-census", output: output})}).restoreWorkersSafe(t.Context(), state)
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}

func TestRestoreWorkersSafeRejectsChangedIdentity(t *testing.T) {
	c := testConfig(t)
	state := &restoreState{namespace: "space", token: "token", helper: &restoreHelper{binary: "/fixture"},
		workers: map[string]restoreWorkerIdentity{"worker": {uid: "original", pod: "pod", podUID: "pod-id"}}}
	output := fmt.Sprintf(`{"workers":[{"metadata":{"name":"worker","uid":"changed"},"workerNamespace":"space","workerPool":"counter-microvm","workerPod":"pod","workerPodUid":"pod-id","labels":{"substrate-aks.io/check":"token","substrate-aks.io/installation":%q},"status":{"state":"WORKER_STATE_ACTIVE"}}],"actors":false,"actorTemplates":false}`, c.Installation.ID)
	_, err := (App{Config: c, Runner: script(t, reply{contains: "publish restore-census", output: output})}).restoreWorkersSafe(t.Context(), state)
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("changed worker accepted: %v", err)
	}
}
