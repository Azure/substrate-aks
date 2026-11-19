package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRestoreCleanupWaitsForSnapshotRelease(t *testing.T) {
	c := testConfig(t)
	r := &restoreRunner{config: c, namespace: "space", actor: "actor", token: "token", poolExists: true}
	state := &restoreState{namespace: "space", token: "token", client: "/client", helper: &restoreHelper{binary: "/fixture"},
		ns: restoreKubeIdentity{Name: "space", UID: "namespace-id"}, pool: restoreKubeIdentity{Name: restorePoolName, Namespace: "space", UID: "pool-id"},
		space: restoreAPIIdentity{Name: "space", UID: "space-id"}, actor: restoreAPIIdentity{Name: "actor", Atespace: "space", UID: "actor-id"},
		template: restoreAPIIdentity{Name: restorePoolName, Atespace: "space", UID: "template-id"}}
	actorStarted, actorRelease := make(chan struct{}), make(chan struct{})
	templateStarted, templateRelease := make(chan struct{}), make(chan struct{})
	censusStarted := make(chan struct{}, 1)
	var actorDone, templateDone atomic.Bool
	// The pinned delete workflows finish snapshot release before returning.
	// Source: b34fbee742906494b5eb4f1f36987176bf8770a4,
	// cmd/ateapi/internal/controlapi/workflow_delete.go:31-96 and workflow_template_delete.go:29-56.
	runner := prerequisiteRunner(func(ctx context.Context, cmd Command) ([]byte, error) {
		command := strings.Join(cmd.Args, " ")
		switch {
		case strings.Contains(command, " delete actor actor "):
			close(actorStarted)
			<-actorRelease
			data, err := r.Run(ctx, cmd)
			actorDone.Store(err == nil)
			return data, err
		case strings.Contains(command, " delete actor-template "):
			if !actorDone.Load() {
				t.Error("template deletion started before actor snapshot release")
			}
			close(templateStarted)
			<-templateRelease
			data, err := r.Run(ctx, cmd)
			templateDone.Store(err == nil)
			return data, err
		case strings.Contains(command, " publish restore-census "), strings.Contains(command, " delete --raw /apis/"),
			strings.Contains(command, " delete worker "), strings.Contains(command, " delete pod "):
			if !actorDone.Load() || !templateDone.Load() {
				t.Error("capacity check or removal preceded snapshot release")
			}
			if strings.Contains(command, " publish restore-census ") {
				select {
				case censusStarted <- struct{}{}:
				default:
				}
			}
		}
		return r.Run(ctx, cmd)
	})
	done, finished := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(finished)
		done <- (App{Config: c, Runner: runner}).restoreCleanup(t.Context(), state)
	}()
	defer func() {
		for _, ch := range []chan struct{}{actorRelease, templateRelease} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("cleanup goroutine did not finish")
		}
	}()
	await := func(ch <-chan struct{}, label string) {
		t.Helper()
		select {
		case <-ch:
		case err := <-done:
			t.Fatalf("cleanup ended before %s: %v", label, err)
		case <-time.After(5 * time.Second):
			t.Fatalf("cleanup did not reach %s", label)
		}
	}
	assertNoCensus := func() {
		t.Helper()
		select {
		case <-censusStarted:
			t.Fatal("census began before snapshot release completed")
		default:
		}
	}
	await(actorStarted, "actor deletion")
	assertNoCensus()
	close(actorRelease)
	await(templateStarted, "template deletion")
	assertNoCensus()
	close(templateRelease)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not finish")
	}
	commands := strings.Join(r.calls, "\n")
	if !strings.Contains(commands, "publish restore-census") || !strings.Contains(commands, "delete --raw /apis/ate.dev") ||
		!strings.Contains(commands, "delete --raw /api/v1/namespaces/") {
		t.Fatalf("cleanup did not reach census and removals: %s", commands)
	}
}

