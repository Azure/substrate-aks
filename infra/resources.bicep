param location string
param installationId string
param clusterName string
param kubernetesVersion string
param systemVmSize string
param systemCount int
param workerVmSize string
param workerCount int
@allowed(['Managed', 'Ephemeral'])
param workerOsDiskType string = 'Managed'
@minValue(30)
@maxValue(2048)
param workerOsDiskSizeGB int = 128
@description('0 keeps the AKS default.')
@minValue(0)
@maxValue(250)
param workerMaxPods int = 0
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
param registryName string

var tags = {
  'substrate-aks-installation': installationId
}
var snapshotsContainer = 'snapshots'
var assetsContainer = 'assets'
var blobContributor = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'ba92f5b4-2d11-453d-a403-e96b0029c9fe')
var blobReader = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '2a2b9908-6ea1-4ae2-8e65-a410df84e7d1')
var acrPull = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '7f951dda-4ed3-4680-a7ca-43fe172d538d')

resource registry 'Microsoft.ContainerRegistry/registries@2025-11-01' existing = {
  name: registryName
}

resource cluster 'Microsoft.ContainerService/managedClusters@2025-05-01' = {
  name: clusterName
  location: location
  tags: tags
  identity: {
    type: 'SystemAssigned'
  }
  properties: {
    dnsPrefix: clusterName
    kubernetesVersion: kubernetesVersion
    enableRBAC: true
    ...(operatorObjectId != '' ? {
      aadProfile: {
        managed: true
        enableAzureRBAC: true
        tenantID: tenant().tenantId
      }
      disableLocalAccounts: true
    } : {})
    oidcIssuerProfile: {
      enabled: true
    }
    securityProfile: {
      workloadIdentity: {
        enabled: true
      }
    }
    autoUpgradeProfile: {
      upgradeChannel: 'none'
      nodeOSUpgradeChannel: 'NodeImage'
    }
    apiServerAccessProfile: {
      authorizedIPRanges: apiAuthorizedCidrs
    }
    networkProfile: {
      networkPlugin: 'azure'
      networkPluginMode: 'overlay'
      networkDataplane: 'cilium'
      networkPolicy: 'cilium'
      loadBalancerSku: 'standard'
      outboundType: 'loadBalancer'
      podCidr: '10.244.0.0/16'
      serviceCidr: '10.0.0.0/16'
      dnsServiceIP: '10.0.0.10'
    }
    storageProfile: {
      diskCSIDriver: {
        enabled: true
      }
      fileCSIDriver: {
        enabled: false
      }
    }
    agentPoolProfiles: [
      {
        name: 'system'
        mode: 'System'
        count: systemCount
        vmSize: systemVmSize
        osType: 'Linux'
        osSKU: 'Ubuntu'
        osDiskType: 'Managed'
        osDiskSizeGB: 128
        type: 'VirtualMachineScaleSets'
      }
      union({
        name: 'microvm'
        mode: 'User'
        count: workerCount
        vmSize: workerVmSize
        osType: 'Linux'
        osSKU: 'Ubuntu'
        osDiskType: workerOsDiskType
        osDiskSizeGB: workerOsDiskSizeGB
        type: 'VirtualMachineScaleSets'
        nodeTaints: ['ate.dev/sandboxClass=microvm:NoSchedule']
        nodeLabels: {
          'substrate-aks/runtime': 'microvm'
        }
      }, workerMaxPods == 0 ? {} : { maxPods: workerMaxPods })
    ]
  }
}

resource clusterOperator 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (operatorObjectId != '') {
  name: guid(cluster.id, operatorObjectId, 'b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b')
  scope: cluster
  properties: {
    principalId: operatorObjectId
    principalType: operatorPrincipalType
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b')
  }
}

resource kubeletPull 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(registry.id, cluster.id, acrPull)
  scope: registry
  properties: {
    principalId: cluster.properties.identityProfile.kubeletidentity.objectId
    principalType: 'ServicePrincipal'
    roleDefinitionId: acrPull
  }
}

resource storage 'Microsoft.Storage/storageAccounts@2023-05-01' = {
  name: 'substrate${uniqueString(resourceGroup().id, installationId)}'
  location: location
  tags: tags
  sku: {
    name: 'Standard_LRS'
  }
  kind: 'StorageV2'
  properties: {
    allowBlobPublicAccess: false
    allowSharedKeyAccess: false
    supportsHttpsTrafficOnly: true
    minimumTlsVersion: 'TLS1_2'
  }
}

resource blobs 'Microsoft.Storage/storageAccounts/blobServices@2023-05-01' = {
  parent: storage
  name: 'default'
}

resource snapshots 'Microsoft.Storage/storageAccounts/blobServices/containers@2023-05-01' = {
  parent: blobs
  name: snapshotsContainer
  properties: {
    publicAccess: 'None'
  }
}

resource assets 'Microsoft.Storage/storageAccounts/blobServices/containers@2023-05-01' = {
  parent: blobs
  name: assetsContainer
  properties: {
    publicAccess: 'None'
  }
}

resource nodeIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${clusterName}-node-store'
  location: location
  tags: tags
}

resource controlIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${clusterName}-control-store'
  location: location
  tags: tags
}

resource nodeFederation 'Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials@2023-01-31' = {
  parent: nodeIdentity
  name: 'atelet'
  properties: {
    issuer: cluster.properties.oidcIssuerProfile.issuerURL
    subject: 'system:serviceaccount:ate-system:atelet'
    audiences: ['api://AzureADTokenExchange']
  }
}

resource controlFederation 'Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials@2023-01-31' = {
  parent: controlIdentity
  name: 'ate-api-server'
  properties: {
    issuer: cluster.properties.oidcIssuerProfile.issuerURL
    subject: 'system:serviceaccount:ate-system:ate-api-server'
    audiences: ['api://AzureADTokenExchange']
  }
}

resource nodeSnapshots 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(snapshots.id, nodeIdentity.id, blobContributor)
  scope: snapshots
  properties: {
    principalId: nodeIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: blobContributor
  }
}

resource controlSnapshots 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(snapshots.id, controlIdentity.id, blobContributor)
  scope: snapshots
  properties: {
    principalId: controlIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: blobContributor
  }
}

resource nodeAssets 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(assets.id, nodeIdentity.id, blobReader)
  scope: assets
  properties: {
    principalId: nodeIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: blobReader
  }
}

resource assetPublisher 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(assets.id, assetPublisherObjectId, blobContributor)
  scope: assets
  properties: {
    principalId: assetPublisherObjectId
    ...(assetPublisherPrincipalType != '' ? { principalType: assetPublisherPrincipalType } : {})
    roleDefinitionId: blobContributor
  }

}

resource operatorSnapshots 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (snapshotReaderObjectId != '') {
  name: guid(snapshots.id, snapshotReaderObjectId, blobReader)
  scope: snapshots
  properties: {
    principalId: snapshotReaderObjectId
    ...(snapshotReaderPrincipalType != '' ? { principalType: snapshotReaderPrincipalType } : {})
    roleDefinitionId: blobReader
  }
}

output infrastructure object = {
  clusterName: cluster.name
  resourceGroup: resourceGroup().name
  storageAccount: storage.name
  accountUrl: storage.properties.primaryEndpoints.blob
  snapshotsContainer: snapshotsContainer
  assetsContainer: assetsContainer
  nodeClientId: nodeIdentity.properties.clientId
  controlClientId: controlIdentity.properties.clientId
  tenantId: tenant().tenantId
}
