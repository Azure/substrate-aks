package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type restoreReconcileRunner struct {
	base                *restoreCheckRunner
	phase               string
	client              []byte
	foreign             bool
	fingerprintMismatch bool
	calls               []string
}

func (r *restoreReconcileRunner) Run(ctx context.Context, cmd Command) ([]byte, error) {
	command := strings.Join(cmd.Args, " ")
	r.calls = append(r.calls, command)
	switch {
	case strings.Contains(command, "get configmap substrate-aks-install"):
		id := r.base.c.Installation.ID
		if r.foreign {
			id = "foreign"
		}
		fp := fingerprint(r.base.c, testInfrastructure())
		if r.fingerprintMismatch {
			fp = "foreign"
		}
		return []byte(fmt.Sprintf(`{"metadata":{"name":"substrate-aks-install","namespace":"ate-system"},"data":{"installationId":%q,"fingerprint":%q,"phase":%q}}`, id, fp, r.phase)), nil
	case strings.Contains(command, "publish check-images"):
		return []byte(`{"status":"available","checked":3,"expected":3}`), nil
	case strings.Contains(command, "kubectl") && strings.Contains(command, " apply -f -"):
		var list struct {
			Items []struct {
				Data map[string]string `json:"data"`
			} `json:"items"`
		}
		if json.Unmarshal(cmd.Input, &list) == nil {
			for _, item := range list.Items {
				if item.Data["phase"] != "" {
					r.phase = item.Data["phase"]
				}
			}
		}
		return nil, nil
	case strings.Contains(command, "rollout status daemonset"), strings.Contains(command, "delete daemonset"), strings.Contains(command, "storage blob upload"), strings.Contains(command, "go run ./cmd/ate-setup deploy"):
		return nil, nil
	case strings.Contains(command, "go install ./cmd/kubectl-ate"):
		return nil, os.WriteFile(filepath.Join(r.base.clientDir, "kubectl-ate"), r.client, 0700)
	case strings.Contains(command, "publish restore-worker-pools") && r.base.restore.poolExists:
		return []byte(`{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[{"metadata":{"name":"leftover","namespace":"space","uid":"uid"}}]}`), nil
	}
	return r.base.Run(ctx, cmd)
}

func TestRestoreInstallClientAndCheckReconcile(t *testing.T) {
	dir, revision := restoreTestClient(t)
	client, err := os.ReadFile(filepath.Join(dir, "kubectl-ate"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "kubectl-ate")); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t)
	c.Substrate.Source.Commit = revision
	c.Substrate.Images.Repository = "example.invalid/images"
	c.Substrate.Runtime.AssetsDir = t.TempDir()
	root := writeRestorePreparedFixture(t, c)
	writeAssetFixture(t, root, c.Substrate.Runtime.AssetsDir)
	recovery := &restoreRunner{config: c}
	base := &restoreCheckRunner{c: c, clientDir: dir, restore: recovery}
	r := &restoreReconcileRunner{base: base, phase: "installing:ate-system", client: client}
	var messages []string
	a := App{Config: c, Runner: r, Progress: func(f string, args ...any) { messages = append(messages, fmt.Sprintf(f, args...)) }}
	lock, err := Lock(c)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := a.Install(t.Context(), "ate-system"); err != nil {
		t.Fatalf("reconciling install: %v", err)
	}
	if r.phase != "deployed" {
		t.Fatalf("phase=%q", r.phase)
	}
	if _, err := a.SetupClient(t.Context()); err != nil {
		t.Fatalf("repairing client: %v", err)
	}
	recovery.poolExists = true
	if err := a.Check(t.Context(), false); err != nil {
		t.Fatalf("checking busy installation: %v", err)
	}
	if !strings.Contains(strings.Join(messages, "\n"), "RESTORE_SKIPPED_BUSY") {
		t.Fatal("busy installation did not skip restore")
	}
}
