//go:build integration

package installer

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

var contractRoot = flag.String("contract-root", "", "checkout of the exact configured experimental commit")

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(runContractTests(m))
}

func runContractTests(m *testing.M) (code int) {
	if *contractRoot == "" {
		root, err := os.MkdirTemp("", "substrate-aks-contract-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() {
			if err := os.RemoveAll(root); err != nil {
				fmt.Fprintf(os.Stderr, "cleaning temporary contract checkout: %v\n", err)
				code = 1
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		fmt.Fprintf(os.Stderr, "Preparing public upstream contract source at %s\n", DefaultCommit)
		for _, args := range [][]string{
			{"init", "--quiet"},
			{"fetch", "--quiet", "--depth=1", DefaultRepository, DefaultCommit},
			{"checkout", "--quiet", "--detach", "FETCH_HEAD"},
		} {
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Dir, cmd.Env = root, cleanEnvironment(os.Environ())
			if output, err := cmd.CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "preparing pinned contract checkout: %v\n%s", err, output)
				return 1
			}
		}
		*contractRoot = root
	}
	return m.Run()
}

func TestPinnedManagedPostgresContract(t *testing.T) {
	root := pinnedContractRoot(t)
	command := exec.Command("go", "test", "./cmd/ate-setup/internal/config", "./cmd/ate-setup/internal/steps",
		"-run", "^(TestLoadPostgres.*|TestRecordNeverWritesASecret|TestPlanPostgres|TestBundledPostgresManifestExists|TestBuildAPIServerEnvVars|TestCreateAPIServerEnvVars.*|TestAnnotateAPIServerEnvHash|TestStampAPIServerEnvHash)$",
		"-count=1")
	command.Dir = root
	data, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("pinned external PostgreSQL selection, secret reconciliation or recording contract failed: %v\n%s", err, data)
	}
}

func pinnedContractRoot(t *testing.T) string {
	t.Helper()
	if *contractRoot == "" {
		t.Fatal("provide -contract-root pointing at the pinned checkout; this test never skips missing prerequisites")
	}
	root, err := filepath.Abs(*contractRoot)
	if err != nil {
		t.Fatal(err)
	}
	head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != DefaultCommit {
		t.Fatalf("contract checkout is not the selected pin: %s: %v", head, err)
	}
	dirty, err := exec.Command("git", "-C", root, "diff", "HEAD", "--", "manifests", "demos/counter").Output()
	if err != nil || len(dirty) != 0 {
		t.Fatalf("contract checkout must have pristine pinned manifests and counter templates, not previously adapted files: %v", err)
	}
	return root
}

