package installer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// transformYAML is reserved for the sandbox asset adapter. Kubernetes
// deployment configuration lives in deploy/.
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

// The plugin's kustomize Components, relative to its repository root.
const (
	pluginComponent       = "config/kustomization.yaml"
	pluginAssetsComponent = "config/assets/kustomization.yaml"
)

var pluginValuePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9./:@_-]*$`)

type placeholder struct {
	token, value string
	count        int
}

// pluginComponents reads the pinned plugin's own kustomize Components and
// fills in its documented placeholders with this installation's values. Each
// placeholder must appear exactly as often as the reviewed plugin uses it, and
// none may remain, so a plugin layout change fails here rather than deploying
// half-configured sidecars.
func pluginComponents(dir string, c Config, i Infrastructure) ([]deploy.External, error) {
	if err := c.ValidateInstall(); err != nil {
		return nil, err
	}
	for rel, want := range map[string][]string{
		"config":        {"assets", "kustomization.yaml"},
		"config/assets": {"kustomization.yaml"},
	} {
		entries, err := os.ReadDir(filepath.Join(dir, rel))
		if err != nil {
			return nil, err
		}
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		if !slices.Equal(names, want) {
			return nil, fmt.Errorf("plugin %s holds %v, not the reviewed self-contained component; update the Azure adapter", rel, names)
		}
	}
	for _, value := range []string{c.Substrate.Images.Plugin, i.AccountURL, i.SnapshotsContainer, i.AssetsContainer, i.ControlClientID, i.NodeClientID, i.TenantID} {
		// Placeholders are filled textually, so values must stay plain YAML scalars.
		if !pluginValuePattern.MatchString(value) {
			return nil, fmt.Errorf("refusing to place %q in the plugin component", value)
		}
	}
	fill := func(rel string, placeholders []placeholder) ([]byte, error) {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return nil, err
		}
		text := string(data)
		for _, p := range placeholders {
			if n := strings.Count(text, p.token); n != p.count {
				return nil, fmt.Errorf("plugin %s has %d %s placeholders, want %d; update the Azure adapter", rel, n, p.token, p.count)
			}
			text = strings.ReplaceAll(text, p.token, p.value)
		}
		for _, leftover := range []string{"REPLACE_WITH", "replace-with-"} {
			if strings.Contains(text, leftover) {
				return nil, fmt.Errorf("plugin %s has an unfilled %s placeholder; update the Azure adapter", rel, leftover)
			}
		}
		return []byte(text), nil
	}
	component, err := fill(pluginComponent, []placeholder{
		{"example.invalid/replace-with-azure-blob-objectstore-plugin:latest", c.Substrate.Images.Plugin, 2},
		{"https://REPLACE_WITH_ACCOUNT.blob.core.windows.net", i.AccountURL, 2},
		{"REPLACE_WITH_CONTAINERS", i.SnapshotsContainer, 2},
		{"REPLACE_WITH_CONTROL_CLIENT_ID", i.ControlClientID, 1},
		{"REPLACE_WITH_NODE_CLIENT_ID", i.NodeClientID, 1},
		{"REPLACE_WITH_TENANT_ID", i.TenantID, 2},
	})
	if err != nil {
		return nil, err
	}
	assets, err := fill(pluginAssetsComponent, []placeholder{
		{"REPLACE_WITH_ASSET_CONTAINERS", i.AssetsContainer, 1},
	})
	if err != nil {
		return nil, err
	}
	return []deploy.External{{Path: "plugin", Kustomization: component}, {Path: "plugin/assets", Kustomization: assets}}, nil
}

func adaptWorkload(data []byte, c Config, plugin []deploy.External, node bool) ([]byte, error) {
	kind, name := "Deployment", "ate-api-server"
	v := deploy.Values{External: plugin}
	if node {
		kind, name, v.Component = "DaemonSet", "atelet", "actor-images"
		if d := c.Substrate.Atelet; d != nil && (d.SnapshotUploadDelay != nil || d.SnapshotDetachDelay != nil) {
			v.Sizing = &deploy.Sizing{}
			if d.SnapshotUploadDelay != nil {
				v.Sizing.UploadDelay = *d.SnapshotUploadDelay
			}
			if d.SnapshotDetachDelay != nil {
				v.Sizing.DetachDelay = *d.SnapshotDetachDelay
			}
		}
	} else {
		v.Sizing = deploymentSizing(c.Substrate, name)
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
		if doc["kind"] != kind || meta["name"] != name {
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
		if pod["serviceAccountName"] != name {
			return errors.New("upstream service account differs from the federated identity subject")
		}
		plugin, err := named(pod["initContainers"], "objectstore-plugin")
		if err != nil {
			return err
		}
		if plugin["restartPolicy"] != "Always" {
			return errors.New("upstream objectstore plugin is no longer a native sidecar")
		}
		// The plugin component adds these; upstream copies would merge with them.
		env, _ := plugin["env"].([]any)
		for _, entry := range env {
			if e, ok := entry.(map[string]any); ok && strings.HasPrefix(fmt.Sprint(e["name"]), "AZURE_") {
				return errors.New("upstream objectstore plugin already sets Azure environment; review the Azure adapter")
			}
		}
		if _, err := named(pod["volumes"], "azure-identity-token"); err == nil {
			return errors.New("upstream already defines an azure-identity-token volume; review the Azure adapter")
		}
		if node {
			if v.Sizing != nil {
				atelet, err := named(pod["containers"], "atelet")
				if err != nil {
					return err
				}
				args, ok := atelet["args"].([]any)
				if !ok {
					return errors.New("upstream atelet arguments missing")
				}
				for _, arg := range args {
					s, ok := arg.(string)
					if !ok || strings.HasPrefix(s, "--snapshot-upload-delay") || strings.HasPrefix(s, "--snapshot-detach-delay") {
						return errors.New("upstream atelet snapshot delay argument contract changed")
					}
				}
			}
			return checkActorImages(pod)
		}
		if v.Sizing != nil {
			if _, err := named(pod["containers"], name); err != nil {
				return err
			}
			v.Sizing.ContainerOrder, err = containerOrder(pod)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("upstream manifest missing %s/%s", kind, name)
	}
	rendered, err := deploy.Render(data, v)
	if err != nil {
		return nil, err
	}
	// The plugin component selects workloads by label; confirm it reached this one.
	err = inspectYAML(rendered, func(doc object) error {
		meta, err := child(doc, "metadata")
		if err != nil || doc["kind"] != kind || meta["name"] != name {
			return err
		}
		pod, err := nested(doc, "spec", "template", "spec")
		if err != nil {
			return err
		}
		sidecar, err := named(pod["initContainers"], "objectstore-plugin")
		if err != nil {
			return err
		}
		if sidecar["image"] != c.Substrate.Images.Plugin {
			return fmt.Errorf("the Azure Blob plugin component did not configure %s/%s", kind, name)
		}
		return nil
	})
	return rendered, err
}

func containerOrder(pod object) ([]string, error) {
	containers, ok := pod["containers"].([]any)
	if !ok {
		return nil, errors.New("upstream workload containers missing")
	}
	names := make([]string, 0, len(containers))
	for _, entry := range containers {
		container, ok := entry.(map[string]any)
		name, named := container["name"].(string)
		if !ok || !named || name == "" || slices.Contains(names, name) {
			return nil, errors.New("upstream workload has invalid or duplicate container names")
		}
		names = append(names, name)
	}
	return names, nil
}

func deploymentSizing(s SubstrateConfig, name string) *deploy.Sizing {
	for _, workload := range s.sizedWorkloads() {
		if workload.name != name {
			continue
		}
		containers := make(map[string]*deploy.Resources)
		for _, container := range workload.containers {
			r := container.resources
			if r != nil && (len(r.Requests) != 0 || len(r.Limits) != 0) {
				containers[container.name] = &deploy.Resources{Requests: r.Requests, Limits: r.Limits}
			}
		}
		if workload.replicas != nil || len(containers) != 0 {
			return &deploy.Sizing{Workload: name, Replicas: workload.replicas, Containers: containers}
		}
	}
	return nil
}

func adaptRouter(data []byte, c Config) ([]byte, error) {
	sizing := deploymentSizing(c.Substrate, "atenet-router")
	if sizing == nil {
		return data, nil
	}
	found := false
	var order []string
	err := inspectYAML(data, func(doc object) error {
		meta, err := child(doc, "metadata")
		if err != nil {
			return err
		}
		if doc["kind"] != "Deployment" || meta["name"] != "atenet-router" {
			return nil
		}
		if found || meta["namespace"] != "ate-system" {
			return errors.New("upstream router identity or namespace changed")
		}
		found = true
		pod, err := nested(doc, "spec", "template", "spec")
		if err != nil {
			return err
		}
		for name := range sizing.Containers {
			if _, err := named(pod["containers"], name); err != nil {
				return err
			}
		}
		order, err = containerOrder(pod)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("upstream router Deployment missing")
	}
	sizing.ContainerOrder = order
	return deploy.Render(data, deploy.Values{Sizing: sizing})
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

var adaptedSourcePaths = []string{
	"manifests/ate-install/atelet.yaml",
	"manifests/ate-install/ate-api-server.yaml",
	"manifests/ate-install/postgres/postgres.yaml",
	"manifests/ate-install/ate-otel-config.yaml",
	"manifests/ate-install/base/kustomization.yaml",
	"demos/counter/counter-microvm.yaml.tmpl",
	"manifests/ate-install/atenet-router.yaml",
}

func adaptSource(root string, c Config, plugin []deploy.External) ([]string, error) {
	paths := adaptedSourcePaths
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
			data, err = adaptWorkload(data, c, plugin, n == 0)
		case 2:
			data, err = adaptPostgres(data)
		case 3:
			data, err = deploy.Render(data, deploy.Values{Component: "telemetry"})
		case 4:
			data, err = deploy.AdaptBase(data)
		case 5:
			data, err = adaptDemo(data)
		case 6:
			data, err = adaptRouter(data, c)
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
