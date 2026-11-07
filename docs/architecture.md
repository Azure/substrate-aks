# Installation architecture

[Quickstart](../README.md) | [Configuration](configuration.md) | [Operations](operations.md)

## Design choice: declarative provider configuration

Bicep defines the Azure infrastructure. Kustomize components define the Azure
workload settings. The CLI connects them: it collects settings, previews changes,
requests approval, provisions resources, and runs the installation.

Keeping workload settings in YAML makes them reviewable without reading the
installer code.

| Layer | Responsibility |
| --- | --- |
| Upstream Substrate | Base manifests, CRDs, certificates, release images, and workload deployment |
| [Azure Blob plugin](https://github.com/agent-substrate-plugins/objectstore-azure-blob) | Object-store sidecars, their workload identity, and their kustomize components |
| [Azure components](../deploy/README.md) | ACR access, PostgreSQL, telemetry, and demo scheduling |
| [Bicep](../infra) | AKS, ACR, Blob containers, identities, and permissions |
| Installer CLI | Configuration, approval, validation, sequencing, and diagnostics |

## Implemented today versus target architecture

Today, the installer needs exact upstream and plugin commits; by default the
upstream commit is a tagged release. It renders the plugin's components and the
Azure components into a private copy of that source and invokes its
`ate-setup`. By default it imports the release's published images; runtime
assets and the client must match the same revision.

`ate-setup` still assumes Google Cloud Storage on any cluster other than kind,
so the installer applies the microVM SandboxConfig itself and passes
placeholder values for the GCS snapshot transfer account, which only the
replaced in-tree sidecar would read. Upstream
[agent-substrate/substrate#3651](https://github.com/agent-substrate/substrate/pull/3651)
proposes `--storage-backend=external` and `--component` for out-of-tree
providers; the installer should move to them once released, without rewriting a
source checkout.

> [!IMPORTANT]
> There is no supported standalone Kustomize, Helm, or GitOps installation yet.
> `kubectl kustomize deploy` is not an installation command.

## Current deployment flow

```text
Pinned upstream manifests + plugin components + Azure components + settings
                              |
                     In-memory rendering
                              |
                    Managed source checkout
                              |
                     Upstream ate-setup
                              |
                     Kubernetes workloads
```

The components are embedded in the binary; no external Kustomize executable is
needed. Upstream `ate-setup` handles the base installation and component redeploys.

| Azure setting | Result |
| --- | --- |
| Snapshot storage | The plugin's Blob sidecars with separate node and control-plane identities |
| MicroVM assets | Content-addressed blobs in the assets container, served by the node sidecar |
| Actor image access | Native AKS ACR credential provider using the node's identity |
| PostgreSQL | One replica with a 32 GiB Azure Disk; no backups or HA |
| Telemetry | GKE monitoring removed; exporters disabled |
| Demo workers | Scheduled on the dedicated microVM node pool |

The sandbox runtime assets are adapted separately in Go. The counter demo
stores snapshots under ate-api-server's cluster-wide snapshot location.
Renderer inputs and compatibility checks live in
[`deploy/render.go`](../deploy/render.go) and
[`internal/installer/manifests.go`](../internal/installer/manifests.go).

## Checks outside Kustomize

The installer checks resource ownership, image availability, asset checksums,
KVM support, certificates, and workload readiness. Rendering valid YAML alone
does not establish that the cluster can run microVMs.

Managed source checkouts are verified before reuse. Do not edit them; change
the source components instead. An incompatible upstream structure or unexpected
local edit stops installation rather than applying an unreviewed patch.

## Scope and lifecycle boundaries

Each installation gets a new cluster in a dedicated resource group. Teardown
deletes that entire group, not just Substrate. The installation ID links the
configuration, local state, and resource tags; it is not an access credential.

Existing-cluster installation, application-only uninstall, and recovery from a
fresh workstation are not implemented. Each installation's settings and local
state are saved by name in the user state directory; keep them while the
installation exists, because they contain more than disposable build cache.

See [configuration](configuration.md) for permissions and network exposure, and
[operations](operations.md#retry-and-teardown) for retries and deletion.
