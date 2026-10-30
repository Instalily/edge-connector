# gcp-hub

Runs one customer's edge hub in GCP, next to the solution that uses it: a
Compute Engine VM on Container-Optimized OS, in the solution's own VPC.

```
 customer's connector ──TCP 443──► tunnel IP (external passthrough NLB) ─┐
                                                                          ├─► hub VM (MIG of 1, no external IP)
 solution's Cloud Run ──TCP port──► endpoint IP (internal passthrough NLB) ┘
```

## What it creates

- **Instance group:** a managed instance group of one VM, on `cos-stable`, with
  Shielded VM and OS Login.
  - A failing `/healthz` replaces the VM.
  - A changed `image` rolls out a new VM before the old one stops.
- **Container:** cloud-init installs a systemd unit that, on every start:
  1. reads the three TLS secrets from Secret Manager into tmpfs;
  2. runs the digest-pinned hub image read-only, as UID 65532, with no
     capabilities and logs to journald.
- **Tunnel:** a static external IP with an external passthrough network load
  balancer on TCP 443. TLS ends at the hub, so it sees each connector's
  certificate.
- **Endpoints:** a static internal IP with an internal passthrough load
  balancer, one port per endpoint. It sends connections only to a hub with a
  connector attached (`/readyz`), so during a rollout they wait for the
  connectors to move to the new VM instead of failing on it.
- **Firewall:**
  - 443 from `tunnel_source_ranges`;
  - the endpoint ports from `endpoint_source_ranges` only;
  - 8080 from Google's health checkers.
- **Service account:** it can read the three TLS secrets and write logs and
  metrics, nothing else.

## Before applying

- **APIs:** `compute.googleapis.com` and `secretmanager.googleapis.com` are
  enabled.
- **Egress:** the subnetwork has Cloud NAT, so the VM, which has no external
  IP, can pull the image from ECR Public and reach Secret Manager. Private
  Google Access alone covers Secret Manager, but not the image.
- **Secrets:** they exist and hold PEM. Create them outside OpenTofu so their
  contents stay out of state. The hub certificate must name the address
  connectors dial: `tunnel_ip`, or a DNS name for it. Reserve the address first
  (`-target=module.edge_hub.google_compute_address.tunnel`) if you issue
  against the IP.

With enrollment (recommended), the hub CA's key and the token signing key stay
in Instalily's secret store. Only the connector CA's key goes to the hub, so
it can sign connector certificates. Replicate the secrets in the hub's region:
the default, automatic replication is `global`, which organization policies
that restrict resource locations reject.

```bash
edgectl ca -name acme -prefix hub-ca -out pki/
edgectl ca -name acme -prefix connector-ca -out pki/
edgectl issue -ca pki/ -ca-prefix hub-ca -name hub -ip <tunnel_ip> -server -out pki/
gcloud secrets create edge-hub-connector-ca     --data-file=pki/connector-ca.crt --replication-policy=user-managed --locations=us-central1
gcloud secrets create edge-hub-connector-ca-key --data-file=pki/connector-ca.key --replication-policy=user-managed --locations=us-central1
gcloud secrets create edge-hub-cert             --data-file=pki/hub.crt          --replication-policy=user-managed --locations=us-central1   # the hub's certificate, then the hub CA
gcloud secrets create edge-hub-key              --data-file=pki/hub.key          --replication-policy=user-managed --locations=us-central1
gcloud secrets create edge-hub-hub-ca           --data-file=pki/hub-ca.crt       --replication-policy=user-managed --locations=us-central1
```

```hcl
  tls = {
    ca_secret   = "projects/acme-solution/secrets/edge-hub-connector-ca"
    cert_secret = "projects/acme-solution/secrets/edge-hub-cert"
    key_secret  = "projects/acme-solution/secrets/edge-hub-key"
  }
  enrollment = {
    connector_ca_key_secret = "projects/acme-solution/secrets/edge-hub-connector-ca-key"
    hub_ca_secret           = "projects/acme-solution/secrets/edge-hub-hub-ca"
    token_public_keys       = file("token.pub") # edgectl token-key's public half
  }
```

Then give the customer a token:

```bash
edgectl token -key token.key -hub-ca pki/hub-ca.crt -identity connector-1
```

Without enrollment, issue the connector's certificate by hand from the
connector CA (`edgectl issue -ca pki/ -ca-prefix connector-ca -name
connector-1 -client -out pki/`), and leave `enrollment` unset.

## Using it

See [`examples/basic`](examples/basic). Pin `source` to a release tag, and
`image` to the hub digest from the same release's notes. Then:

- The customer sets the connector's `hub.address` to `tunnel_address`.
- The solution connects to `endpoints["<name>"]`.

To rotate the hub's certificate or keys, add new secret versions and reset the
VM (`gcloud compute instance-groups managed recreate-instances`). The hub
reads the latest versions at start. Enrolled connectors renew their own
certificates. To add a token signing key, append it to `token_public_keys`,
which rolls out a new VM.

## Limits

- One VM, so a zone outage interrupts tunnels until the VM is back. A
  rollout or a replacement interrupts them only until connectors reconnect to
  the new VM, about a second after the old one stops. Endpoint connections
  fail for a few seconds more, until the load balancer sees the new VM's
  tunnels: about 6 seconds in testing.
- Endpoint ports 22, 443 and 8080 are reserved by the VM.