func TestRestoreCleanupRetainsCapacityWhenSnapshotReleaseFails(t *testing.T) {
	for _, step := range []string{" delete actor actor ", " delete actor-template "} {
		t.Run(step, func(t *testing.T) {
			c := testConfig(t)
			failure := errors.New("snapshot release failed")
			r := &restoreRunner{config: c, namespace: "space", actor: "actor", token: "token", poolExists: true,
				failures: map[string]error{step: failure}}
			state := &restoreState{namespace: "space", token: "token", client: "/client", helper: &restoreHelper{binary: "/fixture"},
				ns: restoreKubeIdentity{Name: "space", UID: "namespace-id"}, pool: restoreKubeIdentity{Name: restorePoolName, Namespace: "space", UID: "pool-id"},
				space: restoreAPIIdentity{Name: "space", UID: "space-id"}, actor: restoreAPIIdentity{Name: "actor", Atespace: "space", UID: "actor-id"},
				template: restoreAPIIdentity{Name: restorePoolName, Atespace: "space", UID: "template-id"}}
			err := (App{Config: c, Runner: r}).restoreCleanup(t.Context(), state)
			if !errors.Is(err, failure) {
				t.Fatalf("snapshot release failure lost: %v", err)
			}
			commands := strings.Join(r.calls, "\n")
			if strings.Contains(commands, "publish restore-census") || strings.Contains(commands, "delete --raw") ||
				strings.Contains(commands, "delete atespace") || !r.poolExists {
				t.Fatalf("capacity was not retained: %s", commands)
			}
		})
	}
}

type restoreRunner struct {
	mu                               sync.Mutex
	config                           Config
	namespace, actor, token, address string
	poolExists                       bool
	cancel                           context.CancelFunc
	cancelAt                         string
	failed                           bool
	failures                         map[string]error
	foreign                          bool
	unsafeCensus                     string
	proxyExitAfter                   <-chan struct{}
	proxyExitErr                     error
	calls                            []string
}

func restoreTestReady(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet && r.URL.Path == "/readyz" {
		fmt.Fprint(w, "ok\n")
		return true
	}
	return false
}

