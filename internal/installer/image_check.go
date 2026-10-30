package installer

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

//go:embed imagecheck/*.go.txt
var imageCheckSource embed.FS

var errImagesMissing = errors.New("configured image is missing")

const (
	imageCheckCredentials = "Resolve registry credentials"
	imageCheckFetch       = "Fetch pinned Substrate source"
	imageCheckTools       = "Prepare image verification tools"
	imageCheckInventory   = "Verify published images and plugin"
)

func ImageCheckSteps() []string {
	return []string{imageCheckCredentials, imageCheckFetch, imageCheckTools, imageCheckInventory}
}

func (a App) imageCheckStep(name string, work func() error) error {
	if a.ImageCheckProgress != nil {
		a.ImageCheckProgress(name, false, nil)
	}
	err := work()
	if a.ImageCheckProgress != nil {
		a.ImageCheckProgress(name, true, err)
	}
	return err
}

func prepareImageChecker(root string) error {
	for _, pkg := range []string{"cmd", "steps"} {
		data, err := imageCheckSource.ReadFile("imagecheck/" + pkg + ".go.txt")
		if err != nil {
			return err
		}
		path := filepath.Join(root, "cmd/ate-setup/internal", pkg, "azure_image_check.go")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("preparing the pinned image checker: %w", err)
		}
		_, writeErr := file.Write(data)
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return err
		}
	}
	return nil
}

func (a App) CheckImages(ctx context.Context) error {
	s := a.Config.Substrate
	var ownedHost, dockerDir string
	if err := a.imageCheckStep(imageCheckCredentials, func() error {
		if err := a.Config.ValidateInstall(); err != nil {
			return err
		}
		azureRegistry := false
		for _, image := range []string{s.Images.Repository, s.Images.Plugin} {
			host, _, _ := strings.Cut(image, "/")
			azureRegistry = azureRegistry || strings.HasSuffix(host, ".azurecr.io")
		}
		if !azureRegistry {
			return nil
		}
		r, err := a.registry(ctx)
		if errors.Is(err, errGroupAbsent) {
			a.progress("Installation resource group is absent; checking saved images with existing registry credentials")
		} else if err != nil {
			return err
		} else if strings.HasPrefix(s.Images.Repository, r.LoginServer+"/") || strings.HasPrefix(s.Images.Plugin, r.LoginServer+"/") {
			ownedHost = r.LoginServer
			dockerDir, err = a.loginRegistry(ctx, r)
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	var root string
	defer func() {
		if root != "" {
			os.RemoveAll(root)
		}
	}()
	if err := a.imageCheckStep(imageCheckFetch, func() error {
		if err := os.MkdirAll(a.Config.workspace(), 0700); err != nil {
			return err
		}
		var err error
		root, err = os.MkdirTemp(a.Config.workspace(), "image-check-")
		if err != nil {
			return err
		}
		return a.fetchPinnedSource(ctx, root)
	}); err != nil {
		return err
	}
	if err := a.imageCheckStep(imageCheckTools, func() error { return prepareImageChecker(root) }); err != nil {
		return err
	}
	return a.imageCheckStep(imageCheckInventory, func() error {
		a.progress("Verifying the pinned publisher's image inventory and the Azure plugin digest")
		data, err := a.Runner.Run(ctx, Command{
			Dir: root,
			Args: []string{"go", "run", "./cmd/ate-setup", "publish", "check-images",
				s.Images.Repository, s.Images.Tag, s.Images.Plugin, ownedHost, dockerDir,
				"--no-dev-env", "--no-report-defaults"},
			Env: []string{"NO_DEV_ENV=1", "VERSION=" + s.Images.Tag},
		})
		if err != nil {
			return fmt.Errorf("running the pinned image checker (no images or install checkpoints changed): %w", err)
		}
		return imageCheckResult(data)
	})
}

func imageCheckResult(data []byte) error {
	var report struct {
		Status   string `json:"status"`
		Image    string `json:"image"`
		Checked  int    `json:"checked"`
		Expected int    `json:"expected"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		return fmt.Errorf("reading the pinned image check result: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("pinned image checker returned more than one result")
	}
	if report.Expected < 3 || report.Checked < 1 || report.Checked > report.Expected {
		return errors.New("pinned image checker returned an incomplete inventory result")
	}
	if report.Status == "available" && report.Checked == report.Expected && report.Image == "" {
		return nil
	}
	if report.Image == "" {
		return errors.New("pinned image checker did not confirm the complete image set")
	}
	switch report.Status {
	case "missing":
		return fmt.Errorf("%w: %s; saved references do not prove publication still exists. Restore the exact image set, or fully destroy this installation, run images --reset-config with the same --config, then retry setup", errImagesMissing, report.Image)
	case "authentication":
		return fmt.Errorf("authentication failed while checking %s; refresh az login for owned ACR or your external registry login, then retry images --check; images were not assumed missing", report.Image)
	case "authorization":
		return fmt.Errorf("access denied while checking %s; verify registry pull permissions (AcrPull/AcrPush for owned ACR) and allow time for role propagation, then retry images --check; images were not assumed missing", report.Image)
	case "network":
		return fmt.Errorf("network or timeout failure while checking %s; check DNS, TLS, connectivity and registry firewall rules, then retry images --check; images were not assumed missing", report.Image)
	case "registry-error":
		return fmt.Errorf("registry check failed for %s; verify the image reference and registry service availability, then retry images --check; images were not assumed missing", report.Image)
	default:
		return errors.New("pinned image checker did not confirm the complete image set")
	}
}

func (a App) ResetImageConfig(ctx context.Context, path string) error {
	if err := a.account(ctx); err != nil {
		return err
	}
	if err := a.guardTarget(ctx); err != nil {
		return err
	}
	if err := a.ownedGroup(ctx, true); !errors.Is(err, errGroupAbsent) {
		if err != nil {
			return err
		}
		return errors.New("refusing to reset saved images while the installation resource group exists; fully destroy it first. Existing installations and checkpoints must not be bypassed")
	}
	return updateImageConfig(path, a.Config, func(c *Config) error {
		c.Substrate.Images = ImagesConfig{}
		return nil
	})
}
