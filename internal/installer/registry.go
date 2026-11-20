package installer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
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
	stop := a.step(ctx, "images.registry")
	r, err := a.registry(ctx)
	stop(err)
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
	stop = a.step(ctx, "images.login")
	dockerDir, err := a.loginRegistry(ctx, r)
	stop(err)
	if err != nil {
		return BuildResult{}, err
	}
	root, err := os.MkdirTemp(c.workspace(), "build-")
	if err != nil {
		return BuildResult{}, err
	}
	defer os.RemoveAll(root)
	stop = a.step(ctx, "images.source_fetch")
	err = a.fetchPinnedSource(ctx, root)
	stop(err)
	if err != nil {
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
		stop = a.step(ctx, "images.source_build")
		_, err := a.Runner.Run(ctx, Command{
			Dir: root, Env: append(append([]string{}, env...), "VERSION="+tag),
			Args: []string{"go", "run", "./cmd/ate-setup", "publish", "release-images",
				"--no-dev-env", "--ko-docker-repo", repo, "--ko-default-platforms", "linux/amd64"},
		})
		stop(err)
		if err != nil {
			return BuildResult{}, err
		}
	}
	ko, err := a.koBinary(ctx, root)
	if err != nil {
		return BuildResult{}, err
	}
	koEnv := []string{"DOCKER_CONFIG=" + dockerDir, "KO_DEFAULTPLATFORMS=linux/amd64"}
	a.progress("Building and pushing the counter demo image, which releases do not publish")
	stop = a.step(ctx, "images.demo_build")
	_, err = a.Runner.Run(ctx, Command{
		Dir: root, Env: append(append([]string{}, koEnv...), "KO_DOCKER_REPO="+repo),
		Args: []string{ko, "build", "--base-import-paths", "--tags", tag, "./" + demoImagePackage},
	})
	stop(err)
	if err != nil {
		return BuildResult{}, err
	}
	pluginRoot, err := os.MkdirTemp(c.workspace(), "plugin-")
	if err != nil {
		return BuildResult{}, err
	}
	defer os.RemoveAll(pluginRoot)
	stop = a.step(ctx, "images.plugin_fetch")
	err = a.fetchPluginSource(ctx, pluginRoot)
	stop(err)
	if err != nil {
		return BuildResult{}, err
	}
	a.progress("Building and pushing the Azure Blob plugin image")
	pluginRepo := repo + "/plugins/objectstore/azure-blob"
	stop = a.step(ctx, "images.plugin_build")
	plugin, err := a.Runner.Run(ctx, Command{
		Dir:  pluginRoot,
		Env:  append(append([]string{}, koEnv...), "KO_DOCKER_REPO="+pluginRepo),
		Args: []string{ko, "build", "--bare", "--tags", tag, "./cmd/azure-blob-objectstore-plugin"},
	})
	stop(err)
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
		stop = a.step(ctx, "images.assets_download")
		_, err := a.Runner.Run(ctx, Command{
			Dir: root, Env: []string{"NO_DEV_ENV=1"},
			Args: []string{"go", "run", "./cmd/ate-setup", "publish", "microvm-assets",
				filepath.Join(root, sandboxTemplate), assetsDir, "amd64", "--no-dev-env", "--no-report-defaults"},
		})
		stop(err)
		if err != nil {
			return BuildResult{}, err
		}
	}
	a.progress("Verifying runtime asset checksums")
	stop = a.step(ctx, "images.assets_verify")
	_, err = verifyRuntimeAssets(root, assetsDir)
	stop(err)
	if err != nil {
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
	stop := a.step(ctx, "images.release_inventory")
	data, err := a.Runner.Run(ctx, Command{
		Dir: root, Env: []string{"NO_DEV_ENV=1"},
		Args: []string{"go", "run", "./cmd/ate-setup", "publish", "release-inventory",
			DefaultReleaseImages, tag, a.Config.Substrate.Source.Commit, "--no-dev-env", "--no-report-defaults"},
	})
	stop(err)
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
	return a.importImages(ctx, r, prefix, tag, inventory)
}

// Release images import a few at a time. An ACR import is a server-side copy,
// so the installer mostly waits on Azure; a small pool overlaps those waits
// without starting many Azure CLI processes at once.
const (
	importWorkers    = 4
	maxImportWorkers = 8
)

// importResult is one image's outcome and the command output it buffered.
type importResult struct {
	index        int
	err          error
	log, verbose *bytes.Buffer
}

