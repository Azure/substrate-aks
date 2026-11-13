package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const restoreNamespaceResources = 1024

var restoreResourceName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

type restoreDiscoveryDiagnostics struct{ app App }

func (w restoreDiscoveryDiagnostics) Write(p []byte) (int, error) {
	if w.app.Progress != nil {
		w.app.Progress("%s", strings.TrimSuffix(string(p), "\n"))
	}
	return len(p), nil
}

type restoreNamespaceObject struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name            string `json:"name"`
		Namespace       string `json:"namespace"`
		UID             string `json:"uid"`
		OwnerReferences []struct {
			UID string `json:"uid"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	InvolvedObject struct {
		UID string `json:"uid"`
	} `json:"involvedObject"`
	Regarding struct {
		UID string `json:"uid"`
	} `json:"regarding"`
}

func restoreNamespaceOwned(objects []restoreNamespaceObject, poolUID string, retained map[string]bool) (map[string]bool, error) {
	owned := make(map[string]bool, len(retained)+len(objects)+1)
	for id := range retained {
		owned[id] = true
	}
	if poolUID != "" {
		owned[poolUID] = true
	}
	pending := make([]restoreNamespaceObject, 0, len(objects))
	for _, obj := range objects {
		id := obj.Metadata.UID
		if id == "" || obj.Metadata.Name == "" {
			return nil, errors.New("namespace inventory has an object without identity")
		}
		switch {
		case obj.Kind == "ServiceAccount" && (obj.Metadata.Name == "default" || obj.Metadata.Name == "ateworker"):
			// The pinned controller creates ateworker; Kubernetes creates default.
			// Source: b34fbee742906494b5eb4f1f36987176bf8770a4,
			// internal/installdefaults/installdefaults.go:50-52.
			key := "base:" + obj.Kind + "/" + obj.Metadata.Name + "/" + id
			if retained != nil && !retained[key] {
				return nil, errors.New("namespace service account identity changed")
			}
			owned[id], owned[key] = true, true
		case obj.Kind == "ConfigMap" && obj.Metadata.Name == "kube-root-ca.crt":
			key := "base:" + obj.Kind + "/" + obj.Metadata.Name + "/" + id
			if retained != nil && !retained[key] {
				return nil, errors.New("namespace trust ConfigMap identity changed")
			}
			owned[id], owned[key] = true, true
		case owned[id]:
		default:
			pending = append(pending, obj)
		}
	}
	for len(pending) > 0 {
		before := len(pending)
		remaining := pending[:0]
		for _, obj := range pending {
			matched := len(obj.Metadata.OwnerReferences) > 0
			for _, ref := range obj.Metadata.OwnerReferences {
				matched = matched && ref.UID != "" && owned[ref.UID]
			}
			if obj.Kind == "Event" {
				matched = (len(obj.Metadata.OwnerReferences) == 0 || matched) &&
					(owned[obj.InvolvedObject.UID] || owned[obj.Regarding.UID])
			}
			if matched {
				owned[obj.Metadata.UID] = true
			} else {
				remaining = append(remaining, obj)
			}
		}
		pending = remaining
		if len(pending) == before {
			return nil, fmt.Errorf("unexpected namespace object %s/%s; refusing namespace teardown", pending[0].Kind, pending[0].Metadata.Name)
		}
	}
	return owned, nil
}

func (a App) restoreNamespaceInventory(ctx context.Context, helper *restoreHelper, namespace, poolUID string, retained map[string]bool) (map[string]bool, error) {
	if helper == nil || helper.binary == "" {
		return nil, errors.New("PREREQUISITE: namespace discovery unavailable")
	}
	data, err := a.Runner.Run(ctx, Command{Dir: helper.root, Args: []string{helper.binary, "publish", "restore-namespace-resources",
		a.kubeconfig(), a.Config.AKS.ClusterName, "--no-dev-env", "--no-report-defaults"},
		Env: []string{"NO_DEV_ENV=1"}, MaxOutput: restoreInventoryBytes, StreamStderr: restoreDiscoveryDiagnostics{app: a}})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) > restoreInventoryBytes || !strings.HasSuffix(string(data), "\n") {
		return nil, errors.New("incomplete or oversized namespace discovery")
	}
	resources := strings.Fields(string(data))
	if len(resources) == 0 || len(resources) > restoreNamespaceResources {
		return nil, errors.New("namespace API discovery is empty or exceeds the resource limit")
	}
	var objects []restoreNamespaceObject
	seen := map[string]bool{}
	total := 0
	for _, resource := range resources {
		if !restoreResourceName.MatchString(resource) {
			return nil, errors.New("invalid namespace API resource name")
		}
		// Metrics-server v0.8.0 exposes pod usage as a projection, not ownership
		// identity: pkg/storage/pod.go.
		if resource == "pods.metrics.k8s.io" {
			continue
		}
		if seen[resource] {
			continue
		}
		seen[resource] = true
		data, err := a.Runner.Run(ctx, Command{Args: []string{"kubectl", "--kubeconfig", a.kubeconfig(), "--context", a.Config.AKS.ClusterName,
			"get", resource, "--namespace=" + namespace, "--output=json"}, MaxOutput: restoreInventoryBytes})
		if err != nil {
			return nil, fmt.Errorf("listing namespace resource %s: %w", resource, err)
		}
		total += len(data)
		if total > 64<<20 {
			return nil, errors.New("namespace inventory exceeds total size limit")
		}
		var list struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Continue  string `json:"continue"`
				Remaining *int   `json:"remainingItemCount"`
			} `json:"metadata"`
			Items json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(data, &list); err != nil || !strings.HasSuffix(list.Kind, "List") ||
			list.Metadata.Continue != "" || list.Metadata.Remaining != nil && *list.Metadata.Remaining != 0 ||
			len(list.Items) == 0 || string(list.Items) == "null" {
			return nil, fmt.Errorf("invalid or incomplete inventory of %s", resource)
		}
		var items []restoreNamespaceObject
		if err := json.Unmarshal(list.Items, &items); err != nil || len(items)+len(objects) > restoreInventoryItems {
			return nil, errors.New("invalid or oversized namespace inventory")
		}
		for i := range items {
			if items[i].Kind == "" {
				items[i].Kind = strings.TrimSuffix(list.Kind, "List")
			}
			if items[i].Metadata.Namespace != namespace || items[i].Kind == "" {
				return nil, errors.New("namespace inventory contains an invalid object")
			}
		}
		objects = append(objects, items...)
	}
	if len(seen) == 0 {
		return nil, errors.New("namespace API discovery has no inventory resources")
	}
	return restoreNamespaceOwned(objects, poolUID, retained)
}
