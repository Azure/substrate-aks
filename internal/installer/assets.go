package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

var assetNames = map[string]string{
	"cloud-hypervisor": "cloud-hypervisor",
	"virtiofsd":        "virtiofsd",
	"kata-kernel":      "vmlinux",
	"kata-image":       "rootfs.img",
}

func sandboxManifest(root, assetsDir string, i Infrastructure) ([]byte, error) {
	if assetsDir == "" {
		return nil, errors.New("substrate.runtime.assetsDir must point at the amd64 assets assembled by the selected revision's hack/microvm-assets/assemble.sh")
	}
	data, err := os.ReadFile(filepath.Join(root, "manifests/microvm/sandboxconfig-microvm.yaml.tmpl"))
	if err != nil {
		return nil, err
	}
	data = []byte(strings.ReplaceAll(strings.ReplaceAll(string(data), "${BUCKET_SCHEME}", "https"),
		"${BUCKET_NAME}", strings.TrimPrefix(i.AccountURL, "https://")+"/"+i.AssetsContainer))
	if strings.Contains(string(data), "${") {
		return nil, errors.New("upstream sandbox template has unsupported substitutions")
	}
	return transformYAML(data, func(doc object) error {
		if doc["kind"] != "SandboxConfig" {
			return errors.New("upstream microVM template is not a SandboxConfig")
		}
		spec, err := child(doc, "spec")
		if err != nil {
			return err
		}
		version, err := named(spec["versions"], "v1")
		if err != nil {
			return err
		}
		assets, err := child(version, "assets")
		if err != nil {
			return err
		}
		amd64, err := child(assets, "amd64")
		if err != nil {
			return err
		}
		if len(amd64) != len(assetNames) {
			return errors.New("upstream microVM asset set changed; update the Azure adapter")
		}
		for name, file := range assetNames {
			entry, err := child(amd64, name)
			if err != nil {
				return err
			}
			wantURL := i.AccountURL + "/" + i.AssetsContainer + "/kata-assets/" + file
			if entry["url"] != wantURL {
				return fmt.Errorf("upstream asset URL changed for %s", name)
			}
		}
		if err := verifyAssetFiles(amd64, assetsDir); err != nil {
			return err
		}
		version["assets"] = object{"amd64": amd64}
		return nil
	})
}

func verifyRuntimeAssets(root, assetsDir string) error {
	data, err := os.ReadFile(filepath.Join(root, "manifests/microvm/sandboxconfig-microvm.yaml.tmpl"))
	if err != nil {
		return err
	}
	var doc object
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	spec, err := child(doc, "spec")
	if err != nil {
		return err
	}
	version, err := named(spec["versions"], "v1")
	if err != nil {
		return err
	}
	amd64, err := nested(version, "assets", "amd64")
	if err != nil {
		return err
	}
	return verifyAssetFiles(amd64, assetsDir)
}

func verifyAssetFiles(amd64 object, assetsDir string) error {
	if len(amd64) != len(assetNames) {
		return errors.New("upstream microVM asset set changed; update the Azure adapter")
	}
	for name, file := range assetNames {
		entry, err := child(amd64, name)
		if err != nil {
			return err
		}
		path := filepath.Join(assetsDir, file)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("asset %s must be a regular file, not a symlink or device", path)
		}
		hash, err := fileChecksum(path)
		if err != nil {
			return err
		}
		if entry["sha256"] != hash {
			return fmt.Errorf("asset %s does not match the selected upstream revision's amd64 SHA-256; reassemble from that revision", path)
		}
	}
	return nil
}

func fileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (a App) uploadAssets(ctx context.Context, i Infrastructure) error {
	for _, file := range []string{"cloud-hypervisor", "virtiofsd", "vmlinux", "rootfs.img"} {
		a.progress("Uploading runtime asset %s to private Blob storage", file)
		if _, err := a.az(ctx, "storage", "blob", "upload", "--auth-mode", "login",
			"--account-name", i.StorageAccount, "--container-name", i.AssetsContainer,
			"--name", "kata-assets/"+file, "--file", filepath.Join(a.Config.Substrate.Runtime.AssetsDir, file),
			"--overwrite", "true", "--no-progress"); err != nil {
			return fmt.Errorf("uploading %s (the configured publisher needs Blob Data Contributor; new RBAC grants can take time to propagate): %w", file, err)
		}
	}
	return nil
}