func (r *restoreRunner) Run(ctx context.Context, cmd Command) ([]byte, error) {
	command := strings.Join(cmd.Args, " ")
	r.mu.Lock()
	if r.namespace == "" && strings.Contains(command, " create -f - --output=json") && strings.Contains(string(cmd.Input), `"kind":"Namespace"`) {
		var namespace struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(cmd.Input, &namespace); err != nil {
			r.mu.Unlock()
			return nil, err
		}
		r.namespace = namespace.Metadata.Name
		r.token = namespace.Metadata.Labels[restoreInvocationLabel]
	}
	if r.actor == "" {
		for i, arg := range cmd.Args {
			if arg == "actor" && i > 0 && cmd.Args[i-1] == "create" && i+1 < len(cmd.Args) {
				r.actor = cmd.Args[i+1]
				break
			}
		}
	}
	r.calls = append(r.calls, command)
	for substring, failure := range r.failures {
		if strings.Contains(command, substring) {
			r.mu.Unlock()
			return nil, failure
		}
	}
	matches := r.cancelAt != "" && strings.Contains(command, r.cancelAt)
	if r.cancelAt == "pool-create" {
		matches = strings.Contains(command, " create -f - --output=json") && strings.Contains(string(cmd.Input), `"kind":"WorkerPool"`)
	}
	if matches && !r.failed {
		r.failed = true
		r.cancel()
		if ctx.Err() != nil {
			r.mu.Unlock()
			return nil, ctx.Err()
		}
	}
	if strings.Contains(command, " proxy actor ") {
		fmt.Fprintf(cmd.StreamStderr, "Proxying %s -> %s/%s:80\n", r.address, r.namespace, r.actor)
		r.mu.Unlock()
		if r.proxyExitAfter != nil {
			<-r.proxyExitAfter
			return nil, r.proxyExitErr
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	apiIdentity := func(name, space, uid string) []byte {
		return []byte(fmt.Sprintf(`{"metadata":{"name":%q,"atespace":%q,"uid":%q}}`, name, space, uid))
	}
	kubeIdentity := func(kind, name, space, uid string) []byte {
		return []byte(fmt.Sprintf(`{"kind":%q,"metadata":{"name":%q,"namespace":%q,"uid":%q,"labels":{"substrate-aks.io/check":%q,"substrate-aks.io/installation":%q}}}`, kind, name, space, uid, r.token, r.config.Installation.ID))
	}
	switch {
	case strings.Contains(command, " create -f - --output=json"):
		if strings.Contains(string(cmd.Input), `"kind":"Namespace"`) {
			return kubeIdentity("Namespace", r.namespace, "", "namespace-id"), nil
		}
		r.poolExists = true
		return kubeIdentity("WorkerPool", restorePoolName, r.namespace, "pool-id"), nil
	case strings.Contains(command, " create atespace "):
		return apiIdentity(r.namespace, "", "space-id"), nil
	case strings.Contains(command, " create actor-template "):
		return apiIdentity(restorePoolName, r.namespace, "template-id"), nil
	case strings.Contains(command, " create actor "):
		return apiIdentity(r.actor, r.namespace, "actor-id"), nil
	case strings.Contains(command, " get atespace "):
		return apiIdentity(r.namespace, "", "space-id"), nil
	case strings.Contains(command, " get actor-template "):
		return []byte(fmt.Sprintf(`{"metadata":{"name":%q,"atespace":%q,"uid":"template-id"},"status":{"goldenSnapshotStatus":{"goldenTag":{"name":"gold"}}}}`, restorePoolName, r.namespace)), nil
	case strings.Contains(command, " get actor "+r.actor):
		return []byte(fmt.Sprintf(`{"metadata":{"name":%q,"atespace":%q,"uid":"actor-id"},"status":{"workerAssignment":{"workerNamespace":%q,"workerPool":"counter-microvm","workerPod":"pod","workerPodUid":"pod-id","worker":{"name":"worker"}}}}`, r.actor, r.namespace, r.namespace)), nil
	case strings.Contains(command, " publish restore-census "):
		full := strings.Contains(command, " full ")
		actors := r.foreign && full
		if full && r.unsafeCensus == "error" {
			return nil, errors.New("incomplete census")
		}
		if !r.poolExists {
			return []byte(fmt.Sprintf(`{"workers":[],"actors":%t,"actorTemplates":false}`, actors)), nil
		}
		worker := fmt.Sprintf(`{"metadata":{"name":"worker","uid":"worker-id"},"workerNamespace":%q,"workerPool":"counter-microvm","workerPod":"pod","workerPodUid":"pod-id","labels":{"substrate-aks.io/check":%q,"substrate-aks.io/installation":%q},"status":{"state":"WORKER_STATE_ACTIVE","registeredEpoch":"1"}}`, r.namespace, r.token, r.config.Installation.ID)
		templates := false
		if full {
			switch r.unsafeCensus {
			case "foreign":
				other := strings.Replace(worker, `"name":"worker"`, `"name":"other"`, 1)
				other = strings.Replace(other, `"uid":"worker-id"`, `"uid":"other-id"`, 1)
				other = strings.Replace(other, `"workerNamespace":`+strconv.Quote(r.namespace), `"workerNamespace":"foreign"`, 1)
				worker += "," + other
			case "allocated":
				worker = strings.Replace(worker, `"state":"WORKER_STATE_ACTIVE"`, `"state":"WORKER_STATE_ACTIVE","allocated":{"actors":1}`, 1)
			case "late-allocated":
				other := strings.Replace(worker, `"name":"worker"`, `"name":"other"`, 1)
				other = strings.Replace(other, `"uid":"worker-id"`, `"uid":"other-id"`, 1)
				other = strings.Replace(other, `"state":"WORKER_STATE_ACTIVE"`, `"state":"WORKER_STATE_ACTIVE","allocated":{"actors":1}`, 1)
				worker += "," + other
			case "changed":
				worker = strings.Replace(worker, `"uid":"worker-id"`, `"uid":"changed"`, 1)
			case "late-changed":
				other := strings.Replace(worker, `"name":"worker"`, `"name":"other"`, 1)
				other = strings.Replace(other, `"uid":"worker-id"`, `"uid":"other-id"`, 1)
				worker = other + "," + strings.Replace(worker, `"uid":"worker-id"`, `"uid":"changed"`, 1)
			case "template":
				templates = true
			}
		}
		return []byte(fmt.Sprintf(`{"workers":[%s],"actors":%t,"actorTemplates":%t}`, worker, actors, templates)), nil
	case strings.Contains(command, " get actors --all-atespaces"):
		if r.foreign {
			return []byte(`{"actors":[{"metadata":{"name":"other","atespace":"other","uid":"foreign"}}]}`), nil
		}
		return []byte(`{}`), nil
	case strings.Contains(command, " get workers -o json"):
		if !r.poolExists {
			return []byte(`{}`), nil
		}
		return []byte(fmt.Sprintf(`{"workers":[{"metadata":{"name":"worker","uid":"worker-id"},"workerNamespace":%q,"workerPool":"counter-microvm","workerPod":"pod","workerPodUid":"pod-id","labels":{"substrate-aks.io/check":%q,"substrate-aks.io/installation":%q},"status":{"state":"WORKER_STATE_ACTIVE","registeredEpoch":"1"}}]}`, r.namespace, r.token, r.config.Installation.ID)), nil
	case strings.Contains(command, " publish restore-namespace-resources "):
		return []byte("workerpools.ate.dev\npods\n"), nil
	case strings.Contains(command, " get pods --namespace="):
		if !r.poolExists {
			return []byte(`{"kind":"PodList","items":[]}`), nil
		}
		return []byte(fmt.Sprintf(`{"kind":"PodList","items":[{"kind":"Pod","metadata":{"name":"pod","namespace":%q,"uid":"pod-id","ownerReferences":[{"uid":"pool-id"}]}}]}`, r.namespace)), nil
	case strings.Contains(command, " get pod pod "):
		if r.unsafeCensus == "pod-change" {
			return kubeIdentity("Pod", "pod", r.namespace, "changed-pod"), nil
		}
		return kubeIdentity("Pod", "pod", r.namespace, "pod-id"), nil
	case strings.Contains(command, " get workerpools.ate.dev --namespace="):
		if !r.poolExists {
			return []byte(`{"kind":"WorkerPoolList","items":[]}`), nil
		}
		return []byte(fmt.Sprintf(`{"kind":"WorkerPoolList","items":[{"kind":"WorkerPool","metadata":{"name":"counter-microvm","namespace":%q,"uid":"pool-id"}}]}`, r.namespace)), nil
	case strings.Contains(command, " get workerpools.ate.dev "+restorePoolName):
		return kubeIdentity("WorkerPool", restorePoolName, r.namespace, "pool-id"), nil
	case strings.Contains(command, " get namespace "+r.namespace):
		return kubeIdentity("Namespace", r.namespace, "", "namespace-id"), nil
	case strings.Contains(command, " delete --raw /apis/"):
		r.poolExists = false
		return nil, nil
	case strings.Contains(command, " delete "), strings.Contains(command, " wait "), strings.Contains(command, " rollout status "), strings.Contains(command, " suspend actor "):
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected command %s", command)
	}
}

func TestRestoreRunCallsSuspendResumeAndCleansUp(t *testing.T) {
	c := testConfig(t)
	namespace, actor, token, err := restoreNames()
	if err != nil {
		t.Fatal(err)
	}
	counter := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if restoreTestReady(w, r) {
			return
		}
		counter++
		fmt.Fprintf(w, "preserved memory count: %d | preserved file counter: %d | preserved rootfs counter: %d\n", counter, counter, counter)
	}))
	defer server.Close()
	r := &restoreRunner{config: c, namespace: namespace, actor: actor, token: token, address: server.Listener.Addr().String()}
	state := &restoreState{namespace: namespace, token: token, client: "/client", helper: &restoreHelper{binary: "/fixture"}}
	pool := []byte(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\napiVersion: ate.dev/v1\nkind: WorkerPool\nmetadata:\n  name: counter-microvm\n  namespace: %s\n", namespace, namespace))
	var lines []string
	a := App{Config: c, Runner: r, Progress: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }}
	if err := a.restoreRun(t.Context(), t.Context(), state, actor, pool, []byte("template")); err != nil {
		t.Fatal(err)
	}
	if counter != 2 || !strings.Contains(strings.Join(lines, "\n"), "RESTORE_PASSED") {
		t.Fatalf("counter=%d progress=%v", counter, lines)
	}
	commands := strings.Join(r.calls, "\n")
	actorDelete := strings.Index(commands, "delete actor "+actor)
	templateDelete := strings.Index(commands, "delete actor-template")
	poolDelete := strings.Index(commands, "delete --raw /apis/ate.dev")
	namespaceDelete := strings.Index(commands, "delete --raw /api/v1/namespaces/")
	if actorDelete < 0 || templateDelete < actorDelete || poolDelete < templateDelete || namespaceDelete < poolDelete {
		t.Fatalf("cleanup order: %s", commands)
	}
}

