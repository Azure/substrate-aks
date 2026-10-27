package installer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageBuildEngineSelection(t *testing.T) {
	unavailable := errors.New("builder unavailable")
	for _, tc := range []struct {
		name, requested, selected string
		replies                   []reply
	}{
		{"prefer Docker", "", "docker", []reply{{contains: "docker info"}, {contains: "docker buildx version"}}},
		{"fallback from stopped Docker", "auto", "podman", []reply{{contains: "docker info", err: unavailable}, {contains: "podman info"}}},
		{"fallback from missing buildx", "auto", "podman", []reply{{contains: "docker info"}, {contains: "docker buildx version", err: unavailable}, {contains: "podman info"}}},
		{"explicit Podman", "podman", "podman", []reply{{contains: "podman info"}}},
		{"explicit Docker does not fallback", "docker", "", []reply{{contains: "docker info", err: unavailable}}},
		{"neither available", "", "", []reply{{contains: "docker info", err: unavailable}, {contains: "podman info", err: unavailable}}},
		{"invalid selection before commands", "other", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			r := script(t, tc.replies...)
			var progress []string
			engine, env, err := (App{Config: c, Runner: r, ContainerEngine: tc.requested,
				Progress: func(format string, _ ...any) { progress = append(progress, format) }}).imageBuildEngine(t.Context())
			if tc.selected == "" {
				if err == nil || engine != "" || env != nil {
					t.Fatalf("unexpected usable builder: %q, %v, %v", engine, env, err)
				}
				return
			}
			if err != nil || engine != tc.selected || len(progress) == 0 {
				t.Fatalf("incorrect builder selection: %q, %v", engine, err)
			}
			if !strings.Contains(strings.Join(env, "|"), "DOCKER_CONFIG="+filepath.Join(c.workspace(), "docker")) {
				t.Fatal("builder lost isolated credentials")
			}
			if engine == "podman" && !strings.Contains(strings.Join(env, "|"), "REGISTRY_AUTH_FILE="+filepath.Join(c.workspace(), "docker/config.json")) {
				t.Fatal("Podman lost isolated auth file")
			}
		})
	}
}

func TestPodmanIgnoresCallerDockerSettings(t *testing.T) {
	c := testConfig(t)
	if err := os.WriteFile(filepath.Join(os.Getenv("DOCKER_CONFIG"), "config.json"), []byte("invalid caller Docker settings"), 0600); err != nil {
		t.Fatal(err)
	}
	r := script(t, reply{contains: "podman info"})
	if _, _, err := (App{Config: c, Runner: r, ContainerEngine: "podman"}).imageBuildEngine(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestImageBuildEngineCancellationDoesNotFallback(t *testing.T) {
	c := testConfig(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := script(t, reply{contains: "docker info", err: context.Canceled})
	_, _, err := (App{Config: c, Runner: r}).imageBuildEngine(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestAvailableContainerEnginesProbesBothAndRequiresUsableBuildx(t *testing.T) {
	unavailable := errors.New("builder unavailable")
	for _, tc := range []struct {
		name    string
		replies []reply
		want    string
	}{
		{"both", []reply{{contains: "docker info"}, {contains: "docker buildx version"}, {contains: "podman info"}}, "docker,podman"},
		{"Docker only", []reply{{contains: "docker info"}, {contains: "docker buildx version"}, {contains: "podman info", err: unavailable}}, "docker"},
		{"Podman only without buildx", []reply{{contains: "docker info"}, {contains: "docker buildx version", err: unavailable}, {contains: "podman info"}}, "podman"},
		{"neither", []reply{{contains: "docker info", err: unavailable}, {contains: "podman info", err: unavailable}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			r := script(t, tc.replies...)
			engines, err := (App{Config: c, Runner: r}).AvailableContainerEngines(t.Context())
			if strings.Join(engines, ",") != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("incorrect usable engines: %v, %v", engines, err)
			}
		})
	}
}
