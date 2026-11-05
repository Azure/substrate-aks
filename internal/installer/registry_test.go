package installer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func registryReply(c Config) reply {
	return reply{contains: "acr list", output: `[{
		"name":"substratetest123","loginServer":"substratetest123.azurecr.io",
		"tags":{"` + ownershipTag + `":"` + c.Installation.ID + `"},
		"adminUserEnabled":false,"anonymousPullEnabled":false
	}]`}
}

func acrLoginReply() reply {
	return reply{contains: "acr login", output: `{"loginServer":"substratetest123.azurecr.io","accessToken":"synthetic-acr-token"}`}
}

func TestRegistryBootstrapKeepsFullDeploymentSeparate(t *testing.T) {
	c := testConfig(t)
	r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
		reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`})
	if err := (App{Config: c, Runner: r}).ProvisionRegistry(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(r.calls[len(r.calls)-1].Args, " ")
	if !strings.Contains(args, "--name "+c.deploymentName()+"-registry ") {
		t.Fatalf("registry bootstrap would overwrite the full deployment outputs: %s", args)
	}
	data, err := os.ReadFile(filepath.Join(c.workspace(), "infra/parameters.json"))
	if err != nil {
		t.Fatal(err)
	}
	var params struct {
		Parameters map[string]struct {
			Value any `json:"value"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(data, &params); err != nil {
		t.Fatal(err)
	}
	if params.Parameters["registryOnly"].Value != true {
		t.Fatal("registry bootstrap would also provision AKS")
	}
}

func TestRegistryRejectsMissingForeignAndInsecureResources(t *testing.T) {
	c := testConfig(t)
	valid := registryReply(c).output
	for _, data := range []string{
		`[]`,
		strings.Replace(valid, c.Installation.ID, "other", 1),
		strings.Replace(valid, "substratetest123.azurecr.io", "untrusted.example.com", 1),
		strings.Replace(valid, `"adminUserEnabled":false`, `"adminUserEnabled":true`, 1),
		strings.Replace(valid, `"anonymousPullEnabled":false`, `"anonymousPullEnabled":true`, 1),
	} {
		r := script(t, accountReply(c), reply{contains: "group exists", output: "true"}, ownerReply(c),
			reply{contains: "acr list", output: data})
		if _, err := (App{Config: c, Runner: r}).registry(t.Context()); err == nil {
			t.Fatalf("accepted missing, unowned or insecure registry: %s", data)
		}
	}
}

