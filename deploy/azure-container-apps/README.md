# Azure Container Apps

Runs the connector as an Azure Container App, for customers who prefer a
managed container to a VM ([`deploy/azure`](../azure)). The customer fills in
one portal form; there is no machine to sign in to or patch.

- [`azuredeploy.bicep`](azuredeploy.bicep) creates:
  - a Container Apps environment, internal, on the customer's subnet;
  - the container app: one replica, always running, with no ingress;
  - a storage account with a 1 GiB Azure Files share, mounted as the
    connector's `state_dir`, so the key it generates and its certificate
    survive restarts;
  - a Log Analytics workspace for the connector's logs, which are the audit
    trail of every connection.
- [`azuredeploy.json`](azuredeploy.json): the same template compiled to ARM
  JSON, for the portal. Don't edit it by hand; CI checks that it matches.

The configuration and the token are Container Apps secrets, mounted as files.

## What the customer needs

- **A dedicated subnet**: empty, at least `/27`, delegated to
  `Microsoft.App/environments`, in a virtual network that routes to the
  targets. Container Apps uses it for this environment alone.
- **The network rules below** on that subnet.
- **Storage that allows shared-key access.** Container Apps mounts Azure Files
  with the account key, so a policy that disables shared keys blocks the
  mount; use the VM template then. A private endpoint on the storage account
  (`file` sub-resource, private DNS zone `privatelink.file.core.windows.net`
  linked to the virtual network) is supported by Azure Files, but untested with
  this template.
- The hub address and a token from Instalily, and the agreed allowlist.

### Network rules

Inbound, nothing for the connector: the app has no ingress. Keep Azure's default
`AllowAzureLoadBalancerInBound` NSG rule (or an equivalent) under any deny-all,
because Azure's health probes reach the environment on TCP 30000-32767.

Outbound from the subnet:

| Destination | Port | Why |
| --- | --- | --- |
| The hub's IP | TCP 443 | The tunnel. |
| The allowlisted targets | Their ports | Connections the hub asks for. |
| `public.ecr.aws` and `d2glxqk2uabbnd.cloudfront.net` | TCP 443 | The connector image. ECR Public serves the image's layers from that CloudFront host (observed 2026-11-16). Neither has fixed IPs: an NSG needs the `Internet` service tag, a firewall these names. |
| Service tag `Storage.<region>` | TCP 445 | The state share (SMB). |
| Service tags `MicrosoftContainerRegistry`, `AzureFrontDoor.FirstParty` | TCP 443 | Container Apps' system containers. |
| Service tag `AzureMonitor` | TCP 443 | Logs to Log Analytics. |
| `168.63.129.16` | TCP and UDP 53 | Azure DNS. Never deny it. |
| The subnet itself | Any | Traffic within the environment. |

Through Azure Firewall, also allow `mcr.microsoft.com`, `*.data.mcr.microsoft.com`,
`packages.aks.azure.com` and `acs-mirror.azureedge.net` on TCP 443, Microsoft's
list for every Container Apps environment. Microsoft's reference:
[Securing a custom VNet with network security groups](https://learn.microsoft.com/en-us/azure/container-apps/firewall-integration).

## Preparing a customer's template

Send each customer a copy with their hub address and allowlist as defaults;
send the token separately.

```bash
jq --arg hub '<hub IP>:443' --argjson allow '[{"cidr": "10.1.2.3", "ports": [1433]}]' \
  '.parameters.hubAddress.defaultValue = $hub | .parameters.allow.defaultValue = $allow' \
  azuredeploy.json > azuredeploy-container-apps-<customer>.json
```

## Deploying

In the portal: **Deploy a custom template**, **Build your own template in the
editor**, **Load file**, **Save**, then fill in the form. Or:

```bash
az deployment group create --resource-group <group> --template-file azuredeploy.bicep \
  --parameters hubAddress=<ip>:443 virtualNetworkName=<vnet> subnetName=<subnet> \
               allow='[{"cidr":"10.1.2.3","ports":[1433]}]' enrollmentToken=<token>
```

## Operating it

| Task | How |
| --- | --- |
| Status and logs | The app's **Log stream**, or `az containerapp logs show -n edge-connector -g <group> --follow`. History is in the workspace's `ContainerAppConsoleLogs_CL` table. |
| Change the allowlist | Set the `config` secret to the new configuration (one line of JSON), then restart the revision: `az containerapp secret set` and `az containerapp revision restart`. |
| Upgrade | `az containerapp update -n edge-connector -g <group> --image <new digest>` |
| Cut off access | Deactivate the app's revision, or delete the app. |
| New token | Needed only if the share is lost, or the app stays stopped longer than the certificate (7 days): set the `token` secret and restart the revision. |

The token stays in the `token` secret after enrollment. It expires within its
lifetime (24 hours by default), and the connector reads it only when it has no
valid certificate.

## Tested

- The template compiles and lints; it has not yet been deployed in Azure.
- The connector's state on Azure Files was tested on 2026-11-13 by mounting
  an SMB 3.1.1 share without Unix extensions, with this template's mount
  options, as `state_dir`, against a test hub issuing 3-minute certificates.
  The connector enrolled, renewed its certificate on the share twice, came
  back from the latest one after a restart, and carried a stream to an
  allowlisted target.

## Rebuilding azuredeploy.json

With [Bicep](https://github.com/Azure/bicep) 0.31.92, the version CI uses:

```bash
bicep build azuredeploy.bicep --outfile azuredeploy.json
```
