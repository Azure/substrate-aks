//go:build integration

package installer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func testRestoreCredentialInterruption(t *testing.T, root, binary string) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process-group cancellation is supported on Linux and macOS")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "credential.go")
	program := filepath.Join(dir, "credential")
	const fixture = `package main
import ("fmt"; "os"; "os/exec"; "path/filepath"; "strconv"; "syscall"; "time")
func main() {
	dir, mode := os.Args[1], os.Args[2]
	if mode == "child" {
		for i := 0; i < 1500; i++ {
			if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
				os.WriteFile(filepath.Join(dir, "late-child"), []byte("late"), 0600)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		os.WriteFile(filepath.Join(dir, "late-child"), []byte("timeout"), 0600)
		return
	}
	n := 1
	if _, err := os.Stat(filepath.Join(dir, "call-1")); err == nil { n = 2 }
	if _, err := os.Stat(filepath.Join(dir, "call-2")); err == nil {
		os.WriteFile(filepath.Join(dir, "extra"), []byte("extra"), 0600)
		os.Exit(1)
	}
	os.WriteFile(filepath.Join(dir, "call-"+strconv.Itoa(n)), []byte(fmt.Sprint(os.Getpid(), " ", syscall.Getpgrp(), " ", os.Getppid())), 0600)
	if mode == "initial" && n == 1 || mode == "refresh" && n == 2 {
		child := exec.Command(os.Args[0], dir, "child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil { os.Exit(1) }
		pgid, err := syscall.Getpgid(child.Process.Pid)
		if err != nil { os.Exit(1) }
		if err := os.WriteFile(filepath.Join(dir, "child.tmp"), []byte(fmt.Sprint(child.Process.Pid, " ", pgid, " ", os.Getpid())), 0600); err != nil { os.Exit(1) }
		if err := os.Rename(filepath.Join(dir, "child.tmp"), filepath.Join(dir, "child")); err != nil { os.Exit(1) }
		if child.Wait() != nil { os.Exit(1) }
		os.WriteFile(filepath.Join(dir, "late-plugin"), []byte("late"), 0600)
	}
	fmt.Print("{\"apiVersion\":\"client.authentication.k8s.io/v1\",\"kind\":\"ExecCredential\",\"status\":{\"token\":\"synthetic-token\"}}")
}`
	if err := os.WriteFile(source, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", program, source)
	build.Env = append(cleanEnvironment(os.Environ()), "GOWORK=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building credential fixture: %v\n%s", err, output)
	}
	for _, stage := range []string{"initial", "refresh"} {
		for _, method := range []string{"deadline", "cancel"} {
			t.Run(stage+"/"+method, func(t *testing.T) {
				work := t.TempDir()
				var requests atomic.Int32
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.Method != http.MethodGet || r.URL.RequestURI() != "/apis/ate.dev/v1/workerpools?limit=1" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
						t.Errorf("incorrect request: %s %s", r.Method, r.URL.RequestURI())
					}
					w.WriteHeader(http.StatusUnauthorized)
				}))
				defer server.Close()
				cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
				config := fmt.Sprintf(`{"apiVersion":"v1","kind":"Config","current-context":"selected","contexts":[{"name":"selected","context":{"cluster":"local","user":"test"}}],"clusters":[{"name":"local","cluster":{"server":%q,"certificate-authority-data":%q}}],"users":[{"name":"test","user":{"exec":{"apiVersion":"client.authentication.k8s.io/v1","interactiveMode":"Never","command":%q,"args":[%q,%q]}}}]}`, server.URL, base64.StdEncoding.EncodeToString(cert), program, work, stage)
				kubeconfig := filepath.Join(work, "kubeconfig")
				if err := os.WriteFile(kubeconfig, []byte(config), 0600); err != nil {
					t.Fatal(err)
				}
				var ctx context.Context
				var cancel context.CancelFunc
				if method == "deadline" {
					ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
				} else {
					ctx, cancel = context.WithCancel(t.Context())
				}
				defer cancel()
				type result struct {
					output []byte
					err    error
				}
				done, finished := make(chan result, 1), make(chan struct{})
				var log bytes.Buffer
				go func() {
					defer close(finished)
					data, err := (ExecRunner{Log: &log}).Run(ctx, Command{
						Dir: root, Args: []string{binary, "publish", "restore-worker-pools", kubeconfig, "selected", "--no-dev-env", "--no-report-defaults"},
						Env: []string{"NO_DEV_ENV=1"}, MaxOutput: restoreInventoryBytes,
					})
					done <- result{data, err}
				}()
				defer func() {
					cancel()
					select {
					case <-finished:
					case <-time.After(5 * time.Second):
						t.Error("helper was not joined")
					}
				}()
				ready := filepath.Join(work, "child")
				readyTimer := time.NewTimer(10 * time.Second)
				defer readyTimer.Stop()
				for {
					if _, err := os.Stat(ready); err == nil {
						break
					}
					select {
					case result := <-done:
						t.Fatalf("helper returned before credential blocked: %v", result.err)
					case <-ctx.Done():
						t.Fatal("credential did not block before deadline")
					case <-readyTimer.C:
						t.Fatal("credential did not start")
					case <-time.After(20 * time.Millisecond):
					}
				}
				if method == "cancel" {
					cancel()
				}
				var got result
				select {
				case got = <-done:
				case <-time.After(7 * time.Second):
					t.Fatal("helper did not stop after parent interruption")
				}
				want := context.Canceled
				if method == "deadline" {
					want = context.DeadlineExceeded
				}
				if !errors.Is(got.err, want) || len(got.output) != 0 {
					t.Fatalf("interruption=%v output=%q", got.err, got.output)
				}
				expectedCalls, expectedRequests := 1, int32(0)
				if stage == "refresh" {
					expectedCalls, expectedRequests = 2, 1
				}
				for n := 1; n <= expectedCalls; n++ {
					if _, err := os.Stat(filepath.Join(work, fmt.Sprintf("call-%d", n))); err != nil {
						t.Fatalf("missing credential call %d: %v", n, err)
					}
				}
				for _, path := range []string{"extra", fmt.Sprintf("call-%d", expectedCalls+1)} {
					if _, err := os.Stat(filepath.Join(work, path)); !os.IsNotExist(err) {
						t.Fatalf("unexpected credential call: %s (%v)", path, err)
					}
				}
				if requests.Load() != expectedRequests {
					t.Fatalf("requests=%d, want %d", requests.Load(), expectedRequests)
				}
				var markers []string
				for n := 1; n <= expectedCalls; n++ {
					markers = append(markers, fmt.Sprintf("call-%d", n))
				}
				markers = append(markers, "child")
				group := 0
				for _, marker := range markers {
					data, err := os.ReadFile(filepath.Join(work, marker))
					if err != nil {
						t.Fatal(err)
					}
					var pid, pgid, ppid int
					if _, err := fmt.Sscan(string(data), &pid, &pgid, &ppid); err != nil || pid <= 0 || pgid <= 0 {
						t.Fatalf("invalid credential process record %q: %v", data, err)
					}
					// The helper is the process ExecRunner starts with Setpgid, so it leads the group
					// that cancellation kills; every credential process must belong to that group.
					if marker == "call-1" {
						group = ppid
					}
					if group != 0 && pgid != group || marker != "child" && pgid != ppid {
						t.Fatalf("credential %s (pid %d, pgid %d, parent %d) is outside the helper's process group", marker, pid, pgid, ppid)
					}
					// A process in the killed group has an uncatchable SIGKILL pending, but under CPU
					// contention it can still be exiting (state R, descriptors closed) after Run returns.
					wait := time.Now().Add(10 * time.Second)
					for restoreCredentialProcessAlive(pid) && time.Now().Before(wait) {
						time.Sleep(20 * time.Millisecond)
					}
					if restoreCredentialProcessAlive(pid) {
						stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
						state := fmt.Sprintf("stat unreadable: %v", err)
						if f := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:])); err == nil && len(f) > 2 {
							state = fmt.Sprintf("state %s ppid %s pgrp %s", f[0], f[1], f[2])
						}
						status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
						pending := fmt.Sprintf("status unreadable: %v", err)
						if _, value, ok := strings.Cut(string(status), "ShdPnd:"); err == nil && ok {
							pending = "ShdPnd " + strings.Fields(value + " ?")[0]
						}
						if p, err := os.FindProcess(pid); err == nil {
							p.Kill()
						}
						t.Errorf("credential %s (pid %d) remains live 10s after interruption: %s, %s (helper group %d)", marker, pid, state, pending, group)
					}
				}
				if err := os.WriteFile(filepath.Join(work, "release"), []byte("release"), 0600); err != nil {
					t.Fatal(err)
				}
				time.Sleep(200 * time.Millisecond)
				for _, path := range []string{"late-child", "late-plugin", "extra"} {
					if _, err := os.Stat(filepath.Join(work, path)); !os.IsNotExist(err) {
						t.Errorf("late credential activity %s: %v", path, err)
					}
				}
				if requests.Load() != expectedRequests || strings.Contains(log.String(), "RESTORE_SKIPPED") || strings.Contains(log.String(), "RESTORE_PASSED") || strings.Contains(log.String(), " create ") {
					t.Fatalf("late request or unexpected outcome: requests=%d log=%s", requests.Load(), log.String())
				}
			})
		}
	}
}

func restoreCredentialProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		return !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH)
	}
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) {
			return false
		}
		if err == nil {
			stat := string(data)
			end := strings.LastIndex(stat, ") ")
			if end >= 0 && len(stat) > end+2 && (stat[end+2] == 'Z' || stat[end+2] == 'X') {
				return false
			}
		}
	}
	return true
}

func TestPinnedImageCheckContract(t *testing.T) {
	root := pinnedContractRoot(t)
	t.Run("counter readiness does not increment", func(t *testing.T) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(root, "demos/counter/counter.go"), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var handlers []*ast.FuncLit
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "HandleFunc" {
				return true
			}
			mux, ok := selector.X.(*ast.Ident)
			if !ok || mux.Name != "defaultMux" {
				return true
			}
			path, ok := call.Args[0].(*ast.BasicLit)
			if !ok || path.Kind != token.STRING {
				return true
			}
			name, err := strconv.Unquote(path.Value)
			if err != nil || name != "/readyz" {
				return true
			}
			handler, ok := call.Args[1].(*ast.FuncLit)
			if !ok {
				t.Fatal("pinned readiness handler is not a function literal")
			}
			handlers = append(handlers, handler)
			return true
		})
		if len(handlers) != 1 {
			t.Fatalf("found %d pinned readiness handlers", len(handlers))
		}
		var handler bytes.Buffer
		if err := format.Node(&handler, fset, handlers[0]); err != nil {
			t.Fatal(err)
		}
		// Exercise the exact pinned handler, not a local reimplementation.
		fixture := `package readiness
import ("net/http"; "net/http/httptest"; "sync/atomic"; "testing")
var ready atomic.Bool
var requestCount atomic.Uint64
func TestHandler(t *testing.T) {
	requestCount.Store(7)
	handler := ` + handler.String() + `
	for _, tc := range []struct{ ready bool; status int; body string }{
		{false, http.StatusServiceUnavailable, "not ready\n"},
		{true, http.StatusOK, "ok\n"},
	} {
		ready.Store(tc.ready)
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if response.Code != tc.status || response.Body.String() != tc.body || requestCount.Load() != 7 {
			t.Fatalf("ready=%v status=%d body=%q count=%d", tc.ready, response.Code, response.Body.String(), requestCount.Load())
		}
	}
}`
		path := filepath.Join(t.TempDir(), "readiness_test.go")
		if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "go", "test", path)
		cmd.Env = cleanEnvironment(os.Environ())
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pinned readiness contract failed: %v\n%s", err, output)
		}
	})
	t.Run("snapshot release precedes delete return", func(t *testing.T) {
		for _, tc := range []struct {
			path, method string
			calls        []string
		}{
			{"workflow_delete.go", "DeleteActor", []string{"ensureExternalSnapshotsReleased", "Join", "finalizeDeleted"}},
			{"workflow_template_delete.go", "DeleteActorTemplate", []string{"ensureGoldenActorDeleted", "ensureGoldenTagDeleted", "finalizeTemplateDeleted"}},
			{"workflow_template_delete.go", "ensureGoldenActorDeleted", []string{"DeleteActor"}},
			{"workflow_template_delete.go", "ensureGoldenTagDeleted", []string{"DeleteTag"}},
			{"workflow_tag.go", "DeleteTag", []string{"ensureTagSnapshotReleased", "finalizeTagDeleted"}},
		} {
			t.Run(tc.method, func(t *testing.T) {
				path := filepath.Join(root, "cmd/ateapi/internal/controlapi", tc.path)
				file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
				if err != nil {
					t.Fatal(err)
				}
				var fn *ast.FuncDecl
				for _, declaration := range file.Decls {
					if candidate, ok := declaration.(*ast.FuncDecl); ok && candidate.Name.Name == tc.method {
						fn = candidate
						break
					}
				}
				if fn == nil {
					t.Fatalf("missing pinned workflow %s", tc.method)
				}
				var calls []string
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
						calls = append(calls, selector.Sel.Name)
					}
					return true
				})
				next := 0
				for _, call := range calls {
					if next < len(tc.calls) && call == tc.calls[next] {
						next++
					}
				}
				if next != len(tc.calls) {
					t.Fatalf("snapshot workflow order changed: %v", calls)
				}
			})
		}
	})
	dest := filepath.Join(t.TempDir(), "source")
	if output, err := exec.Command("git", "clone", "--quiet", "--shared", root, dest).CombinedOutput(); err != nil {
		t.Fatalf("copying pinned source: %v\n%s", err, output)
	}
	if err := prepareHelper(dest); err != nil {
		t.Fatal(err)
	}
	testSource, err := helperSource.ReadFile("helper/inventory_test.go.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "cmd/ate-setup/internal/steps/azure_inventory_test.go"), testSource, 0600); err != nil {
		t.Fatal(err)
	}
	censusFixture := filepath.Join(t.TempDir(), "census.json")
	inventoryTest := exec.CommandContext(t.Context(), "go", "test", "./cmd/ate-setup/internal/steps", "-run", "^TestRestore(Inventory|Census)", "-count=1")
	inventoryTest.Dir = dest
	inventoryTest.Env = append(cleanEnvironment(os.Environ()), "RESTORE_CENSUS_FIXTURE="+censusFixture)
	if output, err := inventoryTest.CombinedOutput(); err != nil {
		t.Fatalf("pinned restore inventory tests failed: %v\n%s", err, output)
	}
	t.Run("later foreign census retains capacity", func(t *testing.T) {
		data, err := os.ReadFile(censusFixture)
		if err != nil {
			t.Fatal(err)
		}
		c := testConfig(t)
		c.Installation.ID = "fixture-install"
		r := &restoreRunner{config: c, namespace: "space", actor: "actor", token: "fixture-token", poolExists: true}
		censusCalls := 0
		runner := prerequisiteRunner(func(ctx context.Context, cmd Command) ([]byte, error) {
			if strings.Contains(strings.Join(cmd.Args, " "), "publish restore-census") {
				censusCalls++
				return data, nil
			}
			return r.Run(ctx, cmd)
		})
		state := &restoreState{namespace: "space", token: "fixture-token", client: "/client", helper: &restoreHelper{binary: "/fixture"},
			ns: restoreKubeIdentity{Name: "space", UID: "namespace-id"}, pool: restoreKubeIdentity{Name: restorePoolName, Namespace: "space", UID: "pool-id"},
			space: restoreAPIIdentity{Name: "space", UID: "space-id"}, actor: restoreAPIIdentity{Name: "actor", Atespace: "space", UID: "actor-id"},
			template: restoreAPIIdentity{Name: restorePoolName, Atespace: "space", UID: "template-id"}}
		err = (App{Config: c, Runner: runner}).restoreCleanup(t.Context(), state)
		if err == nil || !strings.Contains(err.Error(), "worker ownership or placement") || censusCalls != 1 {
			t.Fatalf("later foreign worker accepted: %v calls=%d", err, censusCalls)
		}
		if _, ok := state.workers["worker-1"]; !ok {
			t.Fatal("first page was not checked before the foreign worker")
		}
		commands := strings.Join(r.calls, "\n")
		if !strings.Contains(commands, "delete actor actor") || !strings.Contains(commands, "delete actor-template") ||
			strings.Contains(commands, "delete --raw") || strings.Contains(commands, "delete atespace") ||
			strings.Contains(commands, "delete worker ") || !r.poolExists {
			t.Fatalf("unsafe census did not retain capacity: %s", commands)
		}
	})
	binary := filepath.Join(t.TempDir(), "ate-setup")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./cmd/ate-setup")
	build.Dir = dest
	build.Env = cleanEnvironment(os.Environ())
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building pinned image checker: %v\n%s", err, output)
	}
	t.Run("namespace discovery drives cleanup", func(t *testing.T) {
		for _, failPass := range []int32{0, 1, 2} {
			t.Run(fmt.Sprintf("fail-pass=%d", failPass), func(t *testing.T) {
				c := testConfig(t)
				var discoveries atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api":
						discoveries.Add(1)
						fmt.Fprint(w, `{"kind":"APIVersions","versions":["v1"]}`)
					case "/apis":
						fmt.Fprint(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"metrics.k8s.io","versions":[{"groupVersion":"metrics.k8s.io/v1beta1","version":"v1beta1"}],"preferredVersion":{"groupVersion":"metrics.k8s.io/v1beta1","version":"v1beta1"}},{"name":"other.example.test","versions":[{"groupVersion":"other.example.test/v1","version":"v1"}],"preferredVersion":{"groupVersion":"other.example.test/v1","version":"v1"}},{"name":"ate.dev","versions":[{"groupVersion":"ate.dev/v1","version":"v1"}],"preferredVersion":{"groupVersion":"ate.dev/v1","version":"v1"}}]}`)
					case "/api/v1":
						fmt.Fprint(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[{"name":"pods","namespaced":true,"kind":"Pod","verbs":["list"]}]}`)
					case "/apis/metrics.k8s.io/v1beta1":
						w.WriteHeader(http.StatusServiceUnavailable)
					case "/apis/ate.dev/v1":
						fmt.Fprint(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"ate.dev/v1","resources":[{"name":"workerpools","namespaced":true,"kind":"WorkerPool","verbs":["list"]}]}`)
					case "/apis/other.example.test/v1":
						if discoveries.Load() == failPass {
							w.WriteHeader(http.StatusServiceUnavailable)
						} else {
							fmt.Fprint(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"other.example.test/v1","resources":[{"name":"things","namespaced":true,"kind":"Thing","verbs":["list"]}]}`)
						}
					default:
						t.Errorf("unexpected discovery request %s", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer server.Close()
				config := fmt.Sprintf(`{"apiVersion":"v1","kind":"Config","current-context":"selected","contexts":[{"name":%q,"context":{"cluster":"local"}}],"clusters":[{"name":"local","cluster":{"server":%q}}]}`, c.AKS.ClusterName, server.URL)
				app := App{Config: c}
				if err := os.MkdirAll(filepath.Dir(app.kubeconfig()), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(app.kubeconfig(), []byte(config), 0600); err != nil {
					t.Fatal(err)
				}
				fake := &restoreRunner{config: c, namespace: "space", actor: "actor", token: "token", poolExists: true}
				var calls []string
				var log bytes.Buffer
				var progress []string
				app.Progress = func(_ string, args ...any) { progress = append(progress, fmt.Sprint(args...)) }
				app.Runner = prerequisiteRunner(func(ctx context.Context, cmd Command) ([]byte, error) {
					command := strings.Join(cmd.Args, " ")
					calls = append(calls, command)
					if strings.Contains(command, " publish restore-namespace-resources ") {
						return (ExecRunner{Log: &log}).Run(ctx, cmd)
					}
					if strings.Contains(command, " get things.other.example.test ") {
						return []byte(`{"kind":"ThingList","items":[]}`), nil
					}
					return fake.Run(ctx, cmd)
				})
				state := &restoreState{namespace: "space", token: "token", client: "/client", helper: &restoreHelper{root: dest, binary: binary},
					ns: restoreKubeIdentity{Name: "space", UID: "namespace-id"}, pool: restoreKubeIdentity{Name: restorePoolName, Namespace: "space", UID: "pool-id"},
					space: restoreAPIIdentity{Name: "space", UID: "space-id"}, actor: restoreAPIIdentity{Name: "actor", Atespace: "space", UID: "actor-id"},
					template: restoreAPIIdentity{Name: restorePoolName, Atespace: "space", UID: "template-id"}}
				err := app.restoreCleanup(t.Context(), state)
				if (err != nil) != (failPass != 0) {
					t.Fatalf("unexpected cleanup error: %v", err)
				}
				commands := strings.Join(calls, "\n")
				if failPass == 0 && (discoveries.Load() != 2 || !strings.Contains(commands, "delete --raw /api/v1/namespaces/") ||
					!strings.Contains(log.String(), "metrics API discovery failed") || !strings.Contains(strings.Join(progress, "\n"), "metrics API discovery failed")) {
					t.Fatalf("metrics-only cleanup: discoveries=%d calls=%s log=%s progress=%v", discoveries.Load(), commands, log.String(), progress)
				}
				if failPass == 1 && (strings.Contains(commands, "delete --raw") || !fake.poolExists) ||
					failPass == 2 && strings.Contains(commands, "delete --raw /api/v1/namespaces/") {
					t.Fatalf("unsafe discovery removed capacity: %s", commands)
				}
			})
		}
	})
	t.Run("restore credential interruption", func(t *testing.T) {
		testRestoreCredentialInterruption(t, dest, binary)
	})
	t.Run("missing core discovery retains zero-worker cleanup", func(t *testing.T) {
		c := testConfig(t)
		var coreRequests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api":
				fmt.Fprint(w, `{"kind":"APIVersions","versions":["v1"]}`)
			case "/apis":
				fmt.Fprint(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"metrics.k8s.io","versions":[{"groupVersion":"metrics.k8s.io/v1beta1","version":"v1beta1"}],"preferredVersion":{"groupVersion":"metrics.k8s.io/v1beta1","version":"v1beta1"}},{"name":"other.example.test","versions":[{"groupVersion":"other.example.test/v1","version":"v1"}],"preferredVersion":{"groupVersion":"other.example.test/v1","version":"v1"}}]}`)
			case "/api/v1":
				coreRequests.Add(1)
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)
			case "/apis/metrics.k8s.io/v1beta1":
				w.WriteHeader(http.StatusServiceUnavailable)
			case "/apis/other.example.test/v1":
				fmt.Fprint(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"other.example.test/v1","resources":[{"name":"things","namespaced":true,"kind":"Thing","verbs":["list"]}]}`)
			default:
				t.Errorf("unexpected discovery request %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()
		config := fmt.Sprintf(`{"apiVersion":"v1","kind":"Config","contexts":[{"name":%q,"context":{"cluster":"local"}}],"clusters":[{"name":"local","cluster":{"server":%q}}]}`, c.AKS.ClusterName, server.URL)
		app := App{Config: c}
		if err := os.MkdirAll(filepath.Dir(app.kubeconfig()), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(app.kubeconfig(), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		fake := &restoreRunner{config: c, namespace: "space", actor: "actor", token: "token", poolExists: true}
		var calls []string
		app.Runner = prerequisiteRunner(func(ctx context.Context, cmd Command) ([]byte, error) {
			command := strings.Join(cmd.Args, " ")
			calls = append(calls, command)
			if strings.Contains(command, " publish restore-census ") {
				return []byte(`{"workers":[],"actors":false,"actorTemplates":false}`), nil
			}
			if strings.Contains(command, " publish restore-namespace-resources ") {
				return (ExecRunner{}).Run(ctx, cmd)
			}
			return fake.Run(ctx, cmd)
		})
		state := &restoreState{namespace: "space", token: "token", client: "/client", helper: &restoreHelper{root: dest, binary: binary},
			ns: restoreKubeIdentity{Name: "space", UID: "namespace-id"}, pool: restoreKubeIdentity{Name: restorePoolName, Namespace: "space", UID: "pool-id"},
			space: restoreAPIIdentity{Name: "space", UID: "space-id"}, actor: restoreAPIIdentity{Name: "actor", Atespace: "space", UID: "actor-id"},
			template: restoreAPIIdentity{Name: restorePoolName, Atespace: "space", UID: "template-id"}}
		err := app.restoreCleanup(t.Context(), state)
		commands := strings.Join(calls, "\n")
		if err == nil || coreRequests.Load() == 0 || !strings.Contains(commands, " publish restore-census ") ||
			strings.Contains(commands, "delete --raw") || strings.Contains(commands, "delete atespace") || !fake.poolExists {
			t.Fatalf("unsafe cleanup: error=%v core requests=%d calls=%s", err, coreRequests.Load(), commands)
		}
	})
	t.Run("restore WorkerPool probe", func(t *testing.T) {
		for _, code := range []int{http.StatusOK, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
			t.Run(strconv.Itoa(code), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != http.MethodGet || r.URL.RequestURI() != "/apis/ate.dev/v1/workerpools?limit=1" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
						t.Errorf("incorrect request: %s %s", r.Method, r.URL.RequestURI())
					}
					if code != http.StatusOK {
						w.Header().Set("Retry-After", "60")
					}
					w.WriteHeader(code)
					if code == http.StatusOK {
						fmt.Fprint(w, `{"apiVersion":"ate.dev/v1","kind":"WorkerPoolList","items":[]}`)
					}
				}))
				defer server.Close()
				cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
				config := fmt.Sprintf(`{"apiVersion":"v1","kind":"Config","current-context":"other","contexts":[{"name":"selected","context":{"cluster":"local","user":"test"}},{"name":"other","context":{"cluster":"wrong","user":"test"}}],"clusters":[{"name":"local","cluster":{"server":%q,"certificate-authority-data":%q}},{"name":"wrong","cluster":{"server":"https://127.0.0.1:1"}}],"users":[{"name":"test","user":{"token":"synthetic-token"}}]}`, server.URL, base64.StdEncoding.EncodeToString(cert))
				kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
				if err := os.WriteFile(kubeconfig, []byte(config), 0600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				probe := exec.CommandContext(ctx, binary, "publish", "restore-worker-pools",
					kubeconfig, "selected", "--no-dev-env", "--no-report-defaults")
				probe.Dir, probe.Env = dest, cleanEnvironment(os.Environ())
				output, err := probe.CombinedOutput()
				if calls.Load() != 1 || ctx.Err() != nil || (err == nil) != (code == http.StatusOK) {
					t.Fatalf("status=%d calls=%d error=%v output=%s", code, calls.Load(), err, output)
				}
				if code == http.StatusOK {
					if busy, err := workerPoolPage(output); err != nil || busy {
						t.Fatalf("invalid pool output: %s (%v)", output, err)
					}
				} else if !strings.Contains(string(output), fmt.Sprintf("HTTP status %d", code)) {
					t.Fatalf("missing status error: %s", output)
				}
			})
		}
	})
	t.Run("restore image resolution", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v2/" {
				return
			}
			if r.Method != http.MethodHead {
				t.Errorf("unexpected registry request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Content-Length", "123")
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
		}))
		defer server.Close()
		input, err := json.Marshal([]string{
			"image: ko://go.ate.dev/substrate/cmd/ateworker-microvm",
			"image: ko://go.ate.dev/substrate/demos/counter",
		})
		if err != nil {
			t.Fatal(err)
		}
		resolve := exec.CommandContext(t.Context(), binary, "publish", "resolve-check-images",
			strings.TrimPrefix(server.URL, "http://")+"/images", "synthetic",
			"--no-dev-env", "--no-report-defaults")
		resolve.Dir = dest
		resolve.Env = cleanEnvironment(os.Environ())
		resolve.Stdin = bytes.NewReader(input)
		output, err := resolve.CombinedOutput()
		if err != nil {
			t.Fatalf("resolving images: %v\n%s", err, output)
		}
		var got []string
		if err := json.Unmarshal(output, &got); err != nil || len(got) != 2 ||
			!strings.Contains(got[0], "/ateworker-microvm:synthetic@sha256:") ||
			!strings.Contains(got[1], "/counter:synthetic@sha256:") {
			t.Fatalf("incorrect resolved manifests: %s (%v)", output, err)
		}
	})
	inventory, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "cmd/ate-setup/internal/images/images.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Releases publish the released packages and envoy-dataplane; the installer
	// adds the counter demo and the plugin.
	want := map[string]bool{"envoy-dataplane": true, "counter": true, "plugin": true}
	ast.Inspect(inventory, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "released" {
			return true
		}
		list, ok := spec.Values[0].(*ast.CompositeLit)
		if !ok {
			t.Fatal("publisher inventory is no longer a literal list")
		}
		for _, element := range list.Elts {
			literal, ok := element.(*ast.BasicLit)
			if !ok {
				t.Fatal("publisher inventory entry is not a literal")
			}
			pkg, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			want[path.Base(pkg)] = true
		}
		return false
	})
	if len(want) <= 3 {
		t.Fatal("could not read the pinned publisher's inventory")
	}
	for _, tc := range []struct {
		name, image, status string
		httpStatus          int
		disconnect          bool
		wrongDigest         bool
		tokenFailure        bool
	}{
		{name: "valid full inventory and mixed registry credentials", status: "available"},
		{name: "empty recreated registry", image: "ate-idp-server", status: "missing", httpStatus: http.StatusNotFound},
		{name: "missing podcertcontroller", image: "podcertcontroller", status: "missing", httpStatus: http.StatusNotFound},
		{name: "missing exact plugin digest", image: "plugin", status: "missing", httpStatus: http.StatusNotFound},
		{name: "authentication failure", image: "podcertcontroller", status: "authentication", httpStatus: http.StatusUnauthorized},
		{name: "authorization failure", image: "podcertcontroller", status: "authorization", httpStatus: http.StatusForbidden},
		{name: "network failure", image: "podcertcontroller", status: "network", disconnect: true},
		{name: "registry failure", image: "podcertcontroller", status: "registry-error", httpStatus: http.StatusInternalServerError},
		{name: "wrong plugin digest", image: "plugin", status: "registry-error", wrongDigest: true},
		{name: "token endpoint 404 is not a missing image", image: "ate-idp-server", status: "registry-error", tokenFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const secret = "synthetic-credential-must-not-appear"
			const tag = "contract-images"
			pluginDigest := "sha256:" + strings.Repeat("a", 64)
			var mu sync.Mutex
			seen := map[string]bool{}
			handler := func(user string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					if tc.tokenFailure && r.URL.Path == "/token" {
						w.WriteHeader(http.StatusNotFound)
						w.Write([]byte(secret))
						return
					}
					if r.URL.Path == "/v2/" && r.Method == http.MethodGet {
						if tc.tokenFailure {
							w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+`/token?secret=`+secret+`",service="registry.local"`)
							w.WriteHeader(http.StatusUnauthorized)
						}
						return
					}
					if r.Method != http.MethodHead {
						t.Errorf("image verification made a non-HEAD request: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusMethodNotAllowed)
						return
					}
					username, password, ok := r.BasicAuth()
					if !ok || username != user || password != secret {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					parts := strings.Split(r.URL.Path, "/")
					image := parts[len(parts)-3]
					mu.Lock()
					seen[image] = true
					mu.Unlock()
					if image == tc.image {
						if tc.disconnect {
							connection, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							connection.Close()
							return
						}
						if tc.httpStatus != 0 {
							w.WriteHeader(tc.httpStatus)
							w.Write([]byte(secret))
							return
						}
					}
					digest := "sha256:" + strings.Repeat("b", 64)
					if image == "plugin" && !tc.wrongDigest {
						digest = pluginDigest
					}
					w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
					w.Header().Set("Content-Length", "123")
					w.Header().Set("Docker-Content-Digest", digest)
				}
			}
			owned := httptest.NewServer(handler("owned"))
			defer owned.Close()
			external := httptest.NewServer(handler("external"))
			defer external.Close()
			ownedHost := strings.TrimPrefix(owned.URL, "http://")
			externalHost := strings.TrimPrefix(external.URL, "http://")
			dockerConfig := func(host, user string) string {
				t.Helper()
				dir := t.TempDir()
				data, err := json.Marshal(map[string]any{"auths": map[string]any{
					host: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + secret))},
				}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
				return dir
			}
			repository := ownedHost + "/substrate"
			plugin := externalHost + "/plugin@" + pluginDigest
			check := exec.CommandContext(t.Context(), binary, "publish", "check-images",
				repository, tag, plugin, ownedHost, dockerConfig(ownedHost, "owned"),
				"--no-dev-env", "--no-report-defaults", "--kubeconfig", "/not-a-kubeconfig", "--context", "not-a-cluster")
			check.Dir = dest
			check.Env = append(cleanEnvironment(os.Environ()), "DOCKER_CONFIG="+dockerConfig(externalHost, "external"),
				"VERSION="+tag, "NO_DEV_ENV=1")
			output, err := check.CombinedOutput()
			if err != nil {
				t.Fatalf("checker failed before reporting registry availability: %v\n%s", err, output)
			}
			if strings.Contains(string(output), secret) {
				t.Fatal("checker exposed registry credentials or error-body contents")
			}
			var report struct {
				Status   string `json:"status"`
				Image    string `json:"image"`
				Checked  int    `json:"checked"`
				Expected int    `json:"expected"`
			}
			if err := json.Unmarshal(output, &report); err != nil {
				t.Fatalf("checker output was not a structured result: %v\n%s", err, output)
			}
			if report.Status != tc.status || report.Expected != len(want) {
				t.Fatalf("wrong registry classification or publisher inventory: %+v", report)
			}
			mu.Lock()
			actual := make(map[string]bool, len(seen))
			for image := range seen {
				actual[image] = true
			}
			mu.Unlock()
			if tc.status == "available" {
				if err := imageCheckResult(output); err != nil || !reflect.DeepEqual(actual, want) {
					t.Fatalf("valid image set was not fully checked: %v; checked=%v, expected=%v", err, actual, want)
				}
			} else if err := imageCheckResult(output); err == nil || (!tc.tokenFailure && !actual[tc.image]) {
				t.Fatalf("registry failure became successful validation: %v; %+v", err, report)
			}
		})
	}
	kubeconfig := filepath.Join(t.TempDir(), "invalid-kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("not kubeconfig YAML ["), 0600); err != nil {
		t.Fatal(err)
	}
	deploy := exec.CommandContext(t.Context(), binary, "deploy", "ate-system", "--no-dev-env", "--no-report-defaults",
		"--kubeconfig", kubeconfig, "--context", "not-a-cluster")
	deploy.Dir = dest
	deploy.Env = cleanEnvironment(os.Environ())
	output, err := deploy.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "while reading kubeconfig") {
		t.Fatalf("image checker bypassed normal deployment startup guards: %v\n%s", err, output)
	}
}
