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
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// sandboxTemplate is the pinned microVM SandboxConfig. Its default version
// pins every asset by sha256 at a content-addressed URL.
const sandboxTemplate = "manifests/ate-install/sandboxconfig-microvm.yaml.tmpl"

// assetPrefix is where the template stages assets, as <prefix>/<arch>/<sha256>.
const assetPrefix = "microvm-assets/amd64/"

// assetNames maps each template asset to the file ate-setup assembles.
var assetNames = map[string]string{
	"cloud-hypervisor": "cloud-hypervisor",
	"virtiofsd":        "virtiofsd",
	"kata-kernel":      "vmlinux",
	"kata-image":       "rootfs.img",
}

// sandboxManifest renders the pinned microVM SandboxConfig for this
// installation's asset container, amd64 only, and returns the verified
// asset files keyed by the blob names it references.
func sandboxManifest(root, assetsDir string, i Infrastructure) ([]byte, map[string]string, error) {
	if assetsDir == "" {
		return nil, nil, errors.New("substrate.runtime.assetsDir must point at the amd64 assets assembled for the selected revision")
	}
	data, err := os.ReadFile(filepath.Join(root, sandboxTemplate))
	if err != nil {
		return nil, nil, err
	}
	base := strings.TrimPrefix(i.AccountURL, "https://") + "/" + i.AssetsContainer
	data = []byte(strings.ReplaceAll(strings.ReplaceAll(string(data), "${BUCKET_SCHEME}", "https"),
		"${ASSET_BUCKET_NAME}", base))
	if strings.Contains(string(data), "${") {
		return nil, nil, errors.New("upstream sandbox template has unsupported substitutions")
	}
	var blobs map[string]string
	out, err := transformYAML(data, func(doc object) error {
		if doc["kind"] != "SandboxConfig" {
			return errors.New("upstream microVM template is not a SandboxConfig")
		}
		version, err := defaultSandboxVersion(doc)
		if err != nil {
			return err
		}
		amd64, err := nested(version, "assets", "amd64")
		if err != nil {
			return err
		}
		blobs, err = verifyAssetFiles(amd64, assetsDir)
		if err != nil {
			return err
		}
		for name := range assetNames {
			entry, err := child(amd64, name)
			if err != nil {
				return err
			}
			if entry["url"] != fmt.Sprintf("%s/%s/%s%s", i.AccountURL, i.AssetsContainer, assetPrefix, entry["sha256"]) {
				return fmt.Errorf("upstream asset URL changed for %s", name)
			}
		}
		version["assets"] = object{"amd64": amd64}
		return nil
	})
	return out, blobs, err
}

// defaultSandboxVersion returns the template's only version, which must be
// its default; the adapter reviews one asset set at a time.
func defaultSandboxVersion(doc object) (object, error) {
	spec, err := child(doc, "spec")
	if err != nil {
		return nil, err
	}
	name, ok := spec["defaultVersion"].(string)
	if !ok || name == "" {
		return nil, errors.New("upstream microVM template has no defaultVersion")
	}
	versions, ok := spec["versions"].([]any)
	if !ok || len(versions) != 1 {
		return nil, errors.New("upstream microVM template no longer has exactly one version; update the Azure adapter")
	}
	return named(versions, name)
}

// verifyRuntimeAssets checks assetsDir against the pinned template's amd64
// sha256 values and returns its files keyed by their content-addressed blob
// names.
func verifyRuntimeAssets(root, assetsDir string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(root, sandboxTemplate))
	if err != nil {
		return nil, err
	}
	var doc object
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	version, err := defaultSandboxVersion(doc)
	if err != nil {
		return nil, err
	}
	amd64, err := nested(version, "assets", "amd64")
	if err != nil {
		return nil, err
	}
	return verifyAssetFiles(amd64, assetsDir)
}

func verifyAssetFiles(amd64 object, assetsDir string) (map[string]string, error) {
	if len(amd64) != len(assetNames) {
		return nil, errors.New("upstream microVM asset set changed; update the Azure adapter")
	}
	blobs := map[string]string{}
	for name, file := range assetNames {
		entry, err := child(amd64, name)
		if err != nil {
			return nil, err
		}
		want, ok := entry["sha256"].(string)
		if !ok || !digestPattern.MatchString("asset@sha256:"+want) {
			return nil, fmt.Errorf("upstream asset %s has no fixed sha256", name)
		}
		path := filepath.Join(assetsDir, file)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("asset %s must be a regular file, not a symlink or device", path)
		}
		hash, err := fileChecksum(path)
		if err != nil {
			return nil, err
		}
		if hash != want {
			return nil, fmt.Errorf("asset %s does not match the selected upstream revision's amd64 SHA-256; reassemble from that revision", path)
		}
		blobs[assetPrefix+want] = path
	}
	return blobs, nil
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

func (a App) uploadAssets(ctx context.Context, i Infrastructure, blobs map[string]string) error {
	names := make([]string, 0, len(blobs))
	for name := range blobs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file := filepath.Base(blobs[name])
		a.progress("Uploading runtime asset %s to private Blob storage", file)
		// Blob names are the content's sha256, so overwriting is idempotent.
		if _, err := a.az(ctx, "storage", "blob", "upload", "--auth-mode", "login",
			"--account-name", i.StorageAccount, "--container-name", i.AssetsContainer,
			"--name", name, "--file", blobs[name],
			"--overwrite", "true", "--no-progress"); err != nil {
			return fmt.Errorf("uploading %s (the configured publisher needs Blob Data Contributor; new RBAC grants can take time to propagate): %w", file, err)
		}
	}
	return nil
}
