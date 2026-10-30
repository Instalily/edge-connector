variable "name" {
  description = "Customer slug, e.g. \"acme\". Resource names derive from it as edge-hub-<name>."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,19}[a-z0-9]$", var.name))
    error_message = "name must be 2-21 lowercase letters, digits or hyphens, starting with a letter and not ending with a hyphen."
  }
}

variable "project_id" {
  description = "The GCP project the hub runs in, usually the solution's own."
  type        = string
}

variable "region" {
  description = "Region of the subnetwork, addresses and load balancers."
  type        = string
}

variable "zone" {
  description = "Zone of the hub's instance group, inside region."
  type        = string
}

variable "network" {
  description = "Self link or ID of the VPC network the solution's services reach the hub through."
  type        = string
}

variable "subnetwork" {
  description = "Self link or ID of the subnetwork, in region, for the hub VM and the endpoints' internal address. The VM has no external IP: the subnetwork needs Cloud NAT to pull the image from ECR Public, and NAT or Private Google Access to read its secrets."
  type        = string
}

variable "image" {
  description = "The hub image, pinned by digest: public.ecr.aws/<alias>/edge-hub@sha256:<digest>, from a release's notes."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9.-]+(/[a-z0-9._-]+)+@sha256:[0-9a-f]{64}$", var.image))
    error_message = "image must be pinned by digest (…@sha256:<64 hex digits>), not a tag."
  }
}

variable "machine_type" {
  description = "Machine type of the hub VM."
  type        = string
  default     = "e2-small"
}

variable "tls" {
  description = "Secret Manager secrets, as projects/<project>/secrets/<name>, holding, all PEM: the connector CA certificate (which issues the connectors' certificates); the hub's certificate followed by the hub CA, so enrolling connectors can pin it; and the hub's private key. The hub reads their latest versions at boot. The hub certificate must name the address connectors dial: tunnel_ip, or a DNS name that resolves to it."
  type = object({
    ca_secret   = string
    cert_secret = string
    key_secret  = string
  })

  validation {
    condition = alltrue([
      for s in values(var.tls) : can(regex("^projects/[^/]+/secrets/[A-Za-z0-9_-]+$", s))
    ])
    error_message = "Each tls secret must be projects/<project>/secrets/<name>."
  }
}

variable "enrollment" {
  description = "Lets connectors enroll with a token and renew their own certificates. connector_ca_key_secret holds the connector CA's private key, which the hub signs with; hub_ca_secret the hub CA certificate (never its key). token_public_keys is the PEM of edgectl's token.pub, one or more keys; it is public. Null: connectors use certificates issued by hand."
  type = object({
    connector_ca_key_secret = string
    hub_ca_secret           = string
    token_public_keys       = string
    certificate_lifetime    = optional(string, "168h")
  })
  default = null

  validation {
    condition = var.enrollment == null ? true : (
      can(regex("^projects/[^/]+/secrets/[A-Za-z0-9_-]+$", var.enrollment.connector_ca_key_secret)) &&
      can(regex("^projects/[^/]+/secrets/[A-Za-z0-9_-]+$", var.enrollment.hub_ca_secret)) &&
      strcontains(var.enrollment.token_public_keys, "-----BEGIN PUBLIC KEY-----") &&
      !strcontains(var.enrollment.token_public_keys, "PRIVATE KEY") &&
      can(regex("^[0-9]+h$", var.enrollment.certificate_lifetime))
    )
    error_message = "enrollment needs two projects/<project>/secrets/<name> secrets, token_public_keys as PEM public keys (never the private token.key), and certificate_lifetime in hours, e.g. \"168h\"."
  }
}

variable "allowed_identities" {
  description = "When set, only connectors whose certificate common names are listed may connect."
  type        = list(string)
  default     = []
}

variable "tunnel_source_ranges" {
  description = "CIDRs connectors may dial the tunnel from: the customer's egress addresses when known. Defaults to anywhere, which certificate authentication still guards."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "endpoints" {
  description = "Targets in the customer network to expose. Each gets port on the endpoint IP; the connector dials target (host:port), resolved in the customer network."
  type = list(object({
    name   = string
    port   = number
    target = string
  }))

  validation {
    condition     = length(var.endpoints) > 0 && length(var.endpoints) <= 100
    error_message = "endpoints must list between 1 and 100 targets."
  }

  validation {
    condition = (
      length(distinct([for e in var.endpoints : e.name])) == length(var.endpoints) &&
      length(distinct([for e in var.endpoints : e.port])) == length(var.endpoints)
    )
    error_message = "Endpoint names and ports must be unique."
  }

  validation {
    condition = alltrue([
      for e in var.endpoints : (
        can(regex("^[a-z0-9-]+$", e.name)) &&
        e.port >= 1 && e.port <= 65535 && floor(e.port) == e.port &&
        !contains([22, 443, 8080], e.port) &&
        can(regex("^[^:\\s]+:[0-9]+$", e.target))
      )
    ])
    error_message = "Each endpoint needs a lowercase name, a port other than 22, 443 and 8080 (used by the VM), and a host:port target."
  }
}

variable "endpoint_source_ranges" {
  description = "CIDRs allowed to reach the endpoints: the solution's subnets and Serverless VPC Access connector ranges, never the internet."
  type        = list(string)

  validation {
    condition     = length(var.endpoint_source_ranges) > 0 && !contains(var.endpoint_source_ranges, "0.0.0.0/0")
    error_message = "endpoint_source_ranges must name the solution's own ranges, not 0.0.0.0/0."
  }
}

variable "labels" {
  description = "Labels added to every resource that takes them."
  type        = map(string)
  default     = {}
}
