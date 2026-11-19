package installer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRestoreRunReadinessAndSingleCounterCalls(t *testing.T) {
	for _, tc := range []struct {
		name                string
		retryInitial        bool
		retryResumed        bool
		wrongInitial        bool
		reset               bool
		failPhase, failCode int
		failReadyPhase      int
		cleanupFailure      bool
		timeout             bool
	}{
		{name: "both readiness retries", retryInitial: true, retryResumed: true},
		{name: "initial readiness fails with cleanup failure", failReadyPhase: 1, cleanupFailure: true},
		{name: "resumed readiness fails", failReadyPhase: 2},
		{name: "initial wrong count", wrongInitial: true},
		{name: "reset after resumed readiness retry", retryResumed: true, reset: true},
		{name: "processed initial 504", failPhase: 1, failCode: 504},
		{name: "processed resumed 504", failPhase: 2, failCode: 504},
		{name: "initial 502", failPhase: 1, failCode: 502},
		{name: "resumed 503", failPhase: 2, failCode: 503},
		{name: "initial POST timeout", failPhase: 1, timeout: true},
		{name: "resumed POST timeout", failPhase: 2, timeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			namespace, actor, token, err := restoreNames()
			if err != nil {
				t.Fatal(err)
			}
			var posts, initialGets, resumedGets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/readyz":
					if posts.Load() == 0 {
						if tc.failReadyPhase == 1 {
							w.WriteHeader(http.StatusUnauthorized)
							return
						}
						if initialGets.Add(1) == 1 && tc.retryInitial {
							w.WriteHeader(http.StatusGatewayTimeout)
							return
						}
					} else {
						if tc.failReadyPhase == 2 {
							w.WriteHeader(http.StatusUnauthorized)
							return
						}
						if resumedGets.Add(1) == 1 && tc.retryResumed {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
					}
					fmt.Fprint(w, "ok\n")
				case r.Method == http.MethodPost && r.URL.Path == "/":
					n := int(posts.Add(1))
					if n == tc.failPhase {
						if tc.timeout {
							<-r.Context().Done()
							return
						}
						w.WriteHeader(tc.failCode)
						return
					}
					value := n
					if tc.wrongInitial && n == 1 {
						value = 2
					}
					if tc.reset && n == 2 {
						value = 1
					}
					fmt.Fprintf(w, "preserved memory count: %d | preserved file counter: %d | preserved rootfs counter: %d\n", value, value, value)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			ctx := t.Context()
			if tc.timeout {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
				defer cancel()
			}
			r := &restoreRunner{config: c, namespace: namespace, actor: actor, token: token, address: server.Listener.Addr().String()}
			cleanupErr := errors.New("actor cleanup failed")
			if tc.cleanupFailure {
				r.failures = map[string]error{" delete actor " + actor: cleanupErr}
			}
			state := &restoreState{namespace: namespace, token: token, client: "/client", helper: &restoreHelper{binary: "/fixture"}}
			pool := []byte(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\napiVersion: ate.dev/v1\nkind: WorkerPool\nmetadata:\n  name: counter-microvm\n  namespace: %s\n", namespace, namespace))
			var messages []string
			a := App{Config: c, Runner: r, Progress: func(f string, args ...any) { messages = append(messages, fmt.Sprintf(f, args...)) }}
			err = a.restoreRun(ctx, ctx, state, actor, pool, []byte("template"))
			fail := tc.wrongInitial || tc.reset || tc.failPhase != 0 || tc.failReadyPhase != 0
			if (err != nil) != fail {
				t.Fatalf("unexpected result: %v", err)
			}
			wantPosts := int32(2)
			if tc.wrongInitial || tc.failPhase == 1 || tc.failReadyPhase == 2 {
				wantPosts = 1
			}
			if tc.failReadyPhase == 1 {
				wantPosts = 0
			}
			if posts.Load() != wantPosts {
				t.Fatalf("POSTs=%d, want %d", posts.Load(), wantPosts)
			}
			if tc.retryInitial && initialGets.Load() != 2 || tc.retryResumed && resumedGets.Load() != 2 {
				t.Fatalf("readiness attempts: initial=%d resumed=%d", initialGets.Load(), resumedGets.Load())
			}
			if tc.reset && !strings.Contains(err.Error(), "expected 2, observed 1") ||
				tc.wrongInitial && !strings.Contains(err.Error(), "expected 1, observed 2") {
				t.Fatalf("incorrect count failure: %v", err)
			}
			if tc.failPhase != 0 && !strings.Contains(err.Error(), "not retried") {
				t.Fatalf("ambiguous POST did not report no retry: %v", err)
			}
			if fail {
				phase := "initial counter phase"
				if tc.reset || tc.failPhase == 2 || tc.failReadyPhase == 2 {
					phase = "resumed counter phase"
				}
				if !strings.Contains(err.Error(), phase) {
					t.Fatalf("phase missing: %v", err)
				}
			}
			if tc.timeout && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout identity lost: %v", err)
			}
			if tc.cleanupFailure && !errors.Is(err, cleanupErr) {
				t.Fatalf("cleanup failure not joined: %v", err)
			}
			commands := strings.Join(r.calls, "\n")
			suspends := strings.Count(commands, " suspend actor ")
			wantSuspends := 0
			if wantPosts == 2 || tc.failReadyPhase == 2 {
				wantSuspends = 1
			}
			if suspends != wantSuspends ||
				strings.Count(commands, " create actor "+actor+" ") != 1 ||
				strings.Contains(commands, "delete --raw /api/v1/namespaces/") == tc.cleanupFailure {
				t.Fatalf("unexpected orchestration: %s", commands)
			}
			text := strings.Join(messages, "\n")
			if strings.Contains(text, "RESTORE_SKIPPED") || strings.Contains(text, "RESTORE_PASSED") == fail {
				t.Fatalf("incorrect outcome: %s", text)
			}
		})
	}
}

