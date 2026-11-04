package installer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/deploy"
)

func testMonitoring(c Config) *MonitoringInfrastructure {
	prefix := "/subscriptions/" + c.Azure.SubscriptionID + "/resourceGroups/" + c.Azure.ResourceGroup + "/providers/"
	return &MonitoringInfrastructure{
		Location:        c.MonitoringLocation(),
		WorkspaceID:     prefix + "Microsoft.Monitor/accounts/substrate-test",
		GrafanaID:       prefix + "Microsoft.Dashboard/grafana/substrate-test",
		QueryEndpoint:   "https://substrate-test.eastus2.prometheus.monitor.azure.com",
		GrafanaEndpoint: "https://substrate-test.grafana.azure.com",
	}
}

func TestMonitoringConfigurationAndOutputs(t *testing.T) {
	c := testConfig(t)
	c.Monitoring.Location = "westus3"
	if err := c.Validate(); err == nil {
		t.Fatal("accepted a monitoring region without opt-in")
	}
	c.Monitoring.Enabled = true
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	m := testMonitoring(c)
	if err := validateMonitoringInfrastructure(c, m); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*MonitoringInfrastructure){
		func(m *MonitoringInfrastructure) { m.Location = "eastus2" },
		func(m *MonitoringInfrastructure) {
			m.WorkspaceID = strings.ReplaceAll(m.WorkspaceID, c.Azure.SubscriptionID, c.Installation.ID)
		},
		func(m *MonitoringInfrastructure) { m.GrafanaEndpoint = "https://attacker.invalid" },
		func(m *MonitoringInfrastructure) { m.GrafanaEndpoint += "/api/datasources" },
		func(m *MonitoringInfrastructure) { m.QueryEndpoint += "?token=unexpected" },
		func(m *MonitoringInfrastructure) { m.GrafanaID = "" },
	} {
		bad := *m
		mutate(&bad)
		if err := validateMonitoringInfrastructure(c, &bad); err == nil {
			t.Fatalf("accepted inconsistent monitoring outputs: %+v", bad)
		}
	}
	if err := validateMonitoringInfrastructure(c, nil); err == nil {
		t.Fatal("enabled monitoring accepted missing outputs")
	}
	before := fingerprint(c, testInfrastructure())
	i := testInfrastructure()
	i.Monitoring = m
	if before == fingerprint(c, i) {
		t.Fatal("monitoring did not invalidate the installation fingerprint")
	}
	c.Monitoring = MonitoringConfig{}
	if err := validateMonitoringInfrastructure(c, m); err == nil {
		t.Fatal("disabled configuration accepted enabled deployment")
	}
}

