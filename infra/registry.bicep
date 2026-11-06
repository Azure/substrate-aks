param location string
param installationId string
param publisherObjectId string
@allowed(['', 'User', 'ServicePrincipal', 'Group'])
param publisherPrincipalType string = ''

var acrPush = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '8311e382-0749-4cb8-b61a-304f252e45ec')
// Container Registry Data Importer and Data Reader: az acr import and pulls,
// with no registry configuration rights. Only the release image source imports,
// but the grant does not depend on it: imageSource is installer configuration,
// not a parameter here, so switching it later needs no re-provision.
var acrImporter = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '577a9874-89fd-4f24-9dbd-b5034d0ad23a')

resource registry 'Microsoft.ContainerRegistry/registries@2025-11-01' = {
  name: 'substrate${uniqueString(resourceGroup().id, installationId)}'
  location: location
  tags: {
    'substrate-aks-installation': installationId
  }
  sku: {
    name: 'Basic'
  }
  properties: {
    adminUserEnabled: false
    anonymousPullEnabled: false
    publicNetworkAccess: 'Enabled'
    roleAssignmentMode: 'LegacyRegistryPermissions'
  }
}

resource publisher 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(registry.id, publisherObjectId, acrPush)
  scope: registry
  properties: {
    principalId: publisherObjectId
    ...(publisherPrincipalType != '' ? { principalType: publisherPrincipalType } : {})
    roleDefinitionId: acrPush
  }
}

resource importer 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(registry.id, publisherObjectId, acrImporter)
  scope: registry
  properties: {
    principalId: publisherObjectId
    ...(publisherPrincipalType != '' ? { principalType: publisherPrincipalType } : {})
    roleDefinitionId: acrImporter
  }
}

output registryName string = registry.name
