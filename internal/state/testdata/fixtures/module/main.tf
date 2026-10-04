# Fixture root for the state Secrets in test/fixtures/state/*.yaml
# (hack/fixtures-state.sh). It needs no providers: terraform_data is
# built in, so init never contacts a registry. The outputs use the machine
# role's contract names, with health marked sensitive, so the decoder can
# decode real state. blob inflates the state to force Terraform's -part-N chunks.

terraform {
  backend "kubernetes" {}
}

variable "blob" {
  type    = string
  default = ""
}

resource "terraform_data" "instance" {
  input = { name = "fixture-instance-1" }
}

output "provider_id" {
  value = "fixture://${terraform_data.instance.output.name}"
}

output "addresses" {
  value = [
    { type = "InternalIP", address = "10.0.0.10" },
    { type = "Hostname", address = "fixture-instance-1" },
  ]
}

output "failure_domain" {
  value = null
}

output "interruptible" {
  value = false
}

output "health" {
  value     = { state = "running", healthy = true, message = null, reasons = [] }
  sensitive = true
}

# Sensitive only to keep the CLI from printing the chunked case's 2 MiB.
output "blob" {
  value     = var.blob
  sensitive = true
}