// importImages imports and verifies every inventory image with a bounded
// worker pool. It attempts every image and reports all failures in inventory
// order. Command logs are buffered per image and written in inventory order,
// so concurrent imports never interleave in the log or verbose output.
func (a App) importImages(ctx context.Context, r Registry, prefix, tag string, inventory []releaseImage) error {
	seen := make(map[string]bool, len(inventory))
	for _, image := range inventory {
		if !imageNamePattern.MatchString(image.Name) || !digestPattern.MatchString("image@"+image.Digest) {
			return errors.New("the pinned release inventory returned an invalid image reference")
		}
		if seen[image.Name] {
			return fmt.Errorf("the pinned release inventory lists %s more than once; nothing was imported", image.Name)
		}
		seen[image.Name] = true
	}
	workers := a.importWorkers
	if workers <= 0 {
		workers = importWorkers
	}
	workers = max(1, min(workers, maxImportWorkers, len(inventory)))
	a.progress("Importing %d release images into %s, up to %d at a time", len(inventory), r.LoginServer, workers)
	stop := TimingsFrom(ctx).StepWorkers("images.imports", workers)
	err := a.runImports(ctx, r, prefix, tag, inventory, workers)
	stop(err)
	return err
}

// runImports runs the pool. An ExecRunner is copied per image with buffered
// logs; any other Runner is shared and must be safe for concurrent use.
func (a App) runImports(ctx context.Context, r Registry, prefix, tag string, inventory []releaseImage, workers int) error {
	base, buffered := a.Runner.(ExecRunner)
	jobs := make(chan int)
	results := make(chan importResult)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				if ctx.Err() != nil {
					// Cancelled after scheduling: leave the image unstarted.
					continue
				}
				worker, result := a, importResult{index: i}
				if buffered {
					runner := base
					if base.Log != nil {
						result.log = &bytes.Buffer{}
						runner.Log = result.log
					}
					if base.Verbose != nil {
						result.verbose = &bytes.Buffer{}
						runner.Verbose = result.verbose
					}
					worker.Runner = runner
				}
				result.err = worker.importImage(ctx, r, prefix, tag, inventory[i])
				results <- result
			}
		})
	}
	go func() {
		defer close(jobs)
		for i := range inventory {
			if ctx.Err() != nil {
				return
			}
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	pending := make([]*importResult, len(inventory))
	errs := make([]error, len(inventory))
	next, completed, failed := 0, 0, 0
	flush := func(result *importResult) {
		name := inventory[result.index].Name
		for _, out := range []struct {
			dst io.Writer
			buf *bytes.Buffer
		}{{base.Log, result.log}, {base.Verbose, result.verbose}} {
			if out.dst != nil && out.buf != nil && out.buf.Len() > 0 {
				fmt.Fprintf(out.dst, "[image %s]\n", name)
				out.dst.Write(out.buf.Bytes())
			}
		}
	}
	for result := range results {
		completed++
		name := inventory[result.index].Name
		if result.err != nil {
			failed++
			errs[result.index] = result.err
			a.progress("Could not import and verify %s (%d/%d)", name, completed, len(inventory))
		} else {
			a.progress("Imported and verified %s (%d/%d)", name, completed, len(inventory))
		}
		pending[result.index] = &result
		for next < len(pending) && pending[next] != nil {
			flush(pending[next])
			pending[next] = nil
			next++
		}
	}
	// After a cancellation, images that never started leave gaps.
	for _, result := range pending {
		if result != nil {
			flush(result)
		}
	}
	var failure error
	switch failed {
	case 0:
	case 1:
		for _, err := range errs {
			if err != nil {
				failure = err
			}
		}
	default:
		failure = fmt.Errorf("%d of %d release images were not imported and verified:\n%w", failed, len(inventory), errors.Join(errs...))
	}
	if completed < len(inventory) {
		cause := ctx.Err()
		if cause == nil {
			cause = errors.New("an import worker stopped")
		}
		return errors.Join(fmt.Errorf("importing release images stopped after %d of %d: %w", completed, len(inventory), cause), failure)
	}
	return failure
}

// importImage imports one image by digest and verifies the digest the
// registry recorded.
func (a App) importImage(ctx context.Context, r Registry, prefix, tag string, image releaseImage) error {
	target := prefix + "/" + image.Name + ":" + tag
	// Imports are server-side and keep the digest; --force only retags the
	// verified digest on a retry.
	stop := a.step(ctx, "images.import."+image.Name)
	_, err := a.az(ctx, "acr", "import", "--name", r.Name,
		"--source", DefaultReleaseImages+"/"+image.Name+"@"+image.Digest,
		"--image", target, "--force")
	stop(err)
	if err != nil {
		return fmt.Errorf("importing %s (the upload identity needs Container Registry Data Importer and Data Reader on the owned registry; provision grants it, and new grants can take a few minutes to propagate): %w", image.Name, err)
	}
	stop = a.step(ctx, "images.verify."+image.Name)
	shown, err := a.az(ctx, "acr", "repository", "show", "--name", r.Name, "--image", target)
	if err != nil {
		err = fmt.Errorf("verifying imported %s: %w", image.Name, err)
		stop(err)
		return err
	}
	var manifest struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(shown, &manifest); err != nil || manifest.Digest != image.Digest {
		err = fmt.Errorf("imported %s does not have the verified release digest", target)
		stop(err)
		return err
	}
	stop(nil)
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
