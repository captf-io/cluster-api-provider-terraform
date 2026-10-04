terraform {
  required_version = ">= 1.10"
}

resource "terraform_data" "instance" {
  input = {
    name      = var.machine_name
    tags      = var.captf_tags
    user_data = var.bootstrap_data
  }
}

output "provider_id" {
  value = "noop:///${var.machine_name}"
}

output "addresses" {
  value = [{ type = "InternalIP", address = "10.0.0.1" }]
}

output "failure_domain" {
  value = var.failure_domain
}

output "health" {
  value = { state = "running", healthy = true, message = null, reasons = [] }
}
