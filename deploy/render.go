// Package deploy embeds the Azure configuration applied to the pinned upstream
// manifests. Rendering is in-memory; it never invokes kubectl or other programs.
package deploy

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"text/template"

	"sigs.k8s.io/kustomize/api/filters/patchjson6902"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

//go:embed kustomization.yaml.tmpl base-monitoring.patch.yaml components
var manifests embed.FS

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

// Values supplies runtime configuration, not Kubernetes object structure.
type Values struct {
	Component              string
	Name, Kind, Mode       string
	PluginImage, ClientID  string
	TenantID, AccountURL   string
	Snapshots, Assets      string
	CredentialConfigExists bool
	NodeSelectorExists     bool
}

// Render applies one embedded component to one upstream materialization surface.
func Render(source []byte, v Values) ([]byte, error) {
	fsys := filesys.MakeFsInMemory()
	if err := fsys.WriteFile("/render/upstream.yaml", source); err != nil {
		return nil, err
	}
	quote := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}
	write := func(name, target string) error {
		data, err := manifests.ReadFile(name)
		if err != nil {
			return err
		}
		tmpl, err := template.New(name).Funcs(template.FuncMap{"quote": quote}).Option("missingkey=error").Parse(string(data))
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
	result, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(fsys, "/render")
	if err != nil {
		return nil, fmt.Errorf("rendering Azure %s component: %w", v.Component, err)
	}
	return result.AsYaml()
}
