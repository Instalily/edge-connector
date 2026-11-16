// Creates the Container Apps subnet in an existing virtual network, delegated
// to Microsoft.App/environments, with the given network security group.
// azuredeploy.bicep deploys this into the virtual network's resource group,
// which may differ from its own.

@description('Name of the existing virtual network.')
param virtualNetworkName string

@description('Name of the subnet to create.')
param subnetName string

@description('Its address range: free in the virtual network, at least /27.')
param addressPrefix string

@description('Resource ID of the network security group to attach.')
param networkSecurityGroupId string

resource vnet 'Microsoft.Network/virtualNetworks@2024-01-01' existing = {
  name: virtualNetworkName
}

resource subnet 'Microsoft.Network/virtualNetworks/subnets@2024-01-01' = {
  parent: vnet
  name: subnetName
  properties: {
    addressPrefix: addressPrefix
    networkSecurityGroup: {
      id: networkSecurityGroupId
    }
    delegations: [
      {
        name: 'container-apps'
        properties: {
          serviceName: 'Microsoft.App/environments'
        }
      }
    ]
  }
}

output id string = subnet.id