func TestRestoreRunJoinsPrimaryProxyCleanupAndOutputFailures(t *testing.T) {
	c := testConfig(t)
	namespace, actor, token, err := restoreNames()
	if err != nil {
		t.Fatal(err)
	}
	exit := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if restoreTestReady(w, req) {
			return
		}
		close(exit)
		fmt.Fprint(w, "preserved memory count: 1 | preserved file counter: 1 | preserved rootfs counter: 1\n")
	}))
	defer server.Close()
	primary, proxy, cleanup, output := errors.New("placement failed"), errors.New("proxy exited"),
		errors.New("actor cleanup failed"), errors.New("presentation failed")
	r := &restoreRunner{config: c, namespace: namespace, actor: actor, token: token, address: server.Listener.Addr().String(),
		proxyExitAfter: exit, proxyExitErr: proxy, failures: map[string]error{" delete actor " + actor: cleanup}}
	var failed atomic.Bool
	runner := prerequisiteRunner(func(ctx context.Context, cmd Command) ([]byte, error) {
		if strings.Contains(strings.Join(cmd.Args, " "), " get actor "+actor) && failed.CompareAndSwap(false, true) {
			return nil, primary
		}
		return r.Run(ctx, cmd)
	})
	state := &restoreState{namespace: namespace, token: token, client: "/client", helper: &restoreHelper{binary: "/fixture"}}
	pool := []byte(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\napiVersion: ate.dev/v1\nkind: WorkerPool\nmetadata:\n  name: counter-microvm\n  namespace: %s\n", namespace, namespace))
	a := App{Config: c, Runner: runner, RecoveryReceipt: func(context.Context, string) error { return output }}
	err = a.restoreRun(t.Context(), t.Context(), state, actor, pool, []byte("template"))
	for _, want := range []error{primary, proxy, cleanup, output} {
		if !errors.Is(err, want) {
			t.Fatalf("failure %v not joined: %v", want, err)
		}
	}
	if strings.Contains(strings.Join(r.calls, "\n"), "delete --raw") {
		t.Fatal("removed pool after cleanup failure")
	}
}

