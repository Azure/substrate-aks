# Substrate on AKS

An AKS packaging layer for [Agent Substrate](https://github.com/agent-substrate/substrate),
providing Azure infrastructure, declarative deployment configuration, and a guided installer.

> [!WARNING]
> **Experimental, single-tenant, and billable.** This quickstart creates a new AKS
> cluster in a dedicated resource group. [Teardown](#tearing-down) deletes the
> entire group and all installation data. Do not add unrelated resources to it.

[Quickstart](#quickstart) · [What gets installed](#what-gets-installed) ·
[Installation design](#installation-design) ·
[Try the demo](#try-the-demo) · [Manage](#manage-your-installation) ·
[Tear down](#tearing-down) · [Documentation](#documentation)

## Quickstart

**Prerequisites**

| Requirement | Notes |
| --- | --- |
| Linux or macOS | Installer host; cluster workloads use Linux/amd64 |
| Go 1.27.3+ and Git | Allow automatic toolchain download or install a current supported security release |
| Azure CLI | Authenticated to the target subscription |
| `kubectl` 1.36–1.38 | Within one minor version of AKS 1.37 |
| Azure `kubelogin` | Required for new installs; uses the existing Azure CLI login for managed Entra authentication |
| Docker with buildx or Podman | Setup offers a choice when both work; `--yes` prefers Docker/buildx. Runtime assembly also needs `bash`, `curl`, `tar --zstd`, `zstd`, `readlink -f`, and `sha256sum`; supplied assets skip these checks |
| Azure permissions | Subscription-scope deployments, resource creation, and role assignments; Contributor alone is insufficient |
| AKS capacity | AKS 1.37, VM quota, and nested virtualization in your region |

```sh
git clone https://github.com/Azure/substrate-aks.git
cd substrate-aks
az login
az bicep install
make build
bin/substrate-aks setup
```

The wizard uses your active Azure subscription and identity, asks for a name and
region, and previews changes before applying them. Press `/` to filter regions.
It saves your settings in `deploy.json`.

**When setup finishes, apply the final shell exports it prints** to use the
installation's kubeconfig and `kubectl ate` client. Interactive setup offers an
opt-in context import/selection at the end; declining or `--yes` leaves your
default kubeconfig unchanged. Shell profiles and PATH are never edited.

**Useful variants**

| Command | Use |
| --- | --- |
| `az account set --subscription SUBSCRIPTION_UUID` | Select a different subscription **before** creating the config |
| `bin/substrate-aks setup --config deploy-dev.json` | Use a separate installation config |
| `bin/substrate-aks setup --accessible` | Screen-reader-friendly, line-oriented prompts |
| `bin/substrate-aks setup --container-engine=podman` | Force Podman instead of auto-detecting an image builder |
| `bin/substrate-aks setup --api-cidr YOUR_PUBLIC_IPV4/32` | Set API access explicitly instead of detecting your IP |
| `bin/substrate-aks setup --config deploy.json --yes --with-demo` | Install without prompts using a complete config; accepts billing |

Use `NO_COLOR=1` for unstyled output. See the
[configuration guide](docs/configuration.md) for identity overrides and region
validation, and [operations](docs/operations.md#guided-installation) for noninteractive behavior.

## What gets installed

| Component | Default |
| --- | --- |
| AKS | Kubernetes 1.37; one system node and one dedicated microVM node, `Standard_D4s_v3` |
| Operator access | New configs: managed Entra / Azure RBAC, local accounts disabled; cluster-scoped operator grant |
| Runtime | Kata / cloud-hypervisor microVMs; KVM is checked before installation |
| Substrate | API server, controller, networking, and atelet |
| Database | Bundled PostgreSQL with a 32 GiB Azure Disk; no HA or backups |
| Images | Installer-owned ACR; builds the pinned source and records matching image references |
| Storage | Azure Blob containers for snapshots and runtime assets; workload-identity access |
| Demo | Optional counter microVM workload |

> [!IMPORTANT]
> **This is not a production or multi-tenant deployment.** The AKS API is public
> and IP-restricted by default; Substrate has no public ingress. ACR and Blob
> endpoints are publicly reachable but require authentication. Actor image access
> and Blob permissions are installation-wide, not per actor or tenant.

Snapshot inspection is **opt-in and read-only**, scoped to the snapshots container.
Snapshots can contain actor memory, files, and secrets. Clearing the config field
does not revoke an existing grant; remove it in Azure IAM or tear down.

**Validated configuration:** AKS 1.37 with `Standard_D4s_v3` in `southcentralus`.
Other regions and SKUs still need availability, quota, and KVM validation; the
region selector checks names, not capacity.

<details>
<summary><strong>Source pin and current limits</strong></summary>

- The temporary Azure integration source is
  [`aramase/substrate`](https://github.com/aramase/substrate) at
  `c3b44aa46312e073a6384f68723a7b14dffd5518`, from upstream
  [PR #2595](https://github.com/agent-substrate/substrate/pull/2595).
- There is no default prebuilt image set. Setup builds matching images; do not
  mix the pinned manifests with arbitrary upstream release images.
- Existing-cluster installation, managed PostgreSQL, upgrades, autoscaling,
  dashboards, and shared filesystems are deferred, not permanent exclusions.
- Stateful suspend/resume was validated. Cross-node restoration from Blob and
  long-duration credential refresh remain unverified.

See the [source dependency](docs/configuration.md#source-and-image-settings) and
[validation summary](docs/operations.md#live-acceptance-and-limitations) for details.

</details>

## Installation design

**Declarative Azure configuration, with the CLI as orchestration.** Azure workload
settings live in provider-owned Kustomize components; Bicep describes the cloud
infrastructure. This follows the extensible-install proposal's direction of
cloud-neutral upstream manifests and provider-owned configuration.

Today, the CLI embeds and renders those components into a pinned source checkout
for `ate-setup`. A standalone Kustomize/Helm/GitOps install is **not yet available**.
See [installation architecture](docs/architecture.md) for the design, current
compatibility bridge, and target upstream/provider split.

## Try the demo

Apply setup's printed shell exports first. If you skipped the optional demo,
deploy it with `bin/substrate-aks demo`.

```sh
kubectl ate create actor my-counter \
  -a ate-demo-counter-microvm --template counter-microvm
kubectl ate get actor my-counter -a ate-demo-counter-microvm
kubectl ate suspend actor my-counter -a ate-demo-counter-microvm --mode=sync
kubectl ate resume actor my-counter -a ate-demo-counter-microvm
```

Follow the [upstream counter guide](https://github.com/agent-substrate/substrate/tree/main/demos/counter)
to call the actor over HTTP and compare its state before and after suspension.
See [client setup and authentication](docs/operations.md#client-and-counter-demo) for details.

## Manage your installation

All commands use `deploy.json` by default; add `--config PATH` for another config.

| Command | Purpose |
| --- | --- |
| `bin/substrate-aks init` | Create a config without Azure resources |
| `bin/substrate-aks doctor` | Check local tools; no Azure resource calls |
| `bin/substrate-aks plan` | Preview real infrastructure changes using Bicep what-if |
| `bin/substrate-aks status` | Inspect Substrate pods and persistent volume claims |
| `bin/substrate-aks client` | Install the matching `kubectl ate` and print shell exports |
| `bin/substrate-aks setup --verbose` | Show underlying commands and detailed output |

**Retry:** fix the reported error and rerun setup with the same config. Failures
retain resources and logs; there is no automatic rollback. Keep `deploy.json`
and its installation ID unchanged for retries and teardown.

**Saved settings win:** changing the active Azure subscription or passing setup
configuration flags does not retarget an existing config. If your public IP
changes, update `aks.apiAuthorizedCidrs`, then run `plan` and `provision`.

**Local files:** `.substrate-aks/` holds caches, kubeconfig, credentials, logs,
and deployment records. Keep it private. Deleting it does not delete Azure
resources; teardown does not remove it or the shared Go-installed client.

See [operations](docs/operations.md) for individual lifecycle commands and
diagnostics, and [configuration](docs/configuration.md) for settings and permissions.

## Tearing down

Use the exact command printed by setup, or substitute your config and resource
group below:

```sh
bin/substrate-aks destroy --config deploy.json --confirm-delete substrate-dev
```

> [!CAUTION]
> **Deletes the entire owned resource group:** AKS, ACR and its images, database
> data, snapshots, runtime assets, and identities. There is no automatic recovery.
> The command waits for Azure deletion; local configuration and diagnostics remain.

After complete teardown, clear deleted image references before reusing the config:

```sh
bin/substrate-aks images --reset-config --config deploy.json
bin/substrate-aks setup --config deploy.json
```

Reset is refused while the resource group exists. Alternatively, start with a
fresh config. See [retry and teardown](docs/operations.md#retry-and-teardown).

## Documentation

| Guide | Contents |
| --- | --- |
| [Installation architecture](docs/architecture.md) | Declarative design, Kustomize components, and the temporary upstream bridge |
| [Configuration](docs/configuration.md) | Config fields, identities, permissions, networking, and image/source settings |
| [Operations](docs/operations.md) | Lifecycle commands, client setup, diagnostics, retries, teardown, and validation limits |
| [Azure deployment components](deploy/README.md) | Embedded Kustomize inputs and upstream compatibility contracts |

## Development

```sh
make test
make verify
make build
make bicep
```

These checks do not provision Azure resources. See the
[pinned-source contract tests](docs/operations.md#development-checks)
for the opt-in integration check.

## Contributing

This project welcomes contributions and suggestions. Most contributions require
you to agree to a Contributor License Agreement (CLA) declaring that you have
the right to grant us the rights to use your contribution. For details, visit
[Contributor License Agreements](https://cla.opensource.microsoft.com).

When you submit a pull request, a CLA bot will determine whether you need to
provide a CLA and decorate the PR appropriately. Follow the instructions
provided by the bot. You will only need to do this once across Microsoft
repositories using this CLA.

This project has adopted the
[Microsoft Open Source Code of Conduct](https://opensource.microsoft.com/codeofconduct/).
For more information see the
[Code of Conduct FAQ](https://opensource.microsoft.com/codeofconduct/faq/)
or contact [opencode@microsoft.com](mailto:opencode@microsoft.com).

For security reporting, see [SECURITY.md](SECURITY.md).
For support information, see [SUPPORT.md](SUPPORT.md).

## Trademarks

This project may contain trademarks or logos for projects, products, or
services. Authorized use of Microsoft trademarks or logos is subject to and
must follow
[Microsoft's Trademark & Brand Guidelines](https://www.microsoft.com/legal/intellectualproperty/trademarks/usage/general).
Use of Microsoft trademarks or logos in modified versions of this project must
not cause confusion or imply Microsoft sponsorship. Any use of third-party
trademarks or logos is subject to those third-party's policies.
