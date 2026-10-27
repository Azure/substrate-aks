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
	manifestDir := filepath.Join(root, "manifests/microvm")
	if err := os.MkdirAll(manifestDir, 0700); err != nil {
		t.Fatal(err)
	}
	template := `apiVersion: ate.dev/v1alpha1
kind: SandboxConfig
metadata:
  name: microvm
spec:
  versions:
  - name: v1
    assets:
      arm64: {}
      amd64:
`
	for name, file := range assetNames {
		content := []byte("test asset " + name)
		if err := os.WriteFile(filepath.Join(dir, file), content, 0600); err != nil {
			t.Fatal(err)
		}
		template += fmt.Sprintf("        %s:\n          url: \"${BUCKET_SCHEME}://${BUCKET_NAME}/kata-assets/%s\"\n          sha256: \"%s\"\n", name, file, checksum(content))
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "sandboxconfig-microvm.yaml.tmpl"), []byte(template), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxAssetsPinnedToSelectedRevision(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	writeAssetFixture(t, root, dir)
	data, err := sandboxManifest(root, dir, testInfrastructure())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "arm64") || !strings.Contains(string(data), "https://substratetest123.blob.core.windows.net/assets/kata-assets/") {
		t.Fatalf("incorrect architecture or asset location: %s", data)
	}
	if err := os.WriteFile(filepath.Join(dir, "vmlinux"), []byte("wrong revision"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := sandboxManifest(root, dir, testInfrastructure()); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("accepted mismatched kernel: %v", err)
	}
}
