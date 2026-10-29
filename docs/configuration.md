# Configuration

[Quickstart](../README.md) | [Architecture](architecture.md) | [Operations](operations.md)

## Create a configuration

Run from the repository root:

```sh
make build
az login
bin/substrate-aks init
```

`init` creates `deploy.json` without creating Azure resources or overwriting an
existing file. You can also create it through
[guided setup](operations.md#guided-installation).

| Setting | Default |
| --- | --- |
| Subscription | Active Azure CLI subscription |
| Upload identity | Signed-in identity |
| Kubernetes operator | Signed-in identity, with managed Entra and Azure RBAC |
| Resource group / cluster | `substrate-dev` |
| Region | `eastus2` |
| AKS | Kubernetes 1.37; one system node and one microVM worker |
| MicroVM worker | `Standard_D4s_v3`, count 1 |
| AKS API access | Your detected public IPv4 address, restricted to `/32` |
| Snapshot inspection | Disabled unless requested |

Every lifecycle command accepts `--config PATH`. Relative paths in the file
resolve from its directory. Unknown JSON fields and extra JSON documents are
rejected.

### Example structure

`init` fills in the installation ID, account, and network values. `images` fills
in the image and runtime settings. This example shows the structure; the
placeholders are not valid deployment values.

```json
{
  "installation": {
    "id": "GENERATED_INSTALLATION_UUID",
    "stateDir": ".substrate-aks"
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
      "vmSize": "Standard_D4s_v3",
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

**Keep the installation ID, subscription, resource group, cluster name, and
location unchanged for retries and teardown.** Use a new config for another
installation.

Provisioning stops if another resource group has the same installation tag.
The cluster's own AKS-managed node group is allowed while the tagged main group
exists. If only a node group remains, wait for teardown to finish or investigate
the incomplete deletion before retrying.

After teardown, reset references to deleted images before reusing the config.
See [retry and teardown](operations.md#retry-and-teardown).

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

Use Entra **object IDs**, not application/client IDs. Setup discovers the current
user or service principal; if directory lookup is unavailable, supply the
overrides. `init` and `setup --yes` require explicit overrides when discovery
fails. For another tenant, select the subscription and sign in to that tenant
first.

The identity running image and asset uploads needs the configured publisher
grant, directly or through group membership. The older `--asset-publisher-object-id` flag and
`azure.assetPublisherObjectId` field remain supported.

The corresponding JSON type fields are `azure.assetPublisherPrincipalType` and
`azure.snapshotReaderPrincipalType`. Discovered upload identities save their
type. If omitted, a matching operator's type is reused; a reader matching the
publisher reuses its type. Otherwise older configs retain Azure's directory
inference rather than guessing `User`. Specify types explicitly for unrelated
identities and directory-restricted automation.

### Kubernetes operator authentication

New configs enable managed Entra and Azure RBAC, disable local AKS accounts,
and grant the operator **Azure Kubernetes Service RBAC Cluster Admin** on the
cluster. Installation needs cluster-wide access to create CRDs and RBAC.

The operator defaults to the current Azure CLI identity, even when a different
upload identity is supplied. `kubelogin --login azurecli` reuses your Azure login.
Overriding the operator grants access; it does not sign you in as that identity.
Before billing, setup warns if the signed-in identity differs, or if directory
restrictions prevent verification. Group operators get a membership reminder,
not an object-ID mismatch warning. Another valid cluster grant can also suffice.

Older configs without `aks.operator` retain their legacy authentication behavior.
Setup labels them as legacy and does not migrate running clusters automatically.
Use a new installation for the new defaults.
Provisioning and previews compare an existing cluster's authentication mode and
original deployment parameters. Adding, removing, or changing `aks.operator`
is refused; restore its original values for retries. Authentication migrations
are not implemented.

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

New and saved region names are checked against the subscription's physical
region catalog. Setup additionally checks AKS version availability, system/worker
SKU availability, regional and VM-family CPU quota, and discoverable deployment
and role-assignment permissions before provisioning the registry. Retries check
additional CPU requirements rather than counting existing matching pools twice.
Run `doctor --azure --config deploy.json` for the same read-only cloud checks.

These are snapshots, not reservations or a complete Azure authorization proof.
Zone-only restrictions do not disqualify an unzoned pool. Capacity, conditional
roles, deny policies, provider validation, and microVM/KVM compatibility can still
block later steps. Default `doctor` remains local and needs no config.

By default, setup gets your public IPv4 address from `api.ipify.org` over HTTPS
and restricts API access to that address. No Azure credentials are sent.

| Option | Behavior |
| --- | --- |
| `--api-cidr YOUR_PUBLIC_IPV4/32` | Set an IPv4 CIDR without public-IP lookup |
| Lookup failure | Interactive setup asks for a CIDR; noninteractive commands require an override |
| `--allow-unrestricted-api` | Allow internet-wide API access; Kubernetes authentication still applies |

Unrestricted access cannot be combined with `--api-cidr`. In JSON, set both
`aks.allowUnrestrictedApi: true` and `aks.apiAuthorizedCidrs: []`; an empty
allowlist alone is rejected.

Your saved IP is not refreshed on retries. If it changes, edit
`aks.apiAuthorizedCidrs`, then run `plan` and `provision`.

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

We are intentionally keeping this fork pin while a separate Azure storage-plugin
repository is being created. After that extraction and compatibility validation,
the planned path is to move to a supported upstream revision or release. An exact
SHA verifies content, not permanent availability after a fork branch is deleted
or rewritten; that retention risk remains with the temporary pin.

The source repository and exact commit are already configurable in JSON.
Convenient upstream commit/tag selection through the installer is a follow-up,
not part of this PR. Tags are not currently accepted by `substrate.source.commit`;
the installer requires an exact SHA and compatible images, assets, and client.

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

State lives under `installation.stateDir/installation.id`, normally within
`.substrate-aks/`. It includes source caches, runtime assets, kubeconfig, registry
credentials, logs, deployment records, and generated infrastructure files.

**Keep it private and retain it while the installation exists.** It is not all
rebuildable cache. Deleting it does not delete Azure resources; teardown leaves
the local files and shared Go-installed client in place.

Installations from the earlier `substrate-azure` prototype cannot be adopted.
Start with a fresh configuration.
