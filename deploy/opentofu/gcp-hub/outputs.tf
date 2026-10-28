output "tunnel_ip" {
  description = "External IP connectors dial on port 443. The hub certificate must name it, or a DNS name that resolves to it."
  value       = google_compute_address.tunnel.address
}

output "tunnel_address" {
  description = "host:port for the connector's hub.address, when connectors dial the IP directly."
  value       = "${google_compute_address.tunnel.address}:443"
}

output "endpoint_ip" {
  description = "Internal IP the solution's services connect to."
  value       = google_compute_address.endpoints.address
}

output "endpoints" {
  description = "Each endpoint's internal host:port, by name."
  value       = { for e in var.endpoints : e.name => "${google_compute_address.endpoints.address}:${e.port}" }
}

output "service_account_email" {
  description = "The hub VM's service account."
  value       = google_service_account.hub.email
}

output "instance_group" {
  description = "The hub's managed instance group."
  value       = google_compute_instance_group_manager.hub.instance_group
}
