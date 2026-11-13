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
- **Outbound** from that subnet: TCP 443 to the hub and `public.ecr.aws`, and
  TCP 445 (SMB) to Azure Storage (`*.file.core.windows.net`) for the share. If
  a firewall sits on that path, it also needs the endpoints Microsoft lists for
  Container Apps environments.
- **Storage that allows shared-key access.** Container Apps mounts Azure Files
  with the account key. A policy that disables shared keys, or the storage
  account's public endpoint, blocks the mount; use the VM template then.
- The hub address and a token from Instalily, and the agreed allowlist.

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
