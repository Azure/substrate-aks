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
| PostgreSQL | Bundled default: one replica with a 32 GiB Azure Disk; optional managed services use upstream's external-database contract |
| Telemetry | GKE monitoring removed; exporters disabled |
| Demo workers | Scheduled on the dedicated microVM node pool |

The counter ActorTemplate is a Substrate API document, not a Kubernetes resource.
Its snapshot URL and the sandbox runtime assets are adapted separately in Go.
Renderer inputs and compatibility checks live in
[`deploy/render.go`](../deploy/render.go) and
[`internal/installer/manifests.go`](../internal/installer/manifests.go).

Managed PostgreSQL provisioning stays in conditional Bicep modules, excluded
from registry bootstrap. A static public IP is the explicit AKS outbound IP;
only it is allowed through the database firewall. The cluster's user-assigned
identity receives Network Contributor on that IP before AKS creation, avoiding
a dependency cycle with a system-assigned cluster identity.

HorizonDB deployment uses API `2026-05-01-preview`, with compute and password
authentication on the cluster and an implicit `DefaultPool` for its firewall.
The host comes from the cluster's service-generated FQDN. A constrained
`horizonCreateMode` parameter flows from `main.bicep` through `resources.bicep`
to the leaf's `createMode`. The installer selects Create/Update using live
owned-cluster metadata, including resources left by partial deployments.
The Bicep modules themselves perform no installer-specific state discovery:
[direct callers](infrastructure.md) supply the same parameters independently.

Each full infrastructure deployment has a unique installation-scoped attempt
name. Resource discovery selects the latest successful full attempt in Azure,
including the legacy unsuffixed deployment name, then validates ownership and
outputs. A failed rerun cannot overwrite the previous successful subscription
deployment record. Vault/registry-only attempts are excluded from discovery,
but failed full attempts still prevent generating replacement credentials.
Missing successful outputs are an explicit diagnostic error; there is no
local metadata cache or assumption that missing outputs mean deleted resources.

Managed modes first reconcile an installer-owned Key Vault and a vault-scoped
Secrets Officer grant for the configured operator identity. A bounded,
cancellable metadata-only readiness probe tolerates initial RBAC/DNS propagation
without exposing password values. The vault is the
durable password authority; the full ARM deployment receives a Key Vault secret
reference, not plaintext parameters. Previews use a public, nonsecret placeholder
through stdin, retain Azure error diagnostics, and create neither the vault nor
its secret. Registry-only mode
creates neither.

The declarative `deploy/database-bootstrap.yaml` Job runs the pinned upstream
psql script before API deployment. Credentials are retrieved into installer
memory and passed through protected inputs, without local password files.
The CLI uses upstream's existing external
DSN settings, Secret reconciliation and rollout annotation, rather than patching
the API after install or adding a second settings authority. The seven adapted
source files and bundled fingerprints remain unchanged. Managed resource,
endpoint and schema identity are included in installation fingerprints.

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
