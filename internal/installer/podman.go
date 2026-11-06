package installer

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed publisher/podman.go.txt
var podmanPublisher []byte

func preparePodmanPublisher(root string) error {
	path := filepath.Join(root, "cmd/ate-setup/internal/steps/publish.go")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading upstream Dockerfile publisher: %w", err)
	}
	const call = "images.PublishDockerfileImage("
	if strings.Count(string(data), call) != 1 {
		return errors.New("upstream Dockerfile publisher changed; update the Podman adapter before publishing")
	}
	dockerfile := filepath.Join(root, "cmd/dataplane/envoy/Dockerfile")
	source, err := os.ReadFile(dockerfile)
	if err != nil {
		return fmt.Errorf("reading pinned Envoy Dockerfile: %w", err)
	}
	qualified := string(source)
	for _, image := range []struct{ from, to string }{
		{"FROM --platform=$BUILDPLATFORM rust:bookworm@sha256:", "FROM --platform=$BUILDPLATFORM docker.io/library/rust:bookworm@sha256:"},
		{"FROM envoyproxy/envoy:", "FROM docker.io/envoyproxy/envoy:"},
	} {
		if strings.Count(qualified, image.from) != 1 {
			return errors.New("upstream Envoy base-image contract changed; update the Podman adapter before publishing")
		}
		qualified = strings.Replace(qualified, image.from, image.to, 1)
	}
	helper := filepath.Join(root, "cmd/ate-setup/internal/images/azure_podman.go")
	file, err := os.OpenFile(helper, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("preparing Podman publisher: %w", err)
	}
	_, writeErr := file.Write(podmanPublisher)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if err := os.WriteFile(dockerfile, []byte(qualified), 0600); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Replace(string(data), call, "images.PublishPodmanImage(", 1)), 0600)
}
