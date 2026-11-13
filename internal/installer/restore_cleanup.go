package installer

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type restoreState struct {
	namespace     string
	token         string
	client        string
	helper        *restoreHelper
	ns            restoreKubeIdentity
	pool          restoreKubeIdentity
	space         restoreAPIIdentity
	template      restoreAPIIdentity
	actor         restoreAPIIdentity
	proxy         *restoreProxy
	workers       map[string]restoreWorkerIdentity
	uncertainKind string
	uncertainName string
}

func (a App) restoreCleanup(ctx context.Context, state *restoreState) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreCleanupTimeout)
	defer cancel()
	return a.restoreCleanupWithin(ctx, state)
}

func (a App) restoreCleanupWithin(ctx context.Context, state *restoreState) (resultErr error) {
	a.progress("Cleaning up the temporary restore check")
	joined, proxyErr := state.proxy.stop(ctx)
	if !joined {
		return proxyErr
	}
	defer func() { resultErr = errors.Join(proxyErr, resultErr) }()
	if state.uncertainKind != "" {
		var queryErr error
		switch state.uncertainKind {
		case "Namespace":
			_, queryErr = a.kubectl(ctx, nil, "get", "namespace", state.uncertainName, "--ignore-not-found", "--output=json")
		case "WorkerPool":
			_, queryErr = a.kubectl(ctx, nil, "get", "workerpools.ate.dev", state.uncertainName, "--namespace="+state.namespace, "--ignore-not-found", "--output=json")
		case "atespace":
			_, queryErr = a.ate(ctx, state.client, nil, "get", "atespace", state.uncertainName, "-o", "json")
		case "actor", "actor-template":
			_, queryErr = a.ate(ctx, state.client, nil, "get", state.uncertainKind, state.uncertainName, "-a", state.namespace, "-o", "json")
		}
		return errors.Join(errors.New("creation outcome is uncertain; retaining resources for manual recovery"), queryErr)
	}
	if state.actor.UID != "" {
		if err := a.restoreDeleteAPI(ctx, state.client, "actor", state.space, state.actor); err != nil {
			return fmt.Errorf("deleting restore actor: %w", err)
		}
	}
	if state.template.UID != "" {
		if err := a.restoreDeleteAPI(ctx, state.client, "actor-template", state.space, state.template); err != nil {
			return fmt.Errorf("deleting restore template: %w", err)
		}
	}
	var namespaceObjects map[string]bool
	if state.ns.UID != "" {
		var workers []restoreWorker
		if state.pool.UID != "" {
			var err error
			workers, err = a.restoreWorkersSafe(ctx, state)
			if err != nil {
				return err
			}
		}
		var err error
		namespaceObjects, err = a.restoreNamespaceInventory(ctx, state.helper, state.namespace, state.pool.UID, nil)
		if err != nil {
			return err
		}
		if err := a.restoreVerifyWorkerPods(ctx, state.namespace, workers, namespaceObjects); err != nil {
			return err
		}
	}
	if state.pool.UID != "" {
		path := "/apis/ate.dev/v1/namespaces/" + state.namespace + "/workerpools/" + restorePoolName
		if err := a.restoreDeleteKube(ctx, "workerpools.ate.dev", path, "WorkerPool", state.pool, state.token); err != nil {
			return fmt.Errorf("deleting restore WorkerPool: %w", err)
		}
		if _, err := a.kubectl(ctx, nil, "wait", "workerpools.ate.dev", restorePoolName, "--namespace="+state.namespace, "--for=delete", "--timeout=300s"); err != nil {
			return fmt.Errorf("waiting for restore WorkerPool deletion: %w", err)
		}
		for {
			census, err := a.readRestoreCensus(ctx, state.helper, "workers")
			if err != nil {
				return err
			}
			if len(census.Workers) == 0 {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	if state.space.UID != "" {
		if err := a.restoreCheckAPIIdentity(ctx, state.client, "atespace", state.space); err != nil {
			return err
		}
		if _, err := a.ate(ctx, state.client, nil, "delete", "atespace", state.space.Name); err != nil {
			return fmt.Errorf("deleting empty restore atespace: %w", err)
		}
	}
	if state.ns.UID != "" {
		if _, err := a.restoreNamespaceInventory(ctx, state.helper, state.namespace, "", namespaceObjects); err != nil {
			return err
		}
		if err := a.restoreDeleteKube(ctx, "namespace", "/api/v1/namespaces/"+state.namespace, "Namespace", state.ns, state.token); err != nil {
			return fmt.Errorf("deleting restore namespace: %w", err)
		}
		if _, err := a.kubectl(ctx, nil, "wait", "namespace", state.namespace, "--for=delete", "--timeout=300s"); err != nil {
			return fmt.Errorf("waiting for restore namespace deletion: %w", err)
		}
	}
	return nil
}
