package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"go.yaml.in/yaml/v3"
)

type ContextSwitch struct {
	Path, Previous string
	before         []byte
	exists         bool
}

func PlanContextSwitch(path string) (ContextSwitch, error) {
	if path == "" {
		paths := filepath.SplitList(os.Getenv("KUBECONFIG"))
		if len(paths) > 1 {
			return ContextSwitch{}, errors.New("KUBECONFIG contains multiple files; choose a single --context-file rather than flattening or overwriting them")
		}
		if len(paths) == 1 {
			path = paths[0]
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return ContextSwitch{}, err
			}
			path = filepath.Join(home, ".kube", "config")
		}
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return ContextSwitch{}, err
	}
	data, exists, err := readContextFile(path)
	if err != nil {
		return ContextSwitch{}, err
	}
	config, err := decodeContextFile(data)
	if err != nil {
		return ContextSwitch{}, err
	}
	previous, _ := config["current-context"].(string)
	return ContextSwitch{Path: path, Previous: previous, before: data, exists: exists}, nil
}

func readContextFile(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("context destination must be a regular file, not a symlink or device")
	}
	data, err := os.ReadFile(path)
	return data, true, err
}

func decodeContextFile(data []byte) (object, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return object{"apiVersion": "v1", "kind": "Config"}, nil
	}
	var config object
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&config); err != nil {
		return nil, errors.New("cannot parse kubeconfig destination; it has not been modified")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("kubeconfig must contain exactly one document; destination unchanged")
	}
	if config == nil {
		return nil, errors.New("context destination must contain a Kubernetes Config object")
	}
	for field, value := range map[string]string{"apiVersion": "v1", "kind": "Config"} {
		if _, present := config[field]; !present {
			config[field] = value
		}
	}
	if config["apiVersion"] != "v1" || config["kind"] != "Config" {
		return nil, errors.New("context destination is not a v1 Kubernetes Config")
	}
	if value := config["current-context"]; value != nil {
		if _, ok := value.(string); !ok {
			return nil, errors.New("kubeconfig current-context must be a string")
		}
	}
	for _, field := range []string{"clusters", "users", "contexts"} {
		entries, ok := config[field].([]any)
		if config[field] != nil && !ok {
			return nil, fmt.Errorf("kubeconfig %s must be a list", field)
		}
		names := map[string]bool{}
		for _, entry := range entries {
			value, ok := entry.(object)
			name, _ := value["name"].(string)
			if !ok || name == "" || names[name] {
				return nil, fmt.Errorf("kubeconfig %s contains invalid or duplicate names", field)
			}
			names[name] = true
		}
	}
	return config, nil
}

func (a App) SelectContext(ctx context.Context, selection ContextSwitch) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if selection.Path == a.kubeconfig() {
		return errors.New("context destination must differ from the installer's private kubeconfig")
	}
	if targetInfo, err := os.Stat(selection.Path); err == nil {
		privateInfo, err := os.Stat(a.kubeconfig())
		if err != nil {
			return err
		}
		if os.SameFile(targetInfo, privateInfo) {
			return errors.New("context destination aliases the installer's private kubeconfig; refusing to replace it")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sourceData, err := os.ReadFile(a.kubeconfig())
	if err != nil {
		return err
	}
	if err := validateKubeconfig(sourceData, a.Config.AKS.ClusterName); err != nil {
		return err
	}
	if a.Config.AKS.Operator != nil {
		if err := validateEntraKubeconfig(sourceData, a.Config.AKS.ClusterName); err != nil {
			return err
		}
	}
	source, err := decodeContextFile(sourceData)
	if err != nil {
		return err
	}
	target, err := decodeContextFile(selection.before)
	if err != nil {
		return err
	}
	selected := contextEntry(source, "contexts", a.Config.AKS.ClusterName)
	selectedContext, ok := selected["context"].(object)
	if !ok {
		return errors.New("private kubeconfig has no installation context")
	}
	cluster, _ := selectedContext["cluster"].(string)
	user, _ := selectedContext["user"].(string)
	if cluster == "" || user == "" {
		return errors.New("private kubeconfig context is missing cluster or user references")
	}
	for _, item := range []struct{ field, name string }{
		{"contexts", a.Config.AKS.ClusterName}, {"clusters", cluster}, {"users", user},
	} {
		field, name := item.field, item.name
		entry := contextEntry(source, field, name)
		if entry == nil {
			return fmt.Errorf("private kubeconfig is missing %s entry %s", field, name)
		}
		for _, kind := range []string{"cluster", "user"} {
			settings, _ := entry[kind].(object)
			for _, key := range []string{"certificate-authority", "client-certificate", "client-key", "tokenFile"} {
				if value, ok := settings[key].(string); ok && value != "" && !filepath.IsAbs(value) {
					return fmt.Errorf("private kubeconfig contains relative %s; use its printed export instead of importing it", key)
				}
			}
		}
		existing := contextEntry(target, field, name)
		if existing != nil {
			if !reflect.DeepEqual(entry, existing) {
				return fmt.Errorf("kubeconfig %s entry %q in %q already exists with different settings; destination unchanged. Choose an unused --context-file PATH or deliberately rename/remove only the conflicting entries before retrying", field, name, selection.Path)
			}
			continue
		}
		entries, _ := target[field].([]any)
		target[field] = append(entries, entry)
	}
	target["current-context"] = a.Config.AKS.ClusterName
	data, err := yaml.Marshal(target)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(selection.Path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(selection.Path), ".substrate-context-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	lockPath := selection.Path + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("locking kubeconfig %q; another writer may be active, retry after it finishes: %w", selection.Path, err)
	}
	defer func() { resultErr = errors.Join(resultErr, os.Remove(lockPath)) }()
	if err := lock.Close(); err != nil {
		return err
	}
	current, exists, err := readContextFile(selection.Path)
	if err != nil {
		return err
	}
	if exists != selection.exists || !bytes.Equal(current, selection.before) {
		return errors.New("destination kubeconfig changed while awaiting consent; refusing to overwrite it")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(file.Name(), selection.Path)
}

func contextEntry(config object, field, name string) object {
	entries, _ := config[field].([]any)
	for _, entry := range entries {
		value, _ := entry.(object)
		if value["name"] == name {
			return value
		}
	}
	return nil
}
