# Stub machine-role module: every contract input with the precise type a real
# module declares, and every required output. Used only to validate the
# rendered root with real terraform/tofu binaries (internal/render/README.md).

variable "captf_contract" { type = string }
variable "captf_cluster" {
  type = object({ name = string, namespace = string })
}
variable "captf_object" {
  type = object({ kind = string, name = string, namespace = string })
}
variable "captf_tags" { type = map(string) }
variable "captf_cluster_outputs" { type = any }
variable "machine_name" { type = string }
variable "bootstrap_data" {
  type      = string
  sensitive = true
}
variable "bootstrap_format" { type = string }
variable "failure_domain" {
  type    = string
  default = null
}
variable "kubernetes_version" {
  type    = string
  default = null
}
variable "control_plane" { type = bool }

output "provider_id" { value = "stub://${var.machine_name}" }
output "addresses" {
  value = [{ type = "InternalIP", address = "10.0.0.10" }]
}
output "failure_domain" { value = var.failure_domain }
output "interruptible" { value = false }
output "health" {
  value = { state = "running", healthy = true, message = null, reasons = [] }
}
