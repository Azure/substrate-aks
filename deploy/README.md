# Experimental Azure declarative package

For the design rationale and current-versus-target installation model, see
[installation architecture](../docs/architecture.md). This document covers the
component and renderer contracts for contributors.

These embedded Kustomize components adapt **Substrate
`c3b44aa46312e073a6384f68723a7b14dffd5518`**. They are not a standalone
installer and do not fetch upstream resources. `internal/installer/manifests.go`
checks the upstream contracts and supplies runtime values; `Render` builds each
surface in memory using the pinned Kustomize Go API, with no shell execution.
Runtime strings are JSON-quoted (valid YAML scalars), not interpolated as YAML
structure.

* `storage`: native Azure Blob sidecars, projected Entra identity tokens, and
  node ACR credential-provider configuration. Existing upstream security,
  storage mounts, resources, service accounts, and mTLS settings are retained.
  The ACR provider uses AKS node credentials, separately from the sidecar's
  federated identity.
* `postgres`: small persistent `managed-csi` disk and bounded resources, retaining
  upstream authentication, certificates, reload sidecar, and claim access modes.
* `telemetry`: removes the GKE collector configuration and disables control-plane
  and worker exporters until an Azure observability path is supported.
* `demo`: schedules microVM workers onto the dedicated runtime pool without
  discarding other selectors.
* `base-monitoring.patch.yaml`: removes the pinned GKE-only monitoring reference
  from the upstream Kustomization itself, guarded by a JSON patch `test`.

Rendered output replaces the **same seven managed-source files** consumed by
upstream `ate-setup` full installation and component redeploy. No additional
overlay command is needed at deployment time. The counter ActorTemplate is an
ate API protobuf-shaped document, **not a Kubernetes resource**; its narrow
snapshot URL adaptation stays explicit and outside Kustomize.

Positional JSON patches intentionally depend on the reviewed pin. Go validation
checks atelet's first container and leading credential flags; the patches also
test those positions and the first PostgreSQL claim's name. Drift must fail
rather than silently patching a different object. Pin upgrades require reviewing
these contracts and running the integration test against a pristine checkout,
as well as the focused unit tests.

## Use without the installer CLI

The package takes upstream YAML bytes plus `deploy.Values`. Call `deploy.Render`
with one component at a time:

| Component | Input bytes | Values |
| --- | --- | --- |
| `storage` | Upstream `atelet.yaml` or `ate-api-server.yaml` | `Name`, `Kind`, `Mode`, `PluginImage`, `ClientID`, `TenantID`, `AccountURL`, `Snapshots`; also `Assets` for node mode |
| `postgres` | Upstream `postgres/postgres.yaml` | `Component` only |
| `telemetry` | Upstream `ate-otel-config.yaml` | `Component` only |
| `demo` | Upstream `counter-microvm.yaml.tmpl` WorkerPool | `NodeSelectorExists` must reflect whether the input template already has a selector |

For storage, use `(Name, Kind, Mode) = ("atelet", "DaemonSet", "node")`
or `("ate-api-server", "Deployment", "control")`. `ClientID` is the matching
federated identity's client ID, not the AKS node credential-provider identity.
Set `CredentialConfigExists` when a node input already includes the generated
`substrate-aks-acr-credentials` ConfigMap. Keep `NodeSelectorExists` false when
the demo has no selector; set it true to preserve existing selectors.
The installer derives these presence values and checks additional upstream
contracts before rendering. Direct callers must supply the reviewed upstream
input and correct values; this API does not validate Azure identifiers or pins.

`deploy.AdaptBase` takes the upstream base `kustomization.yaml` bytes and returns
the guarded monitoring-reference edit. The proto-shaped ActorTemplate is not
supported by `Render`.

Output is ordinary Kubernetes YAML. Save each result to its original upstream
path to retain `ate-setup` full-install and component-redeploy behavior, or use
the output in a separate, locally assembled Kustomization. The checked-in
component files contain Go template expressions; `kubectl kustomize deploy`
is not a supported standalone entrypoint.

All patches, templates, and credential-provider configuration ship inside the
Go binary. Rendering needs neither files from this package on disk nor kubectl,
a shell, network access, or cloud credentials. Callers still need the pinned
upstream input locally. Downloading that checkout, building images, pulling
images, and deploying to a cluster are separate operations.
