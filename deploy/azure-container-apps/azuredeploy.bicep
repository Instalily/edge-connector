// The Instalily edge connector as an Azure Container App, in an existing
// virtual network. The app has no ingress, and its environment is internal,
// so nothing can connect to it: the connector only dials out.
//
// It creates:
// - a Container Apps environment on a subnet delegated to
//   Microsoft.App/environments (at least /27): an existing one, or one it
//   creates with a network security group holding the connector's rules;
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

@description('Name of the existing virtual network. It must route to the targets.')
param virtualNetworkName string

@description('Resource group of the virtual network. Leave empty if it is this deployment\'s resource group.')
param virtualNetworkResourceGroup string = ''

@description('Name of the subnet for Container Apps. With Subnet Address Prefix empty: an existing subnet, at least /27, delegated to Microsoft.App/environments and used by nothing else. Otherwise, the name of the subnet this creates.')
param subnetName string

@description('Leave empty to use the existing subnet above. Or give a free range in the virtual network, at least /27 (e.g. 10.20.3.0/27): this then creates the subnet, delegated to Microsoft.App/environments, with a network security group allowing what the connector and Container Apps need. Needs permission to change the virtual network, and Hub Address as IP:port.')
param subnetAddressPrefix string = ''

@description('Only with Subnet Address Prefix: also deny all other outbound traffic from the subnet, so it can reach only what the rules allow.')
param denyOtherOutbound bool = false

@description('Prefix for the names of everything this creates.')
@maxLength(32)
param name string = 'edge-connector'

@description('Region. Defaults to the resource group\'s, which must be the virtual network\'s.')
param location string = resourceGroup().location

@description('Connector image, pinned by digest. Instalily sends a new one for each upgrade.')
param connectorImage string = 'public.ecr.aws/s9u8g9q7/edge-connector@sha256:2473f9b660218932ec4f93b5651d7a64fb922748ec4df42a5e75a564717d2cee'

var createSubnet = !empty(subnetAddressPrefix)
var vnetResourceGroup = empty(virtualNetworkResourceGroup) ? resourceGroup().name : virtualNetworkResourceGroup

// The hub address is IP:port; a security group rule needs them apart.
var hubIp = split(hubAddress, ':')[0]
var hubPort = last(split(hubAddress, ':'))

// Outbound rules for a subnet this creates: the connector's needs, then
// Container Apps' own (Microsoft's "Securing a custom VNet with network
// security groups"). A target named by host goes to the VirtualNetwork tag,
// which covers peered and gateway-connected networks.
var targetRules = [
  for (entry, i) in allow: {
    name: 'allow-target-${i}'
    properties: {
      description: 'An allowlisted target.'
      priority: 200 + i
      direction: 'Outbound'
      access: 'Allow'
      protocol: 'Tcp'
      sourceAddressPrefix: subnetAddressPrefix
      sourcePortRange: '*'
      destinationAddressPrefix: entry.?cidr ?? 'VirtualNetwork'
      destinationPortRanges: map(entry.ports, port => string(port))
    }
  }
]

