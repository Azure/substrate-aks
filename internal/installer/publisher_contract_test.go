//go:build integration

package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedPodmanPublisherContract(t *testing.T) {
	root := pinnedContractRoot(t)
	dest := filepath.Join(t.TempDir(), "source")
	if output, err := exec.Command("git", "clone", "--quiet", "--shared", root, dest).CombinedOutput(); err != nil {
		t.Fatalf("copying pinned source: %v\n%s", err, output)
	}
	if err := prepareImagePublisher(dest); err != nil {
		t.Fatal(err)
	}
	if err := preparePodmanPublisher(dest); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(root, "cmd/dataplane/envoy/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	adapted, err := os.ReadFile(filepath.Join(dest, "cmd/dataplane/envoy/Dockerfile"))
	want := strings.Replace(string(original), "FROM rust:", "FROM docker.io/library/rust:", 1)
	want = strings.Replace(want, "FROM envoyproxy/envoy:", "FROM docker.io/envoyproxy/envoy:", 1)
	if err != nil || string(adapted) != want {
		t.Fatal("Podman base-image qualification changed the pinned image content or build recipe")
	}
	binary := filepath.Join(t.TempDir(), "ate-setup")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./cmd/ate-setup")
	build.Dir, build.Env = dest, cleanEnvironment(os.Environ())
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building real Podman-adapted publisher: %v\n%s", err, output)
	}
	bin := t.TempDir()
	writeTool := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nset -eu\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeTool("go", `test "$*" = "tool -n ko"
	printf '%s\n' "$PUBLISH_TEST_KO"
	`)
	writeTool("ko", `test "$1" = build
	test "$KO_DOCKER_REPO" = example.invalid/substrate
	test "$KO_DEFAULTPLATFORMS" = linux/amd64
	for arg in "$@"; do
	  case "$arg" in ./*) printf 'example.invalid/substrate/%s@sha256:%s\n' "${arg##*/}" "$PUBLISH_TEST_DIGEST" ;; esac
	done
	`)
	writeTool("docker", `printf 'unexpected Docker invocation\n' >&2
	exit 99
	`)
	writeTool("podman", `printf '%s\n' "$*" >> "$PUBLISH_TEST_CALLS"
	test "$2" = --authfile
	test "$3" = "$REGISTRY_AUTH_FILE"
	test -r "$3"
	case "$1" in
	  build)
	    test "$4" = --platform
	    test "$5" = linux/amd64
	    test "$6" = -t
	    test "$7" = example.invalid/substrate/envoy-dataplane:contract-podman
	    test "$8" = "$PUBLISH_TEST_CONTEXT"
	    test "$PUBLISH_TEST_FAILURE" != build
	    ;;
	  push)
	    test "$PUBLISH_TEST_FAILURE" != push
	    test "$4" = --digestfile
	    test "$6" = example.invalid/substrate/envoy-dataplane:contract-podman
	    test "$7" = docker://example.invalid/substrate/envoy-dataplane:contract-podman
	    if [ "$PUBLISH_TEST_FAILURE" = missing ]; then exit 0; fi
	    if [ "$PUBLISH_TEST_FAILURE" = invalid ]; then printf invalid > "$5"; else printf 'sha256:%s\n' "$PUBLISH_TEST_DIGEST" > "$5"; fi
	    ;;
	  *) exit 98 ;;
	esac
	`)
	authfile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(authfile, []byte(`{"auths":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"none", "build", "push", "missing", "invalid"} {
		t.Run(failure, func(t *testing.T) {
			calls := filepath.Join(t.TempDir(), "calls")
			cmd := exec.CommandContext(t.Context(), binary, "publish", "release-images", "--no-dev-env", "--no-report-defaults",
				"--ko-docker-repo", "example.invalid/substrate", "--ko-default-platforms", "linux/amd64")
			cmd.Dir = dest
			cmd.Env = append(cleanEnvironment(os.Environ()), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"PUBLISH_TEST_KO="+filepath.Join(bin, "ko"), "PUBLISH_TEST_DIGEST="+strings.Repeat("a", 64),
				"PUBLISH_TEST_CONTEXT="+filepath.Join(dest, "cmd/dataplane/envoy"), "PUBLISH_TEST_FAILURE="+failure,
				"PUBLISH_TEST_CALLS="+calls, "REGISTRY_AUTH_FILE="+authfile, "DOCKER_CONFIG="+filepath.Dir(authfile), "VERSION=contract-podman")
			output, err := cmd.CombinedOutput()
			if (err == nil) != (failure == "none") {
				t.Fatalf("publisher result for %s: %v\n%s", failure, err, output)
			}
			if failure == "none" && !strings.Contains(string(output), "example.invalid/substrate/envoy-dataplane@sha256:"+strings.Repeat("a", 64)) {
				t.Fatalf("publisher lost pushed Envoy digest:\n%s", output)
			}
			data, readErr := os.ReadFile(calls)
			if readErr != nil || !strings.HasPrefix(string(data), "build ") ||
				strings.Contains(string(data), "\npush ") != (failure != "build") {
				t.Fatalf("publisher did not execute expected Podman build/push sequence: %s, %v\n%s", data, readErr, output)
			}
		})
	}
}

func TestPinnedImagePublisherContract(t *testing.T) {
	root := pinnedContractRoot(t)
	dest := filepath.Join(t.TempDir(), "source")
	if output, err := exec.Command("git", "clone", "--quiet", "--shared", root, dest).CombinedOutput(); err != nil {
		t.Fatalf("copying pinned source: %v\n%s", err, output)
	}
	if err := prepareImagePublisher(dest); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "ate-setup")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./cmd/ate-setup")
	build.Dir = dest
	build.Env = cleanEnvironment(os.Environ())
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the actual adapted publisher: %v\n%s", err, output)
	}
	bin := t.TempDir()
	writeTool := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeTool("go", `if [ "$*" != "tool -n ko" ]; then
  printf 'unexpected Go command\n' >&2
  exit 19
fi
printf '%s\n' "$PUBLISH_TEST_KO"
`)
	const reachedBuilder = "publisher reached ko without Kubernetes"
	writeTool("ko", `if [ "$1" != build ] || [ "$KO_DOCKER_REPO" != example.invalid/substrate ] || [ "$KO_DEFAULTPLATFORMS" != linux/amd64 ]; then
  printf 'publisher lost its build target\n' >&2
  exit 19
fi
printf 'publisher reached ko without Kubernetes\n' >&2
exit 23
`)
	kubeconfig := filepath.Join(t.TempDir(), "invalid-kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("not kubeconfig YAML ["), 0600); err != nil {
		t.Fatal(err)
	}
	run := exec.CommandContext(t.Context(), binary, "publish", "release-images", "--no-dev-env", "--no-report-defaults",
		"--kubeconfig", kubeconfig, "--context", "not-a-cluster",
		"--ko-docker-repo", "example.invalid/substrate", "--ko-default-platforms", "linux/amd64")
	run.Dir = dest
	run.Env = append(cleanEnvironment(os.Environ()), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PUBLISH_TEST_KO="+filepath.Join(bin, "ko"), "VERSION=contract-publish", "KUBECONFIG="+kubeconfig)
	output, err := run.CombinedOutput()
	if err == nil || !strings.Contains(string(output), reachedBuilder) || strings.Contains(string(output), "while reading kubeconfig") {
		t.Fatalf("publisher did not reach its builder without a usable kubeconfig: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "while running ko build --tags=contract-publish") {
		t.Fatalf("publisher did not propagate the controlled build failure: %s", output)
	}

	// Cluster commands must still validate Kubernetes configuration.
	check := exec.CommandContext(t.Context(), binary, "deploy", "ate-system", "--no-dev-env", "--no-report-defaults",
		"--kubeconfig", kubeconfig, "--context", "not-a-cluster")
	check.Dir, check.Env = dest, run.Env
	output, err = check.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "while reading kubeconfig") || strings.Contains(string(output), reachedBuilder) {
		t.Fatalf("publisher adaptation bypassed deployment's Kubernetes checks: %v\n%s", err, output)
	}
}