func TestMonitoringProvisionParametersAndRegistryIsolation(t *testing.T) {
	for _, registryOnly := range []bool{false, true} {
		c := testConfig(t)
		c.Monitoring = MonitoringConfig{Enabled: true, Location: "westus3"}
		r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
			reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`})
		if err := (App{Config: c, Runner: r}).provision(t.Context(), true, registryOnly); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(c.workspace(), "infra", "parameters.json"))
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Parameters map[string]struct {
				Value any `json:"value"`
			} `json:"parameters"`
		}
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		params := document.Parameters
		if params["monitoringEnabled"].Value != true || params["monitoringLocation"].Value != "westus3" ||
			params["registryOnly"].Value != registryOnly {
			t.Fatalf("monitoring parameters lost opt-in, region, or registry-only gate: %+v", params)
		}
	}
	c := testConfig(t)
	c.Monitoring.Enabled = true
	r := script(t, reply{contains: "group exists", output: "true"}, ownerReply(c))
	if err := (App{Config: c, Runner: r}).ownedGroup(t.Context(), false); err == nil ||
		!strings.Contains(err.Error(), "monitoring mode or region") {
		t.Fatalf("accepted enabling monitoring on an existing unmonitored installation: %v", err)
	}
}

func TestMonitoringOwnershipTagCasing(t *testing.T) {
	for _, tc := range []struct {
		tag     string
		enabled bool
	}{
		{"True", true}, {"true", true}, {"False", false}, {"false", false}, {"", false},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			c := testConfig(t)
			c.Monitoring.Enabled = tc.enabled
			group, err := json.Marshal(object{
				"location": c.Azure.Location,
				"tags": object{
					ownershipTag:                        c.Installation.ID,
					"substrate-aks-monitoring":          tc.tag,
					"substrate-aks-monitoring-location": c.MonitoringLocation(),
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			r := script(t, reply{contains: "group exists", output: "true"},
				reply{contains: "group show", output: string(group)})
			if err := (App{Config: c, Runner: r}).ownedGroup(t.Context(), false); err != nil {
				t.Fatalf("rejected matching monitoring mode recorded as %q: %v", tc.tag, err)
			}
		})
	}
}

func TestMonitoringManagedResourceGroupTarget(t *testing.T) {
	c := testConfig(t)
	c.Monitoring.Enabled = true
	workspaceID := testMonitoring(c).WorkspaceID
	for _, tc := range []struct {
		name, manager string
		enabled, main bool
		ok            bool
	}{
		{"owned workspace", workspaceID, true, true, true},
		{"case-insensitive workspace ID", strings.ToUpper(workspaceID), true, true, true},
		{"monitoring disabled", workspaceID, false, true, false},
		{"orphaned workspace group", workspaceID, true, false, false},
		{"foreign subscription", strings.Replace(workspaceID, c.Azure.SubscriptionID, "33333333-3333-4333-8333-333333333333", 1), true, true, false},
		{"foreign resource group", strings.Replace(workspaceID, "/resourceGroups/"+c.Azure.ResourceGroup+"/", "/resourceGroups/another-group/", 1), true, true, false},
		{"different resource provider", strings.Replace(workspaceID, "Microsoft.Monitor/accounts", "Microsoft.Example/accounts", 1), true, true, false},
		{"workspace child resource", workspaceID + "/child", true, true, false},
		{"workspace name missing", workspaceID[:strings.LastIndex(workspaceID, "/")+1], true, true, false},
		{"unmanaged MA group", "", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := c
			config.Monitoring.Enabled = tc.enabled
			groups := "[" + resourceGroupJSON(c, "MA_workspace_managed", tc.manager)
			if tc.main {
				groups += "," + managedNodeGroupJSON(c) + "," + resourceGroupJSON(c, c.Azure.ResourceGroup, "")
			}
			groups += "]"
			r := script(t, reply{contains: "group list --tag", output: groups})
			err := (App{Config: config, Runner: r}).guardTarget(t.Context())
			if (err == nil) != tc.ok {
				t.Fatalf("managed workspace target result = %v; want allowed=%v", err, tc.ok)
			}
			if !tc.main && (err == nil || !strings.Contains(err.Error(), "incomplete teardown")) {
				t.Fatalf("orphaned workspace lost teardown guidance: %v", err)
			}
		})
	}
}

func TestMonitoringExporterConfiguration(t *testing.T) {
	source := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: ate-otel-config
  namespace: ate-system
data:
  OTEL_EXPORTER_OTLP_ENDPOINT: http://gke-managed-otel:4317
`)
	data, err := deploy.Render(source, deploy.Values{Component: "telemetry", MonitoringEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := child(manifestObject(t, data, "ConfigMap", "ate-otel-config"), "data")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"OTEL_METRICS_EXPORTER", "ATE_WORKER_OTEL_METRICS_EXPORTER"} {
		if settings[key] != "otlp" {
			t.Fatalf("%s did not enable OTLP metrics: %v", key, settings)
		}
	}
	if settings["OTEL_SDK_DISABLED"] != "false" || settings["OTEL_TRACES_EXPORTER"] != "none" ||
		settings["ATE_WORKER_OTEL_LOGS_EXPORTER"] != "none" || strings.Contains(string(data), "gke-managed") {
		t.Fatalf("monitoring enabled unsupported signals or retained GKE: %s", data)
	}
}

func monitoringObserved(t *testing.T, services ...string) string {
	t.Helper()
	frames := []object{}
	for _, service := range services {
		frames = append(frames, object{
			"schema": object{"fields": []object{
				{"type": "time"},
				{"type": "number", "labels": object{"service_name": service}},
			}},
			"data": object{"values": [][]int{{1}, {1}}},
		})
	}
	data, err := json.Marshal(object{"results": object{"A": object{"status": 200, "frames": frames}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestGrafanaReconciliationAndFailurePropagation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "update"}[existing], func(t *testing.T) {
			c, i := testConfig(t), testInfrastructure()
			c.Monitoring.Enabled = true
			i.Monitoring = testMonitoring(c)
			sources, method, path := "[]", "POST", "/api/datasources"
			if existing {
				sources = `[{"uid":"substrate-aks-prometheus","type":"prometheus"},{"uid":"substrate-aks-azure-monitor","type":"grafana-azure-monitor-datasource"}]`
				method, path = "PUT", "/api/datasources/uid/"
			}
			r := script(t,
				reply{contains: "--method GET", output: sources},
				reply{contains: "--method " + method + " --url " + i.Monitoring.GrafanaEndpoint + path},
				reply{contains: "--method " + method + " --url " + i.Monitoring.GrafanaEndpoint + path},
				reply{contains: "/api/dashboards/db"},
				reply{contains: "/api/dashboards/db"},
				reply{contains: "/api/ds/query", output: monitoringObserved(t, "ateapi", "atelet", "atecontroller", "atenet-router")},
			)
			if err := (App{Config: c, Runner: r}).configureGrafana(t.Context(), i); err != nil {
				t.Fatal(err)
			}
			for _, call := range r.calls {
				args := strings.Join(call.Args, " ")
				if !strings.Contains(args, "--resource https://dashboard.azure.com") ||
					!strings.Contains(args, "--subscription "+c.Azure.SubscriptionID) {
					t.Fatalf("Grafana lost explicit authentication target: %+v", call)
				}
			}
			var source object
			if err := json.Unmarshal(r.calls[1].Input, &source); err != nil {
				t.Fatal(err)
			}
			settings, _ := child(source, "jsonData")
			if settings["azureAuth"] != true {
				t.Fatalf("Prometheus data source lacks Azure authentication: %v", source)
			}
			var dashboard object
			if err := json.Unmarshal(r.calls[3].Input, &dashboard); err != nil || dashboard["overwrite"] != true {
				t.Fatalf("dashboard reconciliation is not idempotent: %s: %v", r.calls[3].Input, err)
			}
		})
	}
	c, i := testConfig(t), testInfrastructure()
	c.Monitoring.Enabled = true
	i.Monitoring = testMonitoring(c)
	want := errors.New("dashboard import failed")
	r := script(t, reply{contains: "/api/datasources", output: "[]"},
		reply{contains: "/api/datasources"}, reply{contains: "/api/datasources"},
		reply{contains: "/api/dashboards/db", err: want})
	if err := (App{Config: c, Runner: r}).configureGrafana(t.Context(), i); !errors.Is(err, want) {
		t.Fatalf("dashboard failure was hidden: %v", err)
	}
}

func TestMonitoringRejectsMissingMetrics(t *testing.T) {
	for _, tc := range []struct{ name, response string }{
		{"empty", monitoringObserved(t)},
		{"zero", strings.Replace(monitoringObserved(t, "atecontroller"), `[[1],[1]]`, `[[1],[0]]`, 1)},
		{"null", strings.Replace(monitoringObserved(t, "atecontroller"), `[[1],[1]]`, `[[1],[null]]`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			c.Monitoring.Enabled = true
			ctx, cancel := context.WithCancel(t.Context())
			r := script(t, reply{contains: "/api/ds/query", output: tc.response,
				effect: func(Command) { cancel() }})
			err := (App{Config: c, Runner: r}).verifyMetrics(ctx, testMonitoring(c))
			if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "metrics are incomplete") {
				t.Fatalf("missing metrics looked successful: %v", err)
			}
		})
	}
}

