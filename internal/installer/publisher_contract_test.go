//go:build integration

package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runPinnedGoTest adds a test file to a copy of the pinned source and runs it,
// so installer adapters are exercised against the pinned packages themselves.
func runPinnedGoTest(t *testing.T, dest, pkg, name, source string, env ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dest, pkg, name), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	test := exec.CommandContext(t.Context(), "go", "test", "./"+pkg, "-run", "^TestAzure", "-count=1")
	test.Dir, test.Env = dest, append(cleanEnvironment(os.Environ()), env...)
	if output, err := test.CombinedOutput(); err != nil {
		t.Fatalf("pinned %s contract failed: %v\n%s", pkg, err, output)
	}
}

func pinnedSourceCopy(t *testing.T) string {
	t.Helper()
	root := pinnedContractRoot(t)
	dest := filepath.Join(t.TempDir(), "source")
	if output, err := exec.Command("git", "clone", "--quiet", "--shared", root, dest).CombinedOutput(); err != nil {
		t.Fatalf("copying pinned source: %v\n%s", err, output)
	}
	dest, err := filepath.EvalSymlinks(dest)
	if err != nil {
		t.Fatal(err)
	}
	return dest
}

const podmanContractTest = `package images

import (
	"os"
	"strings"
	"testing"
)

func TestAzurePodmanContract(t *testing.T) {
	labels := []string{"--label=org.opencontainers.image.revision=contract", "--label=org.opencontainers.image.version=v1.0.0-contract"}
	for _, failure := range []string{"none", "build", "push", "missing", "invalid"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("PUBLISH_TEST_FAILURE", failure)
			ref, err := PublishPodmanImage(t.Context(), os.Getenv("PUBLISH_TEST_ROOT"), "example.invalid/substrate", "envoy-dataplane",
				"v1.0.0-contract", os.Getenv("PUBLISH_TEST_CONTEXT"), "linux/amd64", labels)
			if failure != "none" {
				if err == nil {
					t.Fatalf("publisher accepted a %s failure: %s", failure, ref)
				}
				return
			}
			if err != nil || ref != "example.invalid/substrate/envoy-dataplane@sha256:"+strings.Repeat("a", 64) {
				t.Fatalf("publisher lost the pushed digest: %q, %v", ref, err)
			}
		})
	}
	if _, err := PublishPodmanImage(t.Context(), os.Getenv("PUBLISH_TEST_ROOT"), "example.invalid/substrate", "envoy-dataplane",
		"v1.0.0-contract", os.Getenv("PUBLISH_TEST_CONTEXT"), "linux/amd64", []string{"--cache-from=type=gha"}); err == nil {
		t.Fatal("publisher accepted a Docker build flag Podman does not implement")
	}
}
`

func TestPinnedPodmanPublisherContract(t *testing.T) {
	root := pinnedContractRoot(t)
	dest := pinnedSourceCopy(t)
	if err := preparePodmanPublisher(dest); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(root, "cmd/dataplane/envoy/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	adapted, err := os.ReadFile(filepath.Join(dest, "cmd/dataplane/envoy/Dockerfile"))
	want := strings.Replace(string(original), " rust:bookworm@", " docker.io/library/rust:bookworm@", 1)
	want = strings.Replace(want, "FROM envoyproxy/envoy:", "FROM docker.io/envoyproxy/envoy:", 1)
	if err != nil || string(adapted) != want {
		t.Fatal("Podman base-image qualification changed the pinned image content or build recipe")
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(t.TempDir(), "ate-setup"), "./cmd/ate-setup")
	build.Dir, build.Env = dest, cleanEnvironment(os.Environ())
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the Podman-adapted publisher: %v\n%s", err, output)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(`#!/bin/sh
set -eu
test "$2" = --authfile
test "$3" = "$REGISTRY_AUTH_FILE"
case "$1" in
  build)
    test "$*" = "build --authfile $REGISTRY_AUTH_FILE --platform linux/amd64 --label org.opencontainers.image.revision=contract --label org.opencontainers.image.version=v1.0.0-contract -t example.invalid/substrate/envoy-dataplane:v1.0.0-contract $PUBLISH_TEST_CONTEXT"
    test "$PUBLISH_TEST_FAILURE" != build
    ;;
  push)
    test "$PUBLISH_TEST_FAILURE" != push
    test "$4" = --digestfile
    test "$6" = example.invalid/substrate/envoy-dataplane:v1.0.0-contract
    test "$7" = docker://example.invalid/substrate/envoy-dataplane:v1.0.0-contract
    if [ "$PUBLISH_TEST_FAILURE" = missing ]; then exit 0; fi
    if [ "$PUBLISH_TEST_FAILURE" = invalid ]; then printf invalid > "$5"; else printf 'sha256:%064d\n' 0 | tr 0 a > "$5"; fi
    ;;
  *) exit 98 ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	authfile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(authfile, []byte(`{"auths":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	runPinnedGoTest(t, dest, "cmd/ate-setup/internal/images", "azure_podman_contract_test.go", podmanContractTest,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "REGISTRY_AUTH_FILE="+authfile,
		"PUBLISH_TEST_ROOT="+dest, "PUBLISH_TEST_CONTEXT="+filepath.Join(dest, "cmd/dataplane/envoy"))
}

const releaseTagContractTest = `package images

import (
	"os"
	"strings"
	"testing"
)

func TestAzureReleaseTagContract(t *testing.T) {
	for _, tag := range strings.Fields(os.Getenv("AZURE_TAGS")) {
		if err := CheckReleaseTag("substratetest123.azurecr.io/substrate", tag); err != nil {
			t.Fatal(err)
		}
	}
}
`

// TestPinnedImagePublisherContract checks that the source track's tags are
// release tags the pinned publisher accepts, and that publishing needs no
// cluster.
func TestPinnedImagePublisherContract(t *testing.T) {
	dest := pinnedSourceCopy(t)
	now := time.Now()
	source := DefaultConfig().Substrate.Source
	untagged := source
	untagged.Tag = ""
	runPinnedGoTest(t, dest, "cmd/ate-setup/internal/images", "azure_tag_contract_test.go", releaseTagContractTest,
		"AZURE_TAGS="+sourceImageTag(source, now)+" "+sourceImageTag(untagged, now)+" "+DefaultTag)
	kubeconfig := filepath.Join(t.TempDir(), "invalid-kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("not kubeconfig YAML ["), 0600); err != nil {
		t.Fatal(err)
	}
	publish := exec.CommandContext(t.Context(), "go", "run", "./cmd/ate-setup", "publish", "release-images", "--no-dev-env", "--no-report-defaults",
		"--ko-docker-repo", "example.invalid/substrate", "--kubeconfig", kubeconfig, "--context", "not-a-cluster")
	publish.Dir, publish.Env = dest, append(cleanEnvironment(os.Environ()), "VERSION=not-a-release-tag")
	output, err := publish.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "is not a release tag") || strings.Contains(string(output), "while reading kubeconfig") {
		t.Fatalf("publisher reached the cluster or skipped release tag validation: %v\n%s", err, output)
	}
}