func TestRestoreRunRejectsCounterResetAndStillCleans(t *testing.T) {
	c := testConfig(t)
	namespace, actor, token, err := restoreNames()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if restoreTestReady(w, req) {
			return
		}
		fmt.Fprint(w, "preserved memory count: 1 | preserved file counter: 1 | preserved rootfs counter: 1\n")
	}))
	defer server.Close()
	r := &restoreRunner{config: c, namespace: namespace, actor: actor, token: token, address: server.Listener.Addr().String()}
	state := &restoreState{namespace: namespace, token: token, client: "/client", helper: &restoreHelper{binary: "/fixture"}}
	pool := []byte(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\napiVersion: ate.dev/v1\nkind: WorkerPool\nmetadata:\n  name: counter-microvm\n  namespace: %s\n", namespace, namespace))
	var messages []string
	err = (App{Config: c, Runner: r, Progress: func(f string, args ...any) { messages = append(messages, fmt.Sprintf(f, args...)) }}).restoreRun(t.Context(), t.Context(), state, actor, pool, []byte("template"))
	if err == nil || !strings.Contains(err.Error(), "unexpected counter memory response") {
		t.Fatalf("counter reset accepted: %v", err)
	}
	if !strings.Contains(strings.Join(r.calls, "\n"), "delete --raw /api/v1/namespaces/") || strings.Contains(strings.Join(messages, "\n"), "RESTORE_PASSED") {
		t.Fatal("counter failure did not clean up or reported success")
	}
}

func TestRestoreRunRetainsWorkersOnForeignActor(t *testing.T) {
	c := testConfig(t)
	namespace, actor, token, err := restoreNames()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if restoreTestReady(w, req) {
			return
		}
		count++
		fmt.Fprintf(w, "preserved memory count: %d | preserved file counter: %d | preserved rootfs counter: %d\n", count, count, count)
	}))
	defer server.Close()
	r := &restoreRunner{config: c, namespace: namespace, actor: actor, token: token, address: server.Listener.Addr().String(), foreign: true}
	state := &restoreState{namespace: namespace, token: token, client: "/client", helper: &restoreHelper{binary: "/fixture"}}
	pool := []byte(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\napiVersion: ate.dev/v1\nkind: WorkerPool\nmetadata:\n  name: counter-microvm\n  namespace: %s\n", namespace, namespace))
	var progress []string
	err = (App{Config: c, Runner: r, Progress: func(f string, args ...any) { progress = append(progress, fmt.Sprintf(f, args...)) }}).restoreRun(t.Context(), t.Context(), state, actor, pool, []byte("template"))
	if err == nil || !strings.Contains(err.Error(), "actors exist") {
		t.Fatalf("foreign actor accepted: %v", err)
	}
	log := strings.Join(r.calls, "\n")
	if strings.Contains(log, "delete --raw") || strings.Contains(log, "delete atespace") {
		t.Fatalf("removed foreign actor capacity: %s", log)
	}
	if strings.Contains(strings.Join(progress, "\n"), "RESTORE_PASSED") {
		t.Fatal("cleanup failure reported restore success")
	}
}

