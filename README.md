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
  certificates. Each customer has its own CAs: a hub accepts only connectors
  whose certificates its connector CA issued, optionally only named ones
  (`allowed_identities`), and connectors verify the hub against its hub CA.
  The protocol version is negotiated with ALPN.
- **Keys stay where they're used.** An enrolling connector generates its own
  key, and Instalily never sees it. Its certificates last 7 days and renew
  automatically, and a tunnel never outlives the certificate that opened it.
  See [Certificates and enrollment](#certificates-and-enrollment).
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
  rotate_after: 5m       # replace each tunnel about this often, opening the new one first
state_dir: /var/lib/edge-connector         # enrolled key, certificate and hub CA; must persist
enrollment:
  token_file: /etc/edge-connector/token    # needed only until the first enrollment
# Or, with certificates issued by hand, instead of state_dir and enrollment:
# tls:
#   ca_file: /etc/edge-connector/tls/ca.crt     # the hub CA
#   cert_file: /etc/edge-connector/tls/tls.crt  # this connector's certificate
#   key_file: /etc/edge-connector/tls/tls.key
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
    ca_file: /etc/edge-hub/tls/connector-ca.crt   # the connector CA
    cert_file: /etc/edge-hub/tls/tls.crt          # the hub's certificate, then the hub CA
    key_file: /etc/edge-hub/tls/tls.key
  allowed_identities: [connector-1]               # optional: certificate common names
enrollment:                                       # optional: lets connectors enroll and renew
  ca_key_file: /etc/edge-hub/tls/connector-ca.key
  hub_ca_file: /etc/edge-hub/tls/hub-ca.crt
  token_keys_file: /etc/edge-hub/token.pub        # one or more Ed25519 public keys
  certificate_lifetime: 168h
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

## Deploying a hub

In GCP, next to the solution it serves, use the
[`gcp-hub`](deploy/opentofu/gcp-hub) OpenTofu module. It runs a Compute Engine
VM on Container-Optimized OS. Connectors reach the hub on a static external IP
on TCP 443, and the solution reaches the endpoints on a static internal IP.
Pin the module's `source` to a release tag, and its `image` to the same
release's hub digest.

## Deploying the connector

- **Kubernetes:** [`deploy/helm/edge-connector`](deploy/helm/edge-connector).
  Required values are `image`, `hub.address`, `allow`, and either
  `enrollment.tokenSecret` (recommended) or `tls.existingSecret`.
  - **Enrollment:** a Secret holding the token under the key `token`. The
    chart runs a StatefulSet, giving each replica its own small
    PersistentVolumeClaim for its key and certificate. The claims are kept if
    the chart is uninstalled. After every replica has enrolled, the token
    Secret can be deleted; replicas added later need a fresh token.
  - **StorageClass:** the claims use the cluster's default StorageClass unless
    `enrollment.storage.storageClassName` names one. Some clusters have no
    default (EKS Auto Mode clusters often don't), so check `kubectl get
    storageclass` first. Without a usable class the pods stay `Pending`, and
    the setting can't be fixed by an upgrade, because a StatefulSet's claim
    template can't change. Uninstall, delete the claims (`kubectl delete pvc
    -l app.kubernetes.io/instance=<release>`), and install again.
  - **Certificates issued by hand:** a Secret holding `ca.crt`, `tls.crt` and
    `tls.key`. The chart runs a Deployment.

  Changing `allow` rolls the pods. An optional NetworkPolicy limits egress to
  DNS, to any address, plus the CIDRs you list:
  - For a target behind a Kubernetes Service, list its ClusterIP as well as
    its pods. Some network plugins, the AWS VPC CNI among them, match the
    address before the Service translates it.
  - Turning the policy on can drop open tunnels once; connectors reconnect
    by themselves.
- **Docker / VM:** [`deploy/compose`](deploy/compose). The token is a file, and
  the state is a named volume.

## Certificates and enrollment

Each customer has two CAs:

- **Hub CA:** signs only the hub's certificate, and connectors trust it. Its
  key stays in Instalily's secret store.
- **Connector CA:** signs connector certificates, and the hub trusts it. The
  hub holds its key so it can issue and renew them.

Someone who takes the hub's key can mint connector identities. That gains
nothing, because those identities only authenticate to that hub. They can't
mint a hub certificate to impersonate the hub to connectors.

### Enrollment

Connectors get their own certificates:

1. Instalily gives the customer a **token**: a short string naming the
   connector's identity and the hub's CA, signed by Instalily, and valid for
   at most 7 days (24 hours by default). Until it expires it can enroll more
   than one replica, so treat it as a secret.
2. The connector generates its own key, which never leaves it. It connects to
   the hub (ALPN `edge-enroll/1`, no client certificate), checks that the
   hub's certificate chains to the CA the token names, and sends the token
   and a certificate request.
3. The hub checks the token's signature and expiry, that it names this hub's
   CA, and that `allowed_identities` (if set) includes the identity. It then
   returns a **7-day** certificate whose name comes from the token, not the
   request.
4. The connector keeps the certificate, its key and the hub's CA in
   `state_dir`, which must persist across restarts. After that it needs no
   token.
5. At two thirds of the certificate's lifetime, the connector renews it with
   a new key, authenticating with the current certificate (ALPN
   `edge-renew/1`). Shortly before the old certificate expires, the connector
   rotates any tunnel still using it: it opens a new tunnel with the renewed
   certificate, then lets the old one drain. The hub closes any tunnel whose
   certificate has expired regardless.

A connector offline for longer than its certificate lasts needs a new token.
To revoke a connector, remove its identity from `allowed_identities`: the hub
then refuses its tunnels and renewals.

### Operator steps

Once per customer, keeping every `.key` in Instalily's secret store:

```bash
edgectl ca -name acme -prefix hub-ca -out pki/
edgectl ca -name acme -prefix connector-ca -out pki/
edgectl issue -ca pki/ -ca-prefix hub-ca -name hub -dns acme.connect.instalily.ai -server -out pki/
```

`hub.crt` holds the hub's certificate followed by the hub CA, which connectors
pin. The hub needs:

- `hub.crt` and `hub.key` as `tunnel.tls.cert_file` and `key_file`;
- `connector-ca.crt` as `tunnel.tls.ca_file`;
- `connector-ca.key` as `enrollment.ca_key_file`;
- `hub-ca.crt` as `enrollment.hub_ca_file`;
- `token.pub` as `enrollment.token_keys_file`.

Once, for all customers:

```bash
edgectl token-key -out pki/   # token.key signs tokens; token.pub goes to hubs
```

For each connector, a token to send the customer:

```bash
edgectl token -key pki/token.key -hub-ca pki/hub-ca.crt -identity connector-1 -expires 24h
```

The customer's configuration then has no `tls` section:

```yaml
hub:
  address: acme.connect.instalily.ai:443
state_dir: /var/lib/edge-connector
enrollment:
  token_file: /etc/edge-connector/token
allow:
  - host: sap-prd.corp.local
    ports: [3300]
```

### Certificates by hand

Connectors can still use certificates issued by hand, through `tls` in their
configuration instead of `state_dir`:

```bash
edgectl issue -ca pki/ -ca-prefix connector-ca -name connector-1 -client -out pki/
```

These aren't renewed automatically. The hub closes a tunnel when its
certificate expires, just as it does for enrolled connectors.

## Releases

A cloud-admin cuts a release by pushing a `vMAJOR.MINOR.PATCH` tag on a `main`
commit. Only cloud-admins can create `v*` tags. [`release.yml`](.github/workflows/release.yml)
then:

1. Reruns all of CI on the tagged commit.
2. Pushes multi-arch (linux/amd64, linux/arm64) `edge-connector` and `edge-hub`
   images to ECR Public, with SBOM and provenance attestations. It refuses to
   overwrite an existing tag, since ECR Public tags are mutable.
3. Signs each image digest with cosign (keyless, through GitHub's OIDC
   identity).
4. Publishes a GitHub release with the pinned image references, the
   `cosign verify` command, and static `edge-connector` and `edgectl` binaries
   for linux/amd64, linux/arm64 and windows/amd64, with `SHA256SUMS`.

Deploy by digest, not by tag, and verify the signature first:

```bash
cosign verify \
  --certificate-identity 'https://github.com/Instalily/edge-connector/.github/workflows/release.yml@refs/tags/vX.Y.Z' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  public.ecr.aws/<alias>/edge-connector@sha256:<digest>
```

Only the release environment's identity can push these images. Instalily's
infra repository holds the repositories, the publisher role and the
release-tag rules.

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
- **TCP only.** No UDP.
- **Logs only.** No Prometheus metrics yet: tunnel count, streams, bytes, and
  refusals by reason.
- **Hub deployment outside GCP.** GCP hubs deploy with
  [`deploy/opentofu/gcp-hub`](deploy/opentofu/gcp-hub). Hubs move to the
  Platform clusters later, along with the solutions.
- **VM image.** Still to build: a Packer image running the connector as a
  systemd unit, for customers without containers.
