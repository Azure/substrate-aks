package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type restoreWorker struct {
	Metadata        restoreAPIIdentity `json:"metadata"`
	WorkerNamespace string             `json:"workerNamespace"`
	WorkerPool      string             `json:"workerPool"`
	WorkerPod       string             `json:"workerPod"`
	WorkerPodUID    string             `json:"workerPodUid"`
	Labels          map[string]string  `json:"labels"`
	Status          *struct {
		State           string          `json:"state"`
		RegisteredEpoch json.Number     `json:"registeredEpoch"`
		Allocated       json.RawMessage `json:"allocated"`
	} `json:"status"`
}

type restoreCensus struct {
	Workers        []restoreWorker
	Actors         bool
	ActorTemplates bool
}

func restoreCensusResult(data []byte) (restoreCensus, error) {
	var result restoreCensus
	if len(data) == 0 || len(data) > restoreInventoryBytes {
		return result, errors.New("invalid restore census output size")
	}
	var envelope struct {
		Workers        *json.RawMessage `json:"workers"`
		Actors         *bool            `json:"actors"`
		ActorTemplates *bool            `json:"actorTemplates"`
	}
	if err := decodeRestoreResult(data, &envelope); err != nil || envelope.Workers == nil || envelope.Actors == nil || envelope.ActorTemplates == nil {
		return result, errors.New("invalid or incomplete restore census")
	}
	if err := json.Unmarshal(*envelope.Workers, &result.Workers); err != nil || result.Workers == nil || len(result.Workers) > restoreInventoryItems {
		return result, errors.New("invalid restore worker census")
	}
	result.Actors, result.ActorTemplates = *envelope.Actors, *envelope.ActorTemplates
	return result, nil
}

func (a App) readRestoreCensus(ctx context.Context, helper *restoreHelper, mode string) (restoreCensus, error) {
	if helper == nil || helper.binary == "" || mode != "workers" && mode != "full" {
		return restoreCensus{}, errors.New("PREREQUISITE: bounded restore census unavailable")
	}
	data, err := a.Runner.Run(ctx, Command{
		Dir: helper.root,
		Args: []string{helper.binary, "publish", "restore-census", a.kubeconfig(), a.Config.AKS.ClusterName,
			mode, "--no-dev-env", "--no-report-defaults"},
		Env: []string{"NO_DEV_ENV=1"}, MaxOutput: restoreInventoryBytes,
	})
	if err != nil {
		return restoreCensus{}, fmt.Errorf("reading bounded restore census: %w", err)
	}
	result, err := restoreCensusResult(data)
	if err != nil {
		return result, err
	}
	seen := map[string]string{}
	seenUID := map[string]bool{}
	for _, worker := range result.Workers {
		if worker.Metadata.Name == "" || worker.Metadata.UID == "" || worker.Metadata.Atespace != "" ||
			strings.TrimSpace(worker.WorkerPod) == "" || strings.TrimSpace(worker.WorkerPodUID) == "" {
			return restoreCensus{}, errors.New("invalid restore worker census identity")
		}
		if _, ok := seen[worker.Metadata.Name]; ok {
			return restoreCensus{}, errors.New("duplicate restore worker census identity")
		}
		if seenUID[worker.Metadata.UID] {
			return restoreCensus{}, errors.New("duplicate restore worker census UID")
		}
		seen[worker.Metadata.Name] = worker.Metadata.UID
		seenUID[worker.Metadata.UID] = true
	}
	return result, nil
}
