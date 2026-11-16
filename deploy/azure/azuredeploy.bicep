// One Ubuntu VM running the Instalily edge connector, in an existing subnet.
// It has no public IP and its network security group admits nothing inbound:
// the connector only dials out. cloud-init.yaml installs and starts it.
//
// azuredeploy.json is compiled from this file (see README.md); edit this one.

@description('Hub address from Instalily, as IP:port, e.g. 203.0.113.10:443.')
param hubAddress string

@description('Enrollment token from Instalily. It is valid for 24 hours by default; ask for a new one if it has expired.')
@secure()
param enrollmentToken string

@description('Targets Instalily may reach, as JSON. Each entry is a "cidr" (a range or one IP) or a "host" (a name, or "*.corp.example" for any name under it), with "ports". Example: [{"cidr": "10.1.2.3", "ports": [1433]}]')
param allow array

@description('Name of the existing virtual network for the VM. Its subnet must route to the targets and allow outbound HTTPS.')
param virtualNetworkName string

@description('Resource group of the virtual network. Leave empty if it is this deployment\'s resource group.')
param virtualNetworkResourceGroup string = ''

@description('Name of the subnet for the VM.')
param subnetName string

@description('Name of the VM and the prefix of its disk, network interface and security group.')
@maxLength(64)
param vmName string = 'edge-connector'

@description('VM size. The connector needs little; Standard_B2s leaves headroom.')
param vmSize string = 'Standard_B2s'

@description('Region. Defaults to the resource group\'s.')
param location string = resourceGroup().location

@description('Connector image, pinned by digest. Instalily sends a new one for each upgrade.')
param connectorImage string = 'public.ecr.aws/s9u8g9q7/edge-connector@sha256:2473f9b660218932ec4f93b5651d7a64fb922748ec4df42a5e75a564717d2cee'

// Linux VMs need an administrator. Nobody knows this password, and the
// security group admits no inbound traffic, so nobody can sign in. Manage the
// VM with Run Command; to sign in, reset the password from the portal first.
@description('Leave as generated. Nobody needs it; see the template\'s comments.')
@secure()
param adminPassword string = '${newGuid()}Aa1!'

var subnetId = resourceId(
  empty(virtualNetworkResourceGroup) ? resourceGroup().name : virtualNetworkResourceGroup,
  'Microsoft.Network/virtualNetworks/subnets',
  virtualNetworkName,
  subnetName
)

// The connector reads YAML, and JSON is YAML, so the configuration is built
// here and written as one line.
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

var cloudInit = replace(
  replace(
    replace(loadTextContent('cloud-init.yaml'), '@@IMAGE@@', connectorImage),
    '@@CONFIG@@',
    string(config)
  ),
  '@@TOKEN@@',
  enrollmentToken
)

resource nsg 'Microsoft.Network/networkSecurityGroups@2024-01-01' = {
  name: '${vmName}-nsg'
  location: location
  properties: {
    securityRules: [
      {
        name: 'deny-all-inbound'
        properties: {
          description: 'The connector accepts no connections; it only dials out.'
          priority: 4096
          direction: 'Inbound'
          access: 'Deny'
          protocol: '*'
          sourceAddressPrefix: '*'
          sourcePortRange: '*'
          destinationAddressPrefix: '*'
          destinationPortRange: '*'
        }
      }
    ]
  }
}

resource nic 'Microsoft.Network/networkInterfaces@2024-05-01' = {
  name: '${vmName}-nic'
  location: location
  properties: {
    networkSecurityGroup: {
      id: nsg.id
    }
    ipConfigurations: [
      {
        name: 'ipconfig1'
        properties: {
          privateIPAllocationMethod: 'Dynamic'
          subnet: {
            id: subnetId
          }
        }
      }
    ]
  }
}

resource vm 'Microsoft.Compute/virtualMachines@2024-07-01' = {
  name: vmName
  location: location
  properties: {
    hardwareProfile: {
      vmSize: vmSize
    }
    securityProfile: {
      securityType: 'TrustedLaunch'
      uefiSettings: {
        secureBootEnabled: true
        vTpmEnabled: true
      }
    }
    storageProfile: {
      imageReference: {
        publisher: 'Canonical'
        offer: 'ubuntu-24_04-lts'
        sku: 'server'
        version: 'latest'
      }
      osDisk: {
        name: '${vmName}-osdisk'
        createOption: 'FromImage'
        deleteOption: 'Delete'
        managedDisk: {
          storageAccountType: 'StandardSSD_LRS'
        }
      }
    }
    osProfile: {
      computerName: vmName
      // Nobody signs in as this user, so its name is not a secret worth a parameter.
      #disable-next-line adminusername-should-not-be-literal
      adminUsername: 'edgeadmin'
      adminPassword: adminPassword
      customData: base64(cloudInit)
      linuxConfiguration: {
        disablePasswordAuthentication: false
      }
    }
    networkProfile: {
      networkInterfaces: [
        {
          id: nic.id
          properties: {
            deleteOption: 'Delete'
          }
        }
      ]
    }
    // Managed boot diagnostics: the serial log shows cloud-init's progress.
    diagnosticsProfile: {
      bootDiagnostics: {
        enabled: true
      }
    }
  }
}

output vmName string = vm.name
output privateIp string = nic.properties.ipConfigurations[0].properties.privateIPAddress
