package installer

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
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
	if err := a.credentials(ctx); err != nil {
		return ClientSetup{}, err
	}
	if err := a.guardInstall(ctx, fingerprint(a.Config, i), "client"); err != nil {
		return ClientSetup{}, err
	}
	root, err := a.prepare(ctx, i)
	if err != nil {
		return ClientSetup{}, err
	}

	binDir, err := a.clientBinDir(ctx, root)
	if err != nil {
		return ClientSetup{}, err
	}
	env := []string{"GOOS=" + runtime.GOOS, "GOARCH=" + runtime.GOARCH}
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

func (a App) clientBinDir(ctx context.Context, root string) (string, error) {
	data, err := a.Runner.Run(ctx, Command{
		Dir:  root,
		Args: []string{"go", "env", "-json", "GOBIN", "GOPATH"},
		Env:  []string{"GOOS=" + runtime.GOOS, "GOARCH=" + runtime.GOARCH},
	})
	if err != nil {
		return "", fmt.Errorf("locating the Go binary directory: %w", err)
	}
	var goEnv struct{ GOBIN, GOPATH string }
	if err := json.Unmarshal(data, &goEnv); err != nil {
		return "", fmt.Errorf("reading the Go binary directory: %w", err)
	}
	binDir := goEnv.GOBIN
	if binDir == "" {
		paths := filepath.SplitList(goEnv.GOPATH)
		if len(paths) == 0 || paths[0] == "" {
			return "", errors.New("go env returned neither GOBIN nor GOPATH; cannot locate kubectl-ate")
		}
		binDir = filepath.Join(paths[0], "bin")
	}
	if !filepath.IsAbs(binDir) {
		return "", fmt.Errorf("Go binary directory must be absolute: %q", binDir)
	}
	return binDir, nil
}

func (a App) matchingClient(ctx context.Context) (string, error) {
	binDir, err := a.clientBinDir(ctx, a.Config.workspace())
	if err != nil {
		return "", fmt.Errorf("PREREQUISITE: cannot locate matching kubectl-ate (%w); run substrate-aks client %s", err, a.Config.Installation.Name)
	}
	name := "kubectl-ate"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(binDir, name)
	info, err := os.Stat(path)
	if err == nil && (!info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0) {
		err = errors.New("client is not an executable regular file")
	}
	if err == nil {
		var build *debug.BuildInfo
		build, err = buildinfo.ReadFile(path)
		if err == nil {
			err = validateClientBuild(build, a.Config.Substrate.Source.Commit)
		}
	}
	if err != nil {
		return "", fmt.Errorf("PREREQUISITE: matching kubectl-ate is unavailable (%w); run substrate-aks client %s", err, a.Config.Installation.Name)
	}
	return path, nil
}

func validateClientBuild(info *debug.BuildInfo, commit string) error {
	if info == nil || info.Path != "go.ate.dev/substrate/cmd/kubectl-ate" {
		return errors.New("binary is not the Substrate client")
	}
	settings := make(map[string]string, len(info.Settings))
	for _, entry := range info.Settings {
		settings[entry.Key] = entry.Value
	}
	if settings["vcs.revision"] != commit || settings["GOOS"] != runtime.GOOS || settings["GOARCH"] != runtime.GOARCH {
		return errors.New("client build does not match the configured revision and host")
	}
	return nil
}
