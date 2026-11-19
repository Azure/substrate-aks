package installer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

func restoreNames() (string, string, string, error) {
	var namespaceID, actorID [16]byte
	if _, err := rand.Read(namespaceID[:]); err != nil {
		return "", "", "", err
	}
	if _, err := rand.Read(actorID[:]); err != nil {
		return "", "", "", err
	}
	token := hex.EncodeToString(namespaceID[:])
	return "substrate-aks-check-" + token, "restore-" + hex.EncodeToString(actorID[:]), token, nil
}

func splitRestorePool(data []byte) ([]byte, []byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var namespace, pool []byte
	for {
		var doc object
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, nil, err
		}
		if len(doc) == 0 {
			continue
		}
		value, err := json.Marshal(doc)
		if err != nil {
			return nil, nil, err
		}
		switch doc["kind"] {
		case "Namespace":
			if namespace != nil {
				return nil, nil, errors.New("duplicate namespace")
			}
			namespace = value
		case "WorkerPool":
			if pool != nil {
				return nil, nil, errors.New("duplicate WorkerPool")
			}
			pool = value
		default:
			return nil, nil, errors.New("unexpected counter object")
		}
	}
	if namespace == nil || pool == nil {
		return nil, nil, errors.New("missing namespace or WorkerPool")
	}
	return namespace, pool, nil
}

