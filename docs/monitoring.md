# Managed metrics and Grafana

[Configuration](configuration.md) | [Operations](operations.md) | [Infrastructure](infrastructure.md)

Monitoring is an **opt-in, installation-wide metrics capability**, independent
of demo workloads or benchmarks. It does not make this experimental installer
a production-ready deployment.

## Enable on a new installation

```sh
bin/substrate-aks init monitored --monitoring
bin/substrate-aks setup monitored
```

`setup monitored --monitoring` also works when creating a new named installation.
For a saved installation, settings come from its `installation.json` record in
the [per-user state directory](configuration.md#create-a-configuration), not
setup flags. Before any provisioning, the equivalent JSON is:

```json
"monitoring": {
  "enabled": true,
  "location": "eastus2"
}
```

`location` is optional and defaults to `azure.location`. Use
`--monitoring-location REGION` when creating an installation to choose another region
supporting both Azure Monitor workspaces and Managed Grafana. The CLI validates
region names; actual service availability, quota, and API acceptance are checked
by Azure during deployment. No alternate region is selected silently.

Old configs without `monitoring` keep exporters disabled and create no monitoring
resources. Monitoring mode and region cannot change after the resource group is
provisioned, including registry bootstrap. Use a fresh installation for this
milestone; enabling monitoring on an existing installation is not an upgrade path.

Register `Microsoft.Monitor`, `Microsoft.Insights`, and `Microsoft.Dashboard` in
the target subscription if required by your subscription's provider policy.
The deployment principal needs resource creation and role-assignment permissions.

## What is provisioned

```text
Substrate components -- OTLP/gRPC --> metrics-only OTel collector
ateworker -- atelet OTLP relay -----> metrics-only OTel collector
                                              |
                         Azure managed Prometheus agent scrapes
                           collector metrics + self-metrics
                           Envoy /stats/prometheus + AKS targets
                                              |
                                    Azure Monitor workspace
                                              |
                                       Managed Grafana
                                              |
                       Azure Monitor data source also reads
                       native Blob and selected PostgreSQL metrics
```

Bicep creates an Azure Monitor workspace, data collection endpoint/rule and AKS
association, and Standard X1 Managed Grafana (Grafana 12). AKS's managed
Prometheus metrics profile is enabled. Registry-only deployments create none of
these resources or Kubernetes agents.

The configured `azure.assetPublisherObjectId` receives **Grafana Admin on this
Grafana instance**. Run installation as that identity; the independently selected
AKS operator is not automatically granted Grafana access. Add other viewers
through Azure IAM using Grafana Viewer. Grafana's managed identity receives
Monitoring Data Reader on the metrics workspace and Monitoring Reader on the
installation's dedicated resource group, not on the subscription. No database
login, Blob data-read role, Grafana API key, or service-principal password is needed.
Grafana and collection endpoints are publicly reachable and authenticated;
private networking is not configured in this milestone.

The installer deploys a bounded, non-root collector in `ate-system` and Azure
`azmonitoring.coreos.com/v1` PodMonitors. It restores OTLP metrics on control-plane
components and workers while keeping OTLP logs and traces disabled. Upstream's
GKE-only PodMonitoring resource remains removed. Component endpoints are not
also scraped for the same application metrics, avoiding duplicate ingestion.

Upstream defaults to denying ingress in `ate-system`. A collector NetworkPolicy
allows OTLP from the control-plane component pods in that namespace, including
atelet's worker relay. It also allows the `ama-metrics` pods in `kube-system`
to scrape application and self-metrics. These rules are additive to upstream's
existing observability policy.

Instance IDs are retained to keep different processes' counters separate. Only
reviewed resource attributes are promoted to metric labels. Upstream metric
attributes remain intact. Source health queries labeled application metrics,
rather than assuming collector `target_info` series survive managed ingestion.
Exponential controller-runtime histograms are projected
to explicit latency buckets; their quantiles are approximate. Ordinary upstream
explicit histograms retain their boundaries.

## Dashboards and installation checks

`install` creates or updates two installer-owned dashboards and their data sources
through Grafana's API, authenticating with the current Azure CLI login:

The [Grafana data-plane token audience](https://learn.microsoft.com/en-us/azure/managed-grafana/how-to-authenticate-data-plane-api)
is `https://dashboard.azure.com`, distinct from the workspace's
`*.grafana.azure.com` hostname.

Telemetry acceptance uses Grafana's backend `/api/ds/query` API, which applies
the Prometheus plugin's managed-identity authentication. The generic data-source
proxy does not apply that authentication and must not be used for this check.

| Dashboard | Includes |
| --- | --- |
| Substrate and AKS | Telemetry sources, collector health, gRPC traffic/status/latency, routing and Envoy latency, lifecycle failures, restore/checkpoint phases, worker readiness/capacity, snapshot size, pod resources/restarts and node utilization |
| Azure dependencies | Blob availability/latency/traffic and native metrics for the provisioned PostgreSQL resource, in its actual region |

Panels are grouped into collapsible sections, expanded by default. Substrate and
AKS starts with traffic and latency, then workers and actor lifecycle, snapshots
and persistence, AKS resources and stability, and finally the telemetry pipeline.
The Azure dashboard puts the selected database's health/capacity and connection
activity before Blob storage. Related charts are paired; unpaired charts span
the full width. Existing chart IDs are retained so panel links survive reordering.

Flexible Server panels show CPU, memory, connections, failed connections, IOPS
and storage. HorizonDB panels use its separate health, CPU/memory, connection,
query, commit latency and storage schema. Metric names, aggregations and units
follow the [Flexible Server](https://learn.microsoft.com/en-us/azure/azure-monitor/reference/supported-metrics/microsoft-dbforpostgresql-flexibleservers-metrics)
and [HorizonDB](https://learn.microsoft.com/en-us/azure/azure-monitor/reference/supported-metrics/microsoft-horizondb-clusters-metrics)
platform references. Bundled PostgreSQL is covered by pod resource and
restart panels, not managed-database metrics or an SQL exporter.

Dashboard UIDs and data-source UIDs beginning `substrate-aks-` are installer-owned.
Reinstalling reconciles them. Clone dashboards under another UID for customization.
The final install output, setup receipt and `status NAME` show the Grafana link,
including when setup runs in the interactive workspace.

Installation waits for the collector rollout and verifies through Grafana that
managed Prometheus reports a healthy collector scrape, a healthy Envoy scrape,
and controller telemetry. Idle services need not emit request histograms:
those panels populate as traffic arrives. A successful ARM deployment alone
is not treated as verified telemetry. Query/import failures remain errors;
missing metrics are never silently replaced with zero.

Initial Grafana RBAC propagation can take five minutes; metric ingestion is
allowed ten minutes. If installation fails, retain the original configuration,
inspect its diagnostic log, and retry `install NAME`. An unsuccessful monitoring
stage does not mark the Substrate installation completed.

For investigation, after applying the printed kubeconfig exports:

```sh
kubectl -n ate-system get deploy substrate-aks-otel
kubectl -n ate-system logs deploy/substrate-aks-otel
kubectl -n ate-system get podmonitors.azmonitoring.coreos.com
kubectl -n kube-system get pods -l rsName=ama-metrics
```

Check the DCR association, Grafana managed identity's reader roles, and the Azure
Monitor workspace if targets never arrive. `No data` can mean missing collection,
no matching workload activity, propagation delay, or insufficient permissions.
Consult source/collector health panels before interpreting latency.

## Costs, lifecycle and limits

Standard Grafana adds ongoing charges even without traffic. Prometheus adds
ingestion/query charges, and collection consumes AKS CPU/memory. Native database
metrics are generally minute-resolution and do not resolve short latency spikes.
Workspace retention follows the managed service's policy; the installer does not
configure a separate archive or custom retention policy.

**Destroying the resource group deletes Grafana, the metrics workspace and its
stored telemetry.** Export information that must survive teardown first.
Shared/existing workspace adoption and independent retention lifecycles are not
implemented.

Azure creates a separate managed resource group for the metrics workspace.
Like AKS's managed node group, it can inherit the installation tag. The target
guard recognizes workspace-managed groups in this installation's subscription
and resource group while the tagged primary group exists. If a managed group
remains after the primary group disappears, wait for teardown to finish; do not
reuse the installation ID or reset image references to bypass that check.

The in-cluster collector is one replica with an in-memory metrics exporter,
100m CPU/256 MiB memory requested and a 512 MiB memory limit. It is not HA and
has no persistent queue. Its refusal counters and resource usage must guide
sizing before production use. Only trusted, single-tenant workloads are in scope.
Actor SDK telemetry, per-actor correlation, logs/traces backends, SQL exporters,
alert policies, SLOs and benchmark automation are outside this milestone.

## Validation

`make test` checks opt-in/default behavior, lifecycle errors, Grafana
reconciliation, backend-specific dashboards and both telemetry modes against
the pinned upstream manifests. `make bicep` compiles the complete infrastructure.
`make test-monitoring` additionally requires Docker: it runs the exact pinned
collector image with non-root/read-only restrictions, injects OTLP metrics, and
checks histogram conversion, process identity separation and self-monitoring.
The test adds an HTTP receiver only for injection; installation uses OTLP/gRPC.

These are local checks, **not evidence of a live Azure monitoring deployment**.
Service availability, IAM propagation and real Azure/Grafana ingestion must be
confirmed by a fresh opt-in installation and its runtime checks.
