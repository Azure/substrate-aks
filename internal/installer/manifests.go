package installer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/substrate-aks/deploy"
	"go.yaml.in/yaml/v3"
)

type object = map[string]any

func child(m object, key string) (object, error) {
	v, ok := m[key].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("upstream manifest: expected object at %s", key)
	}
	return v, nil
}

func nested(m object, keys ...string) (object, error) {
	var err error
	for _, key := range keys {
		m, err = child(m, key)
		if err != nil {
			return nil, err
		}
	}
	return m, nil
}

func named(items any, name string) (object, error) {
	list, ok := items.([]any)
	if !ok {
		return nil, errors.New("upstream manifest: expected a list")
	}
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			if m["name"] == name {
				return m, nil
			}
			if meta, ok := m["metadata"].(map[string]any); ok && meta["name"] == name {
				return m, nil
			}
		}
	}
	return nil, fmt.Errorf("upstream manifest: missing named entry %s", name)
}

// transformYAML is reserved for proto-shaped ActorTemplates and the separate
// sandbox asset adapter. Kubernetes deployment configuration lives in deploy/.
func transformYAML(data []byte, transform func(object) error) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	for {
		var doc object
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		if len(doc) == 0 {
			continue
		}
		if err := transform(doc); err != nil {
			return nil, err
		}
		if err := enc.Encode(doc); err != nil {
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// inspectYAML validates the upstream contract without mutating Kubernetes
// objects. In particular, strategic merge must not silently create a renamed
// upstream container or apply a positional patch to a different list entry.
func inspectYAML(data []byte, inspect func(object) error) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc object
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if len(doc) != 0 {
			if err := inspect(doc); err != nil {
				return err
			}
		}
	}
}