func TestRestoreRunJoinsRecoveryPresentationFailure(t *testing.T) {
	c := testConfig(t)
	namespace, actor, token, err := restoreNames()
	if err != nil {
		t.Fatal(err)
	}
	createErr := errors.New("create failed")
	presentErr := errors.New("presentation failed")
	r := &restoreRunner{config: c, namespace: namespace, actor: actor, token: token, failures: map[string]error{" create atespace ": createErr}}
	state := &restoreState{namespace: namespace, token: token, client: "/client", helper: &restoreHelper{binary: "/fixture"}}
	pool := []byte(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\napiVersion: ate.dev/v1\nkind: WorkerPool\nmetadata:\n  name: counter-microvm\n  namespace: %s\n", namespace, namespace))
	a := App{Config: c, Runner: r, RecoveryReceipt: func(context.Context, string) error { return presentErr }}
	err = a.restoreRun(t.Context(), t.Context(), state, actor, pool, []byte("template"))
	if !errors.Is(err, createErr) || !errors.Is(err, presentErr) || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("failure details not joined: %v", err)
	}
}

func TestRestoreRunCancellationPreservesUncertainty(t *testing.T) {
	c := testConfig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	namespace, actor, token, err := restoreNames()
	if err != nil {
		t.Fatal(err)
	}
	r := &restoreRunner{config: c, namespace: namespace, actor: actor, token: token, cancel: cancel, cancelAt: " create atespace "}
	state := &restoreState{namespace: namespace, token: token, client: "/client", helper: &restoreHelper{binary: "/fixture"}}
	pool := []byte(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\napiVersion: ate.dev/v1\nkind: WorkerPool\nmetadata:\n  name: counter-microvm\n  namespace: %s\n", namespace, namespace))
	err = (App{Config: c, Runner: r}).restoreRun(ctx, ctx, state, actor, pool, []byte("template"))
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("lost cancellation or uncertainty: %v", err)
	}
	commands := strings.Join(r.calls, "\n")
	if !strings.Contains(commands, "get atespace "+namespace) || strings.Contains(commands, " delete ") {
		t.Fatalf("uncertain creation was swept: %s", commands)
	}
}

