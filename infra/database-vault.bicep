targetScope = 'subscription'

param location string
param resourceGroupName string
param installationId string
param databaseBackend string
param databaseLocation string
param vaultName string
param operatorObjectId string

resource group 'Microsoft.Resources/resourceGroups@2024-03-01' = {
  name: resourceGroupName
  location: location
  tags: {
    'substrate-aks-installation': installationId
    'substrate-aks-database': databaseBackend
    'substrate-aks-database-location': databaseLocation
  }
}

module vault 'vault.bicep' = {
  name: 'substrate-vault-${installationId}'
  scope: group
  params: {
    location: location
    installationId: installationId
    vaultName: vaultName
    operatorObjectId: operatorObjectId
  }
}
