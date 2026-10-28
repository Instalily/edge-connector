# edge-connector

Lets Instalily services reach databases, ERPs and internal applications inside
a customer's network (on-premises, AWS, Azure or GCP) without a VPN and
without opening any inbound port.

The customer runs the **connector**, a small container, inside their network.
It dials out to a **hub** that Instalily runs for that customer, and keeps
that tunnel open. When one of our services connects to the hub, the hub asks
the connector to open a connection to a target. The connector checks the
target against an **allowlist that only the customer controls**, dials it,
and passes the bytes through.

```
 Instalily (per-customer account)            Customer network
┌──────────────────────────────┐            ┌───────────────────────────────┐
│ service ──► hub endpoint     │  TLS 1.3   │                               │
│             sap:3300         │◄───────────┤ connector ──► sap-prd:3300    │
│             postgres:5432    │  mutual    │  (dials out    (allowlisted)  │
│                              │  certs     │   on 443)                     │
└──────────────────────────────┘            └───────────────────────────────┘
```

## Security model

These are the properties a customer's security review will ask about.

- **Outbound only.** The connector opens TCP connections to the hub (directly
  or through an HTTP proxy) and to allowlisted targets. It listens on nothing
  but an optional local health port. The Helm chart creates no Service.
- **The customer owns the allowlist.** It lives in the connector's local
  configuration. Nothing the hub sends can change it, and the hub cannot see
  it. A target that isn't on it is refused before any connection is made.
- **One direction only.** Only the hub opens streams. If a connector tries to
  open one, the hub drops its tunnel. Instalily's network is never reachable
  from the customer's side.
- **Mutual authentication.** TLS 1.3 only, and both sides present
  certificates. Each customer has its own CA. A hub accepts only connectors
  whose certificates that CA issued, optionally only named ones
  (`allowed_identities`). Connectors verify the hub's certificate against the
  same CA. The protocol version is negotiated with ALPN.
- **Local audit trail.** The connector logs every stream as JSON on stdout:
  target, outcome, resolved address, bytes each way, duration. These logs stay
  in the customer's environment.
- **Coarse errors.** A refused stream returns only `denied`, `dial_failed`,
  `bad_request` or `busy`. The details are logged locally, not sent to us.
- **Kill switch.** Stopping the connector (`kubectl scale --replicas=0`,
  `docker compose down`) cuts all access immediately.
- **Small attack surface.** The connector is a static Go binary on distroless,
  running as a non-root user with a read-only root filesystem and no Linux
  capabilities. Its dependencies are `hashicorp/yamux` and `gopkg.in/yaml.v3`.
- **End-to-end TLS to targets.** The tunnel carries TCP unchanged, so use the
  target's own TLS (e.g. Postgres `sslmode=verify-full`). Then the connector
  and hub never see plaintext, and credentials for the target stay in
  Instalily's secrets store.

## How targets are matched

Each `allow` entry is a `host` or a `cidr`, with `ports`.

- `host: sap-prd.corp.local` matches that name exactly (case-insensitive).
- `host: "*.db.corp.local"` matches any name ending in `.db.corp.local`, but
  not `db.corp.local` itself.
- `cidr: 10.20.0.0/16` matches addresses in that range; `cidr: 10.20.1.5`
  matches one address.

When a target is a name that no `host` rule matches, the connector resolves it
with the customer's DNS. It then dials only the resolved addresses that fall
inside a `cidr` rule for that port, so a later lookup can't swap in another
address. A target that matches nothing is denied.

Run `connector -config config.yaml -check` to print the rules the connector
will enforce.

## Configuration

Connector (customer-owned):

```yaml
hub:
  address: acme.connect.instalily.ai:443
  server_name: ""        # name to verify on the hub's certificate; defaults to address's host
  connections: 2         # tunnels to keep open
tls:
  ca_file: /etc/edge-connector/tls/ca.crt     # the CA that issued the hub's certificate
  cert_file: /etc/edge-connector/tls/tls.crt  # this connector's certificate
  key_file: /etc/edge-connector/tls/tls.key
proxy: ""                # http:// or https:// proxy; empty uses HTTPS_PROXY / NO_PROXY
allow:
  - host: sap-prd.corp.local
    ports: [3300]
limits:
  max_streams: 256
  dial_timeout: 10s
health_listen: "127.0.0.1:8080"   # /healthz and /readyz; empty disables
```

Hub (Instalily-owned, one per customer):

```yaml
tunnel:
  listen: ":8443"
  tls:
    ca_file: /etc/edge-hub/tls/ca.crt     # the customer's CA, which issues connector certificates
    cert_file: /etc/edge-hub/tls/tls.crt  # the hub's certificate
    key_file: /etc/edge-hub/tls/tls.key
  allowed_identities: [connector-1]       # optional: certificate common names
endpoints:
  - name: sap
    listen: ":3300"
    target: sap-prd.corp.local:3300       # resolved and dialed by the connector
open_timeout: 15s
health_listen: ":8080"
```

Both files are read strictly: a misspelled key is an error, not an ignored
setting. Certificates are read again on every handshake, so a rotated
certificate takes effect on the next connection without a restart.

## Deploying the connector

- **Kubernetes:** [`deploy/helm/edge-connector`](deploy/helm/edge-connector).
  The required values are `image`, `hub.address`, `tls.existingSecret`
  (`ca.crt`, `tls.crt`, `tls.key`) and `allow`. Changing `allow` rolls the
  pods. An optional NetworkPolicy limits egress to DNS plus the CIDRs you list.
- **Docker / VM:** [`deploy/compose`](deploy/compose).

## Issuing certificates

Until connectors enroll themselves, `edgectl` issues certificates by hand:

```bash
edgectl ca -name acme -out pki/
edgectl issue -ca pki/ -name hub -dns acme.connect.instalily.ai -server -out pki/
edgectl issue -ca pki/ -name connector-1 -client -out pki/
```

The connector needs `ca.crt`, `connector-1.crt` and `connector-1.key`. The hub
needs `ca.crt`, `hub.crt` and `hub.key`. Keep `ca.key` in Instalily's secrets
store, never in a customer's environment.

## Development

```bash
go test -race ./...    # unit tests, plus an in-process hub and connector over TLS
test/e2e/run.sh        # containers on separated networks, with Postgres as the target
docker build --target connector -t edge-connector .
docker build --target hub -t edge-hub .
```

`test/e2e/run.sh` puts Postgres in a "customer" network that only the
connector can reach, and a client in an "instalily" network that only the hub
can reach. It checks that:

- the client can query Postgres through the hub;
- a 15 MB copy arrives intact;
- a port that isn't allowlisted is refused, and the connector logs the denial;
- the client cannot reach Postgres directly.

## MVP limits and next steps

- **One hub replica per customer.** A connection to an endpoint must land on
  the replica that holds a tunnel. Running several replicas needs either
  connectors that dial every replica, or replicas that route to each other.
- **Manual certificates.** Next is enrollment: a one-time token, a key that
  never leaves the connector, and short-lived certificates renewed
  automatically.
- **TCP only.** No UDP.
- **Logs only.** No Prometheus metrics yet: tunnel count, streams, bytes, and
  refusals by reason.
- **Release pipeline.** Still to build: CI, signed images (cosign), an SBOM,
  and published versions customers can pin.
- **Hub deployment.** Still to build: an OpenTofu module in `Instalily/infra`
  for client workload accounts, with an NLB passing TLS through to the hub,
  and per-endpoint Services for our workloads.
- **VM image.** Still to build: a Packer image running the connector as a
  systemd unit, for customers without containers.
