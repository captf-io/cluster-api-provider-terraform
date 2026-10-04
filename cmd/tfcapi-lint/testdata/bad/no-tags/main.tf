terraform {
  required_version = ">= 1.10"
}

resource "terraform_data" "load_balancer" {
  input = {
    tags = var.captf_tags
  }
}

output "control_plane_endpoint" {
  value = var.control_plane_endpoint != null ? var.control_plane_endpoint : { host = "lb.example", port = 6443 }
}

output "failure_domains" {
  value = [{ name = "zone-a", control_plane = true }]
}

output "exports" {
  value = { network_id = terraform_data.load_balancer.id }
}

output "health" {
  value = { state = "running", healthy = true, message = null, reasons = [] }
}
