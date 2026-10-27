targetScope = 'subscription'

param location string
param resourceGroupName string
param installationId string
param clusterName string
param kubernetesVersion string = '1.37'
param workerVmSize string = 'Standard_D4s_v3'
@minValue(1)
param workerCount int = 1
@description('Allowed AKS API IPv4 CIDRs. An empty array disables the allowlist; the CLI requires explicit unrestricted-access opt-in.')
param apiAuthorizedCidrs array
param assetPublisherObjectId string
param snapshotReaderObjectId string = ''
param registryOnly bool = false

resource group 'Microsoft.Resources/resourceGroups@2024-03-01' = {
  name: resourceGroupName
  location: location
  tags: {
    'substrate-aks-installation': installationId
  }
}

module registry 'registry.bicep' = {
  name: 'substrate-acr-${installationId}'
  scope: group
  params: {
    location: location
    installationId: installationId
    publisherObjectId: assetPublisherObjectId
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
    snapshotReaderObjectId: snapshotReaderObjectId
    registryName: registry.outputs.registryName
  }
}

output infrastructure object = registryOnly ? {} : resources!.outputs.infrastructure
