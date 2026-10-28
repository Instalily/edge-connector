mock_provider "google" {
  mock_resource "google_service_account" {
    defaults = {
      email  = "edge-hub-acme@acme-solution.iam.gserviceaccount.com"
      member = "serviceAccount:edge-hub-acme@acme-solution.iam.gserviceaccount.com"
    }
  }
}

variables {
  name       = "acme"
  project_id = "acme-solution"
  region     = "us-central1"
  zone       = "us-central1-a"
  network    = "projects/acme-solution/global/networks/main"
  subnetwork = "projects/acme-solution/regions/us-central1/subnetworks/main"
  image      = "public.ecr.aws/instalily/edge-hub@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

  tls = {
    ca_secret   = "projects/acme-solution/secrets/edge-hub-ca"
    cert_secret = "projects/acme-solution/secrets/edge-hub-cert"
    key_secret  = "projects/acme-solution/secrets/edge-hub-key"
  }

  endpoints = [
    { name = "sap", port = 3300, target = "sap-prd.corp.local:3300" },
    { name = "postgres", port = 5432, target = "db.corp.local:5432" },
  ]
  endpoint_source_ranges = ["10.8.0.0/28"]
}

run "hub_config_maps_endpoints_to_unprivileged_container_ports" {
  command = plan

  assert {
    condition = yamldecode(local.hub_config).endpoints == [
      { name = "sap", listen = ":20000", target = "sap-prd.corp.local:3300" },
      { name = "postgres", listen = ":20001", target = "db.corp.local:5432" },
    ]
    error_message = "Endpoint i must listen on container port 20000+i and target its configured host:port."
  }

  assert {
    condition = (
      yamldecode(local.hub_config).tunnel.listen == ":8443" &&
      yamldecode(local.hub_config).health_listen == ":8080" &&
      yamldecode(local.hub_config).tunnel.tls.key_file == "/etc/edge-hub/tls/tls.key"
    )
    error_message = "The hub must listen on 8443 for tunnels and 8080 for health, with TLS files from /etc/edge-hub/tls."
  }
}

run "cloud_init_runs_the_pinned_image_hardened_and_publishes_only_the_hub_ports" {
  command = plan

  assert {
    condition     = can(yamldecode(local.user_data).write_files)
    error_message = "user-data must be valid cloud-config YAML."
  }

  assert {
    condition = strcontains(
      one([for f in yamldecode(local.user_data).write_files : f.content if f.path == "/etc/systemd/system/edge-hub.service"]),
      "docker run --rm --name edge-hub --read-only --cap-drop ALL --security-opt no-new-privileges --user 65532:65532 --log-driver journald -v /run/edge-hub:/etc/edge-hub:ro -p 443:8443 -p 8080:8080 -p 3300:20000 -p 5432:20001 ${var.image} -config /etc/edge-hub/config.yaml"
    )
    error_message = "The unit must run the digest-pinned image read-only, unprivileged, with exactly the tunnel, health and endpoint ports published."
  }

  assert {
    condition = yamldecode(local.user_data).runcmd == [
      "iptables -w -A INPUT -p tcp --dport 443 -j ACCEPT",
      "iptables -w -A INPUT -p tcp --dport 8080 -j ACCEPT",
      "iptables -w -A INPUT -p tcp --dport 3300 -j ACCEPT",
      "iptables -w -A INPUT -p tcp --dport 5432 -j ACCEPT",
      "systemctl daemon-reload",
      "systemctl start edge-hub.service",
    ]
    error_message = "cloud-init must open exactly the hub's host ports and start the unit."
  }

  assert {
    condition = alltrue([
      for s in values(var.tls) : strcontains(
        one([for f in yamldecode(local.user_data).write_files : f.content if f.path == "/var/lib/edge-hub/fetch-tls.sh"]),
        "fetch '${s}'"
      )
    ])
    error_message = "The boot script must fetch each configured TLS secret."
  }
}

run "vm_has_no_external_ip_and_reads_only_its_own_secrets" {
  command = plan

  assert {
    condition     = length(google_compute_instance_template.hub.network_interface[0].access_config) == 0
    error_message = "The hub VM must not have an external IP."
  }

  assert {
    condition = (
      toset([for m in google_secret_manager_secret_iam_member.hub : m.secret_id]) == toset(values(var.tls)) &&
      alltrue([for m in google_secret_manager_secret_iam_member.hub : m.role == "roles/secretmanager.secretAccessor"])
    )
    error_message = "The hub's service account may read exactly its three TLS secrets."
  }

  assert {
    condition     = google_compute_instance_template.hub.shielded_instance_config[0].enable_secure_boot
    error_message = "The hub VM must use Secure Boot."
  }
}

run "firewall_admits_tunnel_endpoints_and_health_checks_only" {
  command = plan

  assert {
    condition = (
      flatten([for a in google_compute_firewall.tunnel.allow : a.ports]) == ["443"] &&
      flatten([for a in google_compute_firewall.endpoints.allow : a.ports]) == ["3300", "5432"] &&
      google_compute_firewall.endpoints.source_ranges == toset(["10.8.0.0/28"]) &&
      flatten([for a in google_compute_firewall.health_checks.allow : a.ports]) == ["8080"]
    )
    error_message = "Firewall rules must admit 443 for tunnels, only the endpoint ports from the solution's ranges, and 8080 from health checkers."
  }

  assert {
    condition     = google_compute_forwarding_rule.tunnel.ports == toset(["443"]) && google_compute_forwarding_rule.tunnel.load_balancing_scheme == "EXTERNAL"
    error_message = "The tunnel must be an external passthrough load balancer on 443 only."
  }

  assert {
    condition     = google_compute_forwarding_rule.endpoints.load_balancing_scheme == "INTERNAL"
    error_message = "Endpoints must be reachable on an internal load balancer only."
  }
}

run "rejects_a_tag_instead_of_a_digest" {
  command = plan
  variables {
    image = "public.ecr.aws/instalily/edge-hub:v0.1.0"
  }
  expect_failures = [var.image]
}

run "rejects_endpoint_ports_the_vm_uses" {
  command = plan
  variables {
    endpoints = [{ name = "https", port = 443, target = "app.corp.local:443" }]
  }
  expect_failures = [var.endpoints]
}

run "rejects_duplicate_endpoint_ports" {
  command = plan
  variables {
    endpoints = [
      { name = "a", port = 5432, target = "a.corp.local:5432" },
      { name = "b", port = 5432, target = "b.corp.local:5432" },
    ]
  }
  expect_failures = [var.endpoints]
}

run "rejects_endpoints_open_to_the_internet" {
  command = plan
  variables {
    endpoint_source_ranges = ["0.0.0.0/0"]
  }
  expect_failures = [var.endpoint_source_ranges]
}