func TestMonitoringAcceptsIdleInstallation(t *testing.T) {
	c := testConfig(t)
	c.Monitoring.Enabled = true
	r := script(t, reply{contains: "--method POST", output: monitoringObserved(t, "atecontroller")})
	if err := (App{Config: c, Runner: r}).verifyMetrics(t.Context(), testMonitoring(c)); err != nil {
		t.Fatal(err)
	}
	var request struct {
		Queries []struct {
			Expr       string `json:"expr"`
			Instant    bool   `json:"instant"`
			Range      bool   `json:"range"`
			Datasource struct {
				UID string `json:"uid"`
			} `json:"datasource"`
		} `json:"queries"`
	}
	if err := json.Unmarshal(r.calls[0].Input, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Queries) != 1 || !request.Queries[0].Instant || request.Queries[0].Range ||
		request.Queries[0].Datasource.UID != prometheusDataSource ||
		!strings.Contains(strings.Join(r.calls[0].Args, " "), "/api/ds/query") {
		t.Fatalf("acceptance check bypassed Grafana's authenticated backend query: %s", r.calls[0].Input)
	}
	query := request.Queries[0].Expr
	if !strings.Contains(query, `workqueue_depth{service_name="atecontroller"}`) {
		t.Fatal("idle acceptance must query controller application metrics, not target_info or request-only metrics")
	}
	for _, target := range []string{"substrate-aks-otel", "atenet-router"} {
		if !strings.Contains(query, `min(up{namespace="ate-system",pod=~"`+target+`-.*"}) == 1`) {
			t.Fatalf("idle acceptance no longer requires all %s scrape endpoints to be healthy", target)
		}
	}
}

