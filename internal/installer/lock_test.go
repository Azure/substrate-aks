//go:build linux || darwin

package installer

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestLockForSharesOnlyAHeldInstallationLock(t *testing.T) {
	c := testConfig(t)
	other := testConfig(t)
	held, err := Lock(c)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := LockFor(t.Context(), c); !errors.Is(err, ErrInstallationInUse) {
		t.Fatalf("a context without the held lock locked again: %v", err)
	}
	// An equivalent path names the same installation.
	alias := c
	alias.Installation.Dir = filepath.Join(c.Installation.Dir, ".")
	ctx := WithHeldLock(t.Context(), alias)
	shared, err := LockFor(ctx, c)
	if err != nil {
		t.Fatalf("the held lock was not shared: %v", err)
	}
	if err := shared.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(c); !errors.Is(err, ErrInstallationInUse) {
		t.Fatalf("closing a shared lock released the holder's lock: %v", err)
	}
	// The held lock covers only its own installation.
	own, err := LockFor(ctx, other)
	if err != nil {
		t.Fatalf("another installation was not locked: %v", err)
	}
	if _, err := Lock(other); !errors.Is(err, ErrInstallationInUse) {
		t.Fatalf("LockFor did not lock another installation: %v", err)
	}
	if err := own.Close(); err != nil {
		t.Fatal(err)
	}
}
