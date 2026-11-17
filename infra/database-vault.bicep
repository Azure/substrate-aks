targetScope = 'subscription'

param location string
param resourceGroupName string
param installationId string
param databaseBackend string
param databaseLocation string
param databaseSkuName string
param vaultName string
param operatorObjectId string
@allowed(['User', 'ServicePrincipal', 'Group'])
param operatorPrincipalType string

resource group 'Microsoft.Resources/resourceGroups@2024-03-01' = {
  name: resourceGroupName
  location: location
  tags: {
    'substrate-aks-installation': installationId
    'substrate-aks-database': databaseBackend
    'substrate-aks-database-location': databaseLocation
    'substrate-aks-database-sku': databaseSkuName
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
    operatorPrincipalType: operatorPrincipalType
  }
}
