package installer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFingerprintDeploymentContract(t *testing.T) {
	const expected = "1448f6d91894aa0a20c395b010919c2b45b20c874bfe5c7726671de8ddc5c777"
	if got := fingerprint(testConfig(t), testInfrastructure()); got != expected {
		t.Fatalf("deployment contract changed the installation fingerprint: %s", got)
	}
}

func TestPreparedSourceRejectsModifiedFiles(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	fp := fingerprint(c, i)
	root := filepath.Join(c.workspace(), "source-"+fp+"-v2")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	p := prepared{Fingerprint: fp, Files: map[string]string{}}
	for _, name := range []string{"atelet.yaml", "api.yaml", "postgres.yaml", "otel.yaml", "base.yaml", "demo.yaml"} {
		data := []byte("prepared " + name)
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		p.Files[name] = checksum(data)
	}
	marker, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".substrate-aks-prepared.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	t.Run("unexpected Go modification", func(t *testing.T) {
		r := script(t, reply{contains: "git rev-parse HEAD", output: c.Substrate.Source.Commit},
			reply{contains: "git diff --name-only HEAD", output: "cmd/ate-setup/main.go\n"})
		_, err := (App{Config: c, Runner: r}).prepare(t.Context(), i)
		if err == nil || !strings.Contains(err.Error(), "unexpected tracked source modification") {
			t.Fatalf("accepted modified source: %v", err)
		}
	})
	t.Run("untracked enumeration failure", func(t *testing.T) {
		want := errors.New("cannot enumerate source files")
		r := script(t, reply{contains: "git rev-parse HEAD", output: c.Substrate.Source.Commit},
			reply{contains: "git diff --name-only HEAD"},
			reply{contains: "git ls-files --others -z --", err: want})
		if _, err := (App{Config: c, Runner: r}).prepare(t.Context(), i); !errors.Is(err, want) {
			t.Fatalf("lost source verification failure: %v", err)
		}
	})
	t.Run("modified adapter output", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "atelet.yaml"), []byte("reverted to GCS"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := (App{Config: c, Runner: script(t)}).prepare(t.Context(), i)
		if err == nil || !strings.Contains(err.Error(), "managed source changed") {
			t.Fatalf("accepted modified adapter output: %v", err)
		}
	})
}

func TestPreparedSourceChecksUntrackedFiles(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	root, _ := clientFixture(t, c)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.go\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := ExecRunner{}
	git := func(args ...string) []byte {
		t.Helper()
		data, err := runner.Run(t.Context(), Command{Dir: root, Args: append([]string{"git"}, args...)})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	git("init", "--quiet")
	git("add", "--", ".", ":!.substrate-aks-prepared.json")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "core.hooksPath="+t.TempDir(),
		"commit", "--quiet", "-m", "Prepared-source test fixture")
	c.Substrate.Source.Commit = strings.TrimSpace(string(git("rev-parse", "HEAD")))
	marker, err := os.ReadFile(filepath.Join(root, ".substrate-aks-prepared.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p prepared
	if err := json.Unmarshal(marker, &p); err != nil {
		t.Fatal(err)
	}
	p.Fingerprint = fingerprint(c, i)
	dest := filepath.Join(c.workspace(), "source-"+p.Fingerprint+"-v2")
	if err := os.Rename(root, dest); err != nil {
		t.Fatal(err)
	}
	marker, err = json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, ".substrate-aks-prepared.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	a := App{Config: c, Runner: runner}
	if got, err := a.prepare(t.Context(), i); err != nil || got != dest {
		t.Fatalf("rejected the installer's untracked completion marker: %q, %v", got, err)
	}
	for _, path := range []string{"cmd/ate-setup/extra.go", "cmd/kubectl-ate/extra.go", "ignored.go", ".substrate-aks-prepared.json.go"} {
		t.Run(path, func(t *testing.T) {
			file := filepath.Join(dest, path)
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("package main\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Remove(file); err != nil {
					t.Error(err)
				}
			})
			if _, err := a.prepare(t.Context(), i); err == nil ||
				!strings.Contains(err.Error(), "unexpected untracked source file") || !strings.Contains(err.Error(), path) {
				t.Fatalf("accepted added Go source %s: %v", path, err)
			}
		})
	}
}

func TestUpstreamCommandsPinTargetAndImages(t *testing.T) {
	c := testConfig(t)
	r := script(t, reply{contains: "go run ./cmd/ate-setup deploy atelet"})
	if err := (App{Config: c, Runner: r}).upstream(t.Context(), "/managed/source", "deploy", "atelet"); err != nil {
		t.Fatal(err)
	}

	call := r.calls[0]
	command := strings.Join(call.Args, " ")
	for _, want := range []string{
		"--context " + c.AKS.ClusterName, "--kubeconfig " + filepath.Join(c.workspace(), "kubeconfig"),
		"--image-repo " + c.Substrate.Images.Repository, "--image-tag " + c.Substrate.Images.Tag,
		"--snapshot-location azblob://snapshots/cluster/", "--no-dev-env",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("upstream command lost %s: %v", want, call.Args)
		}
	}
	if strings.Contains(command, "--kind") || call.Dir != "/managed/source" {
		t.Fatalf("command used the wrong deployment profile or checkout: %+v", call)
	}
}

