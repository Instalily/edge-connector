// The Instalily edge connector as an Azure Container App, in an existing
// virtual network. The app has no ingress, and its environment is internal,
// so nothing can connect to it: the connector only dials out.
//
// It creates:
// - a Container Apps environment on an empty subnet delegated to
//   Microsoft.App/environments (at least /27);
// - a storage account with a 1 GiB Azure Files share, where the connector keeps
//   the key it generates and its certificate across restarts;
// - a Log Analytics workspace for the connector's logs, its audit trail;
// - the container app: one replica, always running.
//
// azuredeploy.json is compiled from this file (see README.md); edit this one.

@description('Hub address from Instalily, as IP:port, e.g. 203.0.113.10:443.')
param hubAddress string

@description('Enrollment token from Instalily. It is valid for 24 hours by default; ask for a new one if it has expired.')
@secure()
param enrollmentToken string

@description('Targets Instalily may reach, as JSON. Each entry is a "cidr" (a range or one IP) or a "host" (a name, or "*.corp.example" for any name under it), with "ports". Example: [{"cidr": "10.1.2.3", "ports": [1433]}]')
param allow array

@description('Name of the existing virtual network. Its subnet must route to the targets and allow outbound HTTPS.')
param virtualNetworkName string

@description('Resource group of the virtual network. Leave empty if it is this deployment\'s resource group.')
param virtualNetworkResourceGroup string = ''

@description('Name of an empty subnet, at least /27, delegated to Microsoft.App/environments. Container Apps uses it for the environment alone.')
param subnetName string

@description('Prefix for the names of everything this creates.')
@maxLength(32)
param name string = 'edge-connector'

@description('Region. Defaults to the resource group\'s, which must be the virtual network\'s.')
param location string = resourceGroup().location

@description('Connector image, pinned by digest. Instalily sends a new one for each upgrade.')
param connectorImage string = 'public.ecr.aws/s9u8g9q7/edge-connector@sha256:2473f9b660218932ec4f93b5651d7a64fb922748ec4df42a5e75a564717d2cee'

var subnetId = resourceId(
  empty(virtualNetworkResourceGroup) ? resourceGroup().name : virtualNetworkResourceGroup,
  'Microsoft.Network/virtualNetworks/subnets',
  virtualNetworkName,
  subnetName
)

// The connector reads YAML, and JSON is YAML, so the configuration is built
// here and mounted as a file from a secret.
var config = {
  hub: {
    address: hubAddress
    connections: 2
  }
  state_dir: '/var/lib/edge-connector'
  enrollment: {
    token_file: '/etc/edge-connector/token'
  }
  allow: allow
  health_listen: '127.0.0.1:8080'
}

resource logs 'Microsoft.OperationalInsights/workspaces@2023-09-01' = {
  name: '${name}-logs'
  location: location
  properties: {
    sku: {
      name: 'PerGB2018'
    }
    retentionInDays: 30
  }
}

// Container Apps mounts Azure Files over SMB with the account key, so shared
// key access stays on. Nothing else is enabled.
resource storage 'Microsoft.Storage/storageAccounts@2023-05-01' = {
  name: 'edgec${uniqueString(resourceGroup().id, name)}'
  location: location
  kind: 'StorageV2'
  sku: {
    name: 'Standard_LRS'
  }
  properties: {
    minimumTlsVersion: 'TLS1_2'
    supportsHttpsTrafficOnly: true
    allowBlobPublicAccess: false
    allowSharedKeyAccess: true
  }
}

resource fileService 'Microsoft.Storage/storageAccounts/fileServices@2023-05-01' = {
  parent: storage
  name: 'default'
}

resource stateShare 'Microsoft.Storage/storageAccounts/fileServices/shares@2023-05-01' = {
  parent: fileService
  name: 'state'
  properties: {
    shareQuota: 1
  }
}

resource environment 'Microsoft.App/managedEnvironments@2024-03-01' = {
  name: '${name}-env'
  location: location
  properties: {
    appLogsConfiguration: {
      destination: 'log-analytics'
      logAnalyticsConfiguration: {
        customerId: logs.properties.customerId
        sharedKey: logs.listKeys().primarySharedKey
      }
    }
    vnetConfiguration: {
      infrastructureSubnetId: subnetId
      internal: true
    }
    workloadProfiles: [
      {
        name: 'Consumption'
        workloadProfileType: 'Consumption'
      }
    ]
  }
}

resource environmentStorage 'Microsoft.App/managedEnvironments/storages@2024-03-01' = {
  parent: environment
  name: 'state'
  properties: {
    azureFile: {
      accountName: storage.name
      accountKey: storage.listKeys().keys[0].value
      shareName: stateShare.name
      accessMode: 'ReadWrite'
    }
  }
}

resource app 'Microsoft.App/containerApps@2024-03-01' = {
  name: name
  location: location
  properties: {
    environmentId: environment.id
    workloadProfileName: 'Consumption'
    configuration: {
      activeRevisionsMode: 'Single'
      // No ingress: the connector accepts no connections.
      secrets: [
        {
          name: 'config'
          value: string(config)
        }
        {
          name: 'token'
          value: enrollmentToken
        }
      ]
    }
    template: {
      containers: [
        {
          name: 'connector'
          image: connectorImage
          args: ['-config', '/etc/edge-connector/config.yaml']
          resources: {
            cpu: json('0.25')
            memory: '0.5Gi'
          }
          volumeMounts: [
            {
              volumeName: 'config'
              mountPath: '/etc/edge-connector'
            }
            {
              volumeName: 'state'
              mountPath: '/var/lib/edge-connector'
            }
          ]
        }
      ]
      // One replica, always running: tunnels need a running connector, and
      // the state share holds one connector's key.
      scale: {
        minReplicas: 1
        maxReplicas: 1
      }
      volumes: [
        {
          name: 'config'
          storageType: 'Secret'
          secrets: [
            {
              secretRef: 'config'
              path: 'config.yaml'
            }
            {
              secretRef: 'token'
              path: 'token'
            }
          ]
        }
        {
          name: 'state'
          storageType: 'AzureFile'
          storageName: environmentStorage.name
          // The image runs as UID 65532; it owns the share, and nobody else in
          // the container can read the key.
          mountOptions: 'uid=65532,gid=65532,dir_mode=0700,file_mode=0600'
        }
      ]
    }
  }
}

output containerAppName string = app.name
output storageAccountName string = storage.name
output logAnalyticsWorkspaceName string = logs.name
