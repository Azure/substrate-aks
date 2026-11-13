package installer

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestRestoreLimitsAndFreshNames(t *testing.T) {
	if restoreReadyTimeout != 10*time.Minute || restoreCallTimeout != 2*time.Minute ||
		restoreSuspendTimeout != 5*time.Minute || restoreCleanupTimeout != 10*time.Minute {
		t.Fatal("restore operation budgets changed")
	}
	first, actor, token, err := restoreNames()
	if err != nil {
		t.Fatal(err)
	}
	second, nextActor, _, err := restoreNames()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || actor == nextActor ||
		!regexp.MustCompile(`^substrate-aks-check-[0-9a-f]{32}$`).MatchString(first) ||
		!regexp.MustCompile(`^restore-[0-9a-f]{32}$`).MatchString(actor) ||
		first != "substrate-aks-check-"+token {
		t.Fatalf("restore identities are not fresh and scoped")
	}
}

func TestRestoreRenderKeepsMemoryAndIsolatesWorkerSelection(t *testing.T) {
	pool := []byte(`apiVersion: v1
kind: Namespace
metadata:
  name: ate-demo-counter-microvm
---
apiVersion: ate.dev/v1
kind: WorkerPool
metadata:
  name: counter-microvm
  namespace: ate-demo-counter-microvm
  labels:
    workload: counter-microvm
spec:
  replicas: 2
  workerImage: ko://go.ate.dev/substrate/cmd/ateworker-microvm
  template:
    resources: {}
`)
	template := []byte(`metadata:
  atespace: ate-demo-counter-microvm
  name: counter-microvm
workerSelector:
  matchLabels:
    workload: counter-microvm
snapshotConfig:
  preferredFidelity: SNAPSHOT_FIDELITY_MEMORY
containers:
  - name: counter
    image: ko://go.ate.dev/substrate/demos/counter
`)
	ns := "substrate-aks-check-0123456789abcdef0123456789abcdef"
	token := "0123456789abcdef0123456789abcdef"
	gotPool, gotTemplate, err := renderRestoreManifests(pool, template, ns, "install-id", token)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gotPool)+string(gotTemplate), "ate-demo-counter-microvm") {
		t.Fatal("original namespace retained")
	}
	pools := 0
	if err := inspectYAML(gotPool, func(doc object) error {
		if doc["kind"] == "WorkerPool" {
			pools++
			meta, _ := child(doc, "metadata")
			labels, _ := child(meta, "labels")
			if meta["namespace"] != ns || labels[restoreInvocationLabel] != token || labels["workload"] != restorePoolName {
				t.Fatalf("incorrect pool identity: %v", meta)
			}
			podLabels, err := nested(doc, "spec", "template", "labels")
			if err != nil || podLabels[restoreInvocationLabel] != token || podLabels[restoreInstallationLabel] != "install-id" {
				t.Fatalf("missing worker pod ownership labels: %v, %v", podLabels, err)
			}
		}
		return nil
	}); err != nil || pools != 1 {
		t.Fatalf("pool manifest: %v", err)
	}
	var result object
	if err := yaml.Unmarshal(gotTemplate, &result); err != nil {
		t.Fatal(err)
	}
	selector, err := nested(result, "workerSelector", "matchLabels")
	if err != nil || selector[restoreInvocationLabel] != token || selector["workload"] != restorePoolName {
		t.Fatalf("incorrect selector: %v, %v", selector, err)
	}
	for _, invalid := range [][]byte{
		[]byte(strings.Replace(string(template), "SNAPSHOT_FIDELITY_MEMORY", "SNAPSHOT_FIDELITY_ROOTFS", 1)),
		[]byte(strings.Replace(string(template), "workload: counter-microvm", "workload: other", 1)),
	} {
		if _, _, err := renderRestoreManifests(pool, invalid, ns, "install-id", token); err == nil {
			t.Fatal("accepted unexpected counter template")
		}
	}
}
