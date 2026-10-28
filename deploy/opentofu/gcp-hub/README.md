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
  balancer, one port per endpoint.
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
- **Secrets:** the three exist and hold PEM. Create them outside OpenTofu so
  their contents stay out of state. The hub certificate must name the address
  connectors dial: `tunnel_ip`, or a DNS name for it. Reserve the address first
  (`-target=module.edge_hub.google_compute_address.tunnel`) if you issue
  against the IP.

```bash
edgectl ca -name acme -out pki/
edgectl issue -ca pki/ -name hub -ip <tunnel_ip> -server -out pki/
edgectl issue -ca pki/ -name connector-1 -client -out pki/
gcloud secrets create edge-hub-ca   --data-file=pki/ca.crt
gcloud secrets create edge-hub-cert --data-file=pki/hub.crt
gcloud secrets create edge-hub-key  --data-file=pki/hub.key
```

Keep `ca.key` in Instalily's secrets store, never on the hub. The customer
gets `ca.crt`, `connector-1.crt` and `connector-1.key`.

## Using it

See [`examples/basic`](examples/basic). Pin `source` to a release tag, and
`image` to the hub digest from the same release's notes. Then:

- The customer sets the connector's `hub.address` to `tunnel_address`.
- The solution connects to `endpoints["<name>"]`.

To rotate certificates, add new secret versions and reset the VM
(`gcloud compute instance-groups managed recreate-instances`). The hub reads
the latest versions at start.

## Limits

- One VM, so a zone outage or a VM replacement interrupts tunnels until
  connectors reconnect, typically within a minute.
- Endpoint ports 22, 443 and 8080 are reserved by the VM.
