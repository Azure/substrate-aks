# Configuration

[Quickstart](../README.md) | [Architecture](architecture.md) | [Operations](operations.md)

## Create a configuration

Run from the repository root:

```sh
make build
az login
bin/substrate-aks init
```

`init [NAME]` saves an installation named `substrate-dev` (or `NAME`) without
creating Azure resources. It never overwrites an existing installation. You can
also create one through [guided setup](operations.md#guided-installation).

| Setting | Default |
| --- | --- |
| Subscription | Active Azure CLI subscription |
| Upload identity | Signed-in identity |
| Kubernetes operator | Signed-in identity, with managed Entra and Azure RBAC |
| Resource group / cluster | `substrate-dev` (`init` and `setup --yes`; interactive setup asks) |
| Region | `eastus2` |
| AKS | Kubernetes 1.37; one system node and one microVM worker |
| Node size | `Standard_D4s_v6` for the system node and the microVM worker (count 1); see [microVM worker pool](#microvm-worker-pool) |
| AKS API access | Your detected public IPv4 address, restricted to `/32` |
| Snapshot inspection | Disabled unless requested |

Each installation is saved as `installations/NAME/installation.json` in the
installer's home directory, a per-user state directory:

| Platform | Location |
| --- | --- |
| Linux | `$XDG_STATE_HOME/substrate-aks`, default `~/.local/state/substrate-aks` |
| macOS | `~/Library/Application Support/substrate-aks` |
| Override | `SUBSTRATE_AKS_HOME` |

It is not a cache directory because it holds credentials and the only local
record of billable resources.
Lifecycle commands select it by `NAME`; `bin/substrate-aks list` shows saved
installations. Unknown JSON fields and extra JSON documents are rejected.

### Example structure

`init` fills in the installation ID, account, and network values. `images` fills
in the image and runtime settings. This example shows the structure; the
placeholders are not valid deployment values.

```json
{
  "installation": {
    "id": "GENERATED_INSTALLATION_UUID",
    "name": "substrate-dev",
    "ownership": {
      "resourceGroup": "created",
      "cluster": "created"
    }
  },
  "azure": {
    "subscriptionId": "YOUR_SUBSCRIPTION_UUID",
    "location": "eastus2",
    "resourceGroup": "substrate-dev",
    "assetPublisherObjectId": "YOUR_ENTRA_OBJECT_UUID",
    "assetPublisherPrincipalType": "User"
  },
  "aks": {
    "clusterName": "substrate-dev",
    "kubernetesVersion": "1.37",
    "apiAuthorizedCidrs": ["YOUR_PUBLIC_IPV4/32"],
    "operator": {
      "objectId": "YOUR_OPERATOR_ENTRA_OBJECT_UUID",
      "principalType": "User"
    },
    "workerPool": {
      "vmSize": "Standard_D4s_v6",
      "count": 1
    }
  },
  "substrate": {
    "source": {
      "repository": "https://github.com/aramase/substrate.git",
      "commit": "c3b44aa46312e073a6384f68723a7b14dffd5518"
    },
    "images": {
      "repository": "",
      "tag": "",
      "plugin": ""
    },
    "runtime": {
      "assetsDir": ""
    }
  }
}
```

### Saved settings are authoritative

Setup uses saved settings, not newly discovered defaults or configuration flags.
Changing your active Azure account does not retarget an existing installation.

**Keep the installation ID, name, subscription, resource group, cluster name, and
location unchanged for retries and teardown.** Run setup with a new name for
another installation. `ownership` records that the installer created the
resource group and cluster; only `created` is supported today.

Provisioning stops if another resource group has the same installation tag.
The cluster's own AKS-managed node group is allowed while the tagged main group
exists. If only a node group remains, wait for teardown to finish or investigate
the incomplete deletion before retrying.

If the resource group is deleted outside the installer, reset references to
deleted images before reusing the installation. See [retry and teardown](operations.md#retry-and-teardown).

## MicroVM worker pool

Sandboxes run on a dedicated, tainted `microvm` node pool. The system pool is
always one `Standard_D4s_v6` node. Set the worker pool in `installation.json`
before running `plan` and `provision`:

| Field | Default | Limits |
| --- | --- | --- |
| `aks.workerPool.vmSize` | `Standard_D4s_v6` | An explicit `Standard_*` Azure VM size that supports nested virtualization |
| `aks.workerPool.count` | `1` | 1-100 nodes |

The 100-node limit keeps the cluster within its overlay pod address range
(`10.244.0.0/16`, one `/24` per node, so 256 nodes in total) with room for
upgrade surge nodes. AKS itself allows up to 1,000 nodes per pool.

Preflight checks that the size is offered in the region and that regional and
VM-family vCPU quota covers both pools; request more quota before provisioning
a large pool. Use a v6 or v7 x86 D, E or F size when running more than one node,
so memory snapshots can restore on any node; see
[live acceptance and limitations](operations.md#live-acceptance-and-limitations).

## Azure identity and permissions

The operator needs permission to list resource groups, create subscription-scope
deployments, provision the resources, and assign roles.
**Contributor alone is insufficient** because it cannot assign roles.

Select your subscription before creating a config:

```sh
az account set --subscription SUBSCRIPTION_UUID
```

| Override | Purpose |
| --- | --- |
| `--subscription UUID` | Subscription for a new config |
| `--identity-object-id UUID` | Identity that uploads images and runtime assets |
| `--identity-principal-type User\|ServicePrincipal\|Group` | Type of an overridden upload identity |
| `--operator-object-id UUID` | Identity granted cluster-scoped AKS RBAC Cluster Admin |
| `--operator-principal-type User\|ServicePrincipal\|Group` | Type of an overridden operator; defaults to User |
| `--snapshot-reader-object-id UUID` | Separate identity for read-only snapshot inspection |
| `--snapshot-reader-principal-type User\|ServicePrincipal\|Group` | Type of a separate snapshot reader |

Use Entra **object IDs**, not application (client) IDs. Setup discovers the
signed-in user or service principal. If directory lookup is unavailable, supply
the overrides; `init` and `setup --yes` require them. For another tenant, sign in
to that tenant and select its subscription first.

Whoever runs image and asset uploads needs the publisher grant, directly or
through group membership. The older `--asset-publisher-object-id` flag and
`azure.assetPublisherObjectId` field still work.

Principal types are saved as `azure.assetPublisherPrincipalType` and
`azure.snapshotReaderPrincipalType`. Discovered identities record their type,
and an identity that matches another configured one reuses its type. Otherwise
Azure infers the type from the directory. Set types explicitly for unrelated
identities and for automation without directory access.

### Kubernetes operator authentication

New configs enable managed Entra and Azure RBAC, disable local AKS accounts,
and grant the operator **Azure Kubernetes Service RBAC Cluster Admin** on the
cluster. Installation needs cluster-wide access to create CRDs and RBAC.

The operator defaults to the signed-in Azure CLI identity, even when you
override the upload identity, and `kubelogin --login azurecli` reuses that login.
Overriding the operator grants access to another identity; it does not sign you
in as it. Before billing, setup warns if the signed-in identity is not the
operator or cannot be verified. For a group operator, it reminds you to check
membership instead. Any other valid cluster grant also works.

Older configs without `aks.operator` keep their legacy authentication and are
labeled as legacy; running clusters are not migrated. Adding, removing, or
changing `aks.operator` on an existing cluster is refused, so restore the
original values to retry. Use a new installation for the new defaults.

### Snapshot inspection

Set `azure.snapshotReaderObjectId` or use `--snapshot-reader-object-id`.
Guided setup can grant access to the upload identity instead.

> [!WARNING]
> Snapshots may contain actor memory, files, and secrets. Inspection grants
> read-only access to the snapshots container, not the assets container.

Removing or changing the field does not revoke an existing grant. Remove the
old assignment in Azure IAM, or tear down the installation. Runtime storage
access uses separate identities and does not depend on this optional grant.

## Region and API access

Region names are checked against the subscription's region list. Before
provisioning the registry, setup also checks the AKS version, VM size
availability, regional and VM-family CPU quota, and the deployment and
role-assignment permissions it can discover. On retries, only the additional CPU
is counted against quota. Run `doctor NAME --azure` for the same
read-only checks.

These checks are a snapshot, not a reservation or a complete authorization proof.
Capacity, conditional roles, deny policies, provider validation, and KVM
compatibility can still block later steps. Zone-only restrictions do not
disqualify a pool without zones.

By default, setup gets your public IPv4 address from `api.ipify.org` over HTTPS
and restricts API access to that address. No Azure credentials are sent.
Interactive setup shows the detected address and asks whether to keep that
restriction (the default) or allow anyone on the internet.

| Option | Behavior |
| --- | --- |
| `--api-cidr YOUR_PUBLIC_IPV4/32` | Set an IPv4 CIDR without public-IP lookup |
| Lookup failure | Interactive setup asks for a CIDR; noninteractive commands require an override |
| `--allow-unrestricted-api` | Allow internet-wide API access; Kubernetes authentication still applies |

Unrestricted access cannot be combined with `--api-cidr`. In JSON, set both
`aks.allowUnrestrictedApi: true` and `aks.apiAuthorizedCidrs: []`; an empty
allowlist alone is rejected.

Your saved IP is not refreshed on retries. If it changes, kubectl loses access
to the cluster: put the new address in `aks.apiAuthorizedCidrs` in the
installation's `installation.json`, then run `substrate-aks plan NAME` and
`substrate-aks provision NAME`.

### Network and authorization boundaries

| Surface | Access |
| --- | --- |
| AKS API | Public, IP-restricted by default, with managed Entra and Azure RBAC for new configs |
| Substrate services | No public ingress or load balancer |
| ACR | Public endpoint; authentication required; admin and anonymous access disabled |
| Blob | Public endpoints; authentication required; public Blob access and shared keys disabled |
| Runtime storage | Separate node/control identities with container-scoped grants |
| Actor authorization | Trusted single tenant; no per-actor Blob or per-tenant image isolation |

These are not private-endpoint or multi-tenant deployments. Cluster Entra access
also does not provide personal actor authorization; see
[client authentication](operations.md#client-and-counter-demo).

## Source and image settings

The temporary default source is
[`aramase/substrate`](https://github.com/aramase/substrate) at
`c3b44aa46312e073a6384f68723a7b14dffd5518`, for the Azure integration in
[agent-substrate/substrate#2595](https://github.com/agent-substrate/substrate/pull/2595).

Setup builds matching images into the installation's ACR. There is no default
prebuilt image set. Do not mix the pinned manifests with arbitrary upstream
release images.

This fork pin is temporary while the Azure storage plugin moves to its own
repository; the plan is then to move to a supported upstream revision or
release. An exact SHA verifies content but cannot guarantee the commit stays
available if the fork branch is deleted or rewritten.

The source repository and commit are configurable in JSON. `substrate.source.commit`
must be an exact SHA, not a tag, with compatible images, assets, and client.

To supply compatible artifacts yourself, configure these before `prepare` or
`install`:

| Field | Value |
| --- | --- |
| `substrate.source.repository` | HTTPS Git URL |
| `substrate.source.commit` | Exact 40-character commit SHA |
| `substrate.images.repository` | Compatible Substrate image repository |
| `substrate.images.tag` | Matching release tag that will not be overwritten |
| `substrate.images.plugin` | Azure plugin reference with `@sha256:...` |
| `substrate.runtime.assetsDir` | Directory containing `cloud-hypervisor`, `virtiofsd`, `vmlinux`, and `rootfs.img` |

Only amd64 runtime assets are configured. Their checksums must match the selected
source revision. A source change may also require
[adapter changes](architecture.md#current-deployment-flow); it is not an upgrade
mechanism.

`setup` and `install` check that saved images are still available. You can check
separately with `images --check`. After teardown, use
[`images --reset-config`](operations.md#retry-and-teardown) to clear references
to the deleted registry.

## Local workspace

Each installation's directory holds its saved settings, source caches, runtime
assets, kubeconfig, registry credentials, logs, deployment records, and
generated infrastructure files.

**Keep it private and retain it while the installation exists.** It is not all
rebuildable cache. Deleting it does not delete Azure resources; remove those
with `az group delete --name RESOURCE_GROUP`. A successful `destroy` removes the
directory; the shared Go-installed client stays in place.

Installations from the earlier `substrate-azure` prototype cannot be adopted.
Start with a fresh configuration.
