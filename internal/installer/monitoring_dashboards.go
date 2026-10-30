package installer

import "strings"

type monitoringSection struct {
	title  string
	panels []object
}

func monitoringDashboard(uid, title string, sections []monitoringSection) object {
	nextID := 1
	for _, section := range sections {
		for _, panel := range section.panels {
			nextID = max(nextID, panel["id"].(int)+1)
		}
	}
	var panels []object
	y := 0
	for _, section := range sections {
		panels = append(panels, object{
			"id": nextID, "title": section.title, "type": "row",
			"collapsed": false, "panels": []object{},
			"gridPos": object{"x": 0, "y": y, "w": 24, "h": 1},
		})
		nextID++
		y++
		for n, panel := range section.panels {
			width := 12
			if n == len(section.panels)-1 && n%2 == 0 {
				width = 24
			}
			panel["gridPos"] = object{"x": (n % 2) * 12, "y": y + (n/2)*8, "w": width, "h": 8}
			panels = append(panels, panel)
		}
		y += ((len(section.panels) + 1) / 2) * 8
	}
	return object{
		"uid": uid, "title": title, "schemaVersion": 39, "version": 1,
		"tags": []string{"substrate-aks", "installer-managed"}, "editable": false,
		"time": object{"from": "now-1h", "to": "now"}, "refresh": "30s",
		"links": []object{
			{"title": "Substrate and AKS", "type": "link", "url": "/d/substrate-aks-overview", "keepTime": true},
			{"title": "Azure dependencies", "type": "link", "url": "/d/substrate-aks-dependencies", "keepTime": true},
		},
		"panels": panels,
	}
}

func metricsPanel(id int, title, unit, description string, source object, target object) object {
	target["refId"], target["datasource"] = "A", source
	return object{
		"id": id, "title": title, "description": description, "type": "timeseries",
		"datasource": source, "targets": []object{target},
		"fieldConfig": object{"defaults": object{"unit": unit, "noValue": "No data"}, "overrides": []any{}},
	}
}

