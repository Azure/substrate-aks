using '../main.bicep'

param location = 'southcentralus'
param resourceGroupName = readEnvironmentVariable('SUBSTRATE_RESOURCE_GROUP')
param installationId = readEnvironmentVariable('SUBSTRATE_INSTALLATION_ID')
param clusterName = readEnvironmentVariable('SUBSTRATE_CLUSTER_NAME')
param apiAuthorizedCidrs = [
  readEnvironmentVariable('SUBSTRATE_API_CIDR')
]
param assetPublisherObjectId = readEnvironmentVariable('SUBSTRATE_OPERATOR_OBJECT_ID')
param operatorObjectId = readEnvironmentVariable('SUBSTRATE_OPERATOR_OBJECT_ID')
param databaseBackend = 'horizondb'
param databaseLocation = 'koreacentral'
param horizonCreateMode = readEnvironmentVariable('HORIZON_CREATE_MODE')
param databaseVaultName = readEnvironmentVariable('SUBSTRATE_DATABASE_VAULT_NAME')
param databaseCredentials = az.getSecret(
  readEnvironmentVariable('AZURE_SUBSCRIPTION_ID'),
  readEnvironmentVariable('SUBSTRATE_INPUT_VAULT_GROUP'),
  readEnvironmentVariable('SUBSTRATE_INPUT_VAULT_NAME'),
  'database-credentials'
)
