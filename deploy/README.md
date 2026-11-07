# Azure deployment components

These Kustomize components configure Substrate for Azure. The installer embeds
them, fills in values from your deployment, and passes the result to upstream
`ate-setup`.

| Component | Purpose |
| --- | --- |
| `actor-images` | Pull private actor images from ACR with the node's identity |
| `postgres` | Run PostgreSQL on a 32 GiB Azure Disk |
| `telemetry` | Disable the GKE-specific monitoring configuration |
| `demo` | Place microVM workers on the dedicated node pool |

The object-store sidecars are configured by the
[Azure Blob plugin's own components](https://github.com/agent-substrate-plugins/objectstore-azure-blob#deployment),
which the installer fetches at its pinned commit, fills with the installation's
identities, account and containers, and renders before these.

**This is not a standalone installation package.** The templates need the
[pinned upstream source](../docs/configuration.md#source-and-image-settings) and
deployment values. Use the [installer](../README.md#quickstart), not
`kubectl kustomize deploy`. [#25](https://github.com/Azure/substrate-aks/issues/25) tracks a standalone Component,
kustomization, and Helm chart.

## Working on the components

Edit the YAML under [`components/`](components). The renderer is in
[`render.go`](render.go); its Go API comments describe the required inputs.
[`internal/installer/manifests.go`](../internal/installer/manifests.go) checks
upstream compatibility and handles inputs that are not Kubernetes resources.

When changing patches or the source pin, run the
[development checks](../CONTRIBUTING.md#development-checks), including the
pinned-source contracts. Patches are tied to the reviewed upstream structure;
a pin update can require adapter changes.

See [architecture](../docs/architecture.md) for the deployment flow and the
current limits on using these components outside the CLI.
