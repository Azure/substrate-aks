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

func TestRuntimePrerequisitesCheckArchiveCapability(t *testing.T) {
	c := testConfig(t)
	c.Substrate.Images = ImagesConfig{}
	missing := errors.New("tar does not support --zstd")
	r := prerequisiteRunner(func(_ context.Context, command Command) ([]byte, error) {
		if strings.Join(command.Args, " ") == "zstd --compress --stdout" {
			if len(command.Input) == 0 {
				t.Fatal("compression probe received no tar archive")
			}
			return []byte("compressed probe"), nil
		}
		if strings.Join(command.Args, " ") == "tar --zstd -tf -" {
			if string(command.Input) != "compressed probe" {
				t.Fatal("tar did not receive the compression probe")
			}
			return nil, missing
		}
		return nil, nil
	})
	if err := (App{Config: c, Runner: r}).RuntimePrerequisites(t.Context()); !errors.Is(err, missing) {
		t.Fatalf("version-only checks missed incompatible tar: %v", err)
	}
}