var fixedRules = [
  {
    name: 'allow-hub'
    properties: {
      description: 'The tunnel to Instalily\'s hub.'
      priority: 100
      direction: 'Outbound'
      access: 'Allow'
      protocol: 'Tcp'
      sourceAddressPrefix: subnetAddressPrefix
      sourcePortRange: '*'
      destinationAddressPrefix: hubIp
      destinationPortRange: hubPort
    }
  }
  {
    name: 'allow-image'
    properties: {
      description: 'The connector image: public.ecr.aws and its CloudFront layer host, which have no fixed IPs.'
      priority: 300
      direction: 'Outbound'
      access: 'Allow'
      protocol: 'Tcp'
      sourceAddressPrefix: subnetAddressPrefix
      sourcePortRange: '*'
      destinationAddressPrefix: 'Internet'
      destinationPortRange: '443'
    }
  }
  {
    name: 'allow-storage-smb'
    properties: {
      description: 'The Azure Files share that holds the connector\'s key.'
      priority: 310
      direction: 'Outbound'
      access: 'Allow'
      protocol: 'Tcp'
      sourceAddressPrefix: subnetAddressPrefix
      sourcePortRange: '*'
      destinationAddressPrefix: 'Storage'
      destinationPortRange: '445'
    }
  }
  {
    name: 'allow-container-registry'
    properties: {
      description: 'Container Apps system containers.'
      priority: 320
      direction: 'Outbound'
      access: 'Allow'
      protocol: 'Tcp'
      sourceAddressPrefix: subnetAddressPrefix
      sourcePortRange: '*'
      destinationAddressPrefix: 'MicrosoftContainerRegistry'
      destinationPortRange: '443'
    }
  }
  {
    name: 'allow-front-door-first-party'
    properties: {
      description: 'A dependency of MicrosoftContainerRegistry.'
      priority: 330
      direction: 'Outbound'
      access: 'Allow'
      protocol: 'Tcp'
      sourceAddressPrefix: subnetAddressPrefix
      sourcePortRange: '*'
      destinationAddressPrefix: 'AzureFrontDoor.FirstParty'
      destinationPortRange: '443'
    }
  }
  {
    name: 'allow-azure-monitor'
    properties: {
      description: 'The connector\'s logs, to Log Analytics.'
      priority: 340
      direction: 'Outbound'
      access: 'Allow'
      protocol: 'Tcp'
      sourceAddressPrefix: subnetAddressPrefix
      sourcePortRange: '*'
      destinationAddressPrefix: 'AzureMonitor'
      destinationPortRange: '443'
    }
  }
  {
    name: 'allow-azure-dns'
    properties: {
      description: 'Azure DNS. Microsoft warns that denying it breaks the environment.'
      priority: 350
      direction: 'Outbound'
      access: 'Allow'
      protocol: '*'
      sourceAddressPrefix: subnetAddressPrefix
      sourcePortRange: '*'
      destinationAddressPrefix: '168.63.129.16'
      destinationPortRange: '53'
    }
  }
  {
    name: 'allow-within-subnet'
    properties: {
      description: 'Traffic within the Container Apps environment.'
      priority: 360
      direction: 'Outbound'
      access: 'Allow'
      protocol: '*'
      sourceAddressPrefix: subnetAddressPrefix
      sourcePortRange: '*'
      destinationAddressPrefix: subnetAddressPrefix
      destinationPortRange: '*'
    }
  }
  {
    name: 'allow-load-balancer-probes'
    properties: {
      description: 'Azure\'s health probes to the environment.'
      priority: 100
      direction: 'Inbound'
      access: 'Allow'
      protocol: 'Tcp'
      sourceAddressPrefix: 'AzureLoadBalancer'
      sourcePortRange: '*'
      destinationAddressPrefix: subnetAddressPrefix
      destinationPortRange: '30000-32767'
    }
  }
]

var denyRule = [
  {
    name: 'deny-other-outbound'
    properties: {
      description: 'Everything the rules above do not allow.'
      priority: 4096
      direction: 'Outbound'
      access: 'Deny'
      protocol: '*'
      sourceAddressPrefix: '*'
      sourcePortRange: '*'
      destinationAddressPrefix: '*'
      destinationPortRange: '*'
    }
  }
]

resource nsg 'Microsoft.Network/networkSecurityGroups@2024-01-01' = if (createSubnet) {
  name: '${name}-nsg'
  location: location
  properties: {
    securityRules: concat(fixedRules, targetRules, denyOtherOutbound ? denyRule : [])
  }
}

module subnet 'subnet.bicep' = if (createSubnet) {
  name: '${name}-subnet'
  scope: resourceGroup(vnetResourceGroup)
  params: {
    virtualNetworkName: virtualNetworkName
    subnetName: subnetName
    addressPrefix: subnetAddressPrefix
    networkSecurityGroupId: nsg.id
  }
}

var subnetId = createSubnet
  ? subnet.outputs.id
  : resourceId(vnetResourceGroup, 'Microsoft.Network/virtualNetworks/subnets', virtualNetworkName, subnetName)

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
