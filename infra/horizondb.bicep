param location string
param installationId string
param egressIP string
@description('Create a new cluster, or Update an existing cluster with the same installation identity. Callers must select the mode explicitly when reprovisioning.')
@allowed(['Create', 'Update'])
param createMode string = 'Create'
@secure()
param adminPassword string

resource cluster 'Microsoft.HorizonDb/clusters@2026-05-01-preview' = {
  name: 'pg${uniqueString(resourceGroup().id, installationId)}'
  location: location
  tags: {
    'substrate-aks-installation': installationId
  }
  properties: {
    version: '17'
    createMode: createMode
    administratorLogin: 'substrateadmin'
    administratorLoginPassword: adminPassword
    vCores: 2
    processorType: 'Intel'
    replicaCount: 1
    zonePlacementPolicy: 'BestEffort'
    authConfig: {
      passwordAuth: 'Enabled'
      entraIdAuth: 'Disabled'
    }
  }
}

resource pool 'Microsoft.HorizonDb/clusters/pools@2026-05-01-preview' existing = {
  parent: cluster
  name: 'DefaultPool'
}

resource firewall 'Microsoft.HorizonDb/clusters/pools/firewallRules@2026-05-01-preview' = {
  parent: pool
  name: 'aks-egress'
  properties: {
    startIpAddress: egressIP
    endIpAddress: egressIP
  }
}

output resourceId string = cluster.id
output host string = cluster.properties.fullyQualifiedDomainName
