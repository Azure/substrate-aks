# Substrate on AKS

Run [Agent Substrate](https://github.com/agent-substrate/substrate) on Azure
Kubernetes Service (AKS). The guided installer creates the Azure resources,
builds the images, and installs Substrate.

> [!WARNING]
> **Experimental, single-tenant, and billable.** This quickstart creates a new AKS
> cluster in a dedicated resource group. [Teardown](#tearing-down) deletes the
> entire group and all installation data. Do not add unrelated resources to it.

[Quickstart](#quickstart) · [What gets installed](#what-gets-installed) ·
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
| Azure `kubelogin` | Signs `kubectl` in to the cluster with your Azure CLI login |
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
make setup
```

`make setup` builds `bin/substrate-aks` and runs `bin/substrate-aks setup`. Pass
setup flags with `ARGS`, for example `make setup ARGS="--accessible"`.

Setup checks your tools and Azure access, asks for a name and region, and
saves your choices as a named installation in your user state directory
(`~/.local/state/substrate-aks` on Linux, `~/Library/Application Support/substrate-aks`
on macOS).
It previews every billable change and waits for your approval before creating
anything. Press `?` in the workspace for keys.

**When setup finishes, run the shell exports it prints.** They point `kubectl`
at the new cluster and put `kubectl ate` on your `PATH`. Setup can also add the
cluster to your regular kubeconfig; it asks first, and never edits shell profiles.

**Useful variants**

| Command | Use |
| --- | --- |
| `az account set --subscription SUBSCRIPTION_UUID` | Select a different subscription **before** creating an installation |
| `bin/substrate-aks setup NAME` | Resume or reconcile a saved installation |
| `bin/substrate-aks setup --accessible` | Screen-reader-friendly, line-oriented prompts |
| `bin/substrate-aks setup --container-engine=podman` | Force Podman instead of auto-detecting an image builder |
| `bin/substrate-aks setup --api-cidr YOUR_PUBLIC_IPV4/32` | Set API access explicitly instead of detecting your IP |
| `bin/substrate-aks init flex --database flexible-server --database-location westus3` | Save a new installation that uses Azure PostgreSQL Flexible Server |
| `bin/substrate-aks init horizon --database horizondb --database-location eastus` | Save a new installation that uses Azure HorizonDB (preview) |
| `bin/substrate-aks setup --monitoring` | Opt in managed Prometheus and Grafana for a new installation |
| `bin/substrate-aks setup --yes --with-demo` | Install `substrate-dev` without prompts; accepts billing and selects the kube-context |

Use `NO_COLOR=1` for plain output. See [configuration](docs/configuration.md) for
identity overrides and [operations](docs/operations.md#guided-installation) for
automation options.

The [Bicep infrastructure can also be deployed directly](docs/infrastructure.md)
without running the installer; SQL bootstrap and application installation remain
separate steps.

## What gets installed

| Component | Default |
| --- | --- |
| AKS | Kubernetes 1.37; one system node and one dedicated microVM node, `Standard_D4s_v6` |
| Operator access | Microsoft Entra sign-in and cluster-scoped Azure RBAC; local accounts disabled |
| Runtime | Kata / cloud-hypervisor microVMs; KVM is checked before installation |
| Substrate | API server, controller, networking, and atelet |
| Database | Bundled PostgreSQL with a 32 GiB Azure Disk by default; optional Azure PostgreSQL Flexible Server or HorizonDB (preview) |
| Images | Built from the pinned source and stored in a private Azure Container Registry (ACR) |
| Storage | Azure Blob containers for snapshots and runtime assets |
| Demo | Optional counter microVM workload |
| Monitoring | Optional managed Prometheus and Standard Grafana, with Substrate, AKS, Blob and selected PostgreSQL dashboards |

> [!IMPORTANT]
> **This is not a production or multi-tenant deployment.** The AKS API is public
> and IP-restricted by default; Substrate has no public ingress. ACR and Blob
> endpoints are publicly reachable but require authentication. Actor image access
> and Blob permissions are installation-wide, not per actor or tenant.

Snapshot inspection is optional and read-only. Snapshots can contain memory,
files, and secrets. Removing the reader from your config does not revoke an
existing grant; remove it in Azure IAM or tear down.

Setup checks quota and VM availability before billing, but Azure capacity is
not reserved. See the
[tested configurations and remaining limits](docs/operations.md#live-acceptance-and-limitations)
before choosing a different region or VM size.

Managed PostgreSQL uses passwords retained in Key Vault because the pinned client
cannot renew Entra database tokens. See [database configuration](docs/configuration.md#postgresql-backend)
for costs, availability, credential retention, and destructive teardown.

[Managed metrics and Grafana](docs/monitoring.md) are opt-in and add ongoing
charges. Collection is metrics-only; logs, traces and alert policies are not
configured.

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

Commands take an installation `NAME`. When you have only one installation, the
name is optional, except for `init`, `setup` and `destroy`: without a name,
`init` and `setup --yes` create `substrate-dev`, interactive `setup` asks for a
name, and `destroy` refuses to run.

| Command | Purpose |
| --- | --- |
| `bin/substrate-aks list` | List saved installations |
| `bin/substrate-aks init [NAME]` | Save an installation without creating Azure resources |
| `bin/substrate-aks doctor` | Check local tools; no Azure resource calls |
| `bin/substrate-aks plan` | Preview real infrastructure changes using Bicep what-if |
| `bin/substrate-aks status` | Inspect Substrate pods and persistent volume claims |
| `bin/substrate-aks client` | Install the matching `kubectl ate` and print shell exports |
| `bin/substrate-aks setup --verbose` | Show underlying commands and detailed output |

**Retry:** fix the reported error and rerun `setup NAME`. Resources
and logs remain after a failure; there is no automatic rollback.

**Keep the target unchanged:** saved settings take precedence over setup flags
and your active Azure subscription. Run setup with a new name for a different
installation. Declining the billing confirmation for a new installation
discards it; nothing is left in `list`.
If your public IP changes, kubectl loses access to the cluster. Put the new
address in `aks.apiAuthorizedCidrs` in the installation's `installation.json`,
then run `substrate-aks plan NAME` and `substrate-aks provision NAME`.

**Keep local state private:** each installation's directory under
`installations/NAME/` in the state directory contains its saved settings,
kubeconfig, logs, and deployment records as well as caches. Set
`SUBSTRATE_AKS_HOME` to use a different location. Deleting this directory does
not delete Azure resources; remove those with
`az group delete --name RESOURCE_GROUP`.

See [operations](docs/operations.md) for individual lifecycle commands and
diagnostics, and [configuration](docs/configuration.md) for settings and permissions.

## Tearing down

Use the exact command printed by setup, or substitute your installation name
and resource group below:

```sh
bin/substrate-aks destroy substrate-dev --confirm-delete substrate-dev
```

> [!CAUTION]
> **Deletes the entire owned resource group:** AKS, ACR and its images, database
> data, snapshots, runtime assets, and identities. There is no automatic recovery.
> The command waits for Azure deletion, then removes the installation's local
> directory. The shared Go-installed client stays in place.

If the resource group was deleted outside the installer, clear the deleted image
references before reusing the saved installation:

```sh
bin/substrate-aks images substrate-dev --reset-config
bin/substrate-aks setup substrate-dev
```

Reset is refused while the resource group exists. See
[retry and teardown](docs/operations.md#retry-and-teardown).

## Documentation

| Guide | Contents |
| --- | --- |
| [Installation architecture](docs/architecture.md) | How Bicep, Kustomize, and the pinned upstream `ate-setup` fit together; no standalone Helm or GitOps install yet |
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
