package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type restoreKubeIdentity struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	UID       string            `json:"uid"`
	Labels    map[string]string `json:"labels"`
}

func restoreKubeObject(data []byte, kind, name, namespace, installID, token string) (restoreKubeIdentity, error) {
	var object struct {
		Kind     string              `json:"kind"`
		Metadata restoreKubeIdentity `json:"metadata"`
	}
	if err := json.Unmarshal(data, &object); err != nil || object.Kind != kind ||
		object.Metadata.Name != name || object.Metadata.Namespace != namespace || object.Metadata.UID == "" ||
		object.Metadata.Labels[restoreInvocationLabel] != token || object.Metadata.Labels[restoreInstallationLabel] != installID {
		return restoreKubeIdentity{}, fmt.Errorf("invalid or changed %s identity", kind)
	}
	return object.Metadata, nil
}

func (a App) restoreCreateKube(ctx context.Context, data []byte, kind, name, namespace, token string) (restoreKubeIdentity, error) {
	result, err := a.kubectl(ctx, data, "create", "-f", "-", "--output=json")
	if err != nil {
		return restoreKubeIdentity{}, fmt.Errorf("creating %s (identity may require manual recovery): %w", kind, err)
	}
	return restoreKubeObject(result, kind, name, namespace, a.Config.Installation.ID, token)
}

func (a App) restoreDeleteKube(ctx context.Context, resource, path, kind string, identity restoreKubeIdentity, token string) error {
	args := []string{"get", resource, identity.Name, "--ignore-not-found", "--output=json"}
	if identity.Namespace != "" {
		args = append(args, "--namespace="+identity.Namespace)
	}
	data, err := a.kubectl(ctx, nil, args...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil
	}
	current, err := restoreKubeObject(data, kind, identity.Name, identity.Namespace, a.Config.Installation.ID, token)
	if err != nil || current.UID != identity.UID {
		return errors.New("Kubernetes object identity changed; refusing deletion")
	}
	options, err := json.Marshal(object{
		"apiVersion": "v1", "kind": "DeleteOptions",
		"preconditions":     object{"uid": identity.UID},
		"propagationPolicy": "Foreground",
	})
	if err != nil {
		return err
	}
	_, err = a.kubectl(ctx, options, "delete", "--raw", path, "-f", "-")
	return err
}
