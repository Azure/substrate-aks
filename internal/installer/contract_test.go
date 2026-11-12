//go:build integration

package installer

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/Azure/substrate-aks/deploy"
	"go.yaml.in/yaml/v3"
)

var (
	contractRoot       = flag.String("contract-root", "", "checkout of the exact configured Substrate commit")
	pluginContractRoot = flag.String("plugin-contract-root", "", "checkout of the exact configured Azure Blob plugin commit")
)

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(runContractTests(m))
}

func runContractTests(m *testing.M) (code int) {
	for _, pin := range []struct {
		root *string
		name string
		s    Source
	}{
		{contractRoot, "upstream", DefaultConfig().Substrate.Source},
		{pluginContractRoot, "plugin", DefaultConfig().Substrate.PluginSource},
	} {
		if *pin.root != "" {
			continue
		}
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
		fmt.Fprintf(os.Stderr, "Preparing public %s contract source at %s\n", pin.name, pin.s.Commit)
		for _, args := range [][]string{
			{"init", "--quiet"},
			{"fetch", "--quiet", "--depth=1", pin.s.Repository, pin.s.Commit},
			{"checkout", "--quiet", "--detach", "FETCH_HEAD"},
		} {
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Dir, cmd.Env = root, cleanEnvironment(os.Environ())
			if output, err := cmd.CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "preparing pinned contract checkout: %v\n%s", err, output)
				return 1
			}
		}
		*pin.root = root
	}
	return m.Run()
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

