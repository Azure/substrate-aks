param location string
param installationId string
param vaultName string
@description('AKS operator who may read and write the database credentials. It already has cluster admin, which exposes the application database credentials. Empty grants no data-plane access.')
param operatorObjectId string = ''
@allowed(['User', 'ServicePrincipal', 'Group'])
param operatorPrincipalType string = 'User'

resource vault 'Microsoft.KeyVault/vaults@2025-05-01' = {
  name: vaultName
  location: location
  tags: {
    'substrate-aks-installation': installationId
  }
  properties: {
    tenantId: subscription().tenantId
    sku: {
      family: 'A'
      name: 'standard'
    }
    enableRbacAuthorization: true
    enabledForTemplateDeployment: true
    enableSoftDelete: true
    softDeleteRetentionInDays: 7
    publicNetworkAccess: 'Enabled'
    networkAcls: {
      bypass: 'AzureServices'
      defaultAction: 'Allow'
    }
    accessPolicies: []
  }
}

resource operator 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (operatorObjectId != '') {
  name: guid(vault.id, operatorObjectId, 'database-secrets')
  scope: vault
  properties: {
    principalId: operatorObjectId
    principalType: operatorPrincipalType
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'b86a8fe4-44ce-4948-aee5-eccb2c155cd7')
  }
}
