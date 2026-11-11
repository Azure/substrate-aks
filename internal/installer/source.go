package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type prepared struct {
	Fingerprint string            `json:"fingerprint"`
	Files       map[string]string `json:"files"`
}

func fingerprint(c Config, i Infrastructure) string {
	// Preserve existing deployment fingerprints independently of the config layout.
	type sourceSettings struct {
		Source
		PluginSource Source `json:"pluginSource"`
		ImageRepo    string `json:"imageRepo"`
		ImageTag     string `json:"imageTag"`
		PluginImage  string `json:"pluginImage"`
		AssetsDir    string `json:"assetsDir"`
	}
	source := sourceSettings{
		c.Substrate.Source, c.Substrate.PluginSource, c.Substrate.Images.Repository, c.Substrate.Images.Tag,
		c.Substrate.Images.Plugin, c.Substrate.Runtime.AssetsDir,
	}
	data, _ := json.Marshal(struct {
		Source         sourceSettings
		Infrastructure Infrastructure
		Adapter        int
	}{source, i, 6})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (a App) prepare(ctx context.Context, i Infrastructure) (string, error) {
	a.progress("Validating the configured source and images")
	if err := a.Config.ValidateInstall(); err != nil {
		return "", err
	}
	fp := fingerprint(a.Config, i)
	// Demo-only adaptations invalidate the cache without changing the installed fingerprint.
	dest := filepath.Join(a.Config.workspace(), "source-"+fp+"-v2")
	markerPath := filepath.Join(dest, ".substrate-aks-prepared.json")
	if data, err := os.ReadFile(markerPath); err == nil {
		a.progress("Verifying the cached managed source checkout")
		var p prepared
		if err := json.Unmarshal(data, &p); err != nil {
			return "", err
		}
		if p.Fingerprint != fp || len(p.Files) != 6 {
			return "", errors.New("managed source marker is inconsistent; do not modify managed checkouts")
		}
		for rel, want := range p.Files {
			if !filepath.IsLocal(rel) {
				return "", errors.New("invalid path in prepared-source marker")
			}
			data, err := os.ReadFile(filepath.Join(dest, rel))
			if err != nil {
				return "", err
			}
			if checksum(data) != want {
				return "", fmt.Errorf("managed source changed at %s; remove %s to fetch it again instead of editing cached source", rel, dest)
			}
		}
		head, err := a.Runner.Run(ctx, Command{Dir: dest, Args: []string{"git", "rev-parse", "HEAD"}})
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(string(head)) != a.Config.Substrate.Source.Commit {
			return "", fmt.Errorf("managed source HEAD changed; remove %s to fetch it again", dest)
		}
		diff, err := a.Runner.Run(ctx, Command{Dir: dest, Args: []string{"git", "diff", "--name-only", "HEAD", "--"}})
		if err != nil {
			return "", err
		}
		for _, path := range strings.Split(strings.TrimSpace(string(diff)), "\n") {
			if path != "" {
				if _, ok := p.Files[path]; !ok {
					return "", fmt.Errorf("unexpected tracked source modification at %s; remove %s to fetch it again", path, dest)
				}
			}
		}
		// Include ignored files: Git ignore rules do not exclude Go build inputs.
		untracked, err := a.Runner.Run(ctx, Command{Dir: dest, Args: []string{"git", "ls-files", "--others", "-z", "--"}})
		if err != nil {
			return "", err
		}
		for _, path := range strings.Split(string(untracked), "\x00") {
			if path != "" && path != ".substrate-aks-prepared.json" {
				return "", fmt.Errorf("unexpected untracked source file at %q; remove %s to fetch it again", path, dest)
			}
		}
		return dest, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("managed source directory %s exists without a completion marker; remove it to fetch it again", dest)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(a.Config.workspace(), 0700); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(a.Config.workspace(), "fetch-")
	if err != nil {
		return "", err
	}
	// stage is an explicitly created temporary checkout, never a user checkout.
	defer os.RemoveAll(stage)
	if err := a.fetchPinnedSource(ctx, stage); err != nil {
		return "", err
	}
	// The plugin checkout is read for its components, never built or kept.
	pluginRoot, err := os.MkdirTemp(a.Config.workspace(), "plugin-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(pluginRoot)
	if err := a.fetchPluginSource(ctx, pluginRoot); err != nil {
		return "", err
	}
	plugin, err := pluginComponents(pluginRoot, a.Config, i)
	if err != nil {
		return "", err
	}
	a.progress("Adapting Substrate manifests for this Azure installation")
	paths, err := adaptSource(stage, a.Config, plugin)
	if err != nil {
		return "", err
	}
	p := prepared{Fingerprint: fp, Files: map[string]string{}}
	for _, rel := range paths {
		data, err := os.ReadFile(filepath.Join(stage, rel))
		if err != nil {
			return "", err
		}
		p.Files[rel] = checksum(data)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(stage, ".substrate-aks-prepared.json"), data, 0600); err != nil {
		return "", err
	}
	if err := os.Rename(stage, dest); err != nil {
		return "", err
	}
	return dest, nil
}

const routerClientSPIFFEID = "spiffe://cluster.local/ns/ate-system/sa/atenet-client"

// Placeholders for ate-setup's GCS-only snapshot transfer account settings.
const (
	transferPlaceholderProject = "substrate-aks-unused"
	transferPlaceholderAccount = "unused"
)

func (a App) upstream(ctx context.Context, root string, args ...string) error {
	c := a.Config
	env := []string{
		"NO_DEV_ENV=1", "KUBECONFIG=" + a.kubeconfig(), "VERSION=" + c.Substrate.Images.Tag,
	}
	databaseEnv, err := a.databaseEnvironment(ctx)
	if err != nil {
		return err
	}
	env = append(env, databaseEnv...)
	host, _, _ := strings.Cut(c.Substrate.Images.Repository, "/")
	if strings.HasSuffix(host, ".azurecr.io") {
		r, err := a.registry(ctx)
		if err != nil {
			return err
		}
		if host == r.LoginServer {
			dockerDir, err := a.loginRegistry(ctx, r)
			if err != nil {
				return err
			}
			env = append(env, "DOCKER_CONFIG="+dockerDir)
		}
	}
	command := []string{"go", "run", "./cmd/ate-setup"}
	command = append(command, args...)
	command = append(command,
		"--context", c.AKS.ClusterName, "--kubeconfig", a.kubeconfig(),
		"--no-dev-env", "--namespace", "ate-system",
		"--record-dir", filepath.Join(c.workspace(), "records"),
		"--rollout-timeout", "10m",
		"--image-repo", c.Substrate.Images.Repository, "--image-tag", c.Substrate.Images.Tag,
		"--ko-default-platforms", "linux/amd64",
		"--snapshot-location", "azblob://snapshots/cluster/",
		"--credential-provider", `{"name":"k8s.io"}`,
		"--ateapi-postgres-storage-class", "managed-csi",
		// kubectl ate proxy presents a pod certificate for this ServiceAccount;
		// atenet-router admits no ingress client unless it is allowed.
		"--ingress-allowed-spiffe-ids", routerClientSPIFFEID,
		// ate-setup requires a GCS snapshot transfer account on every cluster
		// but kind. It only writes the email into a ConfigMap key read by the
		// in-tree sidecar's arguments, which the Azure plugin component
		// replaces, so these placeholders are never used.
		"--gcp-project-id", transferPlaceholderProject,
		"--snapshot-transfer-gsa-name", transferPlaceholderAccount)
	if args[1] == "ate-system" {
		// ate-setup would stage microVM assets to GCS; the installer uploads
		// them to Blob storage and applies the microvm SandboxConfig itself.
		command = append(command, "--sandbox-classes", "gvisor")
	}
	a.progress("Deploying %s and waiting for workloads to become ready (up to 10 minutes per rollout)", strings.Join(args[1:], " "))
	_, err = a.Runner.Run(ctx, Command{
		Dir: root, Args: command, Env: env, SensitiveOutput: c.ManagedDatabase(),
	})
	return err
}
