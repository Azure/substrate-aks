package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	restorePoolName          = "counter-microvm"
	restoreInvocationLabel   = "substrate-aks.io/check"
	restoreInstallationLabel = "substrate-aks.io/installation"
)

func renderRestoreManifests(poolData, templateData []byte, namespace, installation, token string) ([]byte, []byte, error) {
	foundNamespace, foundPool := false, false
	pool, err := transformYAML(poolData, func(doc object) error {
		meta, err := child(doc, "metadata")
		if err != nil {
			return err
		}
		switch doc["kind"] {
		case "Namespace":
			if foundNamespace || meta["name"] != "ate-demo-counter-microvm" {
				return errors.New("unexpected counter namespace")
			}
			foundNamespace = true
			meta["name"] = namespace
			meta["labels"] = object{restoreInvocationLabel: token, restoreInstallationLabel: installation}
		case "WorkerPool":
			if foundPool || meta["name"] != restorePoolName || meta["namespace"] != "ate-demo-counter-microvm" {
				return errors.New("unexpected counter WorkerPool")
			}
			foundPool = true
			meta["namespace"] = namespace
			labels, err := child(meta, "labels")
			if err != nil || labels["workload"] != restorePoolName {
				return errors.New("unexpected counter WorkerPool labels")
			}
			labels[restoreInvocationLabel], labels[restoreInstallationLabel] = token, installation
			template, err := nested(doc, "spec", "template")
			if err != nil {
				return errors.New("counter WorkerPool template missing")
			}
			podLabels, exists := template["labels"]
			if !exists {
				podLabels = object{}
				template["labels"] = podLabels
			}
			podLabelMap, ok := podLabels.(map[string]any)
			if !ok {
				return errors.New("counter WorkerPool template labels are invalid")
			}
			podLabelMap[restoreInvocationLabel], podLabelMap[restoreInstallationLabel] = token, installation
		default:
			return errors.New("unexpected object in counter manifest")
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if !foundNamespace || !foundPool {
		return nil, nil, errors.New("counter namespace or WorkerPool missing")
	}
	var tmpl object
	if err := yaml.Unmarshal(templateData, &tmpl); err != nil {
		return nil, nil, err
	}
	meta, err := child(tmpl, "metadata")
	if err != nil || meta["atespace"] != "ate-demo-counter-microvm" || meta["name"] != restorePoolName {
		return nil, nil, errors.New("unexpected counter template identity")
	}
	snapshot, err := child(tmpl, "snapshotConfig")
	if err != nil || snapshot["preferredFidelity"] != "SNAPSHOT_FIDELITY_MEMORY" {
		return nil, nil, errors.New("counter template does not request memory snapshots")
	}
	selector, err := nested(tmpl, "workerSelector", "matchLabels")
	if err != nil || selector["workload"] != restorePoolName || len(selector) != 1 {
		return nil, nil, errors.New("unexpected counter template selector")
	}
	meta["atespace"] = namespace
	selector[restoreInvocationLabel] = token
	template, err := yaml.Marshal(tmpl)
	if err != nil {
		return nil, nil, err
	}
	return pool, template, nil
}

func (a App) restoreManifests(ctx context.Context, i Infrastructure, helper *restoreHelper, namespace, token string) ([]byte, []byte, error) {
	root, err := a.prepare(ctx, i)
	if err != nil {
		return nil, nil, err
	}
	pool, err := os.ReadFile(filepath.Join(root, "demos/counter/counter-microvm.yaml.tmpl"))
	if err != nil {
		return nil, nil, err
	}
	template, err := os.ReadFile(filepath.Join(root, "demos/counter/counter-microvm-template.yaml.tmpl"))
	if err != nil {
		return nil, nil, err
	}
	pool, template, err = renderRestoreManifests(pool, template, namespace, a.Config.Installation.ID, token)
	if err != nil {
		return nil, nil, err
	}
	env := []string{"NO_DEV_ENV=1"}
	host, _, _ := strings.Cut(a.Config.Substrate.Images.Repository, "/")
	if strings.HasSuffix(host, ".azurecr.io") {
		registry, err := a.registry(ctx)
		if err != nil {
			return nil, nil, err
		}
		if host == registry.LoginServer {
			dir, err := a.loginRegistry(ctx, registry)
			if err != nil {
				return nil, nil, err
			}
			env = append(env, "DOCKER_CONFIG="+dir)
		}
	}
	input, err := json.Marshal([]string{string(pool), string(template)})
	if err != nil {
		return nil, nil, err
	}
	data, err := a.Runner.Run(ctx, Command{Dir: helper.root, Args: []string{helper.binary, "publish", "resolve-check-images",
		a.Config.Substrate.Images.Repository, a.Config.Substrate.Images.Tag, "--no-dev-env", "--no-report-defaults"},
		Env: env, Input: input, MaxOutput: restoreInventoryBytes})
	if err != nil {
		return nil, nil, fmt.Errorf("resolving counter images: %w", err)
	}
	var resolved []string
	if err := json.Unmarshal(data, &resolved); err != nil || len(resolved) != 2 || strings.Contains(resolved[0]+resolved[1], "ko://") {
		return nil, nil, errors.New("invalid resolved counter manifests")
	}
	return []byte(resolved[0]), []byte(resolved[1]), nil
}