func (a App) monitoringDashboards(i Infrastructure) []object {
	prometheus := object{"type": "prometheus", "uid": prometheusDataSource}
	type applicationMetric struct {
		id                             int
		title, unit, description, expr string
	}
	var sections []monitoringSection
	for _, group := range []struct {
		title   string
		metrics []applicationMetric
	}{
		{"Traffic and latency", []applicationMetric{
			{3, "gRPC request rate by method and status", "reqps", "Status is retained rather than assuming a backend-specific success encoding.",
				`sum by (service_name, rpc_method, rpc_response_status_code) (rate(rpc_server_call_duration_seconds_count[5m]))`},
			{4, "gRPC p95 latency", "s", "Server-side request duration, by method.",
				`histogram_quantile(0.95, sum by (le, service_name, rpc_method) (rate(rpc_server_call_duration_seconds_bucket[5m])))`},
			{6, "Envoy full-round-trip p95 latency", "ms", "Includes actor work and response time; not interchangeable with routing duration.",
				`histogram_quantile(0.95, sum by (le) (rate(envoy_http_downstream_rq_time_bucket{envoy_http_conn_manager_prefix=~"ingress_.*"}[5m])))`},
			{5, "Routing p95 latency", "s", "Substrate routing only: excludes actor computation and the response.",
				`histogram_quantile(0.95, sum by (le, ate_router_resume) (rate(atenet_router_route_duration_seconds_bucket[5m])))`},
		}},
		{"Workers and actor lifecycle", []applicationMetric{
			{11, "Workers by state", "short", "Pool namespace and name identify a pool together.",
				`sum by (ate_workerpool_namespace, ate_workerpool_name, ate_worker_state) (ate_workerpool_workers)`},
			{12, "Worker readiness gap", "short", "Desired minus ready workers; a sustained positive gap indicates undelivered capacity.",
				`sum by (ate_workerpool_namespace, ate_workerpool_name) (ate_workerpool_desired_workers) - sum by (ate_workerpool_namespace, ate_workerpool_name) (ate_workerpool_ready_workers)`},
			{7, "Actor lifecycle p95 latency", "s", "Includes success and failure; use the failure-rate panel to interpret it.",
				`histogram_quantile(0.95, sum by (le, ate_actor_operation_name) (rate(ate_actor_lifecycle_operation_duration_seconds_bucket[5m])))`},
			{8, "Actor lifecycle failures", "ops", "Only recorded failures. An absent series is not proof of successful collection.",
				`sum by (ate_actor_operation_name, error_type) (rate(ate_actor_lifecycle_operation_duration_seconds_count{error_type!=""}[5m]))`},
		}},
		{"Snapshots and persistence", []applicationMetric{
			{9, "Restore phase p95 latency", "s", "Phases overlap; do not sum their durations or percentiles. Use total for overall latency.",
				`histogram_quantile(0.95, sum by (le, ate_snapshot_phase) (rate(ate_actor_restore_duration_seconds_bucket[5m])))`},
			{10, "Checkpoint phase p95 latency", "s", "Phases overlap; do not sum their durations or percentiles.",
				`histogram_quantile(0.95, sum by (le, ate_snapshot_phase) (rate(ate_actor_checkpoint_duration_seconds_bucket[5m])))`},
			{13, "Snapshot p95 allocated size", "bytes", "Allocated snapshot image size, not transferred wire bytes.",
				`histogram_quantile(0.95, sum by (le, file_name) (rate(atelet_snapshot_size_bytes_bucket[5m])))`},
		}},
		{"AKS resources and stability", []applicationMetric{
			{14, "Substrate container CPU", "cores", "Kubernetes cAdvisor metrics, including bundled PostgreSQL when selected.",
				`sum by (pod) (rate(container_cpu_usage_seconds_total{namespace="ate-system",container!="",container!="POD"}[5m]))`},
			{15, "Substrate container working set", "bytes", "Includes the metrics collector and bundled PostgreSQL.",
				`sum by (pod) (container_memory_working_set_bytes{namespace="ate-system",container!="",container!="POD"})`},
			{17, "Node CPU utilization", "percentunit", "Per-node utilization from the managed AKS node scrape.",
				`1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m]))`},
			{18, "Node memory utilization", "percentunit", "Available memory compared with total node memory.",
				`1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes`},
			{16, "Container restarts", "short", "Restarts over the last hour; absence of data remains visible.",
				`sum by (pod) (increase(kube_pod_container_status_restarts_total{namespace="ate-system"}[1h]))`},
		}},
		{"Telemetry pipeline", []applicationMetric{
			{1, "Telemetry sources", "short", "Observed services, retaining process identity. Idle request-only sources can be absent; check workload and scrape health before interpreting missing telemetry.",
				`count by (service_name) (count by (service_name, service_instance_id) (workqueue_depth or rpc_server_call_duration_seconds_count or ate_workerpool_workers or atenet_router_route_duration_seconds_count or ate_actor_lifecycle_operation_duration_seconds_count or atelet_snapshot_size_bytes_count))`},
			{2, "Collector scrape health", "short", "1 means reachable; 0 means a failed scrape; no data means the scrape target or ingestion is missing.",
				`min by (pod) (up{namespace="ate-system",pod=~"substrate-aks-otel-.*"})`},
			{19, "Collector accepted datapoints", "ops", "Positive control: a quiet error counter is insufficient if nothing is being received.",
				`sum(rate(otelcol_receiver_accepted_metric_points[5m]))`},
			{20, "Collector refused datapoints", "ops", "Memory pressure or rejected telemetry. A missing series is not automatically zero.",
				`sum(rate(otelcol_receiver_refused_metric_points[5m]))`},
		}},
	} {
		section := monitoringSection{title: group.title}
		for _, spec := range group.metrics {
			section.panels = append(section.panels, metricsPanel(spec.id, spec.title, spec.unit, spec.description, prometheus,
				object{"expr": spec.expr, "range": true}))
		}
		sections = append(sections, section)
	}
	overview := monitoringDashboard("substrate-aks-overview", "Substrate AKS: service and cluster health", sections)

	azure := object{"type": "grafana-azure-monitor-datasource", "uid": platformDataSource}
	type platformMetric struct {
		id                             int
		title, name, unit, aggregation string
	}
	var dependencies []monitoringSection
	addPlatform := func(title, namespace, resourceName, region string, metrics []platformMetric) {
		section := monitoringSection{title: title}
		for _, metric := range metrics {
			section.panels = append(section.panels, metricsPanel(metric.id, metric.title, metric.unit,
				"Native Azure platform metric. No data may mean no activity, ingestion delay, or missing permissions; it is not replaced with zero.",
				azure, object{
					"queryType": "Azure Monitor", "subscription": a.Config.Azure.SubscriptionID,
					"azureMonitor": object{
						"metricNamespace": namespace, "metricName": metric.name, "aggregation": metric.aggregation,
						"timeGrain": "auto", "region": region,
						"resources": []object{{
							"subscription": a.Config.Azure.SubscriptionID, "resourceGroup": i.ResourceGroup,
							"resourceName": resourceName, "metricNamespace": namespace, "region": region,
						}},
					},
				}))
		}
		dependencies = append(dependencies, section)
	}
	if i.Database != nil {
		parts := strings.Split(i.Database.ResourceID, "/")
		name := parts[len(parts)-1]
		if i.Database.Backend == DatabaseFlexible {
			addPlatform("PostgreSQL health and capacity", "Microsoft.DBforPostgreSQL/flexibleServers", name, i.Database.Location, []platformMetric{
				{7, "PostgreSQL CPU", "cpu_percent", "percent", "Average"},
				{8, "PostgreSQL memory", "memory_percent", "percent", "Average"},
				{11, "PostgreSQL IOPS", "iops", "iops", "Average"},
				{12, "PostgreSQL storage used", "storage_used", "bytes", "Maximum"},
			})
			addPlatform("PostgreSQL connections", "Microsoft.DBforPostgreSQL/flexibleServers", name, i.Database.Location, []platformMetric{
				{9, "PostgreSQL connections", "active_connections", "short", "Maximum"},
				{10, "PostgreSQL failed connections", "connections_failed", "short", "Total"},
			})
		} else {
			addPlatform("HorizonDB health and capacity", "Microsoft.HorizonDB/clusters", name, i.Database.Location, []platformMetric{
				{8, "HorizonDB CPU", "CpuUtilization", "percent", "Average"},
				{9, "HorizonDB memory", "MemoryUtilization", "percent", "Average"},
				{7, "HorizonDB instance health", "PostgreSqlInstanceIsAlive", "short", "Minimum"},
				{14, "HorizonDB storage used", "StorageUsed", "bytes", "Maximum"},
			})
			addPlatform("HorizonDB connections and queries", "Microsoft.HorizonDB/clusters", name, i.Database.Location, []platformMetric{
				{10, "HorizonDB connections", "ActiveConnections", "short", "Maximum"},
				{11, "HorizonDB failed connections", "FailedConnections", "short", "Total"},
				{12, "HorizonDB peak query rate", "Queries", "qps", "Maximum"},
				{13, "HorizonDB commit latency", "CommitLatency", "\u00b5s", "Maximum"},
			})
		}
	}
	addPlatform("Blob storage", "Microsoft.Storage/storageAccounts/blobServices", i.StorageAccount+"/default", a.Config.Azure.Location, []platformMetric{
		{1, "Blob availability", "Availability", "percent", "Average"},
		{4, "Blob transactions", "Transactions", "short", "Total"},
		{2, "Blob service latency", "SuccessServerLatency", "ms", "Average"},
		{3, "Blob end-to-end latency", "SuccessE2ELatency", "ms", "Average"},
		{5, "Blob ingress", "Ingress", "bytes", "Total"},
		{6, "Blob egress", "Egress", "bytes", "Total"},
	})
	return []object{overview, monitoringDashboard("substrate-aks-dependencies", "Substrate AKS: Azure storage and PostgreSQL", dependencies)}
}