func TestPreparedSourceReplacesLegacyDemoCache(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	legacy := filepath.Join(c.workspace(), "source-"+fingerprint(c, i))
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(legacy, ".substrate-aks-prepared.json")
	const previousMarker = `{"old":"cache without the Azure ActorTemplate adaptation"}`
	if err := os.WriteFile(marker, []byte(previousMarker), 0600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("stopped before fetching fresh source")
	r := script(t, reply{contains: "git init", err: want})
	if _, err := (App{Config: c, Runner: r}).prepare(t.Context(), i); !errors.Is(err, want) {
		t.Fatalf("prepare reused or rejected the legacy cache instead of fetching fresh source: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != previousMarker {
		t.Fatalf("prepare modified the previous managed checkout: %s, %v", data, err)
	}
}

func TestUpstreamRefreshesOwnedRegistryCredentials(t *testing.T) {
	for _, component := range []string{"ate-system", "atelet", "apiserver", "demo counter-microvm"} {
		t.Run(component, func(t *testing.T) {
			c := testConfig(t)
			c.Substrate.Images.Repository = "substratetest123.azurecr.io/substrate"
			r := script(t, accountReply(c), reply{contains: "group exists", output: "true"},
				ownerReply(c), registryReply(c), acrLoginReply(),
				reply{contains: "go run ./cmd/ate-setup deploy " + component})
			args := append([]string{"deploy"}, strings.Fields(component)...)
			if err := (App{Config: c, Runner: r}).upstream(t.Context(), "/managed/source", args...); err != nil {
				t.Fatal(err)
			}
			login, deploy := r.calls[len(r.calls)-2], r.calls[len(r.calls)-1]
			if !strings.Contains(strings.Join(deploy.Args, " "), "--ingress-allowed-spiffe-ids "+routerClientSPIFFEID) {
				t.Fatal("kubectl ate proxy cannot reach actors through atenet-router")
			}
			dockerEnv := "DOCKER_CONFIG=" + filepath.Join(c.workspace(), "docker")
			for _, call := range []Command{deploy} {
				found := false
				for _, entry := range call.Env {
					found = found || entry == dockerEnv
				}
				if !found {
					t.Fatalf("deploy must use isolated registry credentials: %+v", call)
				}
			}
			if !strings.Contains(strings.Join(login.Args, " "), "--name substratetest123 --subscription "+c.Azure.SubscriptionID) {
				t.Fatalf("login lost registry or subscription: %v", login.Args)
			}
			if !login.SensitiveOutput || !strings.Contains(strings.Join(login.Args, " "), "--expose-token") {
				t.Fatal("login requires Docker or exposes the ACR token")
			}
		})
	}
}

func TestUpstreamRegistryLoginFailureStopsDeployment(t *testing.T) {
	c := testConfig(t)
	c.Substrate.Images.Repository = "substratetest123.azurecr.io/substrate"
	want := errors.New("Azure login expired")
	r := script(t, accountReply(c), reply{contains: "group exists", output: "true"},
		ownerReply(c), registryReply(c), reply{contains: "acr login", err: want})
	err := (App{Config: c, Runner: r}).upstream(t.Context(), "/managed/source", "deploy", "ate-system")
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "ACR login failed") {
		t.Fatalf("login failure was not propagated: %v", err)
	}
}

func TestUpstreamPreservesExternalRegistryCredentials(t *testing.T) {
	for _, repo := range []string{"ghcr.io/example/substrate", "external.azurecr.io/substrate"} {
		t.Run(repo, func(t *testing.T) {
			c := testConfig(t)
			c.Substrate.Images.Repository = repo
			var replies []reply
			if strings.Contains(repo, ".azurecr.io/") {
				replies = append(replies, accountReply(c), reply{contains: "group exists", output: "true"},
					ownerReply(c), registryReply(c))
			}
			r := script(t, append(replies, reply{contains: "go run ./cmd/ate-setup deploy ate-system"})...)
			if err := (App{Config: c, Runner: r}).upstream(t.Context(), "/managed/source", "deploy", "ate-system"); err != nil {
				t.Fatal(err)
			}
			for _, entry := range r.calls[len(r.calls)-1].Env {
				if strings.HasPrefix(entry, "DOCKER_CONFIG=") {
					t.Fatal("external registry credentials replaced with the owned registry's config")
				}
			}
		})
	}
}
