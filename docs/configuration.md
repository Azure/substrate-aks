# Configuration

[Quickstart](../README.md) | [Architecture](architecture.md) | [Operations](operations.md)

## Create a configuration

Run examples from the repository root:

```sh
make build
az login
bin/substrate-aks init
```

`init` creates `deploy.json` without Azure resources and never overwrites an
existing file. Guided [`setup`](operations.md#guided-installation) can create the
same configuration interactively.

| Setting | Default or behavior |
| --- | --- |
| Subscription | Active Azure CLI subscription |
| Upload identity | Signed-in Azure identity's Entra object ID |
| Resource group / cluster | `substrate-dev` |
| Region | `eastus2` |
| AKS | Kubernetes 1.37; one system node and one microVM worker |
| Worker pool | `Standard_D4s_v3`, count 1 |
| AKS API access | Detected public IPv4 `/32` |
| Snapshot inspection | No operator access unless explicitly requested |

The file is strict JSON: unknown fields and trailing documents are errors.
Relative filesystem paths resolve against the configuration file's directory.
Every lifecycle command accepts `--config PATH`.

### Example structure

`init` generates the installation ID and populates account/network values.
The placeholders below illustrate the structure; this is not a ready-to-run
configuration. Image/runtime fields are populated by `images`.

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
    "assetPublisherObjectId": "YOUR_ENTRA_OBJECT_UUID"
  },
  "aks": {
    "clusterName": "substrate-dev",
    "kubernetesVersion": "1.37",
    "apiAuthorizedCidrs": ["YOUR_PUBLIC_IPV4/32"],
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

Setup does not replace a saved subscription, identity, or API allowlist with
discovered defaults or configuration flags. Changing the active Azure account
does not retarget an existing installation. Its subscription name is looked up
for display only; lookup failure warns and retains the saved GUID.

Edit supported settings in the existing file, but **keep the installation ID,
subscription, resource group, cluster name, and location unchanged for retries
and teardown**.
Before either provisioning mode or preview, the CLI lists groups with this
installation's tag in the configured subscription. Another matching group
blocks provisioning; it is not adopted, deleted, or replaced in saved outputs.
AKS propagates this tag to its managed node resource group. That group does not
block retries when its Azure `managedBy` ID matches the exact cluster in the
configured subscription and resource group, and the tagged main group still
exists. An orphaned node group still blocks reuse and image-reference reset.
If the cluster ID differs, restore the original `aks.clusterName` and
`azure.resourceGroup`. If only the node group remains, wait for an in-progress
teardown to finish; if it persists, inspect incomplete teardown or changed
ownership tags rather than creating a second installation.
Use a new configuration for a separate installation.

Target discovery reads the native `az group list` JSON, including top-level
`tags` and `managedBy`, following Azure's
[ResourceGroup schema](https://learn.microsoft.com/en-us/rest/api/resources/resource-groups/list?view=rest-resources-2021-04-01#resourcegroup).
Regression fixtures exercise this response shape without a CLI query projection.
They do not replace live validation of the AKS-managed group relationship or
teardown timing.

After complete teardown, the old config still references deleted images.
Explicitly reset those references before reusing the same target, or create a
fresh config. See [retry and teardown](operations.md#retry-and-teardown).

## Azure identity and permissions

The operator needs subscription-scope deployment access, resource-group listing
access for target consistency checks, and permission to create resource groups,
AKS, ACR, storage, managed identities, and role assignments.
**Contributor alone cannot assign roles**; use an appropriately scoped RBAC
administrator or equivalent.

| Override | Purpose |
| --- | --- |
| `--subscription UUID` | Choose the target subscription for a new config |
| `--identity-object-id UUID` | Choose the identity that will upload images and runtime assets |
| `--snapshot-reader-object-id UUID` | Opt in a separate identity for read-only snapshot inspection |

Select the active subscription with `az account set --subscription UUID` before
creating a configuration. For a subscription override in another tenant or under
another identity, select it first or explicitly supply its identity object ID.
Discovery never changes the active Azure CLI account.

User sign-ins resolve the signed-in user's Entra object ID. Service principal
sign-ins resolve the **service principal object ID**, not its application/client
ID. If directory lookup fails, interactive setup asks for the missing ID;
`init` and `setup --yes` fail with explicit override instructions.

Supplying both subscription and identity overrides skips account/directory
discovery in `init`, but region validation still makes a read-only Azure call.
Azure CLI manages authentication separately from the deployment file.
The identity actually running `images` and uploading assets must match the
configured upload identity.

The older `--asset-publisher-object-id` flag and JSON field
`azure.assetPublisherObjectId` remain supported for compatibility.

### Snapshot inspection

Set `azure.snapshotReaderObjectId` or use the reader flag above. Guided setup can
opt in the configured upload identity instead.

> [!WARNING]
> Snapshots may contain actor memory, files, and secrets. Operator access grants
> **Storage Blob Data Reader on the snapshots container only**, not writes,
> asset-container access, or subscription-wide permissions.

Runtime sidecars have separate container-scoped identities and do not depend on
this optional operator grant. Azure provisioning is incremental: clearing or
changing the reader field **does not revoke an old role assignment**. Remove it
explicitly in Azure IAM, or tear down the installation.

## Region and API access

`init` and `setup` validate names against the selected subscription's physical
region catalog before saving a new config or requesting deployment approval.
Invalid saved names are also rejected: correct `azure.location` before retrying.
This checks names, **not AKS version availability, VM compatibility, or quota**.

New configurations detect public IPv4 through an unauthenticated HTTPS request
to `api.ipify.org` with a ten-second timeout; no Azure credentials are sent.
The result becomes an AKS API `/32` allowlist entry.

| Option | Behavior |
| --- | --- |
| `--api-cidr YOUR_PUBLIC_IPV4/32` | Explicit IPv4 CIDR; avoids public-IP lookup |
| Lookup failure | Interactive setup asks for a CIDR; noninteractive commands fail with override instructions |
| `--allow-unrestricted-api` | Explicitly disable the allowlist; warns about internet-wide API access |

Unrestricted access still requires Kubernetes authentication. It cannot be
combined with `--api-cidr`. In JSON, it requires `aks.allowUnrestrictedApi: true`
and `aks.apiAuthorizedCidrs: []`; an empty allowlist alone is rejected.

The saved allowlist is not redetected on retries. If your public IP changes,
edit `aks.apiAuthorizedCidrs`, then run `plan` and `provision`.

### Network and authorization boundaries

| Surface | Current behavior |
| --- | --- |
| AKS API | Public endpoint, IP-restricted by default; local AKS credentials, not Entra-backed Kubernetes authorization |
| Substrate services | No public ingress or load balancer |
| ACR | Publicly reachable authenticated endpoint; admin/anonymous access disabled; registry RBAC, not ABAC repository roles |
| Blob | Publicly reachable authenticated endpoints; public Blob access and shared-key authentication disabled |
| Runtime storage | OIDC/workload federation; separate node/control identities and container-scoped grants |
| Actor authorization | Trusted single tenant; no per-actor Blob or per-tenant repository isolation |

This is not a production security posture or personal Entra-backed actor
authorization. See [client authentication](operations.md#client-and-counter-demo).

## Source and image settings

The temporary default is
[`aramase/substrate`](https://github.com/aramase/substrate) at
`c3b44aa46312e073a6384f68723a7b14dffd5518`, from upstream
[PR #2595](https://github.com/agent-substrate/substrate/pull/2595).
At implementation time that PR was open and conflicted with upstream.
Repository, commit, and image settings are configurable; a different upstream
deployment contract can also require changes to the
[compatibility bridge](architecture.md#current-deployment-flow).

**There is no guessed default prebuilt image set.** `images` builds the selected
revision into the owned ACR and records compatible references. To supply an
existing image set, configure the following before `prepare` or `install`:

| Field | Required value |
| --- | --- |
| `substrate.source.repository` | HTTPS Git URL |
| `substrate.source.commit` | Exact 40-character commit SHA |
| `substrate.images.repository` | Compatible Substrate release-image repository |
| `substrate.images.tag` | Matching immutable-by-policy release tag |
| `substrate.images.plugin` | Azure plugin reference with `@sha256:...` |
| `substrate.runtime.assetsDir` | Directory with `cloud-hypervisor`, `virtiofsd`, `vmlinux`, and `rootfs.img` |

Do not combine the pinned manifests with arbitrary upstream release images.
Runtime assets are checked against the selected revision's sandbox template;
only amd64 assets are configured. Image bootstrap is not an upgrade mechanism.

Saved image fields do not guarantee that registry content still exists.
`setup` and `install` verify availability before installation work; use
`images --check` to check independently. Teardown retains these fields even
when it deletes the registry. See [retry and teardown](operations.md#retry-and-teardown)
for the explicit `images --reset-config` recovery path.

## Local workspace

`.substrate-aks/` holds per-installation source checkouts, runtime assets,
kubeconfig, logs, deployment records, isolated Docker credentials, and generated
infrastructure files. Its location is `installation.stateDir/installation.id`.
Keep it private; it can contain credentials.

Deleting the directory does not delete Azure resources. Teardown retains local
configuration and diagnostics, and leaves the shared Go-installed client.
Do not delete an active workspace or treat it all as rebuildable cache.

The installer does not migrate/adopt installations from the earlier
`substrate-azure` prototype. Its state paths, tags, labels, and installation
records use `substrate-aks`; use a fresh configuration.
