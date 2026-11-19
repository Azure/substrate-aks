package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	restoreReadyTimeout   = 10 * time.Minute
	restoreCallTimeout    = 2 * time.Minute
	restoreSuspendTimeout = 5 * time.Minute
	restoreCleanupTimeout = 10 * time.Minute
	restoreInventoryBytes = 8 << 20
	restoreInventoryItems = 10000
)

func poolListIncomplete(data []byte) (bool, error) {
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(data, &metadata); err != nil || metadata == nil {
		return false, errors.New("invalid WorkerPool list metadata")
	}
	incomplete := false
	if raw, ok := metadata["continue"]; ok {
		var value string
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return false, errors.New("invalid WorkerPool continuation")
		}
		incomplete = value != ""
	}
	if raw, ok := metadata["remainingItemCount"]; ok {
		var value int64
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil || value < 0 {
			return false, errors.New("invalid WorkerPool remaining count")
		}
		incomplete = incomplete || value != 0
	}
	return incomplete, nil
}

func (a App) ate(ctx context.Context, client string, input []byte, args ...string) ([]byte, error) {
	command := []string{client, "--kubeconfig", a.kubeconfig(), "--context", a.Config.AKS.ClusterName}
	command = append(command, args...)
	return a.Runner.Run(ctx, Command{Args: command, Input: input, MaxOutput: restoreInventoryBytes})
}

func uniqueInventoryJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 128 {
			return errors.New("inventory JSON nesting exceeds limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return errors.New("duplicate or invalid inventory field")
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("invalid inventory object")
			}
		case json.Delim('['):
			for decoder.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("invalid inventory array")
			}
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("inventory JSON has trailing data")
	}
	return nil
}

