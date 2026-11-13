package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type restoreAPIIdentity struct {
	Name     string `json:"name"`
	Atespace string `json:"atespace"`
	UID      string `json:"uid"`
}

func restoreAPIObject(data []byte, name, atespace string) (restoreAPIIdentity, error) {
	var object struct {
		Metadata restoreAPIIdentity `json:"metadata"`
	}
	if err := json.Unmarshal(data, &object); err != nil ||
		object.Metadata.Name != name || object.Metadata.Atespace != atespace || object.Metadata.UID == "" {
		return restoreAPIIdentity{}, errors.New("invalid or changed Substrate API identity")
	}
	return object.Metadata, nil
}

func (a App) restoreCreateAPI(ctx context.Context, client, name, atespace string, input []byte, args ...string) (restoreAPIIdentity, error) {
	data, err := a.ate(ctx, client, input, append(args, "-o", "json")...)
	if err != nil {
		return restoreAPIIdentity{}, fmt.Errorf("creating Substrate API object (identity may require manual recovery): %w", err)
	}
	return restoreAPIObject(data, name, atespace)
}

func (a App) restoreCheckAPIIdentity(ctx context.Context, client, kind string, identity restoreAPIIdentity) error {
	args := []string{"get", kind, identity.Name}
	if identity.Atespace != "" {
		args = append(args, "-a", identity.Atespace)
	}
	args = append(args, "-o", "json")
	data, err := a.ate(ctx, client, nil, args...)
	if err != nil {
		return fmt.Errorf("checking %s identity: %w", kind, err)
	}
	current, err := restoreAPIObject(data, identity.Name, identity.Atespace)
	if err != nil || current.UID != identity.UID {
		return fmt.Errorf("%s identity changed; refusing deletion", kind)
	}
	return nil
}

func (a App) restoreDeleteAPI(ctx context.Context, client, kind string, atespace, identity restoreAPIIdentity) error {
	if err := a.restoreCheckAPIIdentity(ctx, client, "atespace", atespace); err != nil {
		return err
	}
	if err := a.restoreCheckAPIIdentity(ctx, client, kind, identity); err != nil {
		return err
	}
	args := []string{"delete", kind, identity.Name}
	if identity.Atespace != "" {
		args = append(args, "-a", identity.Atespace)
	}
	_, err := a.ate(ctx, client, nil, args...)
	return err
}
