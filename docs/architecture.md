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
| Upstream Substrate | Base manifests, CRDs, certificates, and workload deployment |
| [Azure components](../deploy/README.md) | Blob storage, ACR access, PostgreSQL, and demo scheduling |
| [Bicep](../infra) | AKS, ACR, Blob containers, identities, and permissions |
| Installer CLI | Configuration, approval, validation, sequencing, and diagnostics |

## Implemented today versus target architecture

Today, the installer needs an exact upstream source revision. It renders the
Azure components into a private copy of that source and invokes its `ate-setup`.
Images, runtime assets, and the client must match that revision.

The longer-term goal is to consume compatible upstream release artifacts
without rewriting a source checkout.

> [!IMPORTANT]
> There is no supported standalone Kustomize, Helm, or GitOps installation yet.
> `kubectl kustomize deploy` is not an installation command.

## Current deployment flow

```text
Pinned upstream manifests + Azure components + deployment settings
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
| Snapshot storage | Blob sidecars with separate node and control-plane identities |
| Actor image access | Native AKS ACR credential provider using the node's identity |
| PostgreSQL | One replica with a 32 GiB Azure Disk; no backups or HA |
| Telemetry | GKE monitoring removed; exporters disabled |
| Demo workers | Scheduled on the dedicated microVM node pool |

The counter ActorTemplate is a Substrate API document, not a Kubernetes resource.
Its snapshot URL and the sandbox runtime assets are adapted separately in Go.
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
fresh workstation are not implemented. Keep the configuration and local state:
the workspace contains more than disposable build cache.

See [configuration](configuration.md) for permissions and network exposure, and
[operations](operations.md#retry-and-teardown) for retries and deletion.
