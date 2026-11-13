package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreFailedCheckReconcilesAndSkipsLeftovers(t *testing.T) {
	dir, revision := restoreTestClient(t)
	client, err := os.ReadFile(filepath.Join(dir, "kubectl-ate"))
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig(t)
	c.Substrate.Source.Commit = revision
	c.Substrate.Images.Repository = "example.invalid/images"
	c.Substrate.Runtime.AssetsDir = t.TempDir()
	root := writeRestorePreparedFixture(t, c)
	writeAssetFixture(t, root, c.Substrate.Runtime.AssetsDir)
	recovery := &restoreRunner{config: c, failures: map[string]error{" create actor-template ": errors.New("temporary failure")}}
	base := &restoreCheckRunner{c: c, clientDir: dir, restore: recovery}
	r := &restoreReconcileRunner{base: base, phase: "deployed", client: client}
	var messages []string
	a := App{Config: c, Runner: r, Progress: func(f string, args ...any) { messages = append(messages, fmt.Sprintf(f, args...)) },
		RecoveryReceipt: func(context.Context, string) error { return nil }}
	if err := a.Check(t.Context(), false); err == nil || !recovery.poolExists {
		t.Fatalf("failure did not retain uncertain state: %v", err)
	}
	before := len(recovery.calls)
	if err := a.Install(t.Context(), "ate-system"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetupClient(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(messages, "\n"), "RESTORE_SKIPPED_BUSY") {
		t.Fatal("retry did not skip leftovers")
	}
	for _, call := range recovery.calls[before:] {
		if strings.Contains(call, " delete ") {
			t.Fatalf("retry swept leftover: %s", call)
		}
	}
}

func TestRestoreReconcileRefusesForeignInstallations(t *testing.T) {
	for _, mismatched := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatched), func(t *testing.T) {
			c := testConfig(t)
			base := &restoreCheckRunner{c: c, restore: &restoreRunner{config: c}}
			r := &restoreReconcileRunner{base: base, phase: "deployed", foreign: !mismatched, fingerprintMismatch: mismatched}
			err := (App{Config: c, Runner: r}).Install(t.Context(), "ate-system")
			if err == nil || !strings.Contains(err.Error(), "differ") {
				t.Fatalf("foreign record accepted: %v", err)
			}
			for _, call := range r.calls {
				if strings.Contains(call, "git fetch") || strings.Contains(call, " apply ") {
					t.Fatalf("mutation after foreign record: %s", call)
				}
			}
		})
	}
}
