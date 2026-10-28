# Deploying the infrastructure without the installer

The Bicep modules are a public infrastructure interface. They do not require
`substrate-aks init`, `deploy.json` or Go. Azure CLI with Bicep support, Azure
authentication, regional service capacity and resource/RBAC deployment permissions
are still required. These deployments create billable resources.

`infra/main.bicep` is the subscription entrypoint for AKS, ACR, Blob storage,
identities and the selected database. `infra/resources.bicep` is its resource-group
module; `infra/horizondb.bicep` can also be deployed independently into an existing
group. These templates do **not** publish images, bootstrap SQL roles/schema,
install Substrate or create an installer configuration. Standalone installations
are not automatically adopted by the installer.

## Stable parameters and lifecycle

Keep the subscription, resource group, installation UUID, cluster name and
credentials stable across deployments. Names are derived from group/installation
identity. Choose a fresh UUID yourself and retain it; no Go-generated identifier
is required. Restrict AKS API access with your actual public IPv4 `/32` or an
approved CIDR, never the documentation addresses below.

`horizonCreateMode` on the shared entrypoints, or `createMode` on the HorizonDB
leaf, allows only `Create` and `Update`. Its default is `Create`. Select `Update`
only after verifying that the intended existing legacy-API cluster is owned,
in the expected region and successfully provisioned. Use it for retries where
a cluster already exists, including after another resource failed. Bicep does
not inspect installer checkpoints or choose a mode from deployment history.
An Update is a real cluster PUT, not a skipped reconciliation. It is not a
new-API downgrade, adoption, password rotation or database migration.

The installer is another caller of this interface: it performs those live
ownership/readiness checks and passes the selected mode. This lifecycle input is
the parameterization pattern; API versions and compute profiles remain fixed.

## Subscription deployment

Use `infra/examples/horizondb.bicepparam`. Supply the following nonsecret
environment variables, replacing the documentation values:

```sh
export AZURE_SUBSCRIPTION_ID='YOUR-SUBSCRIPTION-UUID'
export SUBSTRATE_RESOURCE_GROUP='my-substrate'
export SUBSTRATE_CLUSTER_NAME='my-substrate'
export SUBSTRATE_INSTALLATION_ID='YOUR-STABLE-INSTALLATION-UUID'
export SUBSTRATE_OPERATOR_OBJECT_ID='YOUR-ENTRA-OBJECT-UUID'
export SUBSTRATE_API_CIDR='203.0.113.8/32'
export SUBSTRATE_DATABASE_VAULT_NAME='YOUR-UNIQUE-INSTALLATION-VAULT'
export SUBSTRATE_INPUT_VAULT_GROUP='YOUR-EXISTING-SECRET-VAULT-GROUP'
export SUBSTRATE_INPUT_VAULT_NAME='YOUR-EXISTING-SECRET-VAULT'
export HORIZON_CREATE_MODE='Create'
```

The example selects AKS `southcentralus` and HorizonDB `eastus`; edit those
parameters only before creation, after checking your subscription's availability.
The installation vault name must meet Azure's length/naming rules and be globally
available. It is distinct from the existing input vault.

In the **existing input vault**, provision a secret named `database-credentials`
containing a JSON object with an `admin` string holding the stable database
administrator password. Populate it through your approved secret-management
process (for example Azure Portal), not a checked-in file or command argument.
The example's `az.getSecret` compiles to an ARM Key Vault reference; no plaintext
secret is placed in the parameter file. Enable template deployment on the input
vault and authorize the deployment principal to resolve the reference, including
`Microsoft.KeyVault/vaults/deploy/action` as required by Azure.

The root's secure `databaseCredentials` input retains the installer's JSON bundle
interface and consumes its `admin` field. Direct infrastructure callers need
only that field. Bicep does not generate application credentials or copy the input
secret into the newly created installation vault; retain your external credential
authority for standalone operation.

```sh
az deployment sub what-if --subscription "$AZURE_SUBSCRIPTION_ID" \
  --location southcentralus --parameters infra/examples/horizondb.bicepparam
az deployment sub create --subscription "$AZURE_SUBSCRIPTION_ID" \
  --name "my-substrate-$(date -u +%Y%m%d%H%M%S)" \
  --location southcentralus --parameters infra/examples/horizondb.bicepparam
```

For a verified existing cluster, set `HORIZON_CREATE_MODE=Update` and repeat with
the same inputs and a new deployment-attempt name. Inspect actual ARM operation
results; successful compilation or what-if is not provider acceptance.

## Deploy only HorizonDB

Use `infra/examples/horizondb-leaf.bicepparam` with an existing dedicated group.
It needs the same subscription, stable installation UUID, input-vault variables
and lifecycle mode, plus `SUBSTRATE_DATABASE_EGRESS_IP`: the actual static IPv4
of the permitted client's egress. This module allows exactly that one IP; it
does not create AKS, an egress IP or broad Azure-service access.

The leaf takes an ordinary secure `adminPassword`, not a JSON bundle. Its example
references an existing input-vault secret named `database-admin-password` whose
value is the administrator password. Keep that value stable for Update.

```sh
export SUBSTRATE_DATABASE_EGRESS_IP='203.0.113.9'
az deployment group what-if --subscription "$AZURE_SUBSCRIPTION_ID" \
  --resource-group "$SUBSTRATE_RESOURCE_GROUP" \
  --parameters infra/examples/horizondb-leaf.bicepparam
az deployment group create --subscription "$AZURE_SUBSCRIPTION_ID" \
  --resource-group "$SUBSTRATE_RESOURCE_GROUP" \
  --parameters infra/examples/horizondb-leaf.bicepparam
```

Use the returned FQDN with strict certificate/hostname verification. Independently
arrange SQL bootstrap and application deployment if needed. Deleting the dedicated
resource group deletes the database and its data; retain required backups first.
