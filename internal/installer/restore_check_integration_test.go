package installer

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func restoreTestClient(t *testing.T) (string, string) {
	t.Helper()
	root, err := os.MkdirTemp(".", ".restore-client-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.MkdirAll(filepath.Join(root, "cmd", "kubectl-ate"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		"go.mod":                  "module go.ate.dev/substrate\n\ngo 1.27.3\n",
		"cmd/kubectl-ate/main.go": "package main\nfunc main() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "kubectl-ate")
	command := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=true", "-o", path, "./cmd/kubectl-ate")
	command.Dir = root
	command.Env = append(os.Environ(), "GOWORK=off")
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("building client fixture: %v\n%s", err, data)
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			return dir, setting.Value
		}
	}
	t.Fatal("client fixture has no revision")
	return "", ""
}

func TestRestoreCheckBusyAndUnknownInventory(t *testing.T) {
	dir, revision := restoreTestClient(t)
	for _, tc := range []struct {
		name      string
		required  bool
		output    string
		listError error
		want      string
	}{
		{"busy default", false, `{"workers":true,"actors":false,"actorTemplates":false}`, nil, "RESTORE_SKIPPED_BUSY"},
		{"busy required", true, `{"workers":true,"actors":false,"actorTemplates":false}`, nil, "PREREQUISITE"},
		{"unknown", false, `null`, nil, "inventory"},
		{"denied", false, "", errors.New("access denied"), "access denied"},
		{"busy with unknown", false, `null`, nil, "inventory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			c.Substrate.Source.Commit = revision
			r := &restoreCheckRunner{c: c, clientDir: dir, restore: &restoreRunner{config: c}, inventory: []byte(tc.output), inventoryErr: tc.listError}
			if tc.name == "busy with unknown" {
				r.pools = []byte(`{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[{"metadata":{"name":"x","namespace":"n","uid":"u"}}]}`)
			}
			var progress []string
			a := App{Config: c, Runner: r, Progress: func(f string, args ...any) { progress = append(progress, fmt.Sprintf(f, args...)) }}
			lock, err := Lock(c)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			err = a.Check(context.Background(), tc.required)
			text := strings.Join(progress, "\n")
			if err != nil {
				text += "\n" + err.Error()
			}
			if !strings.Contains(text, tc.want) || (err == nil) != (tc.name == "busy default") {
				t.Fatalf("outcome=%v progress=%s", err, text)
			}
			if strings.Contains(text, "RESTORE_PASSED") {
				t.Fatal("busy or unknown inventory claimed restore")
			}
			if tc.name != "busy default" && strings.Contains(text, "RESTORE_SKIPPED_BUSY") {
				t.Fatal("unknown or required inventory became a skip")
			}
			if len(r.restore.calls) != 0 {
				t.Fatalf("restore mutation after busy or unknown inventory: %v", r.restore.calls)
			}
		})
	}
}

func TestRestoreCheckMissingClientFailsBeforeMutation(t *testing.T) {
	c := testConfig(t)
	runner := &restoreCheckRunner{c: c, clientDir: t.TempDir(), restore: &restoreRunner{config: c}}
	var progress []string
	err := (App{Config: c, Runner: runner, Progress: func(f string, args ...any) { progress = append(progress, fmt.Sprintf(f, args...)) }}).Check(t.Context(), false)
	if err == nil || !strings.Contains(err.Error(), "substrate-aks client "+c.Installation.Name) {
		t.Fatalf("missing client guidance: %v", err)
	}
	if len(runner.restore.calls) != 0 || !strings.Contains(strings.Join(progress, "\n"), "RESTORE_FAILED") {
		t.Fatalf("restore mutation or missing failure marker: %v, %v", runner.restore.calls, progress)
	}
}

