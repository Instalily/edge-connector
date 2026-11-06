# Azure VM

Runs the connector on one Ubuntu 24.04 VM in an existing Azure subnet, set up
entirely at first boot. The customer fills in one portal form, and nobody
signs in to the VM.

- [`azuredeploy.bicep`](azuredeploy.bicep): the VM, its network interface,
  and a network security group that denies all inbound traffic. The VM has no
  public IP.
- [`azuredeploy.json`](azuredeploy.json): the same template compiled to ARM
  JSON, for the portal and the "Deploy to Azure" button. Don't edit it by
  hand; CI checks that it matches the Bicep file.
- [`cloud-init.yaml`](cloud-init.yaml): the first-boot setup, which the
  template embeds. It installs Docker, writes the connector's files to
  `/opt/edge-connector` (the same layout as [`deploy/compose`](../compose)),
  starts the connector, and empties the token once the connector has
  enrolled. It works on any Ubuntu 24.04 VM with cloud-init, in any cloud,
  after its three placeholders are filled by hand.

## Preparing a customer's template

Send each customer a copy of `azuredeploy.json` with their hub address and
agreed allowlist as defaults, so the form arrives filled in. Send the token
separately; it never goes in the file.

```bash
jq --arg hub '<hub IP>:443' --argjson allow '[{"cidr": "10.1.2.3", "ports": [1433]}]' \
  '.parameters.hubAddress.defaultValue = $hub | .parameters.allow.defaultValue = $allow' \
  azuredeploy.json > azuredeploy-<customer>.json
```

A pre-filled copy is a file, so the customer loads it in the portal, as
below. Without one, this button opens the same form on the template in `main`,
and the customer types the hub address and allowlist themselves:

[![Deploy to Azure](https://aka.ms/deploytoazurebutton)](https://portal.azure.com/#create/Microsoft.Template/uri/https%3A%2F%2Fraw.githubusercontent.com%2FInstalily%2Fedge-connector%2Fmain%2Fdeploy%2Fazure%2Fazuredeploy.json)

## Deploying

The customer needs:

- the hub address and an enrollment token from Instalily;
- the allowlist agreed with Instalily, as JSON, for example
  `[{"cidr": "10.1.2.3", "ports": [1433]}]`;
- the name of a virtual network and subnet that route to the targets and
  allow outbound HTTPS to the hub, `public.ecr.aws` and the Ubuntu package
  mirrors.

In the portal, open **Deploy a custom template**, choose **Build your own
template in the editor**, **Load file** `azuredeploy.json`, **Save**, then fill
in the form. Or with the Azure CLI:

```bash
az deployment group create --resource-group <group> --template-file azuredeploy.bicep \
  --parameters hubAddress=<ip>:443 virtualNetworkName=<vnet> subnetName=<subnet> \
               allow='[{"cidr":"10.1.2.3","ports":[1433]}]' enrollmentToken=<token>
```

The connector usually enrolls a minute or two after the deployment finishes.
The token must still be valid when the VM first boots.

## Operating it

Nobody can sign in: the administrator password is random and never shown, and
inbound traffic is denied. Use **Run command** (RunShellScript) in the portal,
which goes through the Azure VM agent:

| Task | Script |
| --- | --- |
| Status and recent logs | `cd /opt/edge-connector && docker compose ps && docker compose logs --tail 50 connector` |
| First-boot progress | `cloud-init status --long; journalctl -u edge-connector-clear-token --no-pager` |
| Change the allowlist | Edit `allow` in `/opt/edge-connector/config.yaml` (one line of JSON), then `cd /opt/edge-connector && docker compose up -d --force-recreate` |
| Upgrade | Set the new digest in `/opt/edge-connector/.env`, then `cd /opt/edge-connector && docker compose pull && docker compose up -d` |
| Cut off access | `cd /opt/edge-connector && docker compose stop` |

The serial log under **Boot diagnostics** shows cloud-init's progress, too.

To sign in after all, reset the password under **Reset password** and admit
SSH in the security group.

## Tested

cloud-init.yaml, filled as the template fills it, was booted on an Ubuntu
24.04 VM in Google Cloud on 2026-11-06, against a test hub:

- it installed Docker and started the connector in about a minute;
- the connector enrolled, and the token was emptied 8 seconds later;
- traffic flowed through the hub to an allowlisted target;
- after a reboot the connector came back from its stored certificate, without
  the token.

The template itself compiles and lints, but has not yet been deployed in
Azure.

## Rebuilding azuredeploy.json

With [Bicep](https://github.com/Azure/bicep) 0.31.92, the version CI uses:

```bash
bicep build azuredeploy.bicep --outfile azuredeploy.json
```