func pinnedPluginRoot(t *testing.T) string {
	t.Helper()
	if *pluginContractRoot == "" {
		t.Fatal("provide -plugin-contract-root pointing at the pinned plugin checkout; this test never skips missing prerequisites")
	}
	head, err := exec.Command("git", "-C", *pluginContractRoot, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != DefaultPluginCommit {
		t.Fatalf("plugin contract checkout is not the selected pin: %s: %v", head, err)
	}
	dirty, err := exec.Command("git", "-C", *pluginContractRoot, "status", "--porcelain").Output()
	if err != nil || len(dirty) != 0 {
		t.Fatalf("plugin contract checkout must be pristine: %s: %v", dirty, err)
	}
	return *pluginContractRoot
}

func copyContractManifests(t *testing.T, root string) string {
	t.Helper()
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
	return dest
}

func TestPinnedUpstreamContract(t *testing.T) {
	root := pinnedContractRoot(t)
	pluginRoot := pinnedPluginRoot(t)
	for _, rel := range []string{pluginComponent, pluginAssetsComponent} {
		pinned, err := os.ReadFile(filepath.Join(pluginRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		vendored, err := os.ReadFile(filepath.Join("testdata/plugin", rel))
		if err != nil || !bytes.Equal(pinned, vendored) {
			t.Fatalf("testdata/plugin/%s differs from the pinned plugin; copy it from the pin: %v", rel, err)
		}
	}
	dest := copyContractManifests(t, root)
	c, i := testConfig(t), testInfrastructure()
	plugin, err := pluginComponents(pluginRoot, c, i)
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := os.ReadFile(filepath.Join(root, "cmd/atelet/internal/snapshottracker/background.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		fmt.Sprintf("DefaultUploadDelay = %d * time.Minute", int(defaultSnapshotUploadDelay/time.Minute)),
		fmt.Sprintf("DefaultDetachDelay = %d * time.Minute", int(defaultSnapshotDetachDelay/time.Minute)),
	} {
		if !strings.Contains(string(defaults), line) {
			t.Fatalf("pinned atelet delay default changed: expected %s", line)
		}
	}
	apiManifest, err := os.ReadFile(filepath.Join(root, "manifests/ate-install/ate-api-server.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	apiSpec, err := child(manifestObject(t, apiManifest, "Deployment", "ate-api-server"), "spec")
	if err != nil || apiSpec["replicas"] != defaultAPIServerReplicas {
		t.Fatalf("pinned API server replica default changed: %v, %v", apiSpec, err)
	}
	paths, err := adaptSource(dest, c, plugin)
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
		"manifests/ate-install/atenet-router.yaml",
	}) {
		t.Fatalf("adapter changed managed-source paths consumed by full install and component redeploy: %v", paths)
	}
	// The checksums lock the pre-sizing renderer output for the pinned sources
	// and synthetic installation values above.
	baselineHashes := []string{
		"ec187ac1ee357d0d72d2d2e7287baca88126100e46a9c6b2f6c713abd961ca3e",
		"c02eea429a519d750f47845c2b8133d38ae5c26e618bbdb3354812821a117f67",
		"63ae20e21f8d247aad7712b19f58a2de09166f5f03255f068d97e92442bbdb8e",
		"56d8e49e46f782d89600f317f267944be14d5649b5fca048e411a734683821ae",
		"fe434c96db1e93c8dcbd4ef58e97e367f48981f6bc33510b627c8015e00c5a29",
		"29f26edae8eba7077c90c43f0399223fe727cd7b4f8850c8331fb1f54fe67d10",
		"f3b7aa4af564e604d801faaec39a50beaf63a6ae4d263fc1c291fd65a049e1c0",
	}
	for n, rel := range paths {
		original, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		var baseline []byte
		switch n {
		case 0:
			baseline, err = deploy.Render(original, deploy.Values{External: plugin, Component: "actor-images"})
		case 1:
			baseline, err = deploy.Render(original, deploy.Values{External: plugin})
		case 2:
			baseline, err = deploy.Render(original, deploy.Values{Component: "postgres"})
		case 3:
			baseline, err = deploy.Render(original, deploy.Values{Component: "telemetry"})
		case 4:
			baseline, err = deploy.AdaptBase(original)
		case 5:
			baseline, err = adaptDemo(original)
		case 6:
			baseline = original
		}
		if err != nil {
			t.Fatal(err)
		}
		if checksum(baseline) != baselineHashes[n] {
			t.Fatalf("pre-sizing renderer baseline changed for %s", rel)
		}
		actual, err := os.ReadFile(filepath.Join(dest, rel))
		if err != nil || !bytes.Equal(baseline, actual) {
			t.Fatalf("unset sizing changed %s: %v", rel, err)
		}
	}
	for _, workload := range []struct {
		path, name string
		containers []string
	}{
		{paths[1], "ate-api-server", []string{"ate-api-server"}},
		{paths[6], "atenet-router", []string{"atenet-router", "envoy"}},
	} {
		data, err := os.ReadFile(filepath.Join(dest, workload.path))
		if err != nil {
			t.Fatal(err)
		}
		pod, err := nested(manifestObject(t, data, "Deployment", workload.name), "spec", "template", "spec")
		if err != nil {
			t.Fatal(err)
		}
		containers, ok := pod["containers"].([]any)
		if !ok || len(containers) != len(workload.containers) {
			t.Fatalf("pinned %s containers changed", workload.name)
		}
		for _, name := range workload.containers {
			container, err := named(containers, name)
			if err != nil || container["resources"] != nil {
				t.Fatalf("pinned %s default requests changed: %v, %v", name, container, err)
			}
		}
		if workload.name == "ate-api-server" {
			sidecar, err := named(pod["initContainers"], "objectstore-plugin")
			if err != nil || sidecar["resources"] != nil {
				t.Fatalf("pinned API sidecar default requests changed: %v, %v", sidecar, err)
			}
		}
	}
	t.Run("configured sizing on pinned workloads", func(t *testing.T) {
		replicas, pool := 3, 10
		c.Substrate.Sizing = &SizingConfig{
			APIServer: &APIServerSizing{Replicas: &replicas, PostgresPoolMaxConns: &pool, Resources: &ResourceRequirements{
				Requests: map[string]string{"cpu": "500m", "memory": "1Gi"},
			}},
			Router: &RouterSizing{Replicas: &replicas, Resources: &RouterResources{
				AtenetRouter: &ResourceRequirements{Requests: map[string]string{"cpu": "250m"}},
				Envoy:        &ResourceRequirements{Limits: map[string]string{"memory": "1Gi"}},
			}},
		}
		c.Substrate.Atelet = &AteletConfig{stringPointer("10s"), stringPointer("30s")}
		first, second := copyContractManifests(t, root), copyContractManifests(t, root)
		if _, err := adaptSource(first, c, plugin); err != nil {
			t.Fatal(err)
		}
		if _, err := adaptSource(second, c, plugin); err != nil {
			t.Fatal(err)
		}
		for _, rel := range paths {
			one, err := os.ReadFile(filepath.Join(first, rel))
			if err != nil {
				t.Fatal(err)
			}
			two, err := os.ReadFile(filepath.Join(second, rel))
			if err != nil || !bytes.Equal(one, two) {
				t.Fatalf("repeat render changed %s: %v", rel, err)
			}
		}
		for _, item := range []struct {
			rel, kind, name string
			replicas        int
			resources       map[string]object
		}{
			{paths[1], "Deployment", "ate-api-server", 3, map[string]object{"ate-api-server": {"requests": object{"cpu": "500m", "memory": "1Gi"}}}},
			{paths[6], "Deployment", "atenet-router", 3, map[string]object{"atenet-router": {"requests": object{"cpu": "250m"}}, "envoy": {"limits": object{"memory": "1Gi"}}}},
		} {
			data, err := os.ReadFile(filepath.Join(first, item.rel))
			if err != nil {
				t.Fatal(err)
			}
			doc := manifestObject(t, data, item.kind, item.name)
			spec, err := child(doc, "spec")
			if err != nil || spec["replicas"] != item.replicas {
				t.Fatalf("%s replicas: %v %v", item.name, spec, err)
			}
			pod, err := nested(spec, "template", "spec")
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range item.resources {
				container, err := named(pod["containers"], name)
				if err != nil || !reflect.DeepEqual(container["resources"], want) {
					t.Fatalf("%s resources = %v, %v", name, container["resources"], err)
				}
			}
		}
		data, err := os.ReadFile(filepath.Join(first, paths[0]))
		if err != nil {
			t.Fatal(err)
		}
		assertAzureWorkload(t, data, c, i, true)
		for _, flag := range []string{"--snapshot-upload-delay=10s", "--snapshot-detach-delay=30s"} {
			if strings.Count(string(data), flag) != 1 {
				t.Fatalf("atelet missing configured %s", flag)
			}
		}
	})
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
	for _, forbidden := range []string{"kind: PodMonitoring", "gke-managed-otel", "/home/kubernetes/bin", "/etc/srv/kubernetes/cri_auth_config.yaml", "ATE_STORAGE_BACKEND",
		// The placeholder transfer account must never reach a process.
		"--transfer-service-account"} {
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
	// The demo stores snapshots under ate-api-server's azblob:// --snapshot-location.
	if _, set := snapshot["storageLocation"]; set || snapshot["preferredFidelity"] != "SNAPSHOT_FIDELITY_MEMORY" {
		t.Fatalf("counter ActorTemplate no longer uses the cluster snapshot location with memory snapshots:\n%s", demoTemplate)
	}
	template, err := os.ReadFile(filepath.Join(dest, sandboxTemplate))
	if err != nil {
		t.Fatal(err)
	}
	var doc object
	if err := yaml.Unmarshal(template, &doc); err != nil {
		t.Fatal(err)
	}
	version, err := defaultSandboxVersion(doc)
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
	for name := range assetNames {
		entry, err := child(assets, name)
		if err != nil {
			t.Fatal(err)
		}
		hash, ok := entry["sha256"].(string)
		if !ok || !digestPattern.MatchString("asset@sha256:"+hash) {
			t.Fatalf("asset %s no longer has a fixed SHA-256", name)
		}
		if entry["url"] != "${BUCKET_SCHEME}://${ASSET_BUCKET_NAME}/"+assetPrefix+hash {
			t.Fatalf("asset URL changed: %s", name)
		}
	}
}

// TestPinnedUpstreamFlagsContract runs the pinned ate-setup with every flag
// the installer passes, so a renamed or removed flag fails here.
func TestPinnedUpstreamFlagsContract(t *testing.T) {
	root := pinnedContractRoot(t)
	binary := filepath.Join(t.TempDir(), "ate-setup")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./cmd/ate-setup")
	build.Dir, build.Env = root, cleanEnvironment(os.Environ())
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building pinned ate-setup: %v\n%s", err, output)
	}
	for _, args := range [][]string{{"deploy", "ate-system"}, {"deploy", "atelet"}, {"deploy", "apiserver"}, {"deploy", "demo", "counter-microvm"}} {
		var command Command
		r := prerequisiteRunner(func(_ context.Context, c Command) ([]byte, error) {
			command = c
			return nil, nil
		})
		c := testConfig(t)
		pool := 12
		c.Substrate.Sizing = &SizingConfig{APIServer: &APIServerSizing{PostgresPoolMaxConns: &pool}}
		if err := (App{Config: c, Runner: r}).upstream(t.Context(), root, args...); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(command.Args[:3], []string{"go", "run", "./cmd/ate-setup"}) {
			t.Fatalf("installer no longer runs the pinned ate-setup: %v", command.Args)
		}
		help := exec.CommandContext(t.Context(), binary, append(command.Args[3:], "--help")...)
		help.Dir, help.Env = root, cleanEnvironment(os.Environ())
		if output, err := help.CombinedOutput(); err != nil {
			t.Fatalf("pinned ate-setup rejects installer arguments %v: %v\n%s", command.Args[3:], err, output)
		}
	}
}

// TestPinnedTransferPlaceholderContract keeps the GCS transfer account
// placeholders inert: the pinned ate-setup must read the GCP settings only
// where they were reviewed.
func TestPinnedTransferPlaceholderContract(t *testing.T) {
	root := pinnedContractRoot(t)
	data, err := exec.Command("git", "-C", root, "grep", "-l", "-E", `\.(ProjectID|TransferGSAName|TransferServiceAccount\(\))`,
		"--", "cmd/ate-setup", ":!*_test.go").Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(data))
	want := []string{
		// TransferServiceAccount, written to ATE_API_SNAPSHOT_TRANSFER_SERVICE_ACCOUNT.
		"cmd/ate-setup/internal/config/config.go",
		"cmd/ate-setup/internal/config/credentials.go", // skipped: --context is set
		"cmd/ate-setup/internal/steps/apienv.go",
		"cmd/ate-setup/internal/steps/create.go",  // needs a GKE cluster location and name too
		"cmd/ate-setup/internal/steps/microvm.go", // skipped: --sandbox-classes=gvisor
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pinned ate-setup reads the GCP placeholders in new places; review them: %v", got)
	}
	manifest, err := os.ReadFile(filepath.Join(root, "manifests/ate-install/ate-api-server.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(manifest), "$(ATE_API_SNAPSHOT_TRANSFER_SERVICE_ACCOUNT)"); n != 1 {
		t.Fatalf("the transfer account reaches %d arguments; only the replaced control sidecar argument was reviewed", n)
	}
}

func TestPinnedReleaseInventoryContract(t *testing.T) {
	root := pinnedContractRoot(t)
	dest := filepath.Join(t.TempDir(), "source")
	if output, err := exec.Command("git", "clone", "--quiet", "--shared", root, dest).CombinedOutput(); err != nil {
		t.Fatalf("copying pinned source: %v\n%s", err, output)
	}
	if err := prepareHelper(dest); err != nil {
		t.Fatal(err)
	}
	run := func(revision string) ([]byte, error) {
		cmd := exec.CommandContext(t.Context(), "go", "run", "./cmd/ate-setup", "publish", "release-inventory",
			DefaultReleaseImages, DefaultTag, revision, "--no-dev-env", "--no-report-defaults")
		cmd.Dir, cmd.Env = dest, append(cleanEnvironment(os.Environ()), "NO_DEV_ENV=1")
		cmd.Stderr = os.Stderr
		return cmd.Output()
	}
	data, err := run(DefaultCommit)
	if err != nil {
		t.Fatalf("the pinned release images are not published from the pinned commit: %v", err)
	}
	var inventory []releaseImage
	if err := json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, image := range inventory {
		names[image.Name] = true
	}
	for _, name := range []string{"ateapi", "atelet", "ateworker-microvm", "objectstore-plugin", "envoy-dataplane"} {
		if !names[name] {
			t.Fatalf("release inventory lacks %s: %v", name, inventory)
		}
	}
	if _, err := run(strings.Repeat("0", 40)); err == nil {
		t.Fatal("release inventory accepted images built from another commit")
	}
}

func TestPinnedMicroVMAssetsContract(t *testing.T) {
	root := pinnedContractRoot(t)
	dest := filepath.Join(t.TempDir(), "source")
	if output, err := exec.Command("git", "clone", "--quiet", "--shared", root, dest).CombinedOutput(); err != nil {
		t.Fatalf("copying pinned source: %v\n%s", err, output)
	}
	if err := prepareHelper(dest); err != nil {
		t.Fatal(err)
	}
	assets := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "go", "run", "./cmd/ate-setup", "publish", "microvm-assets",
		filepath.Join(dest, sandboxTemplate), assets, "amd64", "--no-dev-env", "--no-report-defaults")
	cmd.Dir, cmd.Env = dest, append(cleanEnvironment(os.Environ()), "NO_DEV_ENV=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("assembling the pinned microVM assets: %v\n%s", err, output)
	}
	manifest, blobs, err := sandboxManifest(dest, assets, testInfrastructure())
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != len(assetNames) || strings.Contains(string(manifest), "arm64") || strings.Contains(string(manifest), "${") {
		t.Fatalf("sandbox configuration is not the amd64 Blob-backed asset set:\n%s", manifest)
	}
}
