param location string
param installationId string
param egressIP string
@secure()
param adminPassword string

resource cluster 'Microsoft.HorizonDb/clusters@2026-10-01-preview' = {
  name: 'pg${uniqueString(resourceGroup().id, installationId)}'
  location: location
  tags: {
    'substrate-aks-installation': installationId
  }
  properties: {
    postgreSqlVersion: '17'
    network: {
      publicAccess: 'Enabled'
    }
    backup: {
      geoRedundancy: 'Disabled'
      retentionDays: 7
    }
  }
}

resource passwordAuthentication 'Microsoft.HorizonDb/clusters/authentications@2026-10-01-preview' = {
  parent: cluster
  name: 'passwordMethod'
  properties: {
    state: 'Enabled'
    administratorLogin: 'substrateadmin'
    administratorLoginPassword: adminPassword
  }
}

resource entraAuthentication 'Microsoft.HorizonDb/clusters/authentications@2026-10-01-preview' = {
  parent: cluster
  name: 'microsoftEntra'
  dependsOn: [passwordAuthentication]
  properties: {
    state: 'Disabled'
  }
}

resource pool 'Microsoft.HorizonDb/clusters/pools@2026-10-01-preview' = {
  parent: cluster
  name: 'default'
  dependsOn: [entraAuthentication]
  properties: {
    computeModel: {
      type: 'Provisioned'
      computeProcessor: 'Intel'
      computeGeneration: 1
      vCores: 2
    }
    nodeCount: 1
    highAvailability: {
      mode: 'Disabled'
    }
    parameterGroup: {
      parameterGroupResourceId: subscriptionResourceId('Microsoft.HorizonDb/locations/parameterGroups', location, 'default-postgresql-17-provisioned')
      applyMethod: 'Immediate'
    }
    scheduledMaintenance: {
      maintenanceManagementType: 'System'
    }
  }
}

resource firewall 'Microsoft.HorizonDb/clusters/pools/firewallRules@2026-10-01-preview' = {
  parent: pool
  name: 'aks-egress'
  properties: {
    startIpAddress: egressIP
    endIpAddress: egressIP
  }
}

resource endpoint 'Microsoft.HorizonDb/clusters/endpoints@2026-10-01-preview' existing = {
  parent: cluster
  name: cluster.name
}

output resourceId string = cluster.id
output host string = endpoint.properties.fullyQualifiedDomainName
