package installer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type prerequisiteRunner func(context.Context, Command) ([]byte, error)

func (r prerequisiteRunner) Run(ctx context.Context, c Command) ([]byte, error) { return r(ctx, c) }

func TestDoctorPreservesFailuresAndIncludesRemedies(t *testing.T) {
	missing := errors.New("bicep is missing")
	app := App{Runner: prerequisiteRunner(func(_ context.Context, cmd Command) ([]byte, error) {
		if cmd.Args[0] == "az" && cmd.Args[1] == "bicep" {
			return nil, missing
		}
		return []byte(`{"clientVersion":{"major":"1","minor":"33"}}`), nil
	})}
	err := app.Doctor(t.Context())
	if !errors.Is(err, missing) {
		t.Fatalf("doctor lost the underlying failure: %v", err)
	}
	for _, want := range []string{"Azure Bicep", "az bicep install", "kubectl compatibility", "Install kubectl 1.36-1.38"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("doctor lost the failed check or remedy %q: %v", want, err)
		}
	}
}

func TestRuntimePrerequisitesAreModeAware(t *testing.T) {
	c := testConfig(t)
	if err := (App{Config: c, Runner: script(t)}).RuntimePrerequisites(t.Context()); err == nil {
		t.Fatal("prebuilt images accepted missing runtime assets")
	}
	c.Substrate.Images = ImagesConfig{}
	// The pinned source assembles assets in Go, so no shell tools are probed.
	if err := (App{Config: c, Runner: script(t)}).RuntimePrerequisites(t.Context()); err != nil {
		t.Fatalf("asset assembly required local tools: %v", err)
	}
	c.Substrate.Runtime.AssetsDir = t.TempDir()
	for _, name := range []string{"cloud-hypervisor", "virtiofsd", "vmlinux", "rootfs.img"} {
		if err := os.WriteFile(filepath.Join(c.Substrate.Runtime.AssetsDir, name), []byte("asset"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := (App{Config: c, Runner: script(t)}).RuntimePrerequisites(t.Context()); err != nil {
		t.Fatalf("supplied assets required assembly tooling: %v", err)
	}
	path := filepath.Join(c.Substrate.Runtime.AssetsDir, "rootfs.img")
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if err := (App{Config: c, Runner: script(t)}).RuntimePrerequisites(t.Context()); err == nil {
		t.Fatal("empty supplied asset passed preflight")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("vmlinux", path); err != nil {
		t.Fatal(err)
	}
	if err := (App{Config: c, Runner: script(t)}).RuntimePrerequisites(t.Context()); err == nil {
		t.Fatal("symlink supplied asset passed preflight")
	}
}
