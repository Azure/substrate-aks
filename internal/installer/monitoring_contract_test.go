//go:build integration

package installer

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/deploy"
)

func TestPinnedUpstreamMonitoringContract(t *testing.T) {
	root := pinnedContractRoot(t)
	dest := copyContractManifests(t, root)
	c, i := testConfig(t), testInfrastructure()
	c.Monitoring.Enabled = true
	i.Monitoring = testMonitoring(c)
	if _, err := adaptSource(dest, c, i); err != nil {
		t.Fatal(err)
	}
	rendered, err := exec.Command("kubectl", "kustomize", "--load-restrictor=LoadRestrictionsNone",
		filepath.Join(dest, "manifests/ate-install/base")).CombinedOutput()
	if err != nil {
		t.Fatalf("enabled monitoring broke the upstream base: %v\n%s", err, rendered)
	}
	settings, err := child(manifestObject(t, rendered, "ConfigMap", "ate-otel-config"), "data")
	if err != nil {
		t.Fatal(err)
	}
	if settings["OTEL_METRICS_EXPORTER"] != "otlp" || settings["ATE_WORKER_OTEL_METRICS_EXPORTER"] != "otlp" ||
		settings["OTEL_SDK_DISABLED"] != "false" || strings.Contains(string(rendered), "gke-managed-otel") {
		t.Fatalf("upstream did not receive Azure metrics settings: %v", settings)
	}
	for _, workload := range []struct{ kind, name string }{
		{"Deployment", "ate-api-server"}, {"Deployment", "ate-controller"},
		{"DaemonSet", "atelet"}, {"Deployment", "atenet-router"},
	} {
		pod, err := nested(manifestObject(t, rendered, workload.kind, workload.name), "spec", "template", "spec")
		if err != nil {
			t.Fatal(err)
		}
		containers, ok := pod["containers"].([]any)
		if !ok {
			t.Fatalf("%s containers missing", workload.name)
		}
		configured := false
		for _, raw := range containers {
			container, ok := raw.(map[string]any)
			if !ok {
				t.Fatal("invalid container")
			}
			encoded, err := json.Marshal(container["envFrom"])
			if err != nil {
				t.Fatal(err)
			}
			configured = configured || strings.Contains(string(encoded), `"name":"ate-otel-config"`)
		}
		if !configured {
			t.Fatalf("%s no longer consumes the shared telemetry configuration", workload.name)
		}
	}
	router, err := nested(manifestObject(t, rendered, "Deployment", "atenet-router"), "spec", "template", "spec")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(router)
	if err != nil || !strings.Contains(string(encoded), `"name":"envoy-metrics"`) {
		t.Fatalf("upstream no longer exposes the Envoy metrics port: %v", err)
	}
	monitoring, err := deploy.Monitoring()
	if err != nil {
		t.Fatal(err)
	}
	monitor := manifestObject(t, monitoring, "PodMonitor", "substrate-aks-envoy")
	spec, err := child(monitor, "spec")
	if err != nil {
		t.Fatal(err)
	}
	if monitor["apiVersion"] != "azmonitoring.coreos.com/v1" {
		t.Fatal("upstream Envoy is not scraped through Azure's managed API")
	}
	selector, err := child(spec, "selector")
	if err != nil {
		t.Fatal(err)
	}
	labels, err := child(selector, "matchLabels")
	if err != nil || labels["app"] != "atenet-router" {
		t.Fatalf("Envoy monitor no longer matches the upstream router: %v", err)
	}
}
