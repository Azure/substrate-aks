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
	if err := a.fetchSource(ctx, dir, a.Config.Substrate.Source); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, sandboxTemplate)); err != nil {
		return fmt.Errorf("selected source has no microVM SandboxConfig template: %w", err)
	}
	return nil
}

func (a App) fetchPluginSource(ctx context.Context, dir string) error {
	a.progress("Fetching the Azure Blob object-store plugin at the configured commit")
	if err := a.fetchSource(ctx, dir, a.Config.Substrate.PluginSource); err != nil {
		return err
	}
	for _, rel := range []string{pluginComponent, pluginAssetsComponent, "cmd/azure-blob-objectstore-plugin"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			return fmt.Errorf("selected plugin source has no %s deployment contract: %w", rel, err)
		}
	}
	return nil
}

// fetchSource checks out s.Commit into dir. A configured tag must name the
// same commit; the tag is never trusted on its own.
func (a App) fetchSource(ctx context.Context, dir string, s Source) error {
	ref := s.Commit
	if s.Tag != "" {
		ref = "+refs/tags/" + s.Tag + ":refs/tags/" + s.Tag
	}
	for _, args := range [][]string{
		{"git", "init", "--quiet", "."},
		{"git", "fetch", "--quiet", "--depth=1", s.Repository, ref},
	} {
		if _, err := a.Runner.Run(ctx, Command{Dir: dir, Args: args}); err != nil {
			return err
		}
	}
	if s.Tag != "" {
		tagged, err := a.Runner.Run(ctx, Command{Dir: dir, Args: []string{"git", "rev-parse", "--verify", "refs/tags/" + s.Tag + "^{commit}"}})
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(tagged)) != s.Commit {
			return fmt.Errorf("tag %s in %s names %s, not the configured commit %s", s.Tag, s.Repository, strings.TrimSpace(string(tagged)), s.Commit)
		}
	}
	if _, err := a.Runner.Run(ctx, Command{Dir: dir, Args: []string{"git", "checkout", "--quiet", "--detach", s.Commit}}); err != nil {
		return err
	}
	head, err := a.Runner.Run(ctx, Command{Dir: dir, Args: []string{"git", "rev-parse", "HEAD"}})
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(head)) != s.Commit {
		return errors.New("fetched source does not match configured commit")
	}
	return nil
}

// sourceImageTag names images built from source. ate-setup publishes only
// release-style tags, and the timestamp keeps rebuilds from overwriting them.
func sourceImageTag(s Source, now time.Time) string {
	base := s.Tag
	if base == "" {
		base = "v0.0.0"
	}
	return base + "-azure." + s.Commit[:12] + "." + now.UTC().Format("20060102t150405")
}

func (a App) BuildImages(ctx context.Context) (BuildResult, error) {
	a.progress("Checking image bootstrap configuration")
	c := a.Config
	if c.Substrate.Images != (ImagesConfig{}) {
		return BuildResult{}, errors.New("images are already configured; use images --check to verify reuse. Image bootstrap does not implement upgrades or overwrite an existing image set; after complete teardown use images --reset-config")
	}
	source := c.EffectiveImageSource()
	if source == ImageSourceRelease && c.Substrate.Source.Tag == "" {
		return BuildResult{}, errors.New(`release images need substrate.source.tag naming the release of substrate.source.commit; set it, or set substrate.imageSource to "source" to build the commit`)
	}
	r, err := a.registry(ctx)
	if err != nil {
		return BuildResult{}, err
	}
	var engine string
	var env []string
	if source == ImageSourceBuild {
		engine, env, err = a.imageBuildEngine(ctx)
		if err != nil {
			return BuildResult{}, err
		}
	}
	dockerDir, err := a.loginRegistry(ctx, r)
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
	if err := prepareHelper(root); err != nil {
		return BuildResult{}, err
	}
	repo := r.LoginServer + "/substrate"
	var tag string
	if source == ImageSourceRelease {
		tag = c.Substrate.Source.Tag
		if err := a.importReleaseImages(ctx, root, r, repo, tag); err != nil {
			return BuildResult{}, err
		}
	} else {
		if engine == "podman" {
			if err := preparePodmanPublisher(root); err != nil {
				return BuildResult{}, err
			}
		}
		tag = sourceImageTag(c.Substrate.Source, time.Now())
		a.progress("Building and pushing Substrate release images from source; this can take several minutes")
		if _, err := a.Runner.Run(ctx, Command{
			Dir: root, Env: append(append([]string{}, env...), "VERSION="+tag),
			Args: []string{"go", "run", "./cmd/ate-setup", "publish", "release-images",
				"--no-dev-env", "--ko-docker-repo", repo, "--ko-default-platforms", "linux/amd64"},
		}); err != nil {
			return BuildResult{}, err
		}
	}
	ko, err := a.koBinary(ctx, root)
	if err != nil {
		return BuildResult{}, err
	}
	koEnv := []string{"DOCKER_CONFIG=" + dockerDir, "KO_DEFAULTPLATFORMS=linux/amd64"}
	a.progress("Building and pushing the counter demo image, which releases do not publish")
	if _, err := a.Runner.Run(ctx, Command{
		Dir: root, Env: append(append([]string{}, koEnv...), "KO_DOCKER_REPO="+repo),
		Args: []string{ko, "build", "--base-import-paths", "--tags", tag, "./" + demoImagePackage},
	}); err != nil {
		return BuildResult{}, err
	}
	pluginRoot, err := os.MkdirTemp(c.workspace(), "plugin-")
	if err != nil {
		return BuildResult{}, err
	}
	defer os.RemoveAll(pluginRoot)
	if err := a.fetchPluginSource(ctx, pluginRoot); err != nil {
		return BuildResult{}, err
	}
	a.progress("Building and pushing the Azure Blob plugin image")
	pluginRepo := repo + "/plugins/objectstore/azure-blob"
	plugin, err := a.Runner.Run(ctx, Command{
		Dir:  pluginRoot,
		Env:  append(append([]string{}, koEnv...), "KO_DOCKER_REPO="+pluginRepo),
		Args: []string{ko, "build", "--bare", "--tags", tag, "./cmd/azure-blob-objectstore-plugin"},
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
			Dir: root, Env: []string{"NO_DEV_ENV=1"},
			Args: []string{"go", "run", "./cmd/ate-setup", "publish", "microvm-assets",
				filepath.Join(root, sandboxTemplate), assetsDir, "amd64", "--no-dev-env", "--no-report-defaults"},
		}); err != nil {
			return BuildResult{}, err
		}
	}
	a.progress("Verifying runtime asset checksums")
	if _, err := verifyRuntimeAssets(root, assetsDir); err != nil {
		return BuildResult{}, err
	}
	return BuildResult{
		Images: ImagesConfig{Repository: repo, Tag: tag, Plugin: image}, AssetsDir: assetsDir,
	}, nil
}