func TestMonitoringQueryErrors(t *testing.T) {
	for _, retryable := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid-query", true: "permissions-propagating"}[retryable], func(t *testing.T) {
			c := testConfig(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := errors.New("Bad Request: invalid expression")
			if retryable {
				want = errors.New("Forbidden: role assignment is propagating")
			}
			r := script(t, reply{contains: "/api/ds/query", err: want, effect: func(Command) {
				if retryable {
					cancel()
				}
			}})
			err := (App{Config: c, Runner: r}).verifyMetrics(ctx, testMonitoring(c))
			if !errors.Is(err, want) || errors.Is(err, context.Canceled) != retryable {
				t.Fatalf("query error lost its cause or used the wrong retry boundary: %v", err)
			}
		})
	}
}

func TestMonitoringRejectsInvalidBackendResults(t *testing.T) {
	for _, body := range []string{
		`{`,
		`{"results":{}}`,
		`{"results":{"A":{"status":403,"error":"Access denied"}}}`,
		`{"results":{"A":{"status":200,"error":"Query failed"}}}`,
		`{"results":{"A":{"status":200,"frames":[{"schema":{"fields":[{"type":"number"}]},"data":{"values":[]}}]}}}`,
	} {
		c := testConfig(t)
		r := script(t, reply{contains: "/api/ds/query", output: body})
		if err := (App{Config: c, Runner: r}).verifyMetrics(t.Context(), testMonitoring(c)); err == nil {
			t.Fatalf("invalid Grafana backend result was accepted: %s", body)
		}
	}
}

