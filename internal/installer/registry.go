package installer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

type Registry struct {
	Name                 string            `json:"name"`
	LoginServer          string            `json:"loginServer"`
	Tags                 map[string]string `json:"tags"`
	AdminUserEnabled     bool              `json:"adminUserEnabled"`
	AnonymousPullEnabled bool              `json:"anonymousPullEnabled"`
}

type BuildResult struct {
	Images    ImagesConfig
	AssetsDir string
}

func (a App) registry(ctx context.Context) (Registry, error) {
	if err := a.account(ctx); err != nil {
		return Registry{}, err
	}
	if err := a.ownedGroup(ctx, true); err != nil {
		return Registry{}, err
	}
	a.progress("Finding the installation's private container registry")
	data, err := a.az(ctx, "acr", "list", "--resource-group", a.Config.Azure.ResourceGroup)
	if err != nil {
		return Registry{}, err
	}
	var registries []Registry
	if err := json.Unmarshal(data, &registries); err != nil {
		return Registry{}, fmt.Errorf("reading ACR resources: %w", err)
	}
	var owned []Registry
	for _, registry := range registries {
		if registry.Tags[ownershipTag] == a.Config.Installation.ID {
			owned = append(owned, registry)
		}
	}
	if len(owned) != 1 {
		return Registry{}, errors.New("expected one owned ACR; run provision --registry-only before images")
	}
	r := owned[0]
	if !regexp.MustCompile(`^[a-z0-9]{5,50}$`).MatchString(r.Name) ||
		!regexp.MustCompile(`^[a-z0-9-]+\.azurecr\.io$`).MatchString(r.LoginServer) ||
		!(strings.HasPrefix(r.LoginServer, r.Name+".") || strings.HasPrefix(r.LoginServer, r.Name+"-")) ||
		r.AdminUserEnabled || r.AnonymousPullEnabled {
		return Registry{}, errors.New("owned ACR has an invalid login server or permits admin/anonymous access")
	}
	return r, nil
}

func (a App) loginRegistry(ctx context.Context, r Registry) (string, error) {
	dir, config, err := a.registryConfig()
	if err != nil {
		return "", err
	}
	a.progress("Signing in to %s with your Azure identity", r.LoginServer)
	data, err := a.Runner.Run(ctx, Command{
		Args:            []string{"az", "acr", "login", "--name", r.Name, "--subscription", a.Config.Azure.SubscriptionID, "--only-show-errors", "--expose-token", "--output", "json"},
		SensitiveOutput: true,
	})
	if err != nil {
		return "", fmt.Errorf("ACR login failed; verify Azure CLI authentication and registry access (new role grants may need time to propagate): %w", err)
	}
	var token struct {
		AccessToken string `json:"accessToken"`
		LoginServer string `json:"loginServer"`
	}
	if err := json.Unmarshal(data, &token); err != nil || token.AccessToken == "" || token.LoginServer != r.LoginServer {
		return "", errors.New("ACR token response was missing credentials or did not match the owned registry; refresh az login and retry")
	}
	auths := map[string]json.RawMessage{}
	if raw, ok := config["auths"]; ok {
		if err := json.Unmarshal(raw, &auths); err != nil || auths == nil {
			return "", errors.New("isolated registry credentials are invalid; refusing to overwrite them")
		}
	}
	auths[r.LoginServer], err = json.Marshal(map[string]string{
		"auth": base64.StdEncoding.EncodeToString([]byte("00000000-0000-0000-0000-000000000000:" + token.AccessToken)),
	})
	if err != nil {
		return "", err
	}
	config["auths"], err = json.Marshal(auths)
	if err != nil {
		return "", err
	}
	if err := writeRegistryConfig(dir, config); err != nil {
		return "", fmt.Errorf("saving isolated ACR credentials: %w", err)
	}
	return dir, nil
}

func (a App) registryConfig() (string, map[string]json.RawMessage, error) {
	dir := filepath.Join(a.Config.workspace(), "docker")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", nil, err
	}
	config := map[string]json.RawMessage{}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err == nil {
		if err := json.Unmarshal(data, &config); err != nil || config == nil {
			return "", nil, errors.New("isolated registry config.json is invalid; refusing to overwrite its credentials")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	return dir, config, nil
}

func (a App) fetchPinnedSource(ctx context.Context, dir string) error {
	a.progress("Fetching Substrate source at the configured commit")
	for _, args := range [][]string{
		{"git", "init", "--quiet", "."},
		{"git", "fetch", "--quiet", "--depth=1", a.Config.Substrate.Source.Repository, a.Config.Substrate.Source.Commit},
		{"git", "checkout", "--quiet", "--detach", "FETCH_HEAD"},
	} {
		if _, err := a.Runner.Run(ctx, Command{Dir: dir, Args: args}); err != nil {
			return err
		}
	}
	head, err := a.Runner.Run(ctx, Command{Dir: dir, Args: []string{"git", "rev-parse", "HEAD"}})
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(head)) != a.Config.Substrate.Source.Commit {
		return errors.New("fetched source does not match configured commit")
	}
	if _, err := os.Stat(filepath.Join(dir, "internal/plugins/azure-blob/config/kustomization.yaml")); err != nil {
		return fmt.Errorf("selected source has no Azure Blob plugin deployment contract: %w", err)
	}
	return nil
}

