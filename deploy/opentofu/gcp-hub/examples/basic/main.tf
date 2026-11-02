# A solution's GCP root deploying its customer's hub, with connectors that
# enroll. The secrets are created and filled outside OpenTofu, so their
# contents never enter state; see ../../README.md for the edgectl and gcloud
# steps. Keep pki/hub-ca.key and token.key in Instalily's secrets store.

module "edge_hub" {
  source = "git::https://github.com/Instalily/edge-connector.git//deploy/opentofu/gcp-hub?ref=v0.3.0"

  name       = "acme"
  project_id = "acme-solution"
  region     = "us-central1"
  zone       = "us-central1-a"
  network    = "projects/acme-solution/global/networks/main"
  subnetwork = "projects/acme-solution/regions/us-central1/subnetworks/main"

  # From the v0.3.0 release notes.
  image = "public.ecr.aws/s9u8g9q7/edge-hub@sha256:a6be365833a30bb12ffb8efcb5b6071bd824b1bfa5d5f483041ac00dce4969d0"

  tls = {
    ca_secret   = "projects/acme-solution/secrets/edge-hub-connector-ca"
    cert_secret = "projects/acme-solution/secrets/edge-hub-cert"
    key_secret  = "projects/acme-solution/secrets/edge-hub-key"
  }
  enrollment = {
    connector_ca_key_secret = "projects/acme-solution/secrets/edge-hub-connector-ca-key"
    hub_ca_secret           = "projects/acme-solution/secrets/edge-hub-hub-ca"
    token_public_keys       = file("${path.module}/token.pub")
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