func TestRestoreRunCleansAfterProxyExits(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		t.Run(fmt.Sprint(unsafe), func(t *testing.T) {
			c := testConfig(t)
			namespace, actor, token, err := restoreNames()
			if err != nil {
				t.Fatal(err)
			}
			exit := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if restoreTestReady(w, req) {
					return
				}
				close(exit)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			proxyErr := errors.New("proxy exited")
			r := &restoreRunner{config: c, namespace: namespace, actor: actor, token: token,
				address: server.Listener.Addr().String(), proxyExitAfter: exit, proxyExitErr: proxyErr}
			if unsafe {
				r.unsafeCensus = "template"
			}
			state := &restoreState{namespace: namespace, token: token, client: "/client", helper: &restoreHelper{binary: "/fixture"}}
			pool := []byte(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\napiVersion: ate.dev/v1\nkind: WorkerPool\nmetadata:\n  name: counter-microvm\n  namespace: %s\n", namespace, namespace))
			err = (App{Config: c, Runner: r}).restoreRun(t.Context(), t.Context(), state, actor, pool, []byte("template"))
			if !errors.Is(err, proxyErr) || !strings.Contains(err.Error(), "counter POST returned HTTP status") {
				t.Fatalf("primary and proxy failures not joined: %v", err)
			}
			calls := strings.Join(r.calls, "\n")
			for _, operation := range []string{"delete actor " + actor, "delete actor-template"} {
				if !strings.Contains(calls, operation) {
					t.Fatalf("missing cleanup %q: %s", operation, calls)
				}
			}
			if unsafe {
				if !strings.Contains(err.Error(), "actor templates exist") || strings.Contains(calls, "delete --raw") {
					t.Fatalf("unsafe census was not joined or retained: %v\n%s", err, calls)
				}
				return
			}
			for _, operation := range []string{"delete --raw /apis/ate.dev", "delete atespace", "delete --raw /api/v1/namespaces/"} {
				if !strings.Contains(calls, operation) {
					t.Fatalf("missing cleanup %q: %s", operation, calls)
				}
			}
		})
	}
}

func TestRestoreRunRetainsWorkersOnUnsafeCensus(t *testing.T) {
	for _, issue := range []string{"foreign", "allocated", "late-allocated", "changed", "late-changed", "pod-change", "template", "error"} {
		t.Run(issue, func(t *testing.T) {
			c := testConfig(t)
			namespace, actor, token, err := restoreNames()
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if restoreTestReady(w, req) {
					return
				}
				count++
				fmt.Fprintf(w, "preserved memory count: %d | preserved file counter: %d | preserved rootfs counter: %d\n", count, count, count)
			}))
			defer server.Close()
			r := &restoreRunner{config: c, namespace: namespace, actor: actor, token: token, address: server.Listener.Addr().String(), unsafeCensus: issue}
			state := &restoreState{namespace: namespace, token: token, client: "/client", helper: &restoreHelper{binary: "/fixture"}}
			pool := []byte(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\napiVersion: ate.dev/v1\nkind: WorkerPool\nmetadata:\n  name: counter-microvm\n  namespace: %s\n", namespace, namespace))
			if err := (App{Config: c, Runner: r}).restoreRun(t.Context(), t.Context(), state, actor, pool, []byte("template")); err == nil {
				t.Fatal("unsafe census was accepted")
			}
			calls := strings.Join(r.calls, "\n")
			if strings.Contains(calls, "delete --raw") || strings.Contains(calls, "delete atespace") {
				t.Fatalf("removed capacity after unsafe census: %s", calls)
			}
		})
	}
}

func TestRestoreRunJoinsPrimaryAndCleanupFailures(t *testing.T) {
	c := testConfig(t)
	namespace, actor, token, err := restoreNames()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if restoreTestReady(w, req) {
			return
		}
		fmt.Fprint(w, "preserved memory count: 1 | preserved file counter: 1 | preserved rootfs counter: 1\n")
	}))
	defer server.Close()
	primary, cleanup := errors.New("suspend failed"), errors.New("actor cleanup failed")
	r := &restoreRunner{config: c, namespace: namespace, actor: actor, token: token, address: server.Listener.Addr().String(),
		failures: map[string]error{" suspend actor ": primary, " delete actor " + actor: cleanup}}
	state := &restoreState{namespace: namespace, token: token, client: "/client", helper: &restoreHelper{binary: "/fixture"}}
	pool := []byte(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\napiVersion: ate.dev/v1\nkind: WorkerPool\nmetadata:\n  name: counter-microvm\n  namespace: %s\n", namespace, namespace))
	err = (App{Config: c, Runner: r}).restoreRun(t.Context(), t.Context(), state, actor, pool, []byte("template"))
	if !errors.Is(err, primary) || !errors.Is(err, cleanup) {
		t.Fatalf("failure lost: %v", err)
	}
	if strings.Contains(strings.Join(r.calls, "\n"), "delete --raw") {
		t.Fatal("removed pool after actor cleanup failure")
	}
}
