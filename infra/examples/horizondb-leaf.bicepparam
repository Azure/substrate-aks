using '../horizondb.bicep'

param location = 'koreacentral'
param installationId = readEnvironmentVariable('SUBSTRATE_INSTALLATION_ID')
param egressIP = readEnvironmentVariable('SUBSTRATE_DATABASE_EGRESS_IP')
param createMode = readEnvironmentVariable('HORIZON_CREATE_MODE')
param adminPassword = az.getSecret(
  readEnvironmentVariable('AZURE_SUBSCRIPTION_ID'),
  readEnvironmentVariable('SUBSTRATE_INPUT_VAULT_GROUP'),
  readEnvironmentVariable('SUBSTRATE_INPUT_VAULT_NAME'),
  'database-admin-password'
)
