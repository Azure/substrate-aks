//go:build integration

package installer

import (
	"bytes"
	"encoding/json"
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
	type resource struct {
		Type       string
		Properties json.RawMessage
	}
	var main struct{ Resources []resource }
	if err := json.Unmarshal(data, &main); err != nil {
		t.Fatal(err)
	}
	var clusters []string
	for _, r := range main.Resources {
		if r.Type != "Microsoft.Resources/deployments" {
			continue
		}
		var module struct {
			Template struct{ Resources []resource }
		}
		if err := json.Unmarshal(r.Properties, &module); err != nil {
			t.Fatal(err)
		}
		for _, nested := range module.Template.Resources {
			if nested.Type != "Microsoft.ContainerService/managedClusters" {
				continue
			}
			// The pools use union(), so Bicep emits the properties as one ARM expression.
			var expression string
			if err := json.Unmarshal(nested.Properties, &expression); err != nil {
				t.Fatalf("managed cluster properties are not an ARM expression: %s", nested.Properties)
			}
			clusters = append(clusters, expression)
		}
	}
	if len(clusters) != 1 {
		t.Fatalf("compiled template has %d managed clusters, want 1", len(clusters))
	}
	for _, pool := range []string{"system", "microvm"} {
		call := "createObject('name', '" + pool + "'"
		if strings.Count(clusters[0], call) != 1 {
			t.Fatalf("compiled template does not define the %s pool exactly once", pool)
		}
		profile := armCall(clusters[0], strings.Index(clusters[0], call))
		if !strings.Contains(profile, "'upgradeSettings', createObject('maxSurge', '1')") {
			t.Errorf("%s pool does not set upgradeSettings.maxSurge to '1': %s", pool, profile)
		}
	}
}

// armCall returns the ARM function call starting at start, through its closing parenthesis.
func armCall(expression string, start int) string {
	depth, quoted := 0, false
	for i := start; i < len(expression); i++ {
		switch c := expression[i]; {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return expression[start : i+1]
			}
		}
	}
	return expression[start:]
}