func TestRestoreRunCancellationAtLifecycleBoundaries(t *testing.T) {
	for _, step := range []string{
		"create namespace", "create atespace", "create pool", "create actor-template", "create actor",
		"first call", "suspend actor", "second call", "delete actor", "delete actor-template",
		"delete pool", "delete atespace", "delete namespace",
	} {
		t.Run(step, func(t *testing.T) {
			c := testConfig(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			namespace, actor, token, err := restoreNames()
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if restoreTestReady(w, r) {
					return
				}
				count++
				if step == "first call" && count == 1 || step == "second call" && count == 2 {
					cancel()
				}
				fmt.Fprintf(w, "preserved memory count: %d | preserved file counter: %d | preserved rootfs counter: %d\n", count, count, count)
			}))
			defer server.Close()
			r := &restoreRunner{config: c, namespace: namespace, actor: actor, token: token, address: server.Listener.Addr().String(), cancel: cancel}
			switch step {
			case "create namespace":
				r.cancelAt = " create -f - --output=json"
			case "create atespace":
				r.cancelAt = " create atespace "
			case "create actor-template":
				r.cancelAt = " create actor-template "
			case "create actor":
				r.cancelAt = " create actor "
			case "suspend actor":
				r.cancelAt = " suspend actor "
			case "delete actor":
				r.cancelAt = " delete actor " + actor
			case "delete actor-template":
				r.cancelAt = " delete actor-template "
			case "delete pool":
				r.cancelAt = " delete --raw /apis/"
			case "delete atespace":
				r.cancelAt = " delete atespace "
			case "delete namespace":
				r.cancelAt = " delete --raw /api/v1/"
			case "create pool":
				r.cancelAt = "pool-create"
			}
			state := &restoreState{namespace: namespace, token: token, client: "/client", helper: &restoreHelper{binary: "/fixture"}}
			pool := []byte(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\napiVersion: ate.dev/v1\nkind: WorkerPool\nmetadata:\n  name: counter-microvm\n  namespace: %s\n", namespace, namespace))
			err = (App{Config: c, Runner: r}).restoreRun(ctx, ctx, state, actor, pool, []byte("template"))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if step != "first call" && step != "second call" && !r.failed {
				t.Fatalf("cancellation was not injected at %s", step)
			}
			if step != "create namespace" && len(r.calls) < 3 {
				t.Fatalf("cleanup was not attempted: %v", r.calls)
			}
		})
	}
}
