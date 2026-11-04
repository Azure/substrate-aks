package installer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDockerIsolationPreservesContextPluginsAndBuilder(t *testing.T) {
	c := testConfig(t)
	source := os.Getenv("DOCKER_CONFIG")
	original := `{"currentContext":"desktop-linux","cliPluginsExtraDirs":["relative-plugins"],
		"auths":{"private.example":{"auth":"caller-secret"}},"credsStore":"desktop",
		"credHelpers":{"private.example":"custom"}}`
	if err := os.WriteFile(filepath.Join(source, "config.json"), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"contexts/meta/desktop/meta.json", "contexts/tls/desktop/docker/ca.pem", "cli-plugins/docker-buildx"} {
		path = filepath.Join(source, path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("caller-context-or-plugin"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	dest := filepath.Join(c.workspace(), "docker")
	if err := os.MkdirAll(dest, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "config.json"),
		[]byte(`{"auths":{"owned.azurecr.io":{"auth":"owned-token"}},"currentContext":"stale"}`), 0644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		dir, err := (App{Config: c}).prepareDockerConfig()
		if err != nil || dir != dest {
			t.Fatalf("preparing/refreshing Docker configuration: %q, %v", dir, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dest, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		CurrentContext      string
		CLIPluginsExtraDirs []string
		Auths               map[string]struct{ Auth string }
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	extra, err := filepath.Abs("relative-plugins")
	if err != nil {
		t.Fatal(err)
	}
	if config.CurrentContext != "desktop-linux" ||
		!reflect.DeepEqual(config.CLIPluginsExtraDirs, []string{extra, filepath.Join(source, "cli-plugins")}) ||
		len(config.Auths) != 1 || config.Auths["owned.azurecr.io"].Auth != "owned-token" ||
		strings.Contains(string(data), "caller-secret") || strings.Contains(string(data), "credsStore") ||
		strings.Contains(string(data), "credHelpers") {
		t.Fatal("Docker isolation lost caller routing/plugin settings, lost owned credentials, or copied caller credentials")
	}
	for _, path := range []string{"contexts/meta/desktop/meta.json", "contexts/tls/desktop/docker/ca.pem"} {
		data, err := os.ReadFile(filepath.Join(dest, path))
		if err != nil || string(data) != "caller-context-or-plugin" {
			t.Fatalf("Docker context metadata/TLS unavailable: %s: %v", path, err)
		}
	}
	after, err := os.ReadFile(filepath.Join(source, "config.json"))
	if err != nil || string(after) != original {
		t.Fatal("caller Docker credentials were changed")
	}
	info, err := os.Stat(filepath.Join(dest, "config.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("isolated credentials are not private: %v", err)
	}
	env, err := dockerBuildEnvironment(dest)
	if err != nil || !strings.Contains(strings.Join(env, "|"), "BUILDX_CONFIG="+filepath.Join(source, "buildx")) {
		t.Fatalf("selected buildx state was lost: %v, %v", env, err)
	}
	t.Setenv("BUILDX_CONFIG", "custom-builder")
	want, err := filepath.Abs("custom-builder")
	if err != nil {
		t.Fatal(err)
	}
	env, err = dockerBuildEnvironment(dest)
	if err != nil || !strings.Contains(strings.Join(env, "|"), "BUILDX_CONFIG="+want) {
		t.Fatalf("explicit buildx configuration was lost: %v, %v", env, err)
	}
}

func TestDockerConfigRejectsInvalidSettingsWithoutReplacingCredentials(t *testing.T) {
	for _, which := range []string{"caller", "isolated"} {
		t.Run(which, func(t *testing.T) {
			c := testConfig(t)
			source, dest := os.Getenv("DOCKER_CONFIG"), filepath.Join(c.workspace(), "docker")
			if err := os.MkdirAll(dest, 0700); err != nil {
				t.Fatal(err)
			}
			original := `{"auths":{"owned.azurecr.io":{"auth":"preserve-me"}}}`
			if err := os.WriteFile(filepath.Join(dest, "config.json"), []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(source, "config.json")
			if which == "isolated" {
				path = filepath.Join(dest, "config.json")
			}
			invalid := `{"auths":{"secret":"must-not-be-printed"},`
			if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := (App{Config: c}).prepareDockerConfig(); err == nil || strings.Contains(err.Error(), "must-not-be-printed") {
				t.Fatalf("invalid Docker config was ignored or disclosed: %v", err)
			}
			want := original
			if which == "isolated" {
				want = invalid
			}
			after, err := os.ReadFile(filepath.Join(dest, "config.json"))
			if err != nil || string(after) != want {
				t.Fatal("failed preparation replaced isolated credentials")
			}
		})
	}
}
