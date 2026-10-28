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
| PostgreSQL | `bundled`; managed `flexible-server` and `horizondb` are opt-in |

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
  "database": {
    "backend": "bundled"
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
      "count": 1,
      "osDiskType": "Managed",
      "osDiskSizeGB": 128
    }
  },
  "substrate": {
    "source": {
      "repository": "https://github.com/agent-substrate/substrate.git",
      "tag": "v1.0.0",
      "commit": "b34fbee742906494b5eb4f1f36987176bf8770a4"
    },
    "pluginSource": {
      "repository": "https://github.com/agent-substrate-plugins/objectstore-azure-blob.git",
      "commit": "b21387e82cee399c65f49178a665727534e1c073"
    },
    "imageSource": "release",
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
| `aks.workerPool.osDiskType` | `Managed` | `Managed`, or `Ephemeral` to place the OS disk on the VM's local storage |
| `aks.workerPool.osDiskSizeGB` | `128` | 30-2048 GB; an `Ephemeral` disk must fit the VM's local storage, up to 2,040 GB |
| `aks.workerPool.maxPods` | AKS default | 10-250 pods per node, and at least 30 across the pool (`maxPods` × `count`); omit to keep the AKS default |

AKS fixes `osDiskType`, `osDiskSizeGB` and `maxPods` when it creates the pool.
`plan` and `provision` refuse to change them on an existing installation;
destroy it and create a new one instead.

The 100-node limit keeps the cluster within its overlay pod address range
(`10.244.0.0/16`, one `/24` per node, so 256 nodes in total) with room for
upgrade surge nodes. AKS itself allows up to 1,000 nodes per pool.

### Snapshot disk performance

Every actor suspend writes its memory snapshot to the worker's disk under
`/var/lib/ate`, which is on the OS disk, so OS disk throughput bounds how fast a
node can suspend and resume actors.

| Choice | Snapshot disk | Trade-offs |
| --- | --- | --- |
| `Managed`, 128 GB (default) | Premium SSD P10, about 500 IOPS and 100 MB/s | Saturates with a few active actors per node |
| `Managed`, larger `osDiskSizeGB` | Scales with the disk tier, for example 1,024 GB is P30, about 5,000 IOPS and 200 MB/s | Also capped per VM (`Standard_D8s_v7`: 16,000 IOPS, 472 MB/s) |
| `Ephemeral` on a size with local NVMe, such as `Standard_D8ds_v7` | Local NVMe, about 560 MB/s on `Standard_D8ds_v7` | Also lost when the VM moves hosts; shared with container images and OS I/O |

Both OS disk types survive a reboot. A reimage or node image upgrade resets the
OS disk either way, along with any snapshots not yet uploaded to Blob storage, so
drain a node before maintenance; workers get time to suspend and upload their
actors.

With `Ephemeral`, AKS places the OS disk on the local NVMe of v6 and v7 "d"
sizes, and `/var/lib/ate` with it:

- `osDiskSizeGB` sets the space `/var/lib/ate` gets, not the NVMe's full size.
  Snapshots and the image cache share it, so raise it toward the NVMe size
  (440 GB on `Standard_D8ds_v7`) for dense nodes. Preflight checks it fits, and
  Azure caps ephemeral OS disks at 2,040 GB.
- Sizes without local storage, such as `Standard_D8s_v7`, cannot use
  `Ephemeral`; preflight rejects them.

### Pods per node

AKS reserves node memory for each pod a node may run: about 20 MiB per pod plus
50 MiB. At the AKS default of 250 that is about 5 GiB, which actors cannot use;
a `Standard_D8ds_v7` node has 31.3 GiB but 26.3 GiB allocatable. One ateworker
pod hosts many actors, and worker nodes run only a few other pods (networking,
storage drivers, atelet), so about 30 is enough for dedicated worker nodes.
The system pool keeps the AKS default.

### Quota and CPU compatibility

Preflight checks that the size is offered in the region and that regional and
VM-family vCPU quota covers both pools; request more quota before provisioning
a large pool. Use a v6 or v7 x86 D, E or F size when running more than one node,
so memory snapshots can restore on any node; see
[live acceptance and limitations](operations.md#live-acceptance-and-limitations).

## PostgreSQL backend

Choose during guided setup, or when creating a new configuration:

```sh
bin/substrate-aks init --database flexible-server --database-location westus3
# Alternatively, for HorizonDB preview:
bin/substrate-aks init --config horizon.json --database horizondb --database-location eastus
```

`setup --yes` accepts the same creation flags only when its config does not
already exist. Saved configs are authoritative. Old configs without `database`
continue to use bundled PostgreSQL.

| `database.backend` | Initial profile |
| --- | --- |
| `bundled` (default) | Kubernetes PostgreSQL; existing certificates and Azure Disk behavior unchanged |
| `flexible-server` | PostgreSQL 17, General Purpose `Standard_D2ads_v5`, 32 GiB, seven-day locally redundant backups, no HA |
| `horizondb` | Preview, PostgreSQL 17, two Intel vCores, one replica, BestEffort placement; service-default seven-day backups |

Optional `database.location` defaults to `azure.location` and applies only to
managed backends. It can differ from AKS, with cross-region latency and transfer
cost implications. `init` and `setup` check subscription-specific regional
capabilities for PostgreSQL 17 and the selected backend's fixed compute profile
before saving a new config or starting setup stages. Full `plan` and `provision`
repeat this read-only check; registry-only operations do not require database
availability. If Azure no longer advertises the profile, an existing installation
can continue with a warning only after its deployment outputs and actual database
resource ID, region and ownership are verified. Group tags alone are insufficient;
missing resources, invalid responses and API access failures remain errors.
Backend/region immutability checks still apply. For new interactive installations,
the managed database picker offers only service regions whose subscription
capabilities advertise the required profile. Azure answers each region
separately, so Flexible Server discovery (about 50 regions) can take up to a
minute; setup shows it as a timed check. Discovery runs only for the selected
managed backend, never for bundled PostgreSQL or `--yes`. It uses up to four
concurrent read-only checks and fails explicitly
on API or malformed-response errors; it never falls back to the general region
catalog. Accessible mode prints the eligible choices. A picked region is not
checked again immediately; the validation checklist rechecks the saved region
before billing approval. Regions given by flags are checked before saving.
Other validation failures stop noninteractive commands and saved configurations;
no region is changed without user selection. Advertised support does **not**
guarantee quota, live SKU capacity or successful provisioning. Live SQL probes succeeded
in `westus3` for Flexible Server and `eastus` for HorizonDB; advertised regions
and SKUs were not all deployable. Register `Microsoft.DBforPostgreSQL` or
`Microsoft.HorizonDB` in the target subscription before provisioning.

HorizonDB deployment uses API `2026-05-01-preview`, its implicit `DefaultPool`
and the service-generated cluster FQDN. Full `plan`/`provision` inspect live
installation-owned cluster metadata and supply `Create` when none exists or
`Update` for a verified existing, successfully provisioned PostgreSQL 17 cluster
with the expected administrator. Busy, failed, foreign or unverifiable resources
stop deployment rather than being treated as absent. This also covers clusters
left by partial failed deployments, without requiring successful deployment
history. Retries reuse the Key Vault password.

The reusable Bicep entrypoints expose constrained `horizonCreateMode`
(`createMode` at the leaf), defaulting to `Create`. Direct callers explicitly
select `Update`; the modules do not discover lifecycle state themselves.
See [standalone infrastructure deployment](infrastructure.md) for secure
parameter-file examples requiring no installer configuration.
Read-only subscription capabilities still use the newer discovery API; they do
not require its regional default parameter groups or select a CPU generation.
Existing new-API installations are not an automatic migration path.

**Acceptance:** the selected legacy API passed a fresh East US installation,
verified TLS, standalone Bicep deployment, same-config Update and durable counter
continuity across reprovisioning. The first cold actor activation timed out;
the exact counter sequence passed after activation readiness.
Japan East and Korea Central tests of `2026-10-01-preview` provisioned
infrastructure but failed strict TLS because the advertised writable hostnames
did not match the service certificates. This return to a fixed legacy API is
not an automatic fallback on deployment failure.
See [operations](operations.md#live-acceptance-and-limitations).

Managed modes provision the database in the installation's owned resource group,
plus an AKS cluster identity and a static Standard egress public IP. Database
public endpoints accept only that AKS egress IPv4, not all Azure services and
not the operator's AKS API CIDR. Bootstrap runs inside AKS. All database
connections use hostname/certificate verification (`sslmode=verify-full`).
These resources add compute, storage, backup, Key Vault and IP charges.

**Fresh installations only.** Backend/region changes after provisioning are
rejected before deployment. No migration, database adoption, password rotation,
or production-readiness claim is included.

The pinned application does not acquire/renew Entra database tokens. Both
managed options therefore use generated passwords, separate owner/runtime
logins and least-privilege roles. An installer-owned Standard Key Vault is the
credential authority for both backends. Credentials are **not** in `deploy.json`
or persisted to local parameter/state files. The vault name is derived from the
subscription, group and installation ID; retain that configuration for retries.
Provisioning uses an ARM Key Vault secret reference. Bootstrap and application
installation retrieve secrets into process memory and use the existing temporary
bootstrap Secret and upstream application Secret/environment integration.
Kubernetes Secrets and application passwords remain; this is not passwordless
database authentication or CSI integration.

The configured Azure upload identity receives **Key Vault Secrets Officer on
this vault only**, not a subscription-wide grant. Run managed database commands
as that identity or separately grant the installer equivalent vault access.
The deployment identity also needs `Microsoft.KeyVault/vaults/deploy/action`.
The vault has an authenticated public endpoint; pods need no Key Vault identity
or direct vault access. After reconciling the vault and grant, provisioning waits
up to three minutes for recognized RBAC/DNS propagation errors, with cancellation
support. Other access errors fail immediately. The readiness probe lists only
secret identifiers, so access diagnostics remain visible; password reads and
writes still suppress subprocess output.
Missing credentials after database deployment submission fail rather than
generating replacement passwords. Do not edit the vault's `database-credentials`
secret to rotate passwords; database changes and application rollouts must be
coordinated, and automatic rotation is not implemented.

For workspaces from the earlier local-credential implementation, provisioning
imports the validated private `database-credentials.json` into the vault without
changing passwords. The local file is removed only after verifying the vault
copy. A conflicting vault value or failed write preserves the local file and
fails explicitly; installation alone does not create/import a missing secret.

**Resource-group teardown deletes the managed database and its retained service
backups.** This installer provides no restore workflow or automatic recovery.
The vault and its secrets are soft-deleted with seven-day retention; teardown
does not purge them. Reusing a deleted installation's vault name can require
explicit Azure recovery or purge permissions. Prefer a fresh installation ID.

## Azure identity and permissions

The operator needs permission to list resource groups, create subscription-scope
deployments, provision the resources (including selected managed PostgreSQL and
Key Vault resources), and assign roles.
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

The installer is reviewed against two pins:

| Pin | Default |
| --- | --- |
| Substrate | [`v1.0.0`](https://github.com/agent-substrate/substrate/releases/tag/v1.0.0) (`b34fbee742906494b5eb4f1f36987176bf8770a4`) |
| Azure Blob object-store plugin | [`agent-substrate-plugins/objectstore-azure-blob`](https://github.com/agent-substrate-plugins/objectstore-azure-blob) at `b21387e82cee399c65f49178a665727534e1c073`, the head of its unmerged [installation stack](https://github.com/agent-substrate-plugins/objectstore-azure-blob/pull/6) |

Both are exact commits. A configured `tag` must name the configured commit;
the installer checks this on every fetch and never follows a moved tag. An exact
commit cannot guarantee it stays fetchable: the plugin pin is reachable only
from its pull request until the stack merges, so a force-push or squash-merge
there can make fresh installs fail to fetch it until the pin moves.

### Image source

`substrate.imageSource` selects how `images` fills `substrate.images`:

| Value | Behavior |
| --- | --- |
| `release` (default) | Imports the release images upstream publishes for `substrate.source.tag` from `ghcr.io/agent-substrate/substrate` into the installation's ACR, by digest, after checking that each image is labelled as built from `substrate.source.commit` (`org.opencontainers.image.revision`). Needs no Docker or Podman. |
| `source` | Builds the release images from `substrate.source.commit` with the pinned `ate-setup publish release-images`, which needs Docker with buildx or Podman. Tags are `<tag>-azure.<commit>.<timestamp>`, so rebuilds never overwrite earlier images. |

Both build the counter demo image, which releases do not publish, and the Azure
Blob plugin image from `substrate.pluginSource` with ko into the same ACR.
Guided setup always uses `release`; to build from source, run `init`, set
`"imageSource": "source"`, then run `setup` or `images`.

To install images you published yourself, set `substrate.images` directly and
skip `images`:

| Field | Value |
| --- | --- |
| `substrate.source.repository` | HTTPS Git URL |
| `substrate.source.tag` | Optional release tag naming the commit; required by `release` |
| `substrate.source.commit` | Exact 40-character commit SHA |
| `substrate.pluginSource.repository`, `.commit` | The Azure Blob plugin revision whose kustomize components configure the storage sidecars |
| `substrate.images.repository` | Repository holding every release image and `counter` under one tag |
| `substrate.images.tag` | Matching tag that will not be overwritten |
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