func (a App) restoreWaitTemplate(ctx context.Context, state *restoreState) error {
	for {
		data, err := a.ate(ctx, state.client, nil, "get", "actor-template", restorePoolName, "-a", state.namespace, "-o", "json")
		if err != nil {
			return err
		}
		identity, err := restoreAPIObject(data, restorePoolName, state.namespace)
		if err != nil || identity.UID != state.template.UID {
			return errors.New("restore template identity changed")
		}
		var template struct {
			Status struct {
				Golden struct {
					GoldenTag struct {
						Name string `json:"name"`
					} `json:"goldenTag"`
					Error string `json:"errorMessage"`
				} `json:"goldenSnapshotStatus"`
			} `json:"status"`
		}
		if err := json.Unmarshal(data, &template); err != nil {
			return err
		}
		if template.Status.Golden.Error != "" {
			return fmt.Errorf("golden snapshot failed: %s", template.Status.Golden.Error)
		}
		if template.Status.Golden.GoldenTag.Name != "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (a App) restoreCheckPlacement(ctx context.Context, state *restoreState) error {
	data, err := a.ate(ctx, state.client, nil, "get", "actor", state.actor.Name, "-a", state.namespace, "-o", "json")
	if err != nil {
		return err
	}
	identity, err := restoreAPIObject(data, state.actor.Name, state.namespace)
	if err != nil || identity.UID != state.actor.UID {
		return errors.New("restore actor identity changed")
	}
	var actor struct {
		Status struct {
			WorkerAssignment struct {
				WorkerNamespace string `json:"workerNamespace"`
				WorkerPool      string `json:"workerPool"`
				WorkerPod       string `json:"workerPod"`
				WorkerPodUID    string `json:"workerPodUid"`
				Worker          struct {
					Name string `json:"name"`
				} `json:"worker"`
			} `json:"workerAssignment"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &actor); err != nil {
		return err
	}
	assignment := actor.Status.WorkerAssignment
	if assignment.WorkerNamespace != state.namespace || assignment.WorkerPool != restorePoolName || assignment.WorkerPod == "" || assignment.WorkerPodUID == "" || assignment.Worker.Name == "" {
		return errors.New("restore actor was not assigned to its isolated worker pool")
	}
	census, err := a.readRestoreCensus(ctx, state.helper, "workers")
	if err != nil {
		return err
	}
	found := false
	for _, worker := range census.Workers {
		if err := a.restoreWorkerOwned(state, worker); err != nil {
			return err
		}
		if worker.Status == nil {
			continue
		}
		epoch, epochErr := strconv.ParseInt(string(worker.Status.RegisteredEpoch), 10, 64)
		if worker.Metadata.Name == assignment.Worker.Name &&
			worker.WorkerNamespace == state.namespace && worker.WorkerPool == restorePoolName &&
			worker.WorkerPod == assignment.WorkerPod && worker.WorkerPodUID == assignment.WorkerPodUID &&
			worker.Labels[restoreInvocationLabel] == state.token &&
			worker.Labels[restoreInstallationLabel] == a.Config.Installation.ID &&
			worker.Status.State == "WORKER_STATE_ACTIVE" && epochErr == nil && epoch > 0 {
			found = true
		}
	}
	if found {
		return nil
	}
	return errors.New("restore actor worker registration could not be verified")
}

func (a App) restoreRun(ctx, createCtx context.Context, state *restoreState, actorName string, poolManifest, template []byte) (err error) {
	namespace, pool, err := splitRestorePool(poolManifest)
	if err != nil {
		return err
	}
	defer func() {
		cleanup := a.restoreCleanup(ctx, state)
		err = errors.Join(err, ctx.Err(), cleanup)
		if err != nil {
			receipt := a.restoreRecovery(state, actorName)
			a.progress("%s", receipt)
			if a.RecoveryReceipt != nil && createCtx.Err() == nil {
				if presentErr := a.RecoveryReceipt(createCtx, receipt); presentErr != nil {
					err = errors.Join(err, fmt.Errorf("presenting restore recovery after failure: %w", presentErr))
				}
			}
			err = fmt.Errorf("%w\n%s", err, receipt)
		} else {
			a.progress("RESTORE_PASSED")
		}
	}()
	a.progress("Creating isolated namespace and atespace")
	state.uncertainKind, state.uncertainName = "Namespace", state.namespace
	state.ns, err = a.restoreCreateKube(createCtx, namespace, "Namespace", state.namespace, "", state.token)
	if err != nil {
		return err
	}
	state.uncertainKind = ""
	a.progress("Created namespace %s UID %s", state.namespace, state.ns.UID)
	state.uncertainKind, state.uncertainName = "atespace", state.namespace
	state.space, err = a.restoreCreateAPI(createCtx, state.client, state.namespace, "", nil, "create", "atespace", state.namespace)
	if err != nil {
		return err
	}
	state.uncertainKind = ""
	a.progress("Created atespace %s UID %s", state.namespace, state.space.UID)
	state.uncertainKind, state.uncertainName = "WorkerPool", restorePoolName
	state.pool, err = a.restoreCreateKube(createCtx, pool, "WorkerPool", restorePoolName, state.namespace, state.token)
	if err != nil {
		return err
	}
	state.uncertainKind = ""
	a.progress("Created WorkerPool %s UID %s", restorePoolName, state.pool.UID)
	a.progress("Waiting for temporary workers")
	if _, err = a.kubectl(createCtx, nil, "wait", "deployment/"+restorePoolName, "--namespace="+state.namespace, "--for=create", "--timeout=600s"); err != nil {
		return err
	}
	if _, err = a.kubectl(createCtx, nil, "rollout", "status", "deployment/"+restorePoolName, "--namespace="+state.namespace, "--timeout=600s"); err != nil {
		return err
	}
	a.progress("Creating the counter template and waiting for its snapshot")
	state.uncertainKind, state.uncertainName = "actor-template", restorePoolName
	state.template, err = a.restoreCreateAPI(createCtx, state.client, restorePoolName, state.namespace, template, "create", "actor-template", "-f", "-")
	if err != nil {
		return err
	}
	state.uncertainKind = ""
	a.progress("Created actor template %s UID %s", restorePoolName, state.template.UID)
	if err = a.restoreWaitTemplate(createCtx, state); err != nil {
		return err
	}
	state.uncertainKind, state.uncertainName = "actor", actorName
	state.actor, err = a.restoreCreateAPI(createCtx, state.client, actorName, state.namespace, nil, "create", "actor", actorName, "-a", state.namespace, "--template", restorePoolName)
	if err != nil {
		return err
	}
	state.uncertainKind = ""
	a.progress("Created actor %s UID %s", actorName, state.actor.UID)
	callCtx, stopCall := context.WithTimeout(ctx, restoreCallTimeout)
	a.progress("Calling the temporary counter")
	state.proxy, err = a.startRestoreProxy(ctx, callCtx, state.client, state.namespace, actorName)
	if err == nil {
		err = a.restoreActorReady(callCtx, state.proxy.address)
	}
	if err == nil {
		err = restoreCounterCall(callCtx, state.proxy.address, 1)
	}
	if err == nil {
		err = a.restoreCheckPlacement(callCtx, state)
	}
	stopCall()
	if err != nil {
		return fmt.Errorf("initial counter phase: %w", err)
	}
	suspendCtx, stopSuspend := context.WithTimeout(ctx, restoreSuspendTimeout)
	a.progress("Suspending the counter with memory")
	_, err = a.ate(suspendCtx, state.client, nil, "suspend", "actor", actorName, "-a", state.namespace, "--mode=sync")
	stopSuspend()
	if err != nil {
		return err
	}
	callCtx, stopCall = context.WithTimeout(ctx, restoreCallTimeout)
	a.progress("Resuming and calling the counter")
	err = a.restoreActorReady(callCtx, state.proxy.address)
	if err == nil {
		err = restoreCounterCall(callCtx, state.proxy.address, 2)
	}
	if err == nil {
		err = a.restoreCheckPlacement(callCtx, state)
	}
	stopCall()
	if err != nil {
		return fmt.Errorf("resumed counter phase: %w", err)
	}
	return nil
}

func restoreQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func (a App) restoreRecovery(state *restoreState, actor string) string {
	k := "kubectl --kubeconfig " + restoreQuote(a.kubeconfig()) + " --context " + restoreQuote(a.Config.AKS.ClusterName)
	client := restoreQuote(state.client) + " --kubeconfig " + restoreQuote(a.kubeconfig()) + " --context " + restoreQuote(a.Config.AKS.ClusterName)
	ns := restoreQuote(state.namespace)
	lines := []string{
		fmt.Sprintf("Restore recovery: namespace/atespace %s, actor %s, invocation %s", state.namespace, actor, state.token),
		"Before each counter call, bounded readiness GETs may retry without incrementing; the two counter POSTs are not retried.",
		"A timed-out counter request may have executed, so the check fails rather than replaying it.",
		"Before deletion, compare each UID with the private diagnostic log and verify no foreign actors or objects are present.",
		fmt.Sprintf("%s get actors --all-atespaces -o json", client),
		fmt.Sprintf("%s get actor-templates --all-atespaces -o json", client),
		fmt.Sprintf("%s get workers -o json", client),
		fmt.Sprintf("%s get atespace %s -o json", client, ns),
		fmt.Sprintf("%s get actor %s -a %s -o json", client, restoreQuote(actor), ns),
		fmt.Sprintf("%s get actor-template %s -a %s -o json", client, restoreQuote(restorePoolName), ns),
		fmt.Sprintf("%s get workerpools.ate.dev %s --namespace=%s -o json", k, restoreQuote(restorePoolName), ns),
		fmt.Sprintf("%s get namespace %s -o json", k, ns),
		"Pod-usage metrics are excluded projections, not evidence of ownership or absence; inspect real objects.",
		"Automatic cleanup can tolerate a metrics-only discovery failure; this manual inventory may stop until metrics discovery is repaired.",
		fmt.Sprintf(`if RESOURCES=$(%s api-resources --verbs=list --namespaced=true -o name); then for RESOURCE in $RESOURCES; do case "$RESOURCE" in pods.metrics.k8s.io) continue ;; esac; %s get "$RESOURCE" --namespace=%s -o json || break; done; else printf 'API discovery failed\n'; fi`, k, k, ns),
		"After identity and placement verification, delete the actor, then the template; keep workers until both finish.",
		fmt.Sprintf("%s get atespace %s -o json", client, ns),
		fmt.Sprintf("%s get actor %s -a %s -o json", client, restoreQuote(actor), ns),
		fmt.Sprintf("%s delete actor %s -a %s", client, restoreQuote(actor), ns),
		fmt.Sprintf("%s get atespace %s -o json", client, ns),
		fmt.Sprintf("%s get actor-template %s -a %s -o json", client, restoreQuote(restorePoolName), ns),
		fmt.Sprintf("%s delete actor-template %s -a %s", client, restoreQuote(restorePoolName), ns),
		fmt.Sprintf("%s get actors --all-atespaces -o json", client),
		fmt.Sprintf("%s get actor-templates --all-atespaces -o json", client),
		fmt.Sprintf("%s get workers -o json", client),
		"Stop if the inventory is unreadable or foreign templates or placement remain unresolved; never delete foreign objects.",
		"Set POOL_UID and NS_UID to the verified original UIDs from the private log, then:",
		fmt.Sprintf(`printf '{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":"%%s"},"propagationPolicy":"Foreground"}' "$POOL_UID" | %s delete --raw %s -f -`, k, restoreQuote("/apis/ate.dev/v1/namespaces/"+state.namespace+"/workerpools/"+restorePoolName)),
		fmt.Sprintf("%s wait workerpools.ate.dev %s --namespace=%s --for=delete --timeout=300s", k, restoreQuote(restorePoolName), ns),
		fmt.Sprintf("%s get atespace %s -o json", client, ns),
		fmt.Sprintf("%s delete atespace %s", client, ns),
		fmt.Sprintf(`printf '{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":"%%s"},"propagationPolicy":"Foreground"}' "$NS_UID" | %s delete --raw %s -f -`, k, restoreQuote("/api/v1/namespaces/"+state.namespace)),
	}
	return strings.Join(lines, "\n")
}