func workerPoolPage(data []byte) (bool, error) {
	if len(data) == 0 || len(data) > restoreInventoryBytes {
		return false, errors.New("inventory output is empty or exceeds the size limit")
	}
	if err := uniqueInventoryJSON(data); err != nil {
		return false, fmt.Errorf("invalid inventory JSON: %w", err)
	}
	var result map[string]json.RawMessage
	var kind, apiVersion string
	if err := json.Unmarshal(data, &result); err != nil ||
		json.Unmarshal(result["kind"], &kind) != nil || kind != "WorkerPoolList" ||
		json.Unmarshal(result["apiVersion"], &apiVersion) != nil || apiVersion != "ate.dev/v1" {
		return false, errors.New("invalid WorkerPool list kind or API version")
	}
	incomplete := false
	if metadata, ok := result["metadata"]; ok {
		var err error
		incomplete, err = poolListIncomplete(metadata)
		if err != nil {
			return false, err
		}
	}
	items, ok := result["items"]
	if !ok {
		return false, errors.New("WorkerPool inventory is missing items")
	}
	if bytes.Equal(bytes.TrimSpace(items), []byte("null")) {
		return false, errors.New("inventory list is null")
	}
	list := json.NewDecoder(bytes.NewReader(items))
	if start, err := list.Token(); err != nil || start != json.Delim('[') {
		return false, errors.New("invalid inventory list")
	}
	busy := list.More()
	if busy {
		var entry json.RawMessage
		if err := list.Decode(&entry); err != nil || list.More() {
			return false, errors.New("invalid or oversized inventory list")
		}
		var object struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
				UID       string `json:"uid"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(entry, &object); err != nil || object.Metadata.Name == "" || object.Metadata.UID == "" ||
			object.Metadata.Namespace == "" {
			return false, errors.New("invalid inventory entry")
		}
	}
	if end, err := list.Token(); err != nil || end != json.Delim(']') {
		return false, errors.New("incomplete inventory list")
	}
	if incomplete && !busy {
		return false, errors.New("incomplete WorkerPool inventory")
	}
	return busy, nil
}

func restoreInventoryResult(data []byte) (bool, error) {
	if len(data) == 0 || len(data) > restoreInventoryBytes {
		return false, errors.New("invalid restore inventory result size")
	}
	var result struct {
		Workers        *bool `json:"workers"`
		Actors         *bool `json:"actors"`
		ActorTemplates *bool `json:"actorTemplates"`
	}
	if err := decodeRestoreResult(data, &result); err != nil || result.Workers == nil || result.Actors == nil || result.ActorTemplates == nil {
		return false, errors.New("invalid or incomplete restore inventory result")
	}
	return *result.Workers || *result.Actors || *result.ActorTemplates, nil
}

func decodeRestoreResult(data []byte, target any) error {
	if err := uniqueInventoryJSON(data); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 3 ||
		fields["workers"] == nil || fields["actors"] == nil || fields["actorTemplates"] == nil {
		return errors.New("invalid restore result fields")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func (a App) restorePoolBusy(ctx context.Context, helper *restoreHelper) (bool, error) {
	pools, err := a.Runner.Run(ctx, Command{
		Dir: helper.root,
		Args: []string{helper.binary, "publish", "restore-worker-pools", a.kubeconfig(), a.Config.AKS.ClusterName,
			"--no-dev-env", "--no-report-defaults"},
		Env:       []string{"NO_DEV_ENV=1"},
		MaxOutput: restoreInventoryBytes,
	})
	if err != nil {
		return false, fmt.Errorf("listing WorkerPools: %w", err)
	}
	busy, err := workerPoolPage(pools)
	if err != nil {
		return false, fmt.Errorf("reading WorkerPool inventory: %w", err)
	}
	return busy, nil
}

type restoreHelper struct {
	root, binary string
}

func (h *restoreHelper) close() error {
	if h == nil {
		return nil
	}
	return os.RemoveAll(h.root)
}

func (a App) restoreBusy(ctx context.Context, client string) (_ bool, helper *restoreHelper, resultErr error) {
	if client == "" {
		return false, nil, errors.New("PREREQUISITE: matching client is unavailable")
	}
	if err := os.MkdirAll(a.Config.workspace(), 0700); err != nil {
		return false, nil, err
	}
	root, err := os.MkdirTemp(a.Config.workspace(), "restore-inventory-")
	if err != nil {
		return false, nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.RemoveAll(root))
		}
	}()
	if err := a.fetchPinnedSource(ctx, root); err != nil {
		return false, nil, fmt.Errorf("PREREQUISITE: preparing bounded restore inventory: %w", err)
	}
	if err := prepareHelper(root); err != nil {
		return false, nil, fmt.Errorf("PREREQUISITE: preparing bounded restore inventory: %w", err)
	}
	binary := filepath.Join(root, "restore-inventory")
	if _, err := a.Runner.Run(ctx, Command{
		Dir: root, Args: []string{"go", "build", "-o", binary, "./cmd/ate-setup"},
		Env: []string{"NO_DEV_ENV=1"}, MaxOutput: restoreInventoryBytes,
	}); err != nil {
		return false, nil, fmt.Errorf("PREREQUISITE: building bounded restore inventory: %w", err)
	}
	helper = &restoreHelper{root: root, binary: binary}
	busy, err := a.restorePoolBusy(ctx, helper)
	if err != nil {
		return false, nil, err
	}
	data, err := a.Runner.Run(ctx, Command{
		Dir: root,
		Args: []string{binary, "publish", "restore-inventory",
			a.kubeconfig(), a.Config.AKS.ClusterName, "--no-dev-env", "--no-report-defaults"},
		Env: []string{"NO_DEV_ENV=1"}, MaxOutput: restoreInventoryBytes,
	})
	if err != nil {
		return false, nil, fmt.Errorf("listing bounded restore inventory: %w", err)
	}
	hasEntries, err := restoreInventoryResult(data)
	if err != nil {
		return false, nil, fmt.Errorf("reading bounded restore inventory: %w", err)
	}
	return busy || hasEntries, helper, nil
}

// restoreInstalled distinguishes a clean absence from a partial or foreign
// installation without inferring absence from an API error.
func (a App) restoreInstalled(ctx context.Context, i Infrastructure) (bool, error) {
	namespace, err := a.kubectl(ctx, nil, "get", "namespace", "ate-system", "--ignore-not-found", "--output=json")
	if err != nil {
		return false, fmt.Errorf("reading Substrate namespace: %w", err)
	}
	if strings.TrimSpace(string(namespace)) == "" {
		return false, nil
	}
	var ns struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(namespace, &ns); err != nil || ns.Metadata.Name != "ate-system" {
		return false, errors.New("invalid Substrate namespace response")
	}

	data, err := a.kubectl(ctx, nil, "get", "configmap", "substrate-aks-install", "--namespace=ate-system", "--ignore-not-found", "--output=json")
	if err != nil {
		return false, fmt.Errorf("reading Substrate installation record: %w", err)
	}
	var record struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	hasRecord := strings.TrimSpace(string(data)) != ""
	if hasRecord {
		if err := json.Unmarshal(data, &record); err != nil ||
			record.Metadata.Name != "substrate-aks-install" || record.Metadata.Namespace != "ate-system" ||
			record.Data["installationId"] == "" || record.Data["fingerprint"] == "" || record.Data["phase"] == "" {
			return false, errors.New("invalid Substrate installation record")
		}
		if record.Data["installationId"] != a.Config.Installation.ID || record.Data["fingerprint"] != fingerprint(a.Config, i) {
			return false, errors.New("installed source/images/configuration differ; upgrades are not implemented")
		}
		if record.Data["phase"] != "deployed" {
			return false, fmt.Errorf("PREREQUISITE: installation phase is %q; run setup to reconcile", record.Data["phase"])
		}
	}

	data, err = a.kubectl(ctx, nil, "get", "deployment", "ate-api-server", "--namespace=ate-system", "--ignore-not-found", "--output=json")
	if err != nil {
		return false, fmt.Errorf("reading Substrate deployment: %w", err)
	}
	hasDeployment := strings.TrimSpace(string(data)) != ""
	if hasDeployment {
		var deployment struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(data, &deployment); err != nil ||
			deployment.Metadata.Name != "ate-api-server" || deployment.Metadata.Namespace != "ate-system" {
			return false, errors.New("invalid Substrate deployment response")
		}
	}
	if hasRecord != hasDeployment {
		return false, errors.New("PREREQUISITE: installation record and deployment are inconsistent; run setup to reconcile")
	}
	return hasRecord, nil
}
