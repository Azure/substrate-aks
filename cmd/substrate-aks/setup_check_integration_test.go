package main

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/internal/installer"
)

func setupCheckClient(t *testing.T) (string, string) {
	t.Helper()
	root, err := os.MkdirTemp(".", ".setup-client-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.MkdirAll(filepath.Join(root, "cmd/kubectl-ate"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module go.ate.dev/substrate\n\ngo 1.27.3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cmd/kubectl-ate/main.go"), []byte("package main\nfunc main() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "kubectl-ate")
	build := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=true", "-o", path, "./cmd/kubectl-ate")
	build.Dir = root
	build.Env = append(os.Environ(), "GOWORK=off")
	if data, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building client fixture: %v\n%s", err, data)
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range info.Settings {
		if entry.Key == "vcs.revision" && entry.Value != "" {
			return dir, entry.Value
		}
	}
	t.Fatal("client fixture has no revision")
	return "", ""
}

func setupCheckFingerprint(c installer.Config, i installer.Infrastructure) string {
	type sourceSettings struct {
		installer.Source
		PluginSource installer.Source `json:"pluginSource"`
		ImageRepo    string           `json:"imageRepo"`
		ImageTag     string           `json:"imageTag"`
		PluginImage  string           `json:"pluginImage"`
		AssetsDir    string           `json:"assetsDir"`
	}
	s := sourceSettings{c.Substrate.Source, c.Substrate.PluginSource, c.Substrate.Images.Repository, c.Substrate.Images.Tag, c.Substrate.Images.Plugin, c.Substrate.Runtime.AssetsDir}
	data, _ := json.Marshal(struct {
		Source         sourceSettings
		Infrastructure installer.Infrastructure
		Adapter        int
	}{s, i, 6})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func setupCheckPrepared(t *testing.T, c installer.Config, i installer.Infrastructure) {
	t.Helper()
	fp := setupCheckFingerprint(c, i)
	root := filepath.Join(c.Installation.Dir, "source-"+fp+"-v2")
	if err := os.MkdirAll(filepath.Join(root, "manifests/ate-install"), 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for n := 0; n < 6; n++ {
		name := fmt.Sprintf("adapted-%d.yaml", n)
		content := []byte(name)
		if err := os.WriteFile(filepath.Join(root, name), content, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		files[name] = hex.EncodeToString(sum[:])
	}
	marker, _ := json.Marshal(map[string]any{"fingerprint": fp, "files": files})
	if err := os.WriteFile(filepath.Join(root, ".substrate-aks-prepared.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	template := `apiVersion: ate.dev/v1
kind: SandboxConfig
metadata: {name: microvm}
spec:
  defaultVersion: v2
  versions:
  - name: v2
    assets:
      amd64:
`
	for name, file := range map[string]string{"cloud-hypervisor": "cloud-hypervisor", "virtiofsd": "virtiofsd", "kata-kernel": "vmlinux", "kata-image": "rootfs.img"} {
		content := []byte("synthetic " + name)
		if err := os.WriteFile(filepath.Join(c.Substrate.Runtime.AssetsDir, file), content, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		digest := hex.EncodeToString(sum[:])
		template += fmt.Sprintf("        %s:\n          url: \"${BUCKET_SCHEME}://${ASSET_BUCKET_NAME}/microvm-assets/amd64/%s\"\n          sha256: \"%s\"\n", name, digest, digest)
	}
	if err := os.WriteFile(filepath.Join(root, "manifests/ate-install/sandboxconfig-microvm.yaml.tmpl"), []byte(template), 0600); err != nil {
		t.Fatal(err)
	}
}

func setupCheckRestorePrepared(t *testing.T, c installer.Config, i installer.Infrastructure) {
	t.Helper()
	root := filepath.Join(c.Installation.Dir, "source-"+setupCheckFingerprint(c, i)+"-v2")
	if err := os.MkdirAll(filepath.Join(root, "demos/counter"), 0700); err != nil {
		t.Fatal(err)
	}
	pool := `apiVersion: v1
kind: Namespace
metadata: {name: ate-demo-counter-microvm}
---
apiVersion: ate.dev/v1
kind: WorkerPool
metadata:
  name: counter-microvm
  namespace: ate-demo-counter-microvm
  labels: {workload: counter-microvm}
spec:
  replicas: 2
  workerImage: ko://go.ate.dev/substrate/cmd/ateworker-microvm
  template:
    resources: {}
`
	template := `metadata: {name: counter-microvm, atespace: ate-demo-counter-microvm}
workerSelector: {matchLabels: {workload: counter-microvm}}
snapshotConfig: {preferredFidelity: SNAPSHOT_FIDELITY_MEMORY}
containers:
- {name: counter, image: "ko://go.ate.dev/substrate/demos/counter"}
`
	poolPath := "demos/counter/counter-microvm.yaml.tmpl"
	for name, content := range map[string]string{poolPath: pool, "demos/counter/counter-microvm-template.yaml.tmpl": template} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	markerPath := filepath.Join(root, ".substrate-aks-prepared.json")
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	var marker struct {
		Fingerprint string            `json:"fingerprint"`
		Files       map[string]string `json:"files"`
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(pool))
	delete(marker.Files, "adapted-5.yaml")
	marker.Files[poolPath] = hex.EncodeToString(sum[:])
	data, err = json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSetupCallsActualCheckAfterClientAndBeforeDemo(t *testing.T) {
	testSetupActualCheck(t, nil)
}

func testSetupActualCheck(t *testing.T, guided func(setupOptions, commandRunner) error) {
	t.Helper()
	dir, revision := setupCheckClient(t)
	client, err := os.ReadFile(filepath.Join(dir, "kubectl-ate"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "kubectl-ate")); err != nil {
		t.Fatal(err)
	}
	path := testCLIConfig(t)
	c, err := installer.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Substrate.Source.Commit = revision
	c.Substrate.Images = installer.ImagesConfig{Repository: "example.invalid/images", Tag: "synthetic", Plugin: "example.invalid/plugin@sha256:" + strings.Repeat("a", 64)}
	c.Substrate.Runtime.AssetsDir = t.TempDir()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	i := installer.Infrastructure{
		ClusterName: c.AKS.ClusterName, ResourceGroup: c.Azure.ResourceGroup,
		StorageAccount: "substratetest123", AccountURL: "https://substratetest123.blob.core.windows.net",
		SnapshotsContainer: "snapshots", AssetsContainer: "assets",
		NodeClientID: "44444444-4444-4444-8444-444444444444", ControlClientID: "55555555-5555-4555-8555-555555555555",
		TenantID: "66666666-6666-4666-8666-666666666666",
	}
	setupCheckPrepared(t, c, i)
	if guided != nil {
		setupCheckRestorePrepared(t, c, i)
	}
	phase := "installing:ate-system"
	var namespace, token string
	poolExists := guided == nil
	inventories := 0
	var mutationCalls []string
	runner := defaultsRunner(func(_ context.Context, cmd installer.Command) ([]byte, error) {
		joined := strings.Join(cmd.Args, " ")
		switch {
		case strings.Contains(joined, "group exists"):
			return []byte("true"), nil
		case strings.Contains(joined, "group show"):
			return []byte(fmt.Sprintf(`{"location":%q,"tags":{"substrate-aks-installation":%q}}`, c.Azure.Location, c.Installation.ID)), nil
		case strings.Contains(joined, "deployment sub show"):
			return json.Marshal(i)
		case strings.Contains(joined, "aks get-credentials"):
			config := fmt.Sprintf(`{"apiVersion":"v1","kind":"Config","current-context":%q,"contexts":[{"name":%q,"context":{"cluster":"aks","user":"user"}}],"clusters":[{"name":"aks","cluster":{"server":"https://aks.example.invalid"}}],"users":[{"name":"user","user":{"token":"synthetic"}}]}`, c.AKS.ClusterName, c.AKS.ClusterName)
			for j, arg := range cmd.Args {
				if arg == "--file" && j+1 < len(cmd.Args) {
					return nil, os.WriteFile(cmd.Args[j+1], []byte(config), 0600)
				}
			}
			return nil, fmt.Errorf("missing kubeconfig path")
		case strings.Contains(joined, "get --raw /version"):
			return []byte(`{"gitVersion":"v1.37.1"}`), nil
		case strings.Contains(joined, "api-resources"):
			return []byte("podcertificaterequests.certificates.k8s.io\nclustertrustbundles.certificates.k8s.io\n"), nil
		case strings.Contains(joined, "wait node"):
			return nil, nil
		case strings.Contains(joined, "get nodes"):
			return []byte(`{"items":[{"metadata":{"labels":{"substrate-aks/runtime":"microvm"}},"status":{"nodeInfo":{"kubeletVersion":"v1.37.1","architecture":"amd64","operatingSystem":"linux"}}}]}`), nil
		case strings.Contains(joined, "get namespace ate-system"):
			return []byte(`{"metadata":{"name":"ate-system"}}`), nil
		case strings.Contains(joined, "get configmap substrate-aks-install"):
			return []byte(fmt.Sprintf(`{"metadata":{"name":"substrate-aks-install","namespace":"ate-system"},"data":{"installationId":%q,"fingerprint":%q,"phase":%q}}`, c.Installation.ID, setupCheckFingerprint(c, i), phase)), nil
		case strings.Contains(joined, "get deployment ate-api-server"):
			return []byte(`{"metadata":{"name":"ate-api-server","namespace":"ate-system"}}`), nil
		case strings.Contains(joined, "go env -json"):
			return []byte(fmt.Sprintf(`{"GOBIN":%q}`, dir)), nil
		case strings.Contains(joined, "git rev-parse --verify"), strings.Contains(joined, "git rev-parse HEAD"):
			return []byte(revision), nil
		case strings.Contains(joined, "git diff --name-only"):
			return nil, nil
		case strings.Contains(joined, "git ls-files"):
			return []byte(".substrate-aks-prepared.json\x00"), nil
		case strings.Contains(joined, "git init"), strings.Contains(joined, "git fetch"):
			return nil, nil
		case strings.Contains(joined, "git checkout"):
			for _, rel := range []string{"manifests/ate-install", "cmd/ate-setup/internal/cmd", "cmd/ate-setup/internal/steps"} {
				if err := os.MkdirAll(filepath.Join(cmd.Dir, rel), 0700); err != nil {
					return nil, err
				}
			}
			return nil, os.WriteFile(filepath.Join(cmd.Dir, "manifests/ate-install/sandboxconfig-microvm.yaml.tmpl"), []byte("synthetic"), 0600)
		case strings.Contains(joined, "publish check-images"):
			return []byte(`{"status":"available","checked":3,"expected":3}`), nil
		case strings.Contains(joined, "publish resolve-check-images"):
			var manifests []string
			if err := json.Unmarshal(cmd.Input, &manifests); err != nil {
				return nil, err
			}
			for j := range manifests {
				manifests[j] = strings.ReplaceAll(manifests[j], "ko://go.ate.dev/substrate/cmd/ateworker-microvm", "example.invalid/worker@sha256:"+strings.Repeat("a", 64))
				manifests[j] = strings.ReplaceAll(manifests[j], "ko://go.ate.dev/substrate/demos/counter", "example.invalid/counter@sha256:"+strings.Repeat("b", 64))
			}
			return json.Marshal(manifests)
		case strings.Contains(joined, " apply -f -"):
			var list struct {
				Items []struct {
					Data map[string]string `json:"data"`
				} `json:"items"`
			}
			if json.Unmarshal(cmd.Input, &list) == nil {
				for _, item := range list.Items {
					if item.Data["phase"] != "" {
						phase = item.Data["phase"]
					}
				}
			}
			return nil, nil
		case strings.Contains(joined, "rollout status daemonset"), strings.Contains(joined, "delete daemonset"), strings.Contains(joined, "storage blob upload"), strings.Contains(joined, "go run ./cmd/ate-setup deploy"):
			return nil, nil
		case strings.Contains(joined, "go install ./cmd/kubectl-ate"):
			return nil, os.WriteFile(filepath.Join(dir, "kubectl-ate"), client, 0700)
		case strings.Contains(joined, "publish restore-worker-pools"):
			inventories++
			if poolExists {
				return []byte(fmt.Sprintf(`{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[{"metadata":{"name":"counter-microvm","namespace":%q,"uid":"pool-id"}}]}`, namespaceOrOther(namespace))), nil
			}
			return []byte(`{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[]}`), nil
		case strings.Contains(joined, "publish restore-inventory"):
			return []byte(`{"workers":false,"actors":false,"actorTemplates":false}`), nil
		case strings.Contains(joined, "go build -o"):
			return nil, nil
		case strings.Contains(joined, "get workers"), strings.Contains(joined, "get actors"), strings.Contains(joined, "get actor-templates"):
			return []byte(`{}`), nil
		case strings.Contains(joined, " create -f - --output=json"):
			mutationCalls = append(mutationCalls, joined)
			var obj struct {
				Kind     string `json:"kind"`
				Metadata struct {
					Name   string            `json:"name"`
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(cmd.Input, &obj); err != nil {
				return nil, err
			}
			if obj.Kind == "Namespace" {
				namespace = obj.Metadata.Name
				token = obj.Metadata.Labels["substrate-aks.io/check"]
				return []byte(fmt.Sprintf(`{"kind":"Namespace","metadata":{"name":%q,"uid":"namespace-id","labels":{"substrate-aks.io/check":%q,"substrate-aks.io/installation":%q}}}`, namespace, token, c.Installation.ID)), nil
			}
			poolExists = true
			return []byte(fmt.Sprintf(`{"kind":"WorkerPool","metadata":{"name":"counter-microvm","namespace":%q,"uid":"pool-id","labels":{"substrate-aks.io/check":%q,"substrate-aks.io/installation":%q}}}`, namespace, token, c.Installation.ID)), nil
		case strings.Contains(joined, " create atespace "):
			mutationCalls = append(mutationCalls, joined)
			return []byte(fmt.Sprintf(`{"metadata":{"name":%q,"uid":"space-id"}}`, namespace)), nil
		case strings.Contains(joined, " create actor-template "):
			mutationCalls = append(mutationCalls, joined)
			return nil, errors.New("synthetic template failure")
		case strings.Contains(joined, " get actor-template "):
			return []byte(`{}`), nil
		case strings.Contains(joined, " wait deployment/"), strings.Contains(joined, " rollout status deployment/"):
			return nil, nil
		case strings.Contains(joined, " delete "):
			mutationCalls = append(mutationCalls, joined)
			return nil, fmt.Errorf("unexpected delete: %s", joined)
		default:
			return nil, fmt.Errorf("unexpected command: %s", joined)
		}
	})
	var commands, progress []string
	options := setupOptions{home: recordHome(path), name: recordName(path), yes: guided == nil, withDemo: true, skipContextHandoff: true, discovery: subscriptionDisplayRunner(t)}
	execute := func(ctx context.Context, args []string, _, stderr io.Writer) error {
		commands = append(commands, args[0])
		if args[0] != "check" && args[0] != "client" && args[0] != "install" {
			return nil
		}
		current, err := installer.LoadInstallation(recordHome(path), recordName(path))
		if err != nil {
			return err
		}
		lock, err := installer.Lock(current)
		if err != nil {
			return err
		}
		defer lock.Close()
		a := installer.App{Config: current, Runner: runner, Progress: func(f string, args ...any) { progress = append(progress, fmt.Sprintf(f, args...)) }}
		if args[0] == "check" && guided != nil {
			log, err := installer.NewLog(current)
			if err != nil {
				return err
			}
			defer log.Close()
			p := newProgress(stderr, log, "check", false)
			p.session = setupSessionFrom(ctx)
			defer p.stop()
			a.RecoveryReceipt = p.recoveryReceipt
		}
		switch args[0] {
		case "install":
			return a.Install(ctx, "ate-system")
		case "client":
			_, err := a.SetupClient(ctx)
			return err
		default:
			return a.Check(ctx, false)
		}
	}
	if guided != nil {
		err = guided(options, execute)
	} else {
		err = setupForTest(t.Context(), options, strings.NewReader(""), io.Discard, io.Discard, execute)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(progress, "\n"), "RESTORE_SKIPPED_BUSY") {
		t.Fatal("setup did not preserve the busy skip")
	}
	if phase != "deployed" {
		t.Fatalf("interrupted installation was not repaired: %s", phase)
	}
	wantSuffix := ",client,check,demo"
	if guided != nil {
		wantSuffix = ",client,check,check,demo"
		if inventories != 2 || !poolExists || len(mutationCalls) != 4 {
			t.Fatalf("retry inventory=%d pool=%v mutations=%v", inventories, poolExists, mutationCalls)
		}
	}
	if got := strings.Join(commands, ","); !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("incorrect stage order: %s", got)
	}
}

func namespaceOrOther(namespace string) string {
	if namespace != "" {
		return namespace
	}
	return "other"
}
