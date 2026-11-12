param location string
param installationId string
param egressIP string
@description('Compute SKU name, such as Standard_D2ads_v5. It must belong to skuTier and be offered in this region.')
param skuName string = 'Standard_D2ads_v5'
@allowed(['Burstable', 'GeneralPurpose', 'MemoryOptimized'])
param skuTier string = 'GeneralPurpose'
@secure()
param adminPassword string

resource server 'Microsoft.DBforPostgreSQL/flexibleServers@2025-08-01' = {
  name: 'pg${uniqueString(resourceGroup().id, installationId)}'
  location: location
  tags: {
    'substrate-aks-installation': installationId
  }
  sku: {
    name: skuName
    tier: skuTier
  }
  properties: {
    version: '17'
    createMode: 'Default'
    administratorLogin: 'substrateadmin'
    administratorLoginPassword: adminPassword
    storage: { storageSizeGB: 32 }
    backup: {
      backupRetentionDays: 7
      geoRedundantBackup: 'Disabled'
    }
    highAvailability: { mode: 'Disabled' }
    network: { publicNetworkAccess: 'Enabled' }
    authConfig: {
      passwordAuth: 'Enabled'
      activeDirectoryAuth: 'Disabled'
    }
  }
}

resource firewall 'Microsoft.DBforPostgreSQL/flexibleServers/firewallRules@2025-08-01' = {
  parent: server
  name: 'aks-egress'
  properties: {
    startIpAddress: egressIP
    endIpAddress: egressIP
  }
}

output resourceId string = server.id
output host string = server.properties.fullyQualifiedDomainName
