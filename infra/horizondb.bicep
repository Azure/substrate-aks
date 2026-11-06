param location string
param installationId string
param egressIP string
@description('Create for a cluster whose password administrator is not configured yet; Update once it is. The 2026-10-01-preview API rejects resubmitting administratorLogin, so Update omits it and only resets the password. Callers must select Update when reprovisioning.')
@allowed(['Create', 'Update'])
param createMode string = 'Create'
@description('Regional default parameter group for the pool. Initial pool creation requires the service-provided Microsoft.HorizonDb/locations/parameterGroups resource.')
param parameterGroupName string = 'default_pg17'
@secure()
param adminPassword string

// Backup settings are omitted: the service defaults to seven-day, locally
// redundant backups, and some regions reject configurable retention.
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
  }
}

// The first pool becomes the cluster's read/write pool.
resource pool 'Microsoft.HorizonDb/clusters/pools@2026-10-01-preview' = {
  parent: cluster
  name: 'default'
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
      parameterGroupResourceId: subscriptionResourceId('Microsoft.HorizonDb/locations/parameterGroups', location, parameterGroupName)
      applyMethod: 'Immediate'
    }
    scheduledMaintenance: {
      maintenanceManagementType: 'System'
    }
  }
}

// Authentication requires an existing pool.
resource passwordAuthentication 'Microsoft.HorizonDb/clusters/authentications@2026-10-01-preview' = {
  parent: cluster
  name: 'passwordMethod'
  dependsOn: [pool]
  properties: union({
    state: 'Enabled'
    administratorLoginPassword: adminPassword
  }, createMode == 'Create' ? {
    administratorLogin: 'substrateadmin'
  } : {})
}

resource entraAuthentication 'Microsoft.HorizonDb/clusters/authentications@2026-10-01-preview' = {
  parent: cluster
  name: 'microsoftEntra'
  dependsOn: [passwordAuthentication]
  properties: {
    state: 'Disabled'
  }
}

resource firewall 'Microsoft.HorizonDb/clusters/pools/firewallRules@2026-10-01-preview' = {
  parent: pool
  name: 'aks-egress'
  dependsOn: [entraAuthentication]
  properties: {
    startIpAddress: egressIP
    endIpAddress: egressIP
  }
}

// The cluster-scoped endpoint is the read/write connection; it shares the cluster's name.
resource endpoint 'Microsoft.HorizonDb/clusters/endpoints@2026-10-01-preview' existing = {
  parent: cluster
  name: cluster.name
}

output resourceId string = cluster.id
output host string = endpoint.properties.fullyQualifiedDomainName
