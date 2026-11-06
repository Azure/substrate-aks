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
@description('MicroVM worker OS disk: Managed, or Ephemeral on local VM storage (NVMe on v6/v7 sizes with local disks). Fixed at pool creation.')
@allowed(['Managed', 'Ephemeral'])
param workerOsDiskType string = 'Managed'
@description('MicroVM worker OS disk size in GB. Managed disk performance scales with size; an Ephemeral disk must fit the local storage. Fixed at pool creation.')
@minValue(30)
@maxValue(2048)
param workerOsDiskSizeGB int = 128
@description('MicroVM worker pod limit, 10-250, or 0 for the AKS default. AKS reserves about 20 MiB of node memory per pod. Fixed at pool creation.')
@minValue(0)
@maxValue(250)
param workerMaxPods int = 0
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
    operatorObjectId: operatorObjectId
    operatorPrincipalType: operatorPrincipalType
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
    workerOsDiskType: workerOsDiskType
    workerOsDiskSizeGB: workerOsDiskSizeGB
    workerMaxPods: workerMaxPods
    apiAuthorizedCidrs: apiAuthorizedCidrs
    assetPublisherObjectId: assetPublisherObjectId
    assetPublisherPrincipalType: assetPublisherPrincipalType
    snapshotReaderObjectId: snapshotReaderObjectId
    snapshotReaderPrincipalType: snapshotReaderPrincipalType
    operatorObjectId: operatorObjectId
    operatorPrincipalType: operatorPrincipalType
    registryName: registry.outputs.registryName
    databaseBackend: databaseBackend
    databaseLocation: databaseLocation
    horizonCreateMode: horizonCreateMode
    databaseAdminPassword: databaseBackend == 'bundled' ? '' : json(databaseCredentials).admin
  }
}

output infrastructure object = registryOnly ? {} : resources!.outputs.infrastructure
