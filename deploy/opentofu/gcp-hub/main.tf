locals {
  name = "edge-hub-${var.name}"
  tag  = local.name

  labels = merge(var.labels, {
    "instalily-component" = "edge-hub"
    "instalily-customer"  = var.name
  })

  # Inside the container the hub listens on 8443 (tunnel), 8080 (health) and
  # 20000+i for endpoint i, all unprivileged. Docker publishes them on the
  # VM's ports: 443, 8080 and each endpoint's own port.
  tunnel_container_port = 8443
  health_port           = 8080
  endpoint_container_ports = {
    for i, e in var.endpoints : e.name => 20000 + i
  }

  # Secret Manager secrets the VM reads at boot, by the file name they get in
  # /etc/edge-hub/tls. With enrollment, the hub also holds the connector CA's
  # key, to sign with, and the hub CA's certificate, which tokens must name.
  secret_files = merge(
    {
      "ca.crt"  = var.tls.ca_secret
      "tls.crt" = var.tls.cert_secret
      "tls.key" = var.tls.key_secret
    },
    var.enrollment == null ? {} : {
      "connector-ca.key" = var.enrollment.connector_ca_key_secret
      "hub-ca.crt"       = var.enrollment.hub_ca_secret
    },
  )

  hub_config = yamlencode(merge({
    tunnel = {
      listen = ":${local.tunnel_container_port}"
      tls = {
        ca_file   = "/etc/edge-hub/tls/ca.crt"
        cert_file = "/etc/edge-hub/tls/tls.crt"
        key_file  = "/etc/edge-hub/tls/tls.key"
      }
      allowed_identities = var.allowed_identities
    }
    }, var.enrollment == null ? {} : {
    enrollment = {
      ca_key_file          = "/etc/edge-hub/tls/connector-ca.key"
      hub_ca_file          = "/etc/edge-hub/tls/hub-ca.crt"
      token_keys_file      = "/etc/edge-hub/token.pub"
      certificate_lifetime = var.enrollment.certificate_lifetime
    }
    }, {
    endpoints = [
      for e in var.endpoints : {
        name   = e.name
        listen = ":${local.endpoint_container_ports[e.name]}"
        target = e.target
      }
    ]
    open_timeout  = "15s"
    health_listen = ":${local.health_port}"
  }))

  published_ports = concat(
    ["443:${local.tunnel_container_port}", "${local.health_port}:${local.health_port}"],
    [for e in var.endpoints : "${e.port}:${local.endpoint_container_ports[e.name]}"],
  )
  host_ports = concat([443, local.health_port], [for e in var.endpoints : e.port])

  user_data = templatefile("${path.module}/cloud-init.yaml.tftpl", {
    config            = local.hub_config
    image             = var.image
    port_flags        = join(" ", [for p in local.published_ports : "-p ${p}"])
    host_ports        = local.host_ports
    secret_files      = local.secret_files
    token_public_keys = var.enrollment == null ? "" : trimspace(var.enrollment.token_public_keys)
  })

  # Google's health checkers: both passthrough load balancers and autohealing.
  health_check_source_ranges = ["35.191.0.0/16", "130.211.0.0/22", "209.85.152.0/22", "209.85.204.0/22"]
}

# --- Identity -----------------------------------------------------------------

resource "google_service_account" "hub" {
  project      = var.project_id
  account_id   = local.name
  display_name = "Edge hub for ${var.name}"
  description  = "Runs the edge hub VM: reads its TLS secrets and writes logs and metrics, nothing else."
}

resource "google_project_iam_member" "hub" {
  for_each = toset(["roles/logging.logWriter", "roles/monitoring.metricWriter"])

  project = var.project_id
  role    = each.value
  member  = google_service_account.hub.member
}

resource "google_secret_manager_secret_iam_member" "hub" {
  for_each = local.secret_files

  secret_id = each.value
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.hub.member
}

# --- VM -----------------------------------------------------------------------

