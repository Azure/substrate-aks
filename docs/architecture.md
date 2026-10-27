# Installation architecture

[Quickstart](../README.md) | [Configuration](configuration.md) | [Operations](operations.md)

## Design choice: declarative provider configuration

**Azure workload configuration belongs in provider-owned Kustomize components,
not a parallel imperative manifest generator.** The CLI orchestrates provisioning,
validation, and deployment; it should not become the only way to express the
desired Kubernetes configuration.

This follows the direction of the extensible-install proposal: cloud-neutral
upstream release manifests, Kustomize as the main provider extension mechanism,
and upstream Helm packaging. It is a design direction, not a claim that this
repository already offers a standalone declarative installation.

| Layer | Responsibility |
| --- | --- |
| Upstream Substrate | Cloud-neutral manifests, CRDs, bootstrap resources, compatible release artifacts, and Helm packaging |
| Azure provider package | Kustomize components for storage, identity, registry integration, database settings, and demo scheduling |
| Azure infrastructure | Bicep resources for AKS, ACR, Blob storage, identities, and role assignments |
| Installer CLI | Configuration, consent, previews, prerequisites, runtime values, deployment sequencing, and diagnostics |

**Why this split:** Kubernetes settings remain reviewable as YAML, Azure-specific
choices stay out of the upstream base, and a wizard does not introduce a second
definition of the workloads. Cloud provisioning and operational safety checks
remain separate from manifest composition.

## Implemented today versus target architecture

| Area | Today | Target |
| --- | --- | --- |
| Workload configuration | Embedded Azure Kustomize components | Provider-owned components consuming supported upstream release manifests |
| Manifest inputs | Exact source commit and provisioned runtime values | Versioned, cloud-neutral upstream installation artifacts |
| Rendering | Kustomize Go API, in memory | A documented declarative composition contract usable outside the wizard |
| Deployment | Materialized source files consumed by pinned `ate-setup` | Supported declarative installation; `ate-setup` primarily for developer workflows |
| Images and assets | Build matching images and verify runtime assets from the pin, or supply compatible artifacts | Released, compatible image/plugin/asset/client sets |

> [!IMPORTANT]
> **The current package is not a standalone `kubectl apply -k` installation,
> Helm chart, or supported GitOps entrypoint.** The embedded files require
> pinned upstream inputs and runtime values. Do not run `kubectl kustomize deploy`
> as an installation command.

The source bridge is temporary compatibility code, not an extension framework
to expand indefinitely. Replacing it depends on supported upstream contracts;
this document does not define those contracts on upstream's behalf.

## Current deployment flow

```text
Pinned upstream YAML + Azure Kustomize components + provisioned values
                                  |
                     Embedded, in-memory renderer
                                  |
                 Managed source checkout materialization
                                  |
                     Pinned upstream ate-setup
                                  |
                    Kubernetes resources and rollout
```

The binary embeds [`deploy/components`](../deploy/components) and invokes the
Kustomize Go API. Rendering needs neither an external renderer process nor this
repository's files on disk. Values such as identity IDs, storage endpoints, and
plugin digests are supplied by the installer.

The compatibility bridge writes the rendered workload files at the same seven
entry points used by full installation, component redeployment, and demo setup.
Upstream `ate-setup` still handles certificates, CRDs, control-plane installation,
and demo deployment. We do not deploy an incompatible base and patch its sidecars
afterward, select `--kind`, or introduce local S3 storage.

### Azure configuration surfaces

| Surface | Azure-specific configuration |
| --- | --- |
| Storage | Blob sidecars, probes, separate federated identities, and projected tokens |
| Registry | Read-only mounts of the native AKS ACR credential provider and node configuration, plus its credential-provider ConfigMap |
| PostgreSQL | One replica, 32 GiB Azure Disk, 250m CPU and 1 GiB memory requests; no backups or HA |
| Telemetry | Disable exporters and remove the GKE-only monitoring resource |
| Demo | Schedule microVM workers on the dedicated node pool |

Not every installation input is a Kubernetes overlay. The counter ActorTemplate
is a Substrate API document, not a Kubernetes resource; its snapshot URL is
adapted separately. Sandbox runtime asset wiring also retains a narrow Go
adapter. These are explicit remaining boundaries, not evidence of a complete
cloud-neutral upstream package.

See [`deploy/README.md`](../deploy/README.md) for component inputs, renderer APIs,
and the checks required when changing the source pin.

## Checks outside Kustomize

Declarative composition does not replace resource ownership guards, cost
approval, asset checksum verification, KVM probes, certificate readiness, or
rollout diagnostics. Those remain installer responsibilities.

The renderer and installer check the reviewed upstream contract before applying
patches. Positional patches include guarded expectations; an upstream rename or
structural change must fail instead of creating a second container or silently
reverting to GCP settings.

Managed checkouts have completion markers and adapted-file hashes. Reuse verifies
the source commit and rejects unexpected tracked changes. The CLI never modifies
a supplied working checkout. Do not edit its managed source cache.

## Scope and lifecycle boundaries

The current supported mode creates a new cluster in a dedicated resource group.
The installation ID correlates state, tags, and resources; it is not an
authentication boundary. Full group deletion is specific to this quickstart,
not a general ownership model for future existing-cluster support.

Existing-cluster installation, application-only uninstall, recoverable ownership
inventory, and fresh-workstation recovery remain future work. The local workspace
is currently operationally significant; it is not all safely disposable cache.

For current permissions, network exposure, and data-access boundaries, see
[configuration](configuration.md). For retries and destructive teardown, see
[operations](operations.md#retry-and-teardown).
