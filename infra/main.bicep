targetScope = 'subscription'

param location string
param resourceGroupName string
param installationId string
param clusterName string
param kubernetesVersion string = '1.37'
param workerVmSize string = 'Standard_D4s_v6'
@minValue(1)
@maxValue(100)
param workerCount int = 1
@description('Allowed AKS API IPv4 CIDRs. An empty array disables the allowlist; the CLI requires explicit unrestricted-access opt-in.')
param apiAuthorizedCidrs array
param assetPublisherObjectId string
@allowed(['', 'User', 'ServicePrincipal', 'Group'])
param assetPublisherPrincipalType string = ''
param snapshotReaderObjectId string = ''
@allowed(['', 'User', 'ServicePrincipal', 'Group'])
param snapshotReaderPrincipalType string = ''
param operatorObjectId string = ''
@allowed(['User', 'ServicePrincipal', 'Group'])
param operatorPrincipalType string = 'User'
param registryOnly bool = false
param monitoringEnabled bool = false
param monitoringLocation string = location
@allowed(['bundled', 'flexible-server', 'horizondb'])
param databaseBackend string = 'bundled'
param databaseLocation string = location
@description('HorizonDB lifecycle operation. Use Create for a fresh cluster and Update for a verified existing cluster. The installer supplies this from Azure state; direct callers must choose it for retries.')
@allowed(['Create', 'Update'])
param horizonCreateMode string = 'Create'
param databaseVaultName string = ''
@secure()
param databaseCredentials string = ''

resource group 'Microsoft.Resources/resourceGroups@2024-03-01' = {
  name: resourceGroupName
  location: location
  tags: {
    'substrate-aks-installation': installationId
    'substrate-aks-database': databaseBackend
    'substrate-aks-database-location': databaseBackend == 'bundled' ? '' : databaseLocation
    'substrate-aks-monitoring': string(monitoringEnabled)
    'substrate-aks-monitoring-location': monitoringEnabled ? monitoringLocation : ''
  }
}

module registry 'registry.bicep' = {
  name: 'substrate-acr-${installationId}'
  scope: group
  params: {
    location: location
    installationId: installationId
    publisherObjectId: assetPublisherObjectId
    publisherPrincipalType: assetPublisherPrincipalType
  }
}

module vault 'vault.bicep' = if (!registryOnly && databaseBackend != 'bundled') {
  name: 'substrate-vault-${installationId}'
  scope: group
  params: {
    location: location
    installationId: installationId
    vaultName: databaseVaultName
    operatorObjectId: assetPublisherObjectId
  }
}

module resources 'resources.bicep' = if (!registryOnly) {
  name: 'substrate-aks-${installationId}'
  scope: group
  params: {
    location: location
    installationId: installationId
    clusterName: clusterName
    kubernetesVersion: kubernetesVersion
    workerVmSize: workerVmSize
    workerCount: workerCount
    apiAuthorizedCidrs: apiAuthorizedCidrs
    assetPublisherObjectId: assetPublisherObjectId
    assetPublisherPrincipalType: assetPublisherPrincipalType
    snapshotReaderObjectId: snapshotReaderObjectId
    snapshotReaderPrincipalType: snapshotReaderPrincipalType
    operatorObjectId: operatorObjectId
    operatorPrincipalType: operatorPrincipalType
    monitoringEnabled: monitoringEnabled
    monitoringLocation: monitoringLocation
    registryName: registry.outputs.registryName
    databaseBackend: databaseBackend
    databaseLocation: databaseLocation
    horizonCreateMode: horizonCreateMode
    databaseAdminPassword: databaseBackend == 'bundled' ? '' : json(databaseCredentials).admin
  }
}

output infrastructure object = registryOnly ? {} : resources!.outputs.infrastructure