func adaptWorkload(data []byte, c Config, i Infrastructure, node bool) ([]byte, error) {
	v := deploy.Values{
		Component: "storage", Name: "ate-api-server", Kind: "Deployment", Mode: "control",
		PluginImage: c.Substrate.Images.Plugin, ClientID: i.ControlClientID,
		TenantID: i.TenantID, AccountURL: i.AccountURL,
		Snapshots: i.SnapshotsContainer, Assets: i.AssetsContainer,
	}
	if node {
		v.Name, v.Kind, v.Mode, v.ClientID = "atelet", "DaemonSet", "node", i.NodeClientID
	}
	found := false
	err := inspectYAML(data, func(doc object) error {
		meta, err := child(doc, "metadata")
		if err != nil {
			return err
		}
		if node && doc["kind"] == "ConfigMap" && meta["name"] == "substrate-aks-acr-credentials" {
			if meta["namespace"] != "ate-system" {
				return errors.New("Azure credential ConfigMap namespace changed")
			}
			v.CredentialConfigExists = true
		}
		if doc["kind"] != v.Kind || meta["name"] != v.Name {
			return nil
		}
		if found || meta["namespace"] != "ate-system" {
			return errors.New("upstream workload identity or namespace changed")
		}
		found = true
		pod, err := nested(doc, "spec", "template", "spec")
		if err != nil {
			return err
		}
		if pod["serviceAccountName"] != v.Name {
			return errors.New("upstream service account differs from the federated identity subject")
		}
		plugin, err := named(pod["initContainers"], "objectstore-plugin")
		if err != nil {
			return err
		}
		if plugin["restartPolicy"] != "Always" {
			return errors.New("upstream objectstore plugin is no longer a native sidecar")
		}
		if node {
			return checkActorImages(pod)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("upstream manifest missing %s/%s", v.Kind, v.Name)
	}
	return deploy.Render(data, v)
}

func checkActorImages(pod object) error {
	containers, ok := pod["containers"].([]any)
	if !ok || len(containers) == 0 {
		return errors.New("upstream atelet containers missing")
	}
	atelet, ok := containers[0].(map[string]any)
	if !ok || atelet["name"] != "atelet" {
		return errors.New("upstream first container is no longer atelet")
	}
	args, ok := atelet["args"].([]any)
	if !ok || len(args) < 2 {
		return errors.New("upstream atelet credential provider arguments missing")
	}
	for n, arg := range args {
		s, ok := arg.(string)
		if !ok || (n == 0 && !strings.HasPrefix(s, "--image-credential-provider-config=")) ||
			(n == 1 && !strings.HasPrefix(s, "--image-credential-provider-bin-dir=")) ||
			(n > 1 && strings.HasPrefix(s, "--image-credential-provider-")) {
			return errors.New("upstream atelet credential provider argument contract changed")
		}
	}
	for _, entry := range []struct{ name, mount string }{
		{"image-credential-provider-bin", "/run/image-credential-provider/bin"},
		{"image-credential-provider-config", "/run/image-credential-provider/config.yaml"},
	} {
		mount, err := named(atelet["volumeMounts"], entry.name)
		if err != nil {
			return err
		}
		if mount["mountPath"] != entry.mount {
			return errors.New("upstream credential provider mount path changed")
		}
		if _, err := named(pod["volumes"], entry.name); err != nil {
			return err
		}
	}
	return nil
}

func adaptPostgres(data []byte) ([]byte, error) {
	found := false
	err := inspectYAML(data, func(doc object) error {
		if doc["kind"] != "StatefulSet" {
			return nil
		}
		meta, err := child(doc, "metadata")
		if err != nil || found || meta["name"] != "postgres" || meta["namespace"] != "ate-system" {
			return errors.New("unexpected PostgreSQL StatefulSet")
		}
		found = true
		pod, err := nested(doc, "spec", "template", "spec")
		if err != nil {
			return err
		}
		_, err = named(pod["containers"], "postgres")
		return err
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("upstream bundled PostgreSQL StatefulSet missing")
	}
	return deploy.Render(data, deploy.Values{Component: "postgres"})
}

func adaptSource(root string, c Config, i Infrastructure) ([]string, error) {
	paths := []string{
		"manifests/ate-install/atelet.yaml",
		"manifests/ate-install/ate-api-server.yaml",
		"manifests/ate-install/postgres/postgres.yaml",
		"manifests/ate-install/ate-otel-config.yaml",
		"manifests/ate-install/base/kustomization.yaml",
		"demos/counter/counter-microvm.yaml.tmpl",
		"demos/counter/counter-microvm-template.yaml.tmpl",
	}
	// Render every surface before writing any of them, so contract failures
	// cannot leave a checkout containing a mixture of upstream and Azure files.
	rendered := make([][]byte, len(paths))
	for n, rel := range paths {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return nil, err
		}
		switch n {
		case 0, 1:
			data, err = adaptWorkload(data, c, i, n == 0)
		case 2:
			data, err = adaptPostgres(data)
		case 3:
			data, err = deploy.Render(data, deploy.Values{Component: "telemetry", MonitoringEnabled: c.Monitoring.Enabled})
		case 4:
			data, err = deploy.AdaptBase(data)
		case 5:
			data, err = adaptDemo(data)
		case 6:
			data, err = adaptDemoActorTemplate(data)
		}
		if err != nil {
			return nil, fmt.Errorf("adapting %s: %w", rel, err)
		}
		rendered[n] = data
	}
	for n, rel := range paths {
		if err := os.WriteFile(filepath.Join(root, rel), rendered[n], 0600); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

func adaptDemo(data []byte) ([]byte, error) {
	found := false
	v := deploy.Values{Component: "demo"}
	err := inspectYAML(data, func(doc object) error {
		if doc["kind"] != "WorkerPool" {
			return nil
		}
		meta, err := child(doc, "metadata")
		if err != nil || found || meta["name"] != "counter-microvm" || meta["namespace"] != "ate-demo-counter-microvm" {
			return errors.New("unexpected microVM demo WorkerPool")
		}
		found = true
		tmpl, err := nested(doc, "spec", "template")
		if err != nil {
			return err
		}
		if selector, exists := tmpl["nodeSelector"]; exists {
			if _, ok := selector.(map[string]any); !ok {
				return errors.New("upstream demo nodeSelector is not an object")
			}
			v.NodeSelectorExists = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("upstream microVM demo WorkerPool missing")
	}
	return deploy.Render(data, v)
}

// ActorTemplate is an ate API protobuf-shaped document, not a Kubernetes
// resource. Keep its deliberately narrow adaptation outside Kustomize.
func adaptDemoActorTemplate(data []byte) ([]byte, error) {
	found := false
	out, err := transformYAML(data, func(doc object) error {
		meta, err := child(doc, "metadata")
		if err != nil || meta["name"] != "counter-microvm" || meta["atespace"] != "ate-demo-counter-microvm" {
			return errors.New("unexpected microVM demo ActorTemplate")
		}
		snapshot, err := child(doc, "snapshotConfig")
		if err != nil {
			return err
		}
		if snapshot["storageLocation"] != "gs://${BUCKET_NAME}/ate-demo-counter-microvm/" {
			return errors.New("upstream microVM demo snapshot location changed; review the Azure adapter")
		}
		snapshot["storageLocation"] = "azblob://snapshots/ate-demo-counter-microvm/"
		found = true
		return nil
	})
	if err == nil && !found {
		return nil, errors.New("upstream microVM demo ActorTemplate missing")
	}
	return out, err
}
