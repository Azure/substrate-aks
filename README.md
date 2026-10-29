# Substrate on AKS

Run [Agent Substrate](https://github.com/agent-substrate/substrate) on Azure
Kubernetes Service (AKS). The guided installer creates the Azure resources,
builds the images, and installs Substrate.

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
| Go 1.27.3+ and Git | Go can download the required toolchain automatically |
| Azure CLI | Sign in to the target subscription; install Bicep with `az bicep install` |
| `kubectl` 1.36–1.38 | Within one minor version of AKS 1.37 |
| Azure `kubelogin` | Required for new installs; uses the existing Azure CLI login for managed Entra authentication |
| Docker with buildx or Podman | Running and able to build Linux/amd64 images |
| Azure permissions | Subscription-scope deployments, resource creation, and role assignments; Contributor alone is insufficient |
| AKS capacity | AKS 1.37, VM quota, and nested virtualization in your region |

Building runtime assets also needs `bash`, `curl`, `tar --zstd`, `zstd`,
`readlink -f`, and `sha256sum`. Follow the
[macOS setup notes](docs/operations.md#macos-host-setup) for compatible tools.

```sh
git clone https://github.com/Azure/substrate-aks.git
cd substrate-aks
az login
az bicep install
make build
bin/substrate-aks setup
```

Setup asks for a name and region, previews the changes, and requests approval
before creating billable resources. Press `/` to filter regions. Your settings
are saved in `deploy.json`.

**When setup finishes, run the final shell exports it prints.** These select the
installation's kubeconfig and make `kubectl ate` available. Setup also offers to
select the new context in your regular kubeconfig; the default is No.
`--yes` selects it automatically. Shell profiles are never edited.

**Useful variants**

| Command | Use |
| --- | --- |
| `az account set --subscription SUBSCRIPTION_UUID` | Select a different subscription **before** creating the config |
| `bin/substrate-aks setup --config deploy-dev.json` | Use a separate installation config |
| `bin/substrate-aks setup --accessible` | Screen-reader-friendly, line-oriented prompts |
| `bin/substrate-aks setup --container-engine=podman` | Force Podman instead of auto-detecting an image builder |
| `bin/substrate-aks setup --api-cidr YOUR_PUBLIC_IPV4/32` | Set API access explicitly instead of detecting your IP |
| `bin/substrate-aks setup --config deploy.json --yes --with-demo` | Install without prompts using a complete config; accepts billing and selects the kube-context |

Use `NO_COLOR=1` for plain output. See [configuration](docs/configuration.md) for
identity overrides and [operations](docs/operations.md#guided-installation) for
automation options.

## What gets installed

| Component | Default |
| --- | --- |
| AKS | Kubernetes 1.37; one system node and one dedicated microVM node, `Standard_D4s_v3` |
| Operator access | Microsoft Entra sign-in and cluster-scoped Azure RBAC; local accounts disabled |
| Runtime | Kata / cloud-hypervisor microVMs; KVM is checked before installation |
| Substrate | API server, controller, networking, and atelet |
| Database | Bundled PostgreSQL with a 32 GiB Azure Disk; no HA or backups |
| Images | Built from the pinned source and stored in a private Azure Container Registry (ACR) |
| Storage | Azure Blob containers for snapshots and runtime assets |
| Demo | Optional counter microVM workload |

> [!IMPORTANT]
> **This is not a production or multi-tenant deployment.** The AKS API is public
> and IP-restricted by default; Substrate has no public ingress. ACR and Blob
> endpoints are publicly reachable but require authentication. Actor image access
> and Blob permissions are installation-wide, not per actor or tenant.

Snapshot inspection is optional and read-only. Snapshots can contain memory,
files, and secrets. Removing the reader from your config does not revoke an
existing grant; remove it in Azure IAM or tear down.

The region picker checks region names, not quota or VM capacity. See the
[tested configurations and remaining limits](docs/operations.md#live-acceptance-and-limitations)
before choosing a different region or VM size.

## Installation design

Bicep defines the Azure resources; Kustomize components configure the workloads.
The CLI applies those settings through a pinned version of upstream `ate-setup`.

There is no standalone Kustomize, Helm, or GitOps install yet. See
[architecture](docs/architecture.md) for the deployment flow and
[source settings](docs/configuration.md#source-and-image-settings) for the pin.

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

**Retry:** fix the reported error and rerun setup with the same config. Resources
and logs remain after a failure; there is no automatic rollback.

**Keep the target unchanged:** saved settings take precedence over setup flags
and your active Azure subscription. Use a new config for a different installation.
If your public IP changes, edit `aks.apiAuthorizedCidrs`, then run `plan` and
`provision`.

**Keep local state private:** `.substrate-aks/` contains credentials, kubeconfig,
logs, and deployment records as well as caches. Deleting it does not delete
Azure resources. Teardown leaves it and the shared Go-installed client in place.

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
| [Installation architecture](docs/architecture.md) | How Bicep, Kustomize, and upstream Substrate fit together |
| [Configuration](docs/configuration.md) | Config fields, identities, permissions, networking, and image/source settings |
| [Operations](docs/operations.md) | Lifecycle commands, client setup, diagnostics, retries, teardown, and validation limits |
| [Azure deployment components](deploy/README.md) | Where to change Azure workload settings |

## Development

```sh
make test
make verify
make build
make bicep
```

These checks do not provision Azure resources. `make test` includes
pinned-source integration contracts and downloads source and Go modules;
use `make test-unit` for unit tests only. See
[development checks](docs/operations.md#development-checks) for details.

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
