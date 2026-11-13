terraform {
  required_version = ">= 1.9.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "< 9.0.0"
    }
    # Only for the instance group manager's update_policy.min_ready_sec,
    # which the google provider doesn't have. Without a provider block, it
    # uses the same credentials as google.
    google-beta = {
      source  = "hashicorp/google-beta"
      version = ">= 6.50.0, < 8.0.0"
    }
  }
}
