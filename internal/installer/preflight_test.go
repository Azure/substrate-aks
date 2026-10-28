package installer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type prerequisiteRunner func(context.Context, Command) ([]byte, error)

func (r prerequisiteRunner) Run(ctx context.Context, c Command) ([]byte, error) { return r(ctx, c) }

func TestRuntimePrerequisitesAreModeAware(t *testing.T) {
	c := testConfig(t)
	missing := errors.New("zstd missing")
	r := prerequisiteRunner(func(_ context.Context, cmd Command) ([]byte, error) {
		if cmd.Args[0] == "zstd" {
			return nil, missing
		}
		return nil, nil
	})
	if err := (App{Config: c, Runner: r}).RuntimePrerequisites(t.Context()); err == nil {
		t.Fatal("prebuilt images accepted missing runtime assets")
	}
	c.Substrate.Images = ImagesConfig{}
	if err := (App{Config: c, Runner: r}).RuntimePrerequisites(t.Context()); !errors.Is(err, missing) {
		t.Fatalf("assembly tool failure was not surfaced: %v", err)
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