func TestRestoreCheckWorkerPoolFailuresPreventMutation(t *testing.T) {
	dir, revision := restoreTestClient(t)
	for _, tc := range []struct {
		name, output string
		failure      error
	}{
		{"empty continuation", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","metadata":{"continue":"next"},"items":[]}`, nil},
		{"invalid item", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[{}]}`, nil},
		{"missing items", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList"}`, nil},
		{"null", `null`, nil},
		{"duplicate", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[],"items":[]}`, nil},
		{"multiple", `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[{},{}]}`, nil},
		{"overflow", "", errors.New("response exceeds size limit")},
		{"truncated", "", errors.New("incomplete response")},
		{"auth", "", errors.New("access denied")},
		{"redirect", "", errors.New("HTTP status 302")},
		{"throttled", "", errors.New("HTTP status 429")},
		{"unavailable", "", errors.New("HTTP status 503")},
		{"deadline", "", context.DeadlineExceeded},
	} {
		for _, required := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/required=%v", tc.name, required), func(t *testing.T) {
				c := testConfig(t)
				c.Substrate.Source.Commit = revision
				script := &restoreRunner{config: c}
				r := &restoreCheckRunner{c: c, clientDir: dir, restore: script, pools: []byte(tc.output), poolErr: tc.failure,
					inventory: []byte(`{"workers":true,"actors":false,"actorTemplates":false}`)}
				var progress []string
				a := App{Config: c, Runner: r, Progress: func(f string, args ...any) { progress = append(progress, fmt.Sprintf(f, args...)) }}
				err := a.Check(t.Context(), required)
				if err == nil {
					t.Fatal("invalid WorkerPool inventory was accepted")
				}
				if tc.failure != nil && !errors.Is(err, tc.failure) {
					t.Fatalf("WorkerPool failure lost: %v", err)
				}
				messages := strings.Join(progress, "\n")
				if !strings.Contains(messages, "RESTORE_FAILED") || strings.Contains(messages, "RESTORE_SKIPPED") || strings.Contains(messages, "RESTORE_PASSED") || len(script.calls) != 0 {
					t.Fatalf("invalid result or mutation: %s %v", messages, script.calls)
				}
				root := ""
				poolCalls := 0
				for _, call := range r.calls {
					command := strings.Join(call.Args, " ")
					if strings.Contains(command, "publish restore-worker-pools") {
						root = call.Dir
						poolCalls++
					}
					if strings.Contains(command, "publish restore-inventory") {
						t.Fatal("API inventory ran after a WorkerPool probe failure")
					}
				}
				if root == "" || poolCalls != 1 {
					t.Fatalf("WorkerPool helper calls=%d", poolCalls)
				}
				if _, err := os.Stat(root); !os.IsNotExist(err) {
					t.Fatalf("failed probe left the helper behind: %v", err)
				}
			})
		}
	}
}

func TestRestoreCheckAPIFailuresPreventMutation(t *testing.T) {
	dir, revision := restoreTestClient(t)
	type failure struct {
		name, output string
		err          error
	}
	var cases []failure
	for _, kind := range []string{"worker", "actor", "actor template"} {
		for _, problem := range []string{"access denied", "unavailable", "resource exhausted", "empty continuation",
			"invalid token", "multiple items", "invalid identity", "invalid scope", "unknown response",
			"nil response", "truncated response", "oversized response", "transport failure", "deadline", "canceled"} {
			cases = append(cases, failure{name: kind + "/" + problem, err: errors.New(kind + " " + problem)})
		}
	}
	cases = append(cases,
		failure{name: "missing", output: `{"workers":false,"actors":false}`},
		failure{name: "null", output: `{"workers":null,"actors":false,"actorTemplates":false}`},
		failure{name: "wrong type", output: `{"workers":"false","actors":false,"actorTemplates":false}`},
		failure{name: "duplicate", output: `{"workers":false,"workers":false,"actors":false,"actorTemplates":false}`},
		failure{name: "unknown field", output: `{"workers":false,"actors":false,"actorTemplates":false,"other":true}`},
		failure{name: "truncated", output: `{"workers":false`},
		failure{name: "empty", output: ``},
		failure{name: "trailing", output: `{"workers":false,"actors":false,"actorTemplates":false}{}`},
		failure{name: "oversized output", output: strings.Repeat(" ", restoreInventoryBytes+1)},
	)
	for _, tc := range cases {
		for _, required := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/required=%v", tc.name, required), func(t *testing.T) {
				c := testConfig(t)
				c.Substrate.Source.Commit = revision
				script := &restoreRunner{config: c}
				r := &restoreCheckRunner{c: c, clientDir: dir, restore: script, inventory: []byte(tc.output), inventoryErr: tc.err,
					pools: []byte(`{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[{"metadata":{"name":"existing","namespace":"other","uid":"uid"}}]}`)}
				var progress []string
				err := (App{Config: c, Runner: r, Progress: func(f string, args ...any) {
					progress = append(progress, fmt.Sprintf(f, args...))
				}}).Check(t.Context(), required)
				if err == nil || tc.err != nil && !errors.Is(err, tc.err) {
					t.Fatalf("API failure lost: %v", err)
				}
				text := strings.Join(progress, "\n")
				if !strings.Contains(text, "RESTORE_FAILED") || strings.Contains(text, "RESTORE_SKIPPED") ||
					strings.Contains(text, "RESTORE_PASSED") || len(script.calls) != 0 {
					t.Fatalf("failure became a skip or mutation: %s %v", text, script.calls)
				}
				count := 0
				root := ""
				for _, call := range r.calls {
					if strings.Contains(strings.Join(call.Args, " "), "publish restore-inventory") {
						count++
						root = call.Dir
					}
				}
				if count != 1 {
					t.Fatalf("API helper calls=%d", count)
				}
				if _, err := os.Stat(root); !os.IsNotExist(err) {
					t.Fatalf("failed API probe left the helper behind: %v", err)
				}
			})
		}
	}
}

type restoreCheckRunner struct {
	c                               Config
	clientDir                       string
	restore                         *restoreRunner
	pools, inventory                []byte
	inventoryErr, poolErr, buildErr error
	calls                           []Command
}

func (r *restoreCheckRunner) Run(ctx context.Context, cmd Command) ([]byte, error) {
	r.calls = append(r.calls, cmd)
	command := strings.Join(cmd.Args, " ")
	switch {
	case strings.Contains(command, "group exists"):
		return []byte("true"), nil
	case strings.Contains(command, "group show"):
		return []byte(ownerReply(r.c).output), nil
	case strings.Contains(command, "deployment sub show"):
		return json.Marshal(testInfrastructure())
	case strings.Contains(command, "aks get-credentials"):
		for i, arg := range cmd.Args {
			if arg == "--file" && i+1 < len(cmd.Args) {
				if err := os.WriteFile(cmd.Args[i+1], kubeconfigFixture(r.c.AKS.ClusterName), 0600); err != nil {
					return nil, err
				}
			}
		}
		return nil, nil
	case strings.Contains(command, "get --raw /version"):
		return []byte(`{"gitVersion":"v1.37.1"}`), nil
	case strings.Contains(command, "api-resources --api-group=certificates"):
		return []byte("podcertificaterequests.certificates.k8s.io\nclustertrustbundles.certificates.k8s.io\n"), nil
	case strings.Contains(command, "wait node"):
		return nil, nil
	case strings.Contains(command, "get nodes"):
		return []byte(`{"items":[{"metadata":{"labels":{"substrate-aks/runtime":"microvm"}},"status":{"nodeInfo":{"kubeletVersion":"v1.37.1","architecture":"amd64","operatingSystem":"linux"}}}]}`), nil
	case strings.Contains(command, "get namespace ate-system"):
		return []byte(`{"metadata":{"name":"ate-system"}}`), nil
	case strings.Contains(command, "get configmap substrate-aks-install"):
		return []byte(fmt.Sprintf(`{"metadata":{"name":"substrate-aks-install","namespace":"ate-system"},"data":{"installationId":%q,"fingerprint":%q,"phase":"deployed"}}`, r.c.Installation.ID, fingerprint(r.c, testInfrastructure()))), nil
	case strings.Contains(command, "get deployment ate-api-server"):
		return []byte(`{"metadata":{"name":"ate-api-server","namespace":"ate-system"}}`), nil
	case strings.Contains(command, "go env -json"):
		return []byte(fmt.Sprintf(`{"GOBIN":%q}`, r.clientDir)), nil
	case strings.Contains(command, "publish restore-worker-pools"):
		if r.poolErr != nil {
			return nil, r.poolErr
		}
		if r.pools != nil {
			return r.pools, nil
		}
		return []byte(`{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[]}`), nil
	case strings.Contains(command, "publish restore-inventory"):
		if r.inventoryErr != nil {
			return nil, r.inventoryErr
		}
		if r.inventory != nil {
			return r.inventory, nil
		}
		return []byte(`{"workers":false,"actors":false,"actorTemplates":false}`), nil
	case strings.Contains(command, "go build -o"):
		return nil, r.buildErr
	case strings.Contains(command, "get actors --all-atespaces"), strings.Contains(command, "get actor-templates --all-atespaces"):
		return []byte(`{}`), nil
	case strings.Contains(command, "get workers -o json") && r.restore.namespace == "":
		return []byte(`{}`), nil
	case strings.Contains(command, "git rev-parse --verify"):
		return []byte(r.c.Substrate.Source.Commit), nil
	case strings.Contains(command, "git rev-parse HEAD"):
		return []byte(r.c.Substrate.Source.Commit), nil
	case strings.Contains(command, "git diff --name-only"):
		return nil, nil
	case strings.Contains(command, "git ls-files"):
		return []byte(".substrate-aks-prepared.json\x00"), nil
	case strings.Contains(command, "git init"), strings.Contains(command, "git fetch"):
		return nil, nil
	case strings.Contains(command, "git checkout"):
		for _, path := range []string{filepath.Dir(sandboxTemplate), "cmd/ate-setup/internal/cmd", "cmd/ate-setup/internal/steps"} {
			if err := os.MkdirAll(filepath.Join(cmd.Dir, path), 0700); err != nil {
				return nil, err
			}
		}
		if err := os.WriteFile(filepath.Join(cmd.Dir, sandboxTemplate), []byte("asset"), 0600); err != nil {
			return nil, err
		}
		return nil, nil
	case strings.Contains(command, "publish resolve-check-images"):
		var manifests []string
		if err := json.Unmarshal(cmd.Input, &manifests); err != nil {
			return nil, err
		}
		for i := range manifests {
			manifests[i] = strings.ReplaceAll(manifests[i], "ko://go.ate.dev/substrate/cmd/ateworker-microvm", "example.invalid/worker@sha256:"+strings.Repeat("a", 64))
			manifests[i] = strings.ReplaceAll(manifests[i], "ko://go.ate.dev/substrate/demos/counter", "example.invalid/counter@sha256:"+strings.Repeat("b", 64))
		}
		return json.Marshal(manifests)
	}
	return r.restore.Run(ctx, cmd)
}

func writeRestorePreparedFixture(t *testing.T, c Config) string {
	t.Helper()
	root := filepath.Join(c.workspace(), "source-"+fingerprint(c, testInfrastructure())+"-v2")
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
	files := map[string]string{
		"demos/counter/counter-microvm.yaml.tmpl": pool, "first.yaml": "one", "second.yaml": "two",
		"third.yaml": "three", "fourth.yaml": "four", "fifth.yaml": "five",
		"demos/counter/counter-microvm-template.yaml.tmpl": template,
	}
	marker := prepared{Fingerprint: fingerprint(c, testInfrastructure()), Files: map[string]string{}}
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(path, "-template.yaml.tmpl") {
			marker.Files[path] = checksum([]byte(content))
		}
	}
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(filepath.Join(root, ".substrate-aks-prepared.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRestoreCheckInstalledEmptyRunsLifecycle(t *testing.T) {
	dir, revision := restoreTestClient(t)
	for _, required := range []bool{false, true} {
		t.Run(fmt.Sprintf("required=%t", required), func(t *testing.T) {
			c := testConfig(t)
			c.Substrate.Source.Commit = revision
			c.Substrate.Images.Repository = "example.invalid/images"
			writeRestorePreparedFixture(t, c)
			count := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if restoreTestReady(w, r) {
					return
				}
				count++
				fmt.Fprintf(w, "preserved memory count: %d | preserved file counter: %d | preserved rootfs counter: %d\n", count, count, count)
			}))
			defer server.Close()
			script := &restoreRunner{config: c, address: server.Listener.Addr().String()}
			runner := &restoreCheckRunner{c: c, clientDir: dir, restore: script}
			var messages []string
			a := App{Config: c, Runner: runner, Progress: func(f string, args ...any) { messages = append(messages, fmt.Sprintf(f, args...)) },
				RecoveryReceipt: func(_ context.Context, text string) error {
					lookup := "get actor-templates --all-atespaces -o json"
					if len(script.calls) != 0 || !strings.Contains(text, "kubectl") || !strings.Contains(text, "atespace") ||
						strings.Count(text, lookup) != 2 || strings.LastIndex(text, lookup) > strings.Index(text, `"$POOL_UID"`) ||
						!strings.Contains(text, "Stop if the inventory is unreadable") {
						t.Fatalf("receipt was not presented before creation: %s", text)
					}
					return nil
				}}
			lock, err := Lock(c)
			if err != nil {
				t.Fatal(err)
			}

			defer lock.Close()
			if err := a.Check(t.Context(), required); err != nil {
				t.Fatal(err)
			}
			if count != 2 || !strings.Contains(strings.Join(messages, "\n"), "RESTORE_PASSED") {
				t.Fatalf("counter=%d messages=%v", count, messages)
			}
			var root string
			var binary string
			censusCalls := 0
			namespaceCalls := 0
			resolveCalls := 0
			for _, call := range runner.calls {
				command := strings.Join(call.Args, " ")
				if strings.Contains(command, "publish restore-inventory") {
					root = call.Dir
					binary = call.Args[0]
				}
				if strings.Contains(command, "publish restore-census") {
					censusCalls++
					if root == "" || call.Dir != root || call.Args[0] != binary {
						t.Fatal("census did not reuse the prepared helper")
					}
				}
				if strings.Contains(command, "publish restore-namespace-resources") {
					namespaceCalls++
					if root == "" || call.Dir != root || call.Args[0] != binary || call.MaxOutput != restoreInventoryBytes {
						t.Fatal("namespace discovery did not reuse the bounded prepared helper")
					}
				}
				if strings.Contains(command, "publish resolve-check-images") {
					resolveCalls++
					if root == "" || call.Dir != root || call.Args[0] != binary || call.MaxOutput != restoreInventoryBytes {
						t.Fatal("image resolution did not reuse the bounded prepared helper")
					}
				}
				if strings.Contains(command, "restore-resolve-") || strings.Contains(command, "go run ./cmd/ate-setup publish resolve-check-images") {
					t.Fatalf("unexpected second helper build: %s", command)
				}
			}
			if root == "" || censusCalls < 3 || namespaceCalls != 2 || resolveCalls != 1 {
				t.Fatalf("missing helper calls: root=%q census=%d namespace=%d resolve=%d", root, censusCalls, namespaceCalls, resolveCalls)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("temporary helper was not removed: %v", err)
			}
		})
	}
}

func TestRestoreCheckReadinessAndPostFailuresDoNotSkip(t *testing.T) {
	dir, revision := restoreTestClient(t)
	for _, mode := range []string{"readiness", "processed post"} {
		for _, required := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/required=%t", mode, required), func(t *testing.T) {
				c := testConfig(t)
				c.Substrate.Source.Commit = revision
				c.Substrate.Images.Repository = "example.invalid/images"
				writeRestorePreparedFixture(t, c)
				var posts atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && r.URL.Path == "/readyz" {
						if mode == "readiness" {
							w.WriteHeader(http.StatusUnauthorized)
						} else {
							fmt.Fprint(w, "ok\n")
						}
						return
					}
					if r.Method != http.MethodPost || r.URL.Path != "/" {
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						return
					}
					posts.Add(1)
					w.WriteHeader(http.StatusGatewayTimeout)
				}))
				defer server.Close()
				script := &restoreRunner{config: c, address: server.Listener.Addr().String()}
				var messages []string
				a := App{Config: c, Runner: &restoreCheckRunner{c: c, clientDir: dir, restore: script},
					Progress:        func(f string, args ...any) { messages = append(messages, fmt.Sprintf(f, args...)) },
					RecoveryReceipt: func(context.Context, string) error { return nil }}
				err := a.Check(t.Context(), required)
				if err == nil || !strings.Contains(err.Error(), "initial counter phase") {
					t.Fatalf("phase failure lost: %v", err)
				}
				wantPosts := int32(0)
				if mode == "processed post" {
					wantPosts = 1
					if !strings.Contains(err.Error(), "not retried") {
						t.Fatalf("ambiguous request was not described: %v", err)
					}
				}
				text := strings.Join(messages, "\n")
				if posts.Load() != wantPosts || !strings.Contains(text, "RESTORE_FAILED") ||
					strings.Contains(text, "RESTORE_PASSED") || strings.Contains(text, "RESTORE_SKIPPED") ||
					!strings.Contains(strings.Join(script.calls, "\n"), "delete --raw /api/v1/namespaces/") {
					t.Fatalf("unexpected outcome: posts=%d messages=%s calls=%v", posts.Load(), text, script.calls)
				}
			})
		}
	}
}

func TestRestoreCheckRejectsMalformedCounterInBothPhases(t *testing.T) {
	dir, revision := restoreTestClient(t)
	for _, phase := range []int32{1, 2} {
		for _, required := range []bool{false, true} {
			t.Run(fmt.Sprintf("phase=%d/required=%t", phase, required), func(t *testing.T) {
				c := testConfig(t)
				c.Substrate.Source.Commit, c.Substrate.Images.Repository = revision, "example.invalid/images"
				writeRestorePreparedFixture(t, c)
				var posts atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && r.URL.Path == "/readyz" {
						fmt.Fprint(w, "ok\n")
						return
					}
					if r.Method != http.MethodPost || r.URL.Path != "/" {
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						return
					}
					n := posts.Add(1)
					if n == phase {
						fmt.Fprintf(w, "preserved memory count: %d | ", n)
					} else {
						fmt.Fprintf(w, "preserved memory count: %d | preserved file counter: -1 | preserved rootfs counter: 7\n", n)
					}
				}))
				defer server.Close()
				script := &restoreRunner{config: c, address: server.Listener.Addr().String()}
				var messages []string
				a := App{Config: c, Runner: &restoreCheckRunner{c: c, clientDir: dir, restore: script},
					Progress:        func(f string, args ...any) { messages = append(messages, fmt.Sprintf(f, args...)) },
					RecoveryReceipt: func(context.Context, string) error { return nil }}
				err := a.Check(t.Context(), required)
				phaseName := "initial counter phase"
				if phase == 2 {
					phaseName = "resumed counter phase"
				}
				text := strings.Join(messages, "\n")
				if err == nil || !strings.Contains(err.Error(), phaseName) || !strings.Contains(err.Error(), "malformed") ||
					!strings.Contains(err.Error(), "not retried") || posts.Load() != phase ||
					!strings.Contains(text, "RESTORE_FAILED") || strings.Contains(text, "RESTORE_PASSED") ||
					strings.Contains(text, "RESTORE_SKIPPED") || !strings.Contains(strings.Join(script.calls, "\n"), "delete --raw /api/v1/namespaces/") {
					t.Fatalf("err=%v posts=%d messages=%s calls=%v", err, posts.Load(), text, script.calls)
				}
			})
		}
	}
}

func TestRestoreCheckReadinessAndCounterScenariosBothModes(t *testing.T) {
	dir, revision := restoreTestClient(t)
	for _, tc := range []struct {
		name           string
		retryInitial   bool
		retryResumed   bool
		wrongInitial   bool
		reset          bool
		failReadyPhase int32
		failPostPhase  int32
		status         int
		timeout        bool
	}{
		{name: "both readiness retries", retryInitial: true, retryResumed: true},
		{name: "initial readiness failure", failReadyPhase: 1},
		{name: "resumed readiness failure", failReadyPhase: 2},
		{name: "wrong initial count", wrongInitial: true},
		{name: "reset after resumed readiness retry", retryResumed: true, reset: true},
		{name: "processed initial 504", failPostPhase: 1, status: 504},
		{name: "processed resumed 504", failPostPhase: 2, status: 504},
		{name: "initial 502", failPostPhase: 1, status: 502},
		{name: "resumed 503", failPostPhase: 2, status: 503},
		{name: "initial POST timeout", failPostPhase: 1, timeout: true},
		{name: "resumed POST timeout", failPostPhase: 2, timeout: true},
	} {
		for _, required := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/required=%t", tc.name, required), func(t *testing.T) {
				c := testConfig(t)
				c.Substrate.Source.Commit, c.Substrate.Images.Repository = revision, "example.invalid/images"
				writeRestorePreparedFixture(t, c)
				var posts, initialGets, resumedGets atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && r.URL.Path == "/readyz" {
						phase := int32(1)
						if posts.Load() != 0 {
							phase = 2
						}
						if phase == tc.failReadyPhase {
							w.WriteHeader(http.StatusUnauthorized)
							return
						}
						if phase == 1 && initialGets.Add(1) == 1 && tc.retryInitial {
							w.WriteHeader(http.StatusGatewayTimeout)
							return
						}
						if phase == 2 && resumedGets.Add(1) == 1 && tc.retryResumed {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						fmt.Fprint(w, "ok\n")
						return
					}
					if r.Method != http.MethodPost || r.URL.Path != "/" {
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
						return
					}
					n := posts.Add(1)
					if n == tc.failPostPhase {
						if tc.timeout {
							<-r.Context().Done()
							return
						}
						w.WriteHeader(tc.status)
						return
					}
					value := n
					if tc.wrongInitial && n == 1 {
						value = 2
					}
					if tc.reset && n == 2 {
						value = 1
					}
					fmt.Fprintf(w, "preserved memory count: %d | preserved file counter: -1 | preserved rootfs counter: 7\n", value)
				}))
				defer server.Close()
				script := &restoreRunner{config: c, address: server.Listener.Addr().String()}
				var messages []string
				a := App{Config: c, Runner: &restoreCheckRunner{c: c, clientDir: dir, restore: script},
					Progress:        func(f string, args ...any) { messages = append(messages, fmt.Sprintf(f, args...)) },
					RecoveryReceipt: func(context.Context, string) error { return nil }}
				ctx := t.Context()
				if tc.timeout {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
					defer cancel()
				}
				err := a.Check(ctx, required)
				fail := tc.wrongInitial || tc.reset || tc.failReadyPhase != 0 || tc.failPostPhase != 0
				if (err != nil) != fail {
					t.Fatalf("unexpected result: %v", err)
				}
				wantPosts := int32(2)
				if tc.wrongInitial || tc.failReadyPhase == 2 || tc.failPostPhase == 1 {
					wantPosts = 1
				}
				if tc.failReadyPhase == 1 {
					wantPosts = 0
				}
				if posts.Load() != wantPosts || tc.retryInitial && initialGets.Load() != 2 || tc.retryResumed && resumedGets.Load() != 2 {
					t.Fatalf("posts=%d readiness=%d/%d", posts.Load(), initialGets.Load(), resumedGets.Load())
				}
				if fail {
					phase := "initial counter phase"
					if tc.failReadyPhase == 2 || tc.failPostPhase == 2 || tc.reset {
						phase = "resumed counter phase"
					}
					if !strings.Contains(err.Error(), phase) {
						t.Fatalf("phase missing: %v", err)
					}
				}
				if tc.timeout && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("deadline identity lost: %v", err)
				}
				if tc.failPostPhase != 0 && !strings.Contains(err.Error(), "not retried") {
					t.Fatalf("no-replay reason missing: %v", err)
				}
				if tc.reset && !strings.Contains(err.Error(), "expected 2, observed 1") ||
					tc.wrongInitial && !strings.Contains(err.Error(), "expected 1, observed 2") {
					t.Fatalf("count reason missing: %v", err)
				}
				text := strings.Join(messages, "\n")
				commands := strings.Join(script.calls, "\n")
				wantSuspends := 0
				if wantPosts == 2 || tc.failReadyPhase == 2 {
					wantSuspends = 1
				}
				if strings.Contains(text, "RESTORE_SKIPPED") || strings.Contains(text, "RESTORE_PASSED") == fail ||
					strings.Contains(text, "RESTORE_FAILED") != fail || strings.Count(commands, " suspend actor ") != wantSuspends ||
					strings.Count(commands, " create actor ") != 1 || !strings.Contains(commands, "delete --raw /api/v1/namespaces/") {
					t.Fatalf("incorrect outcome: %s\n%s", text, commands)
				}
			})
		}
	}
}

func TestRestoreCheckReceiptFailurePreventsCreation(t *testing.T) {
	dir, revision := restoreTestClient(t)
	for _, tc := range []struct {
		name    string
		receipt func(context.Context, string) error
	}{
		{"unavailable", nil},
		{"failed", func(context.Context, string) error { return errors.New("receipt failure") }},
		{"cancelled", func(context.Context, string) error { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			c.Substrate.Source.Commit = revision
			c.Substrate.Images.Repository = "example.invalid/images"
			writeRestorePreparedFixture(t, c)
			script := &restoreRunner{config: c}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			called := false
			var receipt func(context.Context, string) error
			if tc.receipt != nil {
				receipt = func(ctx context.Context, text string) error {
					called = true
					if tc.name == "cancelled" {
						cancel()
					}
					return tc.receipt(ctx, text)
				}
			}
			a := App{Config: c, Runner: &restoreCheckRunner{c: c, clientDir: dir, restore: script}, RecoveryReceipt: receipt}
			if err := a.Check(ctx, false); err == nil {
				t.Fatal("expected receipt failure")
			}
			if tc.receipt != nil && !called {
				t.Fatal("receipt callback was not reached")
			}
			if len(script.calls) != 0 {
				t.Fatalf("commands after receipt failure: %v", script.calls)
			}
		})
	}
}

func TestRestoreCheckDelayedReceiptAfterDeadlinePreventsCreation(t *testing.T) {
	dir, revision := restoreTestClient(t)
	c := testConfig(t)
	c.Substrate.Source.Commit = revision
	c.Substrate.Images.Repository = "example.invalid/images"
	writeRestorePreparedFixture(t, c)
	script := &restoreRunner{config: c}
	entered, expired, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	a := App{Config: c, Runner: &restoreCheckRunner{c: c, clientDir: dir, restore: script},
		RecoveryReceipt: func(ctx context.Context, _ string) error {
			close(entered)
			<-ctx.Done()
			close(expired)
			<-release
			return nil
		}}
	done := make(chan error, 1)
	go func() { done <- a.Check(ctx, false) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("receipt was not reached before the deadline")
	}
	<-expired
	close(release)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired receipt was accepted: %v", err)
	}
	if len(script.calls) != 0 {
		t.Fatalf("late restore creation: %v", script.calls)
	}
}
