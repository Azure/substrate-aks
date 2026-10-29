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

type imageBuildRunner struct {
	*scriptedRunner
	assetsDir string
}

func (r imageBuildRunner) Run(ctx context.Context, c Command) ([]byte, error) {
	if strings.Join(c.Args, " ") == "git rev-parse HEAD" {
		contract := filepath.Join(c.Dir, "internal/plugins/azure-blob/config")
		if err := os.MkdirAll(contract, 0700); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(contract, "kustomization.yaml"), []byte("resources: []"), 0600); err != nil {
			r.t.Fatal(err)
		}
		writeAssetFixture(r.t, c.Dir, r.assetsDir)
		startup := filepath.Join(c.Dir, "cmd/ate-setup/internal/cmd")
		if err := os.MkdirAll(startup, 0700); err != nil {
			r.t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(c.Dir, "cmd/ate-setup/internal/steps"), 0700); err != nil {
			r.t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(c.Dir, "cmd/ate-setup/internal/images"), 0700); err != nil {
			r.t.Fatal(err)
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
			[]byte("FROM rust:bookworm@sha256:synthetic AS builder\nFROM envoyproxy/envoy:v1.39.4@sha256:synthetic\n"), 0600); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(startup, "root.go"), []byte("package cmd\nfunc setup() error {\n"+
			"\t\tif err := cfg.EnsureClusterCredentials(cmd.Context()); err != nil {\n\t\t\treturn err\n\t\t}\n"+
			"\t\treturn nil\n}\n"), 0600); err != nil {
			r.t.Fatal(err)
		}
	}
	if strings.Contains(strings.Join(c.Args, " "), "publish release-images") {
		data, err := os.ReadFile(filepath.Join(c.Dir, "cmd/ate-setup/internal/cmd/root.go"))
		if err != nil || !strings.Contains(string(data), "env = &steps.Env{Cfg: cfg}") {
			r.t.Fatalf("build did not adapt the upstream publisher to run without Kubernetes: %s, %v", data, err)
		}
	}
	return r.scriptedRunner.Run(ctx, c)
}

func TestImagePublisherRejectsChangedStartup(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cmd/ate-setup/internal/cmd/root.go")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	const source = "package cmd\n// upstream startup changed\n"
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareImagePublisher(root); err == nil || !strings.Contains(err.Error(), "startup changed") {
		t.Fatalf("publisher accepted an unknown startup contract: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != source {
		t.Fatalf("unsupported upstream source was modified: %s, %v", after, err)
	}
}

func TestImageBuildPinsSourceTargetAndCredentials(t *testing.T) {
	for _, failPush := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "push failure"}[failPush], func(t *testing.T) {
			c := testConfig(t)
			c.Substrate.Images = ImagesConfig{}
			registry := "substratetest123.azurecr.io"
			replies := []reply{
				accountReply(c), {contains: "group exists", output: "true"}, ownerReply(c), registryReply(c),
				{contains: "docker info"}, {contains: "docker buildx version"},
				{contains: "docker buildx inspect --bootstrap", output: readyBuildx}, acrLoginReply(),
				{contains: "git init"}, {contains: "git fetch"}, {contains: "git checkout"},
				{contains: "git rev-parse HEAD", output: c.Substrate.Source.Commit},
				{contains: "publish release-images"},
			}
			wantErr := errors.New("image push failed")
			if failPush {
				replies[len(replies)-1].err = wantErr
			} else {
				replies = append(replies,
					reply{contains: "github.com/google/ko@v0.19.1", output: registry + "/substrate/plugins/objectstore/azure-blob@sha256:" + strings.Repeat("a", 64)},
					reply{contains: "hack/microvm-assets/assemble.sh"})
			}
			r := script(t, replies...)
			assetsDir := filepath.Join(c.workspace(), "assets")
			result, err := (App{Config: c, Runner: imageBuildRunner{r, assetsDir}}).BuildImages(t.Context())
			if failPush {
				if !errors.Is(err, wantErr) || result != (BuildResult{}) {
					t.Fatalf("push failure returned usable configuration: %+v, %v", result, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if result.Images.Repository != registry+"/substrate" ||
					result.Images.Plugin != registry+"/substrate/plugins/objectstore/azure-blob@sha256:"+strings.Repeat("a", 64) ||
					!strings.HasPrefix(result.Images.Tag, "azure-"+c.Substrate.Source.Commit[:12]+"-") ||
					result.AssetsDir != assetsDir {
					t.Fatalf("build result lost source/registry pinning: %+v", result)
				}
			}
			for _, call := range r.calls {
				args := strings.Join(call.Args, " ")
				env := strings.Join(call.Env, "|")
				if call.Args[0] == "docker" &&
					(!strings.Contains(env, "DOCKER_CONFIG="+filepath.Join(c.workspace(), "docker")) ||
						!strings.Contains(env, "BUILDX_CONFIG="+filepath.Join(os.Getenv("DOCKER_CONFIG"), "buildx"))) {
					t.Fatal("Docker preflight did not use the publishing credentials/context environment")
				}
				if call.Args[0] == "az" && !strings.Contains(args, "--subscription "+c.Azure.SubscriptionID) {
					t.Fatalf("Azure command lost subscription: %s", args)
				}
				if strings.Contains(args, "publish release-images") &&
					(!strings.Contains(args, "--no-dev-env") ||
						!strings.Contains(env, "DOCKER_CONFIG="+filepath.Join(c.workspace(), "docker")) ||
						!strings.Contains(env, "BUILDX_CONFIG="+filepath.Join(os.Getenv("DOCKER_CONFIG"), "buildx"))) {
					t.Fatal("publisher lost isolated credentials or environment")
				}
				if strings.Contains(args, "git fetch") && !strings.Contains(args, c.Substrate.Source.Commit) {
					t.Fatal("builder fetched an unpinned source")
				}
				if strings.Contains(args, "github.com/google/ko@v0.19.1") &&
					(!strings.Contains(args, "build --bare --tags ") ||
						!strings.Contains(env, "KO_DOCKER_REPO="+registry+"/substrate/plugins/objectstore/azure-blob|")) {
					t.Fatal("plugin publisher would append a command name or publish to the wrong repository")
				}
			}
			temporary, err := filepath.Glob(filepath.Join(c.workspace(), "build-*"))
			if err != nil || len(temporary) != 0 {
				t.Fatalf("temporary build checkout retained: %v, %v", temporary, err)
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
	r := script(t, accountReply(c), reply{contains: "group exists", output: "true"}, ownerReply(c), registryReply(c),
		reply{contains: "podman info"}, acrLoginReply(),
		reply{contains: "git init"}, reply{contains: "git fetch"}, reply{contains: "git checkout"},
		reply{contains: "git rev-parse HEAD", output: c.Substrate.Source.Commit},
		reply{contains: "publish release-images", effect: func(cmd Command) {
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
		}},
		reply{contains: "github.com/google/ko@v0.19.1", output: "substratetest123.azurecr.io/substrate/plugins/objectstore/azure-blob@sha256:" + strings.Repeat("a", 64)},
		reply{contains: "hack/microvm-assets/assemble.sh"})
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
			c.Installation.StateDir = "state"
			c.Substrate.Images = ImagesConfig{}
			path := filepath.Join(t.TempDir(), "deploy.json")
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
