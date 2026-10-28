# A solution's GCP root deploying its customer's hub. The TLS secrets are
# created and filled outside OpenTofu, so their contents never enter state:
#
#   edgectl ca -name acme -out pki/
#   edgectl issue -ca pki/ -name hub -ip <tunnel_ip> -server -out pki/
#   gcloud secrets create edge-hub-ca   --data-file=pki/ca.crt
#   gcloud secrets create edge-hub-cert --data-file=pki/hub.crt
#   gcloud secrets create edge-hub-key  --data-file=pki/hub.key
#
# Keep pki/ca.key in Instalily's secrets store, not on the hub.

module "edge_hub" {
  source = "git::https://github.com/Instalily/edge-connector.git//deploy/opentofu/gcp-hub?ref=v0.1.0"

  name       = "acme"
  project_id = "acme-solution"
  region     = "us-central1"
  zone       = "us-central1-a"
  network    = "projects/acme-solution/global/networks/main"
  subnetwork = "projects/acme-solution/regions/us-central1/subnetworks/main"

  # From the v0.1.0 release notes.
  image = "public.ecr.aws/instalily/edge-hub@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

  tls = {
    ca_secret   = "projects/acme-solution/secrets/edge-hub-ca"
    cert_secret = "projects/acme-solution/secrets/edge-hub-cert"
    key_secret  = "projects/acme-solution/secrets/edge-hub-key"
  }
  allowed_identities = ["connector-1"]

  endpoints = [
    { name = "sap", port = 3300, target = "sap-prd.corp.local:3300" },
  ]
  # The Cloud Run services' Serverless VPC Access connector range.
  endpoint_source_ranges = ["10.8.0.0/28"]
}

# The solution's Cloud Run service connects to this host:port.
output "sap_endpoint" {
  value = module.edge_hub.endpoints["sap"]
}

# The customer sets the connector's hub.address to this.
output "connector_hub_address" {
  value = module.edge_hub.tunnel_address
}