func TestMonitoringCollectorLifecycle(t *testing.T) {
	c := testConfig(t)
	if err := (App{Config: c, Runner: script(t)}).installMetricsCollector(t.Context()); err != nil {
		t.Fatal(err)
	}
	c.Monitoring.Enabled = true
	r := script(t, reply{contains: "wait --for=create crd/podmonitors.azmonitoring.coreos.com"},
		reply{contains: "wait --for=condition=Established crd/podmonitors.azmonitoring.coreos.com"},
		reply{contains: "apply -f -"}, reply{contains: "rollout status deployment/substrate-aks-otel"})
	if err := (App{Config: c, Runner: r}).installMetricsCollector(t.Context()); err != nil {
		t.Fatal(err)
	}
	manifest := r.calls[2].Input
	collector, err := nested(manifestObject(t, manifest, "Deployment", "substrate-aks-otel"), "spec", "template", "spec")
	if err != nil || collector["automountServiceAccountToken"] != false {
		t.Fatalf("collector unnecessarily mounts Kubernetes credentials: %v: %v", collector, err)
	}
	monitor := manifestObject(t, manifest, "PodMonitor", "substrate-aks-envoy")
	if monitor["apiVersion"] != "azmonitoring.coreos.com/v1" {
		t.Fatal("Envoy monitor uses an unmanaged API group")
	}
	policy := manifestObject(t, manifest, "NetworkPolicy", "substrate-aks-otel")
	selector, err := nested(policy, "spec", "podSelector", "matchLabels")
	if err != nil || selector["app"] != "substrate-aks-otel" {
		t.Fatalf("collector ingress rules do not select the collector: %v: %v", selector, err)
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"port":"otlp-grpc"`, `"port":"self-metrics"`, `"kubernetes.io/metadata.name":"kube-system"`, `"rsName":"ama-metrics"`} {
		if !strings.Contains(string(encoded), required) {
			t.Fatalf("collector ingress omitted %s: %s", required, encoded)
		}
	}
}

func TestMonitoringDashboardsSelectProvisionedDatabase(t *testing.T) {
	for _, backend := range []DatabaseBackend{DatabaseBundled, DatabaseFlexible, DatabaseHorizon} {
		c, i := testConfig(t), testInfrastructure()
		if backend != DatabaseBundled {
			i.Database = &DatabaseInfrastructure{Backend: backend, Location: "westus3",
				ResourceID: "/subscriptions/test/resourceGroups/dev/providers/database/servers/selected-postgres"}
		}
		dashboards := (App{Config: c}).monitoringDashboards(i)
		encoded, err := json.Marshal(dashboards)
		if err != nil {
			t.Fatal(err)
		}
		text := string(encoded)
		if strings.Contains(text, "or vector(0)") || strings.Contains(text, "gke") {
			t.Fatal("dashboards mask missing telemetry or retain GKE selectors")
		}
		if backend != DatabaseBundled {
			if !strings.Contains(text, `"region":"westus3"`) || !strings.Contains(text, `"resourceName":"selected-postgres"`) {
				t.Fatal("database dashboard ignored actual resource or its independent region")
			}
			want := "cpu_percent"
			forbidden := "CommitLatency"
			if backend == DatabaseHorizon {
				want, forbidden = forbidden, want
			}
			if !strings.Contains(text, want) || strings.Contains(text, forbidden) {
				t.Fatalf("dashboard mixed backend metric schemas: %s", backend)
			}
		} else if strings.Contains(text, "cpu_percent") || strings.Contains(text, "CommitLatency") {
			t.Fatal("bundled database received managed platform metric queries")
		}
	}

}

func TestMonitoringDashboardSections(t *testing.T) {
	for _, backend := range []DatabaseBackend{DatabaseBundled, DatabaseFlexible, DatabaseHorizon} {
		t.Run(string(backend), func(t *testing.T) {
			c, i := testConfig(t), testInfrastructure()
			dependencySections, dependencyPanels := []string{"Blob storage"}, 6
			if backend != DatabaseBundled {
				i.Database = &DatabaseInfrastructure{Backend: backend, Location: "westus3",
					ResourceID: "/subscriptions/test/resourceGroups/dev/providers/database/servers/selected-postgres"}
				dependencySections = []string{"PostgreSQL health and capacity", "PostgreSQL connections", "Blob storage"}
				dependencyPanels = 12
				if backend == DatabaseHorizon {
					dependencySections = []string{"HorizonDB health and capacity", "HorizonDB connections and queries", "Blob storage"}
					dependencyPanels = 14
				}
			}
			expected := []struct {
				sections []string
				charts   int
			}{
				{[]string{"Traffic and latency", "Workers and actor lifecycle", "Snapshots and persistence", "AKS resources and stability", "Telemetry pipeline"}, 20},
				{dependencySections, dependencyPanels},
			}
			for n, dashboard := range (App{Config: c}).monitoringDashboards(i) {
				panels := dashboard["panels"].([]object)
				var sections []string
				charts := 0
				ids := map[int]bool{}
				sectionEnd := 0
				for index, panel := range panels {
					id := panel["id"].(int)
					if id <= 0 || ids[id] {
						t.Fatalf("dashboard has a missing or duplicate panel ID: %v", panel)
					}
					ids[id] = true
					grid := panel["gridPos"].(object)
					x, y, w, h := grid["x"].(int), grid["y"].(int), grid["w"].(int), grid["h"].(int)
					if x < 0 || y < 0 || w <= 0 || h <= 0 || x+w > 24 {
						t.Fatalf("panel is outside Grafana's grid: %v", panel)
					}
					for _, previous := range panels[:index] {
						other := previous["gridPos"].(object)
						if x < other["x"].(int)+other["w"].(int) && x+w > other["x"].(int) &&
							y < other["y"].(int)+other["h"].(int) && y+h > other["y"].(int) {
							t.Fatalf("panels overlap: %s and %s", panel["title"], previous["title"])
						}
					}
					if panel["type"] == "row" {
						sections = append(sections, panel["title"].(string))
						if x != 0 || w != 24 || h != 1 || y != sectionEnd || panel["collapsed"] != false ||
							len(panel["panels"].([]object)) != 0 {
							t.Fatalf("section is not an expanded, full-width row above its charts: %v", panel)
						}
					} else {
						if len(sections) == 0 {
							t.Fatal("chart is outside a dashboard section")
						}
						charts++
						if panel["title"] == "Telemetry sources" && id != 1 ||
							panel["title"] == "gRPC request rate by method and status" && id != 3 ||
							panel["title"] == "Blob availability" && id != 1 {
							t.Fatalf("reordering broke an existing panel link: %v", panel)
						}
						if panel["title"] == "Snapshot p95 allocated size" || panel["title"] == "Container restarts" {
							if w != 24 {
								t.Fatalf("unpaired chart leaves a blank half-row: %v", panel)
							}
						}
					}
					sectionEnd = max(sectionEnd, y+h)
				}
				if charts != expected[n].charts || strings.Join(sections, "|") != strings.Join(expected[n].sections, "|") {
					t.Fatalf("unexpected dashboard sections or missing charts: %v, %d charts", sections, charts)
				}
			}
		})
	}
}
