//go:build integration

package installer

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	plugin, err := pluginComponents(pinnedPluginRoot(t), c, i)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adaptSource(dest, c, plugin); err != nil {
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
	for _, key := range []string{"OTEL_TRACES_EXPORTER", "OTEL_LOGS_EXPORTER", "ATE_WORKER_OTEL_TRACES_EXPORTER", "ATE_WORKER_OTEL_LOGS_EXPORTER"} {
		if settings[key] != "none" {
			t.Fatalf("monitoring enabled SDK %s: %v", key, settings)
		}
	}
	assertMetricsOnlyOTLPEndpoint(t, settings)
	upstreamRouter, err := os.ReadFile(filepath.Join(root, "manifests/ate-install/atenet-router.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	routerContainer := func(data []byte) object {
		t.Helper()
		pod, err := nested(manifestObject(t, data, "Deployment", "atenet-router"), "spec", "template", "spec")
		if err != nil {
			t.Fatal(err)
		}
		container, err := named(pod["containers"], "atenet-router")
		if err != nil {
			t.Fatal(err)
		}
		return container
	}
	// Without a generic endpoint the router leaves Envoy's trace collector unset.
	atenetRouter := routerContainer(rendered)
	if got, want := atenetRouter["args"], routerContainer(upstreamRouter)["args"]; want == nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("router args = %v; want the upstream args %v", got, want)
	}
	if env, err := named(atenetRouter["env"], "OTEL_EXPORTER_OTLP_ENDPOINT"); err == nil {
		t.Fatalf("router sets its own generic OTLP endpoint: %v", env)
	}
	if encoded, err := json.Marshal(atenetRouter["envFrom"]); err != nil || !strings.Contains(string(encoded), `"name":"ate-otel-config"`) {
		t.Fatalf("router no longer reads the shared telemetry configuration: %s, %v", encoded, err)
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

type predictedResource struct {
	Type       string
	Properties map[string]any
}

// snapshotResources evaluates an infra template locally, without Azure
// access, and returns the resources it predicts. Parameter values are Bicep
// literals.
func snapshotResources(t *testing.T, template string, params map[string]string) []predictedResource {
	t.Helper()
	dir := t.TempDir()
	path, err := filepath.Abs(filepath.Join("../../infra", template))
	if err != nil {
		t.Fatal(err)
	}
	// Bicep accepts only relative template references.
	using, err := filepath.Rel(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	var file strings.Builder
	fmt.Fprintf(&file, "using '%s'\n", filepath.ToSlash(using))
	for name, value := range params {
		fmt.Fprintf(&file, "param %s = %s\n", name, value)
	}
	name := strings.TrimSuffix(template, ".bicep")
	paramFile := filepath.Join(dir, name+".bicepparam")
	if err := os.WriteFile(paramFile, []byte(file.String()), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "az", "bicep", "snapshot", "--file", paramFile, "--only-show-errors",
		"--subscription-id", "22222222-2222-4222-8222-222222222222", "--resource-group", "substrate-test",
		"--location", "eastus2", "--tenant-id", "66666666-6666-4666-8666-666666666666")
	if output, err := command.CombinedOutput(); err != nil {
		if exec.CommandContext(t.Context(), "az", "bicep", "snapshot", "--help").Run() != nil {
			t.Fatalf("contract tests need az bicep snapshot, which requires Bicep CLI 0.41.2 or later; "+
				"run az bicep upgrade, and update the Azure CLI if the command is still missing: %v\n%s", err, output)
		}
		t.Fatalf("evaluating %s: %v\n%s", template, err, output)
	}
	data, err := os.ReadFile(filepath.Join(dir, name+".snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct{ PredictedResources []predictedResource }
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot.PredictedResources
}

func TestManagedBicepMonitoringContract(t *testing.T) {
	root := func(overrides ...string) map[string]string {
		params := map[string]string{
			"location": "'eastus2'", "installationId": "'11111111-1111-4111-8111-111111111111'",
			"clusterName": "'substrate-test'", "kubernetesVersion": "'1.37'", "workerVmSize": "'Standard_D4s_v6'",
			"workerCount": "1", "apiAuthorizedCidrs": "[]", "assetPublisherObjectId": "'33333333-3333-4333-8333-333333333333'",
			"registryName": "'substratetest'",
		}
		for i := 0; i+1 < len(overrides); i += 2 {
			params[overrides[i]] = overrides[i+1]
		}
		return params
	}
	// Direct callers of the leaf module may omit the principal type.
	leaf := map[string]string{
		"location": "'eastus2'", "installationId": "'11111111-1111-4111-8111-111111111111'",
		"clusterName": "'substrate-test'", "grafanaAdminObjectId": "'33333333-3333-4333-8333-333333333333'",
	}
	const grafanaAdminRole = "22926164-76b3-42b3-bc55-97df8dab3e41"
	for _, tc := range []struct {
		name          string
		template      string
		params        map[string]string
		monitoring    bool
		principalType string
	}{
		{"monitoring=false", "resources.bicep", root("monitoringEnabled", "false"), false, ""},
		{"monitoring=true", "resources.bicep", root("monitoringEnabled", "true"), true, ""},
		{"type=User", "resources.bicep", root("monitoringEnabled", "true", "assetPublisherPrincipalType", "'User'"), true, "User"},
		{"type=ServicePrincipal", "resources.bicep",
			root("monitoringEnabled", "true", "assetPublisherPrincipalType", "'ServicePrincipal'"), true, "ServicePrincipal"},
		{"type=Group", "resources.bicep", root("monitoringEnabled", "true", "assetPublisherPrincipalType", "'Group'"), true, "Group"},
		{"leaf", "monitoring.bicep", leaf, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var clusters, admins []predictedResource
			for _, resource := range snapshotResources(t, tc.template, tc.params) {
				role, _ := resource.Properties["roleDefinitionId"].(string)
				switch {
				case resource.Type == "Microsoft.ContainerService/managedClusters":
					clusters = append(clusters, resource)
				case resource.Type == "Microsoft.Authorization/roleAssignments" && strings.HasSuffix(role, "/"+grafanaAdminRole):
					admins = append(admins, resource)
				}
			}
			if tc.template == "resources.bicep" {
				if len(clusters) != 1 {
					t.Fatalf("predicted %d AKS clusters; want 1", len(clusters))
				}
				profile, sent := clusters[0].Properties["azureMonitorProfile"]
				if !tc.monitoring && sent {
					t.Fatalf("disabled monitoring sent an Azure Monitor profile that could turn off existing metrics: %v", profile)
				}
				if tc.monitoring && !reflect.DeepEqual(profile, map[string]any{"metrics": map[string]any{"enabled": true}}) {
					t.Fatalf("enabled monitoring did not turn on managed Prometheus: %v", profile)
				}
			}
			if !tc.monitoring {
				if len(admins) != 0 {
					t.Fatalf("disabled monitoring predicted Grafana Admin assignments: %v", admins)
				}
				return
			}
			if len(admins) != 1 {
				t.Fatalf("predicted %d Grafana Admin assignments; want 1", len(admins))
			}
			properties := admins[0].Properties
			got, sent := properties["principalType"]
			if properties["principalId"] != "33333333-3333-4333-8333-333333333333" ||
				(tc.principalType == "" && sent) || (tc.principalType != "" && got != tc.principalType) {
				t.Fatalf("Grafana Admin does not follow the asset publisher identity and type %q: %v", tc.principalType, properties)
			}
		})
	}
}
