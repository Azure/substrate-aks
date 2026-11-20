// Package deploy embeds the Azure configuration applied to the pinned upstream
// manifests. Rendering is in-memory; it never invokes kubectl or other programs.
package deploy

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"text/template"

	"sigs.k8s.io/kustomize/api/filters/patchjson6902"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

//go:embed kustomization.yaml.tmpl base-monitoring.patch.yaml components database-bootstrap.yaml
var manifests embed.FS

func DatabaseBootstrap() ([]byte, error) {
	return manifests.ReadFile("database-bootstrap.yaml")
}

// AdaptBase removes the pinned GKE monitoring reference without building the
// upstream base or moving its other resources away from their original paths.
func AdaptBase(source []byte) ([]byte, error) {
	patch, err := manifests.ReadFile("base-monitoring.patch.yaml")
	if err != nil {
		return nil, err
	}
	node, err := yaml.Parse(string(source))
	if err != nil {
		return nil, err
	}
	if _, err := (patchjson6902.Filter{Patch: string(patch)}).Filter([]*yaml.RNode{node}); err != nil {
		return nil, fmt.Errorf("upstream GKE monitoring resource changed; review the Azure adapter: %w", err)
	}
	result, err := node.String()
	return []byte(result), err
}

// External is a kustomize Component supplied by the caller, such as an
// object-store plugin's own deployment configuration. Its kustomization must
// be self-contained: inline patches only, no other files.
type External struct {
	// Path names the component below external/, for example plugin/assets.
	Path          string
	Kustomization []byte
}

// Values supplies runtime configuration, not Kubernetes object structure.
type Values struct {
	// Component selects actor-images, postgres, telemetry or demo, or is
	// empty to apply only External components.
	Component string
	// External components apply before Component, in order.
	External []External
	// CredentialConfigExists reports whether atelet input already contains
	// the substrate-aks-acr-credentials ConfigMap.
	CredentialConfigExists bool
	// NodeSelectorExists reports whether demo input already has a nodeSelector.
	NodeSelectorExists bool
}

// Render applies external components, then one embedded component, to
// upstream Kubernetes YAML. actor-images uses atelet.yaml; postgres and
// telemetry use postgres/postgres.yaml and ate-otel-config.yaml. Demo uses the
// WorkerPool from counter-microvm.yaml.tmpl. Callers supply the reviewed
// upstream input and runtime values; Render does not validate source pins.
// Upstream manifest checks and the plugin component values live in
// internal/installer/manifests.go.
func Render(source []byte, v Values) ([]byte, error) {
	fsys := filesys.MakeFsInMemory()
	if err := fsys.WriteFile("/render/upstream.yaml", source); err != nil {
		return nil, err
	}
	write := func(name, target string) error {
		data, err := manifests.ReadFile(name)
		if err != nil {
			return err
		}
		tmpl, err := template.New(name).Option("missingkey=error").Parse(string(data))
		if err != nil {
			return err
		}
		var out bytes.Buffer
		if err := tmpl.Execute(&out, v); err != nil {
			return err
		}
		if err := fsys.MkdirAll(path.Dir(target)); err != nil {
			return err
		}
		return fsys.WriteFile(target, out.Bytes())
	}
	if err := write("kustomization.yaml.tmpl", "/render/kustomization.yaml"); err != nil {
		return nil, err
	}
	for _, external := range v.External {
		if !fs.ValidPath(external.Path) || external.Path == "." {
			return nil, fmt.Errorf("invalid external component path %q", external.Path)
		}
		dir := "/render/external/" + external.Path
		if err := fsys.MkdirAll(dir); err != nil {
			return nil, err
		}
		if err := fsys.WriteFile(dir+"/kustomization.yaml", external.Kustomization); err != nil {
			return nil, err
		}
	}
	if v.Component != "" {
		component := "components/" + v.Component
		if err := fs.WalkDir(manifests, component, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			return write(name, "/render/"+name)
		}); err != nil {
			return nil, err
		}
	}
	result, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(fsys, "/render")
	if err != nil {
		return nil, fmt.Errorf("rendering Azure %s component: %w", v.Component, err)
	}
	return result.AsYaml()
}