// demoImagePackage is the demo the installer deploys. Releases do not publish
// it, so every image source builds it into the owned registry.
const demoImagePackage = "demos/counter"

// koBinary locates the ko the pinned source pins for its own builds.
func (a App) koBinary(ctx context.Context, root string) (string, error) {
	data, err := a.Runner.Run(ctx, Command{Dir: filepath.Join(root, "hack/tools/ko"), Args: []string{"go", "tool", "-n", "ko"}})
	if err != nil {
		return "", fmt.Errorf("locating the pinned ko tool: %w", err)
	}
	binary := strings.TrimSpace(string(data))
	if !filepath.IsAbs(binary) {
		return "", errors.New("the pinned ko tool path is not absolute")
	}
	return binary, nil
}

type releaseImage struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

var imageNamePattern = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*$`)

// importReleaseImages copies every image the pinned release publishes into the
// owned registry by digest, after the pinned source's own inventory confirms
// that each one was built from the configured commit.
func (a App) importReleaseImages(ctx context.Context, root string, r Registry, repo, tag string) error {
	a.progress("Verifying that the %s release images were built from the configured commit", tag)
	data, err := a.Runner.Run(ctx, Command{
		Dir: root, Env: []string{"NO_DEV_ENV=1"},
		Args: []string{"go", "run", "./cmd/ate-setup", "publish", "release-inventory",
			DefaultReleaseImages, tag, a.Config.Substrate.Source.Commit, "--no-dev-env", "--no-report-defaults"},
	})
	if err != nil {
		return fmt.Errorf("verifying release images (nothing was imported): %w", err)
	}
	var inventory []releaseImage
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&inventory); err != nil || len(inventory) < 3 {
		return errors.New("the pinned release inventory returned no usable image list")
	}
	prefix := strings.TrimPrefix(repo, r.LoginServer+"/")
	for _, image := range inventory {
		if !imageNamePattern.MatchString(image.Name) || !digestPattern.MatchString("image@"+image.Digest) {
			return errors.New("the pinned release inventory returned an invalid image reference")
		}
		target := prefix + "/" + image.Name + ":" + tag
		a.progress("Importing %s into %s", image.Name, r.LoginServer)
		// Imports are server-side and keep the digest; --force only retags the
		// verified digest on a retry.
		if _, err := a.az(ctx, "acr", "import", "--name", r.Name,
			"--source", DefaultReleaseImages+"/"+image.Name+"@"+image.Digest,
			"--image", target, "--force"); err != nil {
			return fmt.Errorf("importing %s (the upload identity needs Container Registry Data Importer and Data Reader on the owned registry; provision grants it, and new grants can take a few minutes to propagate): %w", image.Name, err)
		}
		shown, err := a.az(ctx, "acr", "repository", "show", "--name", r.Name, "--image", target)
		if err != nil {
			return err
		}
		var manifest struct {
			Digest string `json:"digest"`
		}
		if err := json.Unmarshal(shown, &manifest); err != nil || manifest.Digest != image.Digest {
			return fmt.Errorf("imported %s does not have the verified release digest", target)
		}
	}
	return nil
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
