package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAssetFixture(t *testing.T, root, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	manifestDir := filepath.Join(root, filepath.Dir(sandboxTemplate))
	if err := os.MkdirAll(manifestDir, 0700); err != nil {
		t.Fatal(err)
	}
	template := `apiVersion: ate.dev/v1
kind: SandboxConfig
metadata:
  name: microvm
spec:
  defaultVersion: v2
  versions:
  - name: v2
    assets:
      arm64: {}
      amd64:
`
	for name, file := range assetNames {
		content := []byte("test asset " + name)
		if err := os.WriteFile(filepath.Join(dir, file), content, 0600); err != nil {
			t.Fatal(err)
		}
		sum := checksum(content)
		template += fmt.Sprintf("        %s:\n          url: \"${BUCKET_SCHEME}://${ASSET_BUCKET_NAME}/microvm-assets/amd64/%s\"\n          sha256: \"%s\"\n", name, sum, sum)
	}
	if err := os.WriteFile(filepath.Join(root, sandboxTemplate), []byte(template), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxAssetsPinnedToSelectedRevision(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	writeAssetFixture(t, root, dir)
	data, blobs, err := sandboxManifest(root, dir, testInfrastructure())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "arm64") || !strings.Contains(string(data), "https://substratetest123.blob.core.windows.net/assets/microvm-assets/amd64/") {
		t.Fatalf("incorrect architecture or asset location: %s", data)
	}
	if len(blobs) != len(assetNames) {
		t.Fatalf("expected one upload per asset, got %v", blobs)
	}
	for blob, path := range blobs {
		sum, err := fileChecksum(path)
		if err != nil || blob != assetPrefix+sum || !strings.Contains(string(data), blob) {
			t.Fatalf("upload %s of %s is not content-addressed or not referenced: %v", blob, path, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "vmlinux"), []byte("wrong revision"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sandboxManifest(root, dir, testInfrastructure()); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("accepted mismatched kernel: %v", err)
	}
}