resource "google_compute_instance_template" "hub" {
  project      = var.project_id
  region       = var.region
  name_prefix  = "${local.name}-"
  machine_type = var.machine_type
  tags         = [local.tag]
  labels       = local.labels

  disk {
    source_image = "projects/cos-cloud/global/images/family/cos-stable"
    boot         = true
    auto_delete  = true
    disk_type    = "pd-balanced"
    disk_size_gb = 20
  }

  # No access_config: the VM has no external IP. Connectors reach it through
  # the tunnel load balancer.
  network_interface {
    subnetwork = var.subnetwork
  }

  service_account {
    email  = google_service_account.hub.email
    scopes = ["cloud-platform"]
  }

  shielded_instance_config {
    enable_secure_boot          = true
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  metadata = {
    "user-data"                 = local.user_data
    "google-logging-enabled"    = "true"
    "google-monitoring-enabled" = "true"
    "enable-oslogin"            = "TRUE"
    "block-project-ssh-keys"    = "true"
  }

  lifecycle {
    create_before_destroy = true
  }

  # The secret grants must exist before a VM boots and reads the secrets.
  depends_on = [google_secret_manager_secret_iam_member.hub]
}

resource "google_compute_health_check" "autohealing" {
  project             = var.project_id
  name                = "${local.name}-autohealing"
  check_interval_sec  = 10
  timeout_sec         = 5
  healthy_threshold   = 2
  unhealthy_threshold = 6

  http_health_check {
    port         = local.health_port
    request_path = "/healthz"
  }
}

resource "google_compute_instance_group_manager" "hub" {
  project            = var.project_id
  zone               = var.zone
  name               = local.name
  base_instance_name = local.name
  target_size        = 1

  version {
    instance_template = google_compute_instance_template.hub.self_link_unique
  }

  auto_healing_policies {
    health_check      = google_compute_health_check.autohealing.id
    initial_delay_sec = 180
  }

  # A new template starts its VM before the old one stops, so a release
  # interrupts tunnels only for as long as connectors take to reconnect.
  update_policy {
    type                  = "PROACTIVE"
    minimal_action        = "REPLACE"
    replacement_method    = "SUBSTITUTE"
    max_surge_fixed       = 1
    max_unavailable_fixed = 0
  }
}

resource "google_compute_region_health_check" "hub" {
  project             = var.project_id
  region              = var.region
  name                = "${local.name}-lb"
  check_interval_sec  = 5
  timeout_sec         = 5
  healthy_threshold   = 2
  unhealthy_threshold = 2

  http_health_check {
    port         = local.health_port
    request_path = "/healthz"
  }
}

# --- Tunnel: external passthrough network load balancer on TCP 443 -------------

resource "google_compute_address" "tunnel" {
  project      = var.project_id
  region       = var.region
  name         = "${local.name}-tunnel"
  address_type = "EXTERNAL"
  labels       = local.labels
}

resource "google_compute_region_backend_service" "tunnel" {
  project               = var.project_id
  region                = var.region
  name                  = "${local.name}-tunnel"
  load_balancing_scheme = "EXTERNAL"
  protocol              = "TCP"
  health_checks         = [google_compute_region_health_check.hub.id]

  backend {
    group          = google_compute_instance_group_manager.hub.instance_group
    balancing_mode = "CONNECTION"
  }
}

resource "google_compute_forwarding_rule" "tunnel" {
  project               = var.project_id
  region                = var.region
  name                  = "${local.name}-tunnel"
  load_balancing_scheme = "EXTERNAL"
  ip_protocol           = "TCP"
  ip_address            = google_compute_address.tunnel.address
  ports                 = ["443"]
  backend_service       = google_compute_region_backend_service.tunnel.id
  labels                = local.labels
}

# --- Endpoints: internal passthrough network load balancer ---------------------

resource "google_compute_address" "endpoints" {
  project      = var.project_id
  region       = var.region
  name         = "${local.name}-endpoints"
  address_type = "INTERNAL"
  subnetwork   = var.subnetwork
  labels       = local.labels
}

resource "google_compute_region_backend_service" "endpoints" {
  project               = var.project_id
  region                = var.region
  name                  = "${local.name}-endpoints"
  load_balancing_scheme = "INTERNAL"
  protocol              = "TCP"
  network               = var.network
  health_checks         = [google_compute_region_health_check.hub.id]

  backend {
    group          = google_compute_instance_group_manager.hub.instance_group
    balancing_mode = "CONNECTION"
  }
}

# All ports on the internal address; the endpoints firewall rule admits only
# the configured ones.
resource "google_compute_forwarding_rule" "endpoints" {
  project               = var.project_id
  region                = var.region
  name                  = "${local.name}-endpoints"
  load_balancing_scheme = "INTERNAL"
  ip_protocol           = "TCP"
  all_ports             = true
  network               = var.network
  subnetwork            = var.subnetwork
  ip_address            = google_compute_address.endpoints.address
  backend_service       = google_compute_region_backend_service.endpoints.id
  labels                = local.labels
}

# --- Firewall -----------------------------------------------------------------

resource "google_compute_firewall" "tunnel" {
  project       = var.project_id
  name          = "${local.name}-tunnel"
  network       = var.network
  description   = "Connectors to the edge hub's tunnel port"
  direction     = "INGRESS"
  source_ranges = var.tunnel_source_ranges
  target_tags   = [local.tag]

  allow {
    protocol = "tcp"
    ports    = ["443"]
  }
}

resource "google_compute_firewall" "endpoints" {
  project       = var.project_id
  name          = "${local.name}-endpoints"
  network       = var.network
  description   = "The solution's services to the edge hub's endpoints"
  direction     = "INGRESS"
  source_ranges = var.endpoint_source_ranges
  target_tags   = [local.tag]

  allow {
    protocol = "tcp"
    ports    = [for e in var.endpoints : tostring(e.port)]
  }
}

resource "google_compute_firewall" "health_checks" {
  project       = var.project_id
  name          = "${local.name}-health-checks"
  network       = var.network
  description   = "Google health checkers to the edge hub's health port"
  direction     = "INGRESS"
  source_ranges = local.health_check_source_ranges
  target_tags   = [local.tag]

  allow {
    protocol = "tcp"
    ports    = [tostring(local.health_port)]
  }
}
