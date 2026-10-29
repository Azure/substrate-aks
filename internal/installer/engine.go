package installer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func ValidateContainerEngine(engine string) error {
	switch engine {
	case "", "auto", "docker", "podman":
		return nil
	default:
		return fmt.Errorf("unknown container engine %q; choose auto, docker or podman", engine)
	}
}

func (a App) DetectContainerEngine(ctx context.Context) (string, error) {
	lock, err := Lock(a.Config)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	engine, _, err := a.imageBuildEngine(ctx)
	return engine, err
}

func (a App) AvailableContainerEngines(ctx context.Context) ([]string, error) {
	lock, err := Lock(a.Config)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	builders, err := a.imageBuildEngines(ctx, true)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(builders))
	for i, builder := range builders {
		names[i] = builder.name
	}
	return names, nil
}

type imageBuilder struct {
	name string
	env  []string
}

func (a App) imageBuildEngine(ctx context.Context) (string, []string, error) {
	builders, err := a.imageBuildEngines(ctx, false)
	if err != nil {
		return "", nil, err
	}
	builder := builders[0]
	a.progress("Using %s for Dockerfile image builds (Go images use ko)", builder.name)
	return builder.name, builder.env, nil
}

func (a App) imageBuildEngines(ctx context.Context, all bool) ([]imageBuilder, error) {
	if err := ValidateContainerEngine(a.ContainerEngine); err != nil {
		return nil, err
	}
	dir, _, err := a.registryConfig()
	if err != nil {
		return nil, err
	}
	engines := []string{"docker", "podman"}
	if a.ContainerEngine == "docker" || a.ContainerEngine == "podman" {
		engines = []string{a.ContainerEngine}
	}
	var failures []error
	var available []imageBuilder
	for _, engine := range engines {
		a.progress("Checking %s image builder", engine)
		env := []string{"DOCKER_CONFIG=" + dir, "REGISTRY_AUTH_FILE=" + filepath.Join(dir, "config.json"), "NO_DEV_ENV=1"}
		probe := func() error {
			probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if engine == "docker" {
				if _, err := a.prepareDockerConfig(); err != nil {
					return err
				}
				var err error
				env, err = dockerBuildEnvironment(dir)
				if err != nil {
					return err
				}
				for _, args := range [][]string{{"docker", "info", "--format", "{{json .ServerVersion}}"}, {"docker", "buildx", "version"}} {
					if _, err := a.Runner.Run(probeCtx, Command{Args: args, Env: env}); err != nil {
						return err
					}
				}
				a.progress("Starting and checking the selected Docker buildx builder (up to 2 minutes; may download or start BuildKit)")
				bootstrapCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				output, err := a.Runner.Run(bootstrapCtx, Command{Args: []string{"docker", "buildx", "inspect", "--bootstrap"}, Env: env})
				if err != nil {
					return err
				}
				// Buildx inspect can report node errors while exiting successfully.
				ready := false
				for _, line := range strings.Split(string(output), "\n") {
					key, value, _ := strings.Cut(strings.TrimSpace(line), ":")
					if key == "Error" {
						return fmt.Errorf("selected Docker buildx builder is not ready: %s", strings.TrimSpace(value))
					}
					if key == "Status" && strings.TrimSpace(value) == "running" {
						ready = true
					}
				}
				if !ready {
					return errors.New("selected Docker buildx builder did not report a running node")
				}
				return nil
			}
			_, err := a.Runner.Run(probeCtx, Command{Args: []string{"podman", "info", "--format", "json"}, Env: env})
			return err
		}
		if err := probe(); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			failures = append(failures, fmt.Errorf("%s: %w", engine, err))
			a.progress("%s image builder unavailable; %v", engine, err)
			continue
		}
		available = append(available, imageBuilder{name: engine, env: env})
		if !all {
			break
		}
		a.progress("%s image builder is available", engine)
	}
	if len(available) != 0 {
		return available, nil
	}
	return nil, fmt.Errorf("no usable image builder: start Docker with buildx or Podman (on macOS, start its machine); use --container-engine=docker|podman to select explicitly: %w", errors.Join(failures...))
}