// Only the disposable build checkout receives this startup adaptation.
func prepareImagePublisher(root string) error {
	path := filepath.Join(root, "cmd/ate-setup/internal/cmd/root.go")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading upstream publisher startup: %w", err)
	}
	const clusterSetup = "\t\tif err := cfg.EnsureClusterCredentials(cmd.Context()); err != nil {\n\t\t\treturn err\n\t\t}\n"
	if strings.Count(string(data), clusterSetup) != 1 {
		return errors.New("upstream publisher startup changed; update the Azure image-build adapter before publishing")
	}
	const publishSetup = "\t\tif cmd.Parent() == publishCmd {\n\t\t\tenv = &steps.Env{Cfg: cfg}\n\t\t\treturn nil\n\t\t}\n\n"
	adapted := strings.Replace(string(data), clusterSetup, publishSetup+clusterSetup, 1)
	if err := os.WriteFile(path, []byte(adapted), 0600); err != nil {
		return fmt.Errorf("adapting upstream publisher startup: %w", err)
	}
	return nil
}

func (a App) BuildImages(ctx context.Context) (BuildResult, error) {
	a.progress("Checking image bootstrap configuration")
	c := a.Config
	if c.Substrate.Images != (ImagesConfig{}) {
		return BuildResult{}, errors.New("images are already configured; use images --check to verify reuse. Image bootstrap does not implement upgrades or overwrite an existing image set; after complete teardown use images --reset-config")
	}
	r, err := a.registry(ctx)
	if err != nil {
		return BuildResult{}, err
	}
	engine, env, err := a.imageBuildEngine(ctx)
	if err != nil {
		return BuildResult{}, err
	}
	_, err = a.loginRegistry(ctx, r)
	if err != nil {
		return BuildResult{}, err
	}
	root, err := os.MkdirTemp(c.workspace(), "build-")
	if err != nil {
		return BuildResult{}, err
	}
	defer os.RemoveAll(root)
	if err := a.fetchPinnedSource(ctx, root); err != nil {
		return BuildResult{}, err
	}
	a.progress("Preparing the image publisher without cluster dependencies")
	if err := prepareImagePublisher(root); err != nil {
		return BuildResult{}, err
	}
	if engine == "podman" {
		if err := preparePodmanPublisher(root); err != nil {
			return BuildResult{}, err
		}
	}
	tag := "azure-" + c.Substrate.Source.Commit[:12] + "-" + time.Now().UTC().Format("20060102t150405.000000000")
	env = append(env, "VERSION="+tag)
	repo := r.LoginServer + "/substrate"
	a.progress("Building and pushing Substrate platform and demo images; this can take several minutes")
	_, err = a.Runner.Run(ctx, Command{
		Dir: root, Env: env,
		Args: []string{"go", "run", "./cmd/ate-setup", "publish", "release-images",
			"--no-dev-env", "--ko-docker-repo", repo, "--ko-default-platforms", "linux/amd64"},
	})
	if err != nil {
		return BuildResult{}, err
	}
	a.progress("Building and pushing the Azure Blob plugin image")
	pluginRepo := repo + "/plugins/objectstore/azure-blob"
	plugin, err := a.Runner.Run(ctx, Command{
		Dir: filepath.Join(root, "internal/plugins/azure-blob"),
		Env: append(append([]string{}, env...), "KO_DOCKER_REPO="+pluginRepo, "KO_DEFAULTPLATFORMS=linux/amd64"),
		Args: []string{"go", "run", "github.com/google/ko@v0.19.1", "build", "--bare",
			"--tags", tag, "./cmd/azure-blob-objectstore-plugin"},
	})
	if err != nil {
		return BuildResult{}, err
	}
	image, err := pluginDigestReference(string(plugin), pluginRepo, tag)
	if err != nil {
		return BuildResult{}, err
	}
	assetsDir := c.Substrate.Runtime.AssetsDir
	if assetsDir == "" {
		a.progress("Downloading and assembling the pinned amd64 microVM runtime assets")
		assetsDir = filepath.Join(c.workspace(), "assets")
		if _, err := a.Runner.Run(ctx, Command{
			Dir: root, Args: []string{"bash", "hack/microvm-assets/assemble.sh"},
			Env: []string{"ARCH=amd64", "OUT=" + assetsDir},
		}); err != nil {
			return BuildResult{}, err
		}
	}
	a.progress("Verifying runtime asset checksums")
	if err := verifyRuntimeAssets(root, assetsDir); err != nil {
		return BuildResult{}, err
	}
	return BuildResult{
		Images: ImagesConfig{Repository: repo, Tag: tag, Plugin: image}, AssetsDir: assetsDir,
	}, nil
}

func pluginDigestReference(output, repository, tag string) (string, error) {
	name, digest, ok := strings.Cut(strings.TrimSpace(output), "@")
	image := repository + "@" + digest
	if !ok || (name != repository && name != repository+":"+tag) || !digestPattern.MatchString(image) {
		return "", errors.New("plugin publisher did not return a digest-pinned image in the expected owned registry repository")
	}
	return image, nil
}

func SaveBuildResult(path string, expected Config, result BuildResult) error {
	return updateImageConfig(path, expected, func(c *Config) error {
		c.Substrate.Images = result.Images
		c.Substrate.Runtime.AssetsDir = result.AssetsDir
		if err := c.ValidateInstall(); err != nil {
			return err
		}
		if result.AssetsDir == "" {
			return errors.New("image building did not produce a runtime asset directory")
		}
		return nil
	})
}

func updateImageConfig(path string, expected Config, update func(*Config) error) error {
	current, err := LoadConfig(path)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, expected) {
		return errors.New("configuration changed during the image operation; refusing to overwrite it")
	}
	c, err := readConfig(path)
	if err != nil {
		return err
	}
	if err := update(&c); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".installation-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	if err := errors.Join(writeErr, syncErr, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
