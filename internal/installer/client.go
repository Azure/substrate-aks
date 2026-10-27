package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
)

// ClientSetup describes the shared upstream client and its installation-specific Kubernetes target.
type ClientSetup struct {
	Kubeconfig string
	Context    string
	BinDir     string
}

func (a App) SetupClient(ctx context.Context) (ClientSetup, error) {
	i, err := a.infrastructure(ctx)
	if err != nil {
		return ClientSetup{}, err
	}
	root, err := a.prepare(ctx, i)
	if err != nil {
		return ClientSetup{}, err
	}
	if err := a.credentials(ctx); err != nil {
		return ClientSetup{}, err
	}
	if err := a.guardInstall(ctx, fingerprint(a.Config, i), "client"); err != nil {
		return ClientSetup{}, err
	}

	env := []string{"GOOS=" + runtime.GOOS, "GOARCH=" + runtime.GOARCH}
	data, err := a.Runner.Run(ctx, Command{
		Dir:  root,
		Args: []string{"go", "env", "-json", "GOBIN", "GOPATH"},
		Env:  env,
	})
	if err != nil {
		return ClientSetup{}, fmt.Errorf("locating the Go binary directory: %w", err)
	}
	var goEnv struct{ GOBIN, GOPATH string }
	if err := json.Unmarshal(data, &goEnv); err != nil {
		return ClientSetup{}, fmt.Errorf("reading the Go binary directory: %w", err)
	}
	binDir := goEnv.GOBIN
	if binDir == "" {
		paths := filepath.SplitList(goEnv.GOPATH)
		if len(paths) == 0 || paths[0] == "" {
			return ClientSetup{}, errors.New("go env returned neither GOBIN nor GOPATH; cannot locate kubectl-ate")
		}
		binDir = filepath.Join(paths[0], "bin")
	}
	if !filepath.IsAbs(binDir) {
		return ClientSetup{}, fmt.Errorf("Go binary directory must be absolute: %q", binDir)
	}
	a.progress("Installing the pinned kubectl-ate client in " + binDir)
	if _, err := a.Runner.Run(ctx, Command{
		Dir: root, Args: []string{"go", "install", "./cmd/kubectl-ate"}, Env: env,
	}); err != nil {
		return ClientSetup{}, fmt.Errorf("installing kubectl-ate: %w", err)
	}
	return ClientSetup{
		Kubeconfig: a.kubeconfig(),
		Context:    a.Config.AKS.ClusterName,
		BinDir:     binDir,
	}, nil
}