func TestPinnedUpstreamContract(t *testing.T) {
	root := pinnedContractRoot(t)
	dest := t.TempDir()
	for _, subtree := range []string{"manifests", "demos/counter"} {
		err := filepath.WalkDir(filepath.Join(root, subtree), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			target := filepath.Join(dest, rel)
			if entry.IsDir() {
				return os.MkdirAll(target, 0700)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, 0600)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	c, i := testConfig(t), testInfrastructure()
	paths, err := adaptSource(dest, c, i)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{
		"manifests/ate-install/atelet.yaml",
		"manifests/ate-install/ate-api-server.yaml",
		"manifests/ate-install/postgres/postgres.yaml",
		"manifests/ate-install/ate-otel-config.yaml",
		"manifests/ate-install/base/kustomization.yaml",
		"demos/counter/counter-microvm.yaml.tmpl",
		"demos/counter/counter-microvm-template.yaml.tmpl",
	}) {
		t.Fatalf("adapter changed managed-source paths consumed by full install and component redeploy: %v", paths)
	}
	data, err := exec.Command("kubectl", "kustomize", "--load-restrictor=LoadRestrictionsNone", filepath.Join(dest, "manifests/ate-install/base")).CombinedOutput()
	if err != nil {
		t.Fatalf("upstream base overlay no longer renders: %v\n%s", err, data)
	}
	rendered := string(data)
	if strings.Count(rendered, c.Substrate.Images.Plugin) != 2 {
		t.Fatal("full deployment does not contain both Azure storage sidecars")
	}
	assertAzureWorkload(t, data, c, i, false)
	assertAzureWorkload(t, data, c, i, true)
	for _, forbidden := range []string{"kind: PodMonitoring", "gke-managed-otel", "/home/kubernetes/bin", "/etc/srv/kubernetes/cri_auth_config.yaml", "ATE_STORAGE_BACKEND"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("Azure deployment retained GKE-specific configuration: %s", forbidden)
		}
	}
	for _, required := range []string{"acr-credential-provider", "substrate-aks-acr-credentials", "/var/lib/kubelet/credential-provider", "/etc/kubernetes/azure.json"} {
		if !strings.Contains(rendered, required) {
			t.Fatalf("Azure deployment lost actor-image authentication: %s", required)
		}
	}
	for _, rel := range []string{"manifests/ate-install/atelet.yaml", "manifests/ate-install/ate-api-server.yaml"} {
		data, err := os.ReadFile(filepath.Join(dest, rel))
		if err != nil {
			t.Fatal(err)
		}
		node := strings.HasSuffix(rel, "/atelet.yaml")
		assertAzureWorkload(t, data, c, i, node)
		original, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		kind, name := "Deployment", "ate-api-server"
		if node {
			kind, name = "DaemonSet", "atelet"
		}
		before, err := nested(manifestObject(t, original, kind, name), "spec", "template", "spec")
		if err != nil {
			t.Fatal(err)
		}
		after, err := nested(manifestObject(t, data, kind, name), "spec", "template", "spec")
		if err != nil {
			t.Fatal(err)
		}
		oldPlugin, err := named(before["initContainers"], "objectstore-plugin")
		if err != nil {
			t.Fatal(err)
		}
		newPlugin, err := named(after["initContainers"], "objectstore-plugin")
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"securityContext", "resources", "envFrom"} {
			if !reflect.DeepEqual(oldPlugin[field], newPlugin[field]) {
				t.Fatalf("%s changed upstream sidecar %s", rel, field)
			}
		}
		oldMounts, ok := oldPlugin["volumeMounts"].([]any)
		if !ok {
			t.Fatal("upstream plugin volume mounts missing")
		}
		for _, old := range oldMounts {
			mount, ok := old.(map[string]any)
			if !ok {
				t.Fatal("upstream plugin mount is not an object")
			}
			updated, err := named(newPlugin["volumeMounts"], mount["name"].(string))
			if err != nil || !reflect.DeepEqual(updated, mount) {
				t.Fatalf("%s changed existing plugin storage mount %v: %v", rel, mount, err)
			}
		}
		if node {
			oldAtelet, err := named(before["containers"], "atelet")
			if err != nil {
				t.Fatal(err)
			}
			newAtelet, err := named(after["containers"], "atelet")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(oldAtelet["args"], newAtelet["args"]) {
				t.Fatal("Azure adaptation changed pinned atelet arguments, including mTLS and graceful shutdown")
			}
		}
	}
	postgres, err := os.ReadFile(filepath.Join(dest, "manifests/ate-install/postgres/postgres.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	postgresSpec, err := child(manifestObject(t, postgres, "StatefulSet", "postgres"), "spec")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := named(postgresSpec["volumeClaimTemplates"], "data")
	if err != nil {
		t.Fatal(err)
	}
	claimSpec, err := child(claim, "spec")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(claimSpec["accessModes"], []any{"ReadWriteOnce"}) || claimSpec["storageClassName"] != "managed-csi" {
		t.Fatalf("Azure PostgreSQL disk patch lost upstream access modes or managed storage: %v", claimSpec)
	}
	demoTemplate, err := os.ReadFile(filepath.Join(dest, "demos/counter/counter-microvm-template.yaml.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	var actorTemplate object
	if err := yaml.Unmarshal(demoTemplate, &actorTemplate); err != nil {
		t.Fatal(err)
	}
	snapshot, err := child(actorTemplate, "snapshotConfig")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot["storageLocation"] != "azblob://snapshots/ate-demo-counter-microvm/" ||
		snapshot["preferredFidelity"] != "SNAPSHOT_FIDELITY_MEMORY" ||
		strings.Contains(string(demoTemplate), "${BUCKET_NAME}") {
		t.Fatalf("counter ActorTemplate did not preserve memory snapshots on Azure Blob:\n%s", demoTemplate)
	}
	template, err := os.ReadFile(filepath.Join(dest, "manifests/microvm/sandboxconfig-microvm.yaml.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	var doc object
	if err := yaml.Unmarshal(template, &doc); err != nil {
		t.Fatal(err)
	}
	spec, err := child(doc, "spec")
	if err != nil {
		t.Fatal(err)
	}
	version, err := named(spec["versions"], "v1")
	if err != nil {
		t.Fatal(err)
	}
	assets, err := nested(version, "assets", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != len(assetNames) {
		t.Fatal("microVM asset set changed")
	}
	for name, file := range assetNames {
		entry, err := child(assets, name)
		if err != nil {
			t.Fatal(err)
		}
		if entry["url"] != "${BUCKET_SCHEME}://${BUCKET_NAME}/kata-assets/"+file {
			t.Fatalf("asset URL changed: %s", name)
		}
		hash, ok := entry["sha256"].(string)
		if !ok || !digestPattern.MatchString("asset@sha256:"+hash) {
			t.Fatalf("asset %s no longer has a fixed SHA-256", name)
		}
	}
}
