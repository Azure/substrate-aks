package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
)

type restoreWorkerIdentity struct {
	uid, pod, podUID string
}

func (a App) restoreWorkerOwned(state *restoreState, worker restoreWorker) error {
	if worker.Metadata.Name == "" || worker.Metadata.UID == "" || worker.Metadata.Atespace != "" ||
		worker.WorkerNamespace != state.namespace || worker.WorkerPool != restorePoolName ||
		worker.WorkerPod == "" || worker.WorkerPodUID == "" ||
		worker.Labels[restoreInvocationLabel] != state.token ||
		worker.Labels[restoreInstallationLabel] != a.Config.Installation.ID {
		return errors.New("worker ownership or placement is unsafe; refusing worker removal")
	}
	identity := restoreWorkerIdentity{uid: worker.Metadata.UID, pod: worker.WorkerPod, podUID: worker.WorkerPodUID}
	if state.workers == nil {
		state.workers = map[string]restoreWorkerIdentity{}
	}
	if old, exists := state.workers[worker.Metadata.Name]; exists && old != identity {
		return errors.New("worker identity changed; refusing worker removal")
	}
	state.workers[worker.Metadata.Name] = identity
	return nil
}

func restoreWorkerIdle(worker restoreWorker) bool {
	if worker.Status == nil {
		return false
	}
	switch worker.Status.State {
	case "WORKER_STATE_ACTIVE", "WORKER_STATE_DRAINING", "WORKER_STATE_UNAVAILABLE":
	default:
		return false
	}
	raw := bytes.TrimSpace(worker.Status.Allocated)
	if len(raw) == 0 {
		// The pinned protobuf treats an omitted allocation message as zero.
		// Source: b34fbee742906494b5eb4f1f36987176bf8770a4,
		// cmd/ateapi/internal/scheduling/scheduling.go:181-204.
		return true
	}
	if bytes.Equal(raw, []byte("null")) {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return false
	}
	for name, value := range fields {
		if name != "actors" && name != "cpuMillicores" && name != "memoryBytes" ||
			bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
		var number json.Number
		if err := json.Unmarshal(value, &number); err != nil {
			return false
		}
		n, err := strconv.ParseInt(string(number), 10, 64)
		if err != nil || n != 0 {
			return false
		}
	}
	return true
}

func (a App) restoreWorkersSafe(ctx context.Context, state *restoreState) ([]restoreWorker, error) {
	census, err := a.readRestoreCensus(ctx, state.helper, "full")
	if err != nil {
		return nil, err
	}
	if census.Actors {
		return nil, errors.New("actors exist; refusing worker removal")
	}
	if census.ActorTemplates {
		return nil, errors.New("actor templates exist; refusing worker removal")
	}
	for _, worker := range census.Workers {
		if err := a.restoreWorkerOwned(state, worker); err != nil {
			return nil, err
		}
		if !restoreWorkerIdle(worker) {
			return nil, errors.New("worker allocation is unsafe; refusing worker removal")
		}
	}
	return census.Workers, nil
}

func (a App) restoreVerifyWorkerPods(ctx context.Context, namespace string, workers []restoreWorker, owned map[string]bool) error {
	for _, worker := range workers {
		if !owned[worker.WorkerPodUID] {
			return errors.New("worker pod is not owned by the restore pool")
		}
		data, err := a.kubectl(ctx, nil, "get", "pod", worker.WorkerPod, "--namespace="+namespace, "--output=json")
		if err != nil {
			return err
		}
		var pod struct {
			Metadata struct {
				Name, Namespace, UID string
			} `json:"metadata"`
		}
		if err := json.Unmarshal(data, &pod); err != nil ||
			pod.Metadata.Name != worker.WorkerPod || pod.Metadata.Namespace != namespace || pod.Metadata.UID != worker.WorkerPodUID {
			return errors.New("worker pod identity changed; refusing worker removal")
		}
	}
	return nil
}
