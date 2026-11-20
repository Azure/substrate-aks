//go:build integration

package installer

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// MaxSystemCount reserves one surge node per pool, so the compiled template
// must pin maxSurge rather than rely on the AKS default.
func TestBicepUpgradeSurgeContract(t *testing.T) {
	var stderr bytes.Buffer
	build := exec.CommandContext(t.Context(), "az", "bicep", "build", "--file", "../../infra/main.bicep", "--stdout")
	build.Stderr = &stderr
	data, err := build.Output()
	if err != nil {
		t.Fatalf("compiling main.bicep: %v\n%s", err, stderr.String())
	}
	// Bicep rejects duplicate properties, so two occurrences mean one per pool.
	if got := strings.Count(string(data), "'upgradeSettings', createObject('maxSurge', '1')"); got != 2 {
		t.Fatalf("compiled template sets maxSurge '1' on %d node pools, want 2", got)
	}
}
