@description('Region supporting both Azure Monitor workspaces and Azure Managed Grafana.')
param location string
param installationId string
@description('Existing AKS cluster in this resource group with azureMonitorProfile.metrics.enabled=true.')
param clusterName string
@description('Entra object ID of the operator reconciling Grafana data sources and dashboards.')
param grafanaAdminObjectId string

var suffix = uniqueString(resourceGroup().id, installationId)
var tags = {
  'substrate-aks-installation': installationId
}

resource cluster 'Microsoft.ContainerService/managedClusters@2025-05-01' existing = {
  name: clusterName
}

resource workspace 'Microsoft.Monitor/accounts@2023-04-03' = {
  name: 'substrate-${suffix}'
  location: location
  tags: tags
  properties: {}
}

resource endpoint 'Microsoft.Insights/dataCollectionEndpoints@2023-03-11' = {
  name: 'substrate-${suffix}'
  location: location
  kind: 'Linux'
  tags: tags
  properties: {
    networkAcls: {
      publicNetworkAccess: 'Enabled'
    }
  }
}

resource rule 'Microsoft.Insights/dataCollectionRules@2023-03-11' = {
  name: 'substrate-${suffix}'
  location: location
  kind: 'Linux'
  tags: tags
  properties: {
    dataCollectionEndpointId: endpoint.id
    dataSources: {
      prometheusForwarder: [{
        name: 'prometheus'
        streams: ['Microsoft-PrometheusMetrics']
      }]
    }
    destinations: {
      monitoringAccounts: [{
        name: 'workspace'
        accountResourceId: workspace.id
      }]
    }
    dataFlows: [{
      streams: ['Microsoft-PrometheusMetrics']
      destinations: ['workspace']
    }]
  }
}

resource association 'Microsoft.Insights/dataCollectionRuleAssociations@2023-03-11' = {
  name: 'substrate-prometheus'
  scope: cluster
  properties: {
    dataCollectionRuleId: rule.id
  }
}

resource grafana 'Microsoft.Dashboard/grafana@2025-08-01' = {
  name: 'substrate-${suffix}'
  location: location
  tags: tags
  sku: {
    name: 'Standard'
    size: 'X1'
  }
  identity: {
    type: 'SystemAssigned'
  }
  properties: {
    apiKey: 'Disabled'
    publicNetworkAccess: 'Enabled'
    zoneRedundancy: 'Disabled'
    grafanaMajorVersion: '12'
    grafanaIntegrations: {
      azureMonitorWorkspaceIntegrations: [{
        azureMonitorWorkspaceResourceId: workspace.id
      }]
    }
  }
}

resource grafanaAdmin 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(grafana.id, grafanaAdminObjectId, 'grafana-admin')
  scope: grafana
  properties: {
    principalId: grafanaAdminObjectId
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '22926164-76b3-42b3-bc55-97df8dab3e41')
  }
}

resource metricsReader 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(workspace.id, grafana.id, 'metrics-reader')
  scope: workspace
  properties: {
    principalId: grafana.identity.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'b0d8363b-8ddd-447d-831f-62ca05bff136')
  }
}

resource platformReader 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(resourceGroup().id, grafana.id, 'platform-reader')
  properties: {
    principalId: grafana.identity.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '43d0d8ad-25c7-4714-9337-8ba259a9fe05')
  }
}

output monitoring object = {
  location: location
  workspaceId: workspace.id
  queryEndpoint: workspace.properties.metrics.prometheusQueryEndpoint
  grafanaId: grafana.id
  grafanaEndpoint: grafana.properties.endpoint
}
