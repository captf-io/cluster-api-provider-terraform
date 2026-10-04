# Stub machinepool-role module: every contract input with the precise type
# a real module declares, and every required output. Used only to validate
# the rendered root with real terraform/tofu binaries
# (internal/render/README.md).

variable "captf_contract" { type = string }
variable "captf_cluster" {
  type = object({ name = string, namespace = string })
}
variable "captf_object" {
  type = object({ kind = string, name = string, namespace = string })
}
variable "captf_tags" { type = map(string) }
variable "captf_cluster_outputs" { type = any }
variable "machinepool_name" { type = string }
variable "replicas" { type = number }
variable "bootstrap_data" {
  type      = string
  sensitive = true
}
variable "bootstrap_format" { type = string }
variable "failure_domains" {
  type    = list(string)
  default = []
}
variable "cluster_failure_domains" {
  type    = list(string)
  default = []
}
variable "kubernetes_version" {
  type    = string
  default = null
}
variable "node_labels" {
  type    = map(string)
  default = {}
}
variable "autoscaling" {
  type = object({ enabled = bool, min = number, max = number })
}

locals {
  ids = sort([for i in range(var.replicas) : "stub://${var.captf_object.name}/${i}"])
}

output "provider_id"      { value = "stub-group://${var.captf_object.name}" }
output "provider_id_list" { value = local.ids }
output "replicas"         { value = var.replicas }
output "instances" {
  value = [for id in local.ids : { provider_id = id, state = "running" }]
}
output "health" {
  value = { state = "running", healthy = true, message = null, reasons = [] }
}