func TestACRLoginWithoutDockerPreservesIsolatedCredentials(t *testing.T) {
	c := testConfig(t)
	t.Setenv("PATH", "")
	dir := filepath.Join(c.workspace(), "docker")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const before = `{"auths":{"other.example":{"auth":"preserve-me"}},"currentContext":"selected"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	r := script(t, acrLoginReply())
	got, err := (App{Config: c, Runner: r}).loginRegistry(t.Context(), Registry{Name: "substratetest123", LoginServer: "substratetest123.azurecr.io"})
	if err != nil || got != dir {
		t.Fatalf("Docker-free login failed: %q, %v", got, err)
	}
	call := r.calls[0]
	if !call.SensitiveOutput || !strings.Contains(strings.Join(call.Args, " "), "--expose-token --output json") ||
		strings.Contains(strings.Join(call.Args, " "), "synthetic-acr-token") {
		t.Fatal("login requires Docker or exposes the token")
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Auths          map[string]struct{ Auth string }
		CurrentContext string
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	auth, err := base64.StdEncoding.DecodeString(config.Auths["substratetest123.azurecr.io"].Auth)
	if err != nil || string(auth) != "00000000-0000-0000-0000-000000000000:synthetic-acr-token" ||
		config.Auths["other.example"].Auth != "preserve-me" || config.CurrentContext != "selected" {
		t.Fatal("login lost owned ACR credentials or changed unrelated isolated settings")
	}
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("login credentials are not private")
	}
}

func TestACRLoginRejectsInvalidTokenWithoutReplacingCredentials(t *testing.T) {
	for _, response := range []string{
		`{"accessToken":"private-token","loginServer":"foreign.azurecr.io"}`,
		`{"accessToken":"","loginServer":"substratetest123.azurecr.io"}`,
		`{"accessToken":"private-token",`,
	} {
		c := testConfig(t)
		dir := filepath.Join(c.workspace(), "docker")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		const before = `{"auths":{"other.example":{"auth":"preserve"}}}`
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(before), 0600); err != nil {
			t.Fatal(err)
		}
		r := script(t, reply{contains: "acr login", output: response})
		_, err := (App{Config: c, Runner: r}).loginRegistry(t.Context(), Registry{Name: "substratetest123", LoginServer: "substratetest123.azurecr.io"})
		if err == nil || strings.Contains(err.Error(), "private-token") {
			t.Fatalf("invalid token accepted or disclosed: %v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != before {
			t.Fatal("invalid token replaced existing credentials")
		}
	}
}

// fetchReplies scripts a pinned checkout of s, including the tag check.
func fetchReplies(s Source) []reply {
	replies := []reply{{contains: "git init"}, {contains: "git fetch --quiet --depth=1 " + s.Repository}}
	if s.Tag != "" {
		replies = append(replies, reply{contains: "git rev-parse --verify refs/tags/" + s.Tag + "^{commit}", output: s.Commit + "\n"})
	}
	return append(replies, reply{contains: "git checkout --quiet --detach " + s.Commit}, reply{contains: "git rev-parse HEAD", output: s.Commit})
}

// imageBuildRunner gives each scripted checkout the files a real pinned
// source or plugin checkout contains.
type imageBuildRunner struct {
	*scriptedRunner
	assetsDir string
}

func (r imageBuildRunner) Run(ctx context.Context, c Command) ([]byte, error) {
	if strings.Join(c.Args, " ") == "git rev-parse HEAD" {
		if strings.HasPrefix(filepath.Base(c.Dir), "plugin-") {
			if err := os.CopyFS(c.Dir, os.DirFS("testdata/plugin")); err != nil {
				r.t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(c.Dir, "cmd/azure-blob-objectstore-plugin"), 0700); err != nil {
				r.t.Fatal(err)
			}
		} else {
			writeAssetFixture(r.t, c.Dir, r.assetsDir)
			for _, dir := range []string{"cmd", "steps", "images"} {
				if err := os.MkdirAll(filepath.Join(c.Dir, "cmd/ate-setup/internal", dir), 0700); err != nil {
					r.t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(c.Dir, "cmd/ate-setup/internal/steps/publish.go"),
				[]byte("images.PublishDockerfileImage(ctx, args)\n"), 0600); err != nil {
				r.t.Fatal(err)
			}
			context := filepath.Join(c.Dir, "cmd/dataplane/envoy")
			if err := os.MkdirAll(context, 0700); err != nil {
				r.t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(context, "Dockerfile"),
				[]byte("FROM --platform=$BUILDPLATFORM rust:bookworm@sha256:synthetic AS builder\nFROM envoyproxy/envoy:v1.39.5@sha256:synthetic\n"), 0600); err != nil {
				r.t.Fatal(err)
			}
		}
	}
	return r.scriptedRunner.Run(ctx, c)
}

const testKo = "/go/bin/ko"

var testReleaseInventory = []releaseImage{
	{"ateapi", "sha256:" + strings.Repeat("1", 64)},
	{"atelet", "sha256:" + strings.Repeat("2", 64)},
	{"envoy-dataplane", "sha256:" + strings.Repeat("3", 64)},
}

func releaseImportReplies(t *testing.T, c Config, digest func(releaseImage) string) []reply {
	data, err := json.Marshal(testReleaseInventory)
	if err != nil {
		t.Fatal(err)
	}
	replies := []reply{{contains: "publish release-inventory " + DefaultReleaseImages + " " + c.Substrate.Source.Tag + " " + c.Substrate.Source.Commit, output: string(data)}}
	for _, image := range testReleaseInventory {
		replies = append(replies,
			reply{contains: "acr import --name substratetest123 --source " + DefaultReleaseImages + "/" + image.Name + "@" + image.Digest +
				" --image substrate/" + image.Name + ":" + c.Substrate.Source.Tag + " --force"},
			reply{contains: "acr repository show --name substratetest123 --image substrate/" + image.Name + ":" + c.Substrate.Source.Tag,
				output: `{"digest":"` + digest(image) + `"}`})
	}
	return replies
}

// extraImageReplies scripts the images every source builds: the demo,
// which releases omit, and the Azure plugin.
func extraImageReplies(c Config, tag string) []reply {
	const registry = "substratetest123.azurecr.io"
	replies := []reply{
		{contains: "go tool -n ko", output: testKo + "\n"},
		{contains: testKo + " build --base-import-paths --tags " + tag + " ./demos/counter"},
	}
	replies = append(replies, fetchReplies(c.Substrate.PluginSource)...)
	return append(replies,
		reply{contains: testKo + " build --bare --tags " + tag + " ./cmd/azure-blob-objectstore-plugin",
			output: registry + "/substrate/plugins/objectstore/azure-blob@sha256:" + strings.Repeat("a", 64)},
		reply{contains: "publish microvm-assets"})
}

func TestReleaseImagesImportVerifiedDigests(t *testing.T) {
	c := testConfig(t)
	c.Substrate.Images = ImagesConfig{}
	t.Setenv("PATH", "")
	replies := []reply{accountReply(c), {contains: "group exists", output: "true"}, ownerReply(c), registryReply(c), acrLoginReply()}
	replies = append(replies, fetchReplies(c.Substrate.Source)...)
	replies = append(replies, releaseImportReplies(t, c, func(image releaseImage) string { return image.Digest })...)
	replies = append(replies, extraImageReplies(c, c.Substrate.Source.Tag)...)
	r := script(t, replies...)
	assetsDir := filepath.Join(c.workspace(), "assets")
	result, err := (App{Config: c, Runner: imageBuildRunner{r, assetsDir}, ContainerEngine: "docker"}).BuildImages(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	const registry = "substratetest123.azurecr.io"
	if result.Images != (ImagesConfig{Repository: registry + "/substrate", Tag: DefaultTag,
		Plugin: registry + "/substrate/plugins/objectstore/azure-blob@sha256:" + strings.Repeat("a", 64)}) || result.AssetsDir != assetsDir {
		t.Fatalf("release images were not recorded at the release tag in the owned registry: %+v", result)
	}
	for _, call := range r.calls {
		args := strings.Join(call.Args, " ")
		env := strings.Join(call.Env, "|")
		if call.Args[0] == "docker" || call.Args[0] == "podman" {
			t.Fatalf("release images needed a container engine: %s", args)
		}
		if call.Args[0] == "az" && !strings.Contains(args, "--subscription "+c.Azure.SubscriptionID) {
			t.Fatalf("Azure command lost subscription: %s", args)
		}
		if call.Args[0] == testKo && (!strings.Contains(env, "DOCKER_CONFIG="+filepath.Join(c.workspace(), "docker")) ||
			!strings.Contains(env, "KO_DEFAULTPLATFORMS=linux/amd64")) {
			t.Fatalf("ko lost the isolated registry login or platform: %s", env)
		}
		if strings.Contains(args, "./demos/counter") && !strings.Contains(env, "KO_DOCKER_REPO="+registry+"/substrate") {
			t.Fatal("demo image was published outside the release image repository")
		}
		if strings.Contains(args, "azure-blob-objectstore-plugin") && !strings.Contains(env, "KO_DOCKER_REPO="+registry+"/substrate/plugins/objectstore/azure-blob") {
			t.Fatal("plugin image was published to the wrong repository")
		}
		if strings.Contains(args, "publish microvm-assets") && !strings.Contains(args, sandboxTemplate+" "+assetsDir+" amd64") {
			t.Fatalf("assets were not assembled from the pinned template: %s", args)
		}
	}
	for _, pattern := range []string{"build-*", "plugin-*"} {
		temporary, err := filepath.Glob(filepath.Join(c.workspace(), pattern))
		if err != nil || len(temporary) != 0 {
			t.Fatalf("temporary checkout retained: %v, %v", temporary, err)
		}
	}
}

func TestReleaseImagesRejectUnverifiedImports(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replies func(*testing.T, Config) []reply
	}{
		{"revision mismatch", func(t *testing.T, c Config) []reply {
			return []reply{{contains: "publish release-inventory", err: errors.New("built from another commit")}}
		}},
		{"invalid inventory", func(t *testing.T, c Config) []reply {
			return []reply{{contains: "publish release-inventory", output: `[{"name":"../escape","digest":"sha256:` + strings.Repeat("1", 64) + `"},{"name":"b","digest":"x"},{"name":"c","digest":"y"}]`}}
		}},
		{"changed digest", func(t *testing.T, c Config) []reply {
			return releaseImportReplies(t, c, func(releaseImage) string { return "sha256:" + strings.Repeat("f", 64) })[:3]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			c.Substrate.Images = ImagesConfig{}
			replies := []reply{accountReply(c), {contains: "group exists", output: "true"}, ownerReply(c), registryReply(c), acrLoginReply()}
			replies = append(replies, fetchReplies(c.Substrate.Source)...)
			replies = append(replies, tc.replies(t, c)...)
			r := script(t, replies...)
			result, err := (App{Config: c, Runner: imageBuildRunner{r, t.TempDir()}}).BuildImages(t.Context())
			if err == nil || result != (BuildResult{}) {
				t.Fatalf("unverified release images were accepted: %+v, %v", result, err)
			}
		})
	}
}

func TestReleaseImagesRequireReleaseTag(t *testing.T) {
	c := testConfig(t)
	c.Substrate.Images = ImagesConfig{}
	c.Substrate.Source.Tag = ""
	_, err := (App{Config: c, Runner: script(t)}).BuildImages(t.Context())
	if err == nil || !strings.Contains(err.Error(), "substrate.source.tag") {
		t.Fatalf("release images were imported without a release tag: %v", err)
	}
}

func TestSourceTagFetchRejectsMovedTag(t *testing.T) {
	c := testConfig(t)
	r := script(t, reply{contains: "git init"}, reply{contains: "git fetch --quiet --depth=1 " + DefaultRepository + " +refs/tags/" + DefaultTag + ":refs/tags/" + DefaultTag},
		reply{contains: "git rev-parse --verify", output: strings.Repeat("b", 40) + "\n"})
	err := (App{Config: c, Runner: r}).fetchSource(t.Context(), t.TempDir(), c.Substrate.Source)
	if err == nil || !strings.Contains(err.Error(), "not the configured commit") {
		t.Fatalf("accepted a tag that names another commit: %v", err)
	}
}

func TestSourceImageBuild(t *testing.T) {
	for _, failPush := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "push failure"}[failPush], func(t *testing.T) {
			c := testConfig(t)
			c.Substrate.Images = ImagesConfig{}
			c.Substrate.ImageSource = ImageSourceBuild
			registry := "substratetest123.azurecr.io"
			replies := []reply{
				accountReply(c), {contains: "group exists", output: "true"}, ownerReply(c), registryReply(c),
				{contains: "docker info"}, {contains: "docker buildx version"},
				{contains: "docker buildx inspect --bootstrap", output: readyBuildx}, acrLoginReply(),
			}
			replies = append(replies, fetchReplies(c.Substrate.Source)...)
			var tag string
			replies = append(replies, reply{contains: "publish release-images", effect: func(cmd Command) {
				for _, entry := range cmd.Env {
					if v, ok := strings.CutPrefix(entry, "VERSION="); ok {
						tag = v
					}
				}
			}})
			wantErr := errors.New("image push failed")
			if failPush {
				replies[len(replies)-1].effect, replies[len(replies)-1].err = nil, wantErr
			} else {
				prefix := DefaultTag + "-azure." + c.Substrate.Source.Commit[:12] + "."
				extra := extraImageReplies(c, "")
				extra[1].contains = testKo + " build --base-import-paths --tags " + prefix
				extra[len(extra)-2].contains = testKo + " build --bare --tags " + prefix
				replies = append(replies, extra...)
			}
			r := script(t, replies...)
			assetsDir := filepath.Join(c.workspace(), "assets")
			app := App{Config: c, Runner: imageBuildRunner{r, assetsDir}}
			result, err := app.BuildImages(t.Context())
			if failPush {
				if !errors.Is(err, wantErr) || result != (BuildResult{}) {
					t.Fatalf("push failure returned usable configuration: %+v, %v", result, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !releasePattern.MatchString(tag) || !strings.HasPrefix(tag, DefaultTag+"-azure."+c.Substrate.Source.Commit[:12]+".") ||
				result.Images.Tag != tag || result.Images.Repository != registry+"/substrate" || result.AssetsDir != assetsDir {
				t.Fatalf("source build lost its release-style tag or registry: %q %+v", tag, result)
			}
			for _, call := range r.calls {
				args := strings.Join(call.Args, " ")
				env := strings.Join(call.Env, "|")
				if call.Args[0] == "docker" &&
					(!strings.Contains(env, "DOCKER_CONFIG="+filepath.Join(c.workspace(), "docker")) ||
						!strings.Contains(env, "BUILDX_CONFIG="+filepath.Join(os.Getenv("DOCKER_CONFIG"), "buildx"))) {
					t.Fatal("Docker preflight did not use the publishing credentials/context environment")
				}
				if strings.Contains(args, "publish release-images") &&
					(!strings.Contains(args, "--no-dev-env") || !strings.Contains(args, "--ko-docker-repo "+registry+"/substrate") ||
						!strings.Contains(env, "DOCKER_CONFIG="+filepath.Join(c.workspace(), "docker")) ||
						!strings.Contains(env, "BUILDX_CONFIG="+filepath.Join(os.Getenv("DOCKER_CONFIG"), "buildx"))) {
					t.Fatal("publisher lost isolated credentials or environment")
				}
				if strings.Contains(args, "acr import") {
					t.Fatal("source build imported release images")
				}
			}
		})
	}
}

func TestImagesCannotReplaceConfiguredInstall(t *testing.T) {
	_, err := (App{Config: testConfig(t), Runner: script(t)}).BuildImages(t.Context())
	if err == nil || !strings.Contains(err.Error(), "already configured") {
		t.Fatalf("image bootstrap could replace an installed image set: %v", err)
	}
}

func TestPodmanImageBuildWiring(t *testing.T) {
	c := testConfig(t)
	c.Substrate.Images = ImagesConfig{}
	c.Substrate.ImageSource = ImageSourceBuild
	replies := []reply{accountReply(c), {contains: "group exists", output: "true"}, ownerReply(c), registryReply(c),
		{contains: "podman info"}, acrLoginReply()}
	replies = append(replies, fetchReplies(c.Substrate.Source)...)
	replies = append(replies, reply{contains: "publish release-images", effect: func(cmd Command) {
		data, err := os.ReadFile(filepath.Join(cmd.Dir, "cmd/ate-setup/internal/steps/publish.go"))
		if err != nil || !strings.Contains(string(data), "images.PublishPodmanImage(") {
			t.Fatal("Podman selected but publisher still uses Docker")
		}
		data, err = os.ReadFile(filepath.Join(cmd.Dir, "cmd/ate-setup/internal/images/azure_podman.go"))
		if err != nil || !bytes.Equal(data, podmanPublisher) {
			t.Fatal("Podman helper was not injected into disposable source")
		}
		if !strings.Contains(strings.Join(cmd.Env, "|"), "REGISTRY_AUTH_FILE="+filepath.Join(c.workspace(), "docker/config.json")) {
			t.Fatal("Podman publisher lost isolated registry authentication")
		}
	}})
	extra := extraImageReplies(c, "")
	extra[1].contains = testKo + " build --base-import-paths --tags v1.0.0-rc.3-azure."
	extra[len(extra)-2].contains = testKo + " build --bare --tags v1.0.0-rc.3-azure."
	r := script(t, append(replies, extra...)...)
	result, err := (App{Config: c, Runner: imageBuildRunner{r, filepath.Join(c.workspace(), "assets")}, ContainerEngine: "podman"}).BuildImages(t.Context())
	if err != nil || result.Images.Repository != "substratetest123.azurecr.io/substrate" {
		t.Fatalf("Podman image publication failed: %+v, %v", result, err)
	}
}

func TestPodmanPublisherRejectsChangedContract(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cmd/ate-setup/internal/steps/publish.go")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	const before = "package steps\n// changed publication contract\n"
	if err := os.WriteFile(path, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	if err := preparePodmanPublisher(root); err == nil || !strings.Contains(err.Error(), "publisher changed") {
		t.Fatalf("unknown upstream contract accepted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != before {
		t.Fatal("unsupported upstream publisher was modified")
	}
}

func TestPluginDigestReference(t *testing.T) {
	const repository = "owned.azurecr.io/substrate/plugins/objectstore/azure-blob"
	const tag = "azure-reviewed-build"
	digest := "sha256:" + strings.Repeat("a", 64)
	want := repository + "@" + digest
	for _, tc := range []struct {
		name, output string
		valid        bool
	}{
		{"digest only", want, true},
		{"ko tagged digest", repository + ":" + tag + "@" + digest + "\n", true},
		{"wrong repository", repository + "/other@" + digest, false},
		{"wrong registry", "foreign.azurecr.io/substrate/plugins/objectstore/azure-blob@" + digest, false},
		{"wrong tag", repository + ":other@" + digest, false},
		{"missing digest", repository + ":" + tag, false},
		{"malformed digest", repository + "@sha256:abc", false},
		{"multiple references", want + "\n" + want, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pluginDigestReference(tc.output, repository, tag)
			if tc.valid {
				if err != nil || got != want {
					t.Fatalf("digest normalization failed: %q, %v", got, err)
				}
			} else if err == nil || got != "" {
				t.Fatalf("accepted an unexpected publication reference: %q, %v", got, err)
			}
		})
	}
}

func TestSaveBuildResultPreservesDeploymentAndRejectsConcurrentEdits(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "save", true: "concurrent edit"}[changed], func(t *testing.T) {
			c := testConfig(t)
			c.Substrate.Images = ImagesConfig{}
			path := filepath.Join(c.Installation.Dir, "installation.json")
			data, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			expected, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				data = []byte(strings.Replace(string(data), "eastus2", "southcentralus", 1))
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			result := BuildResult{Images: testConfig(t).Substrate.Images, AssetsDir: "/test/assets"}
			err = SaveBuildResult(path, expected, result)
			if changed {
				after, readErr := os.ReadFile(path)
				if err == nil || readErr != nil || string(after) != string(data) {
					t.Fatal("concurrent configuration edit was overwritten")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			saved, err := readConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			c.Substrate.Images = result.Images
			c.Substrate.Runtime.AssetsDir = result.AssetsDir
			c.Installation.Dir = ""
			if !reflect.DeepEqual(saved, c) {
				t.Fatal("image recording changed installation identity, relative state path or deployment settings")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("image recording relaxed configuration permissions")
			}
		})
	}
}
