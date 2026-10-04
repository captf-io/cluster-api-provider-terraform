# Stub cluster-role module: every contract input with the precise type a real
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
variable "control_plane_endpoint" {
  type    = object({ host = string, port = number })
  default = null
}
variable "kubernetes_version" {
  type    = string
  default = null
}
variable "control_plane_initialized" { type = bool }
variable "cluster_network" {
  type = object({
    pods            = list(string)
    services        = list(string)
    service_domain  = string
    api_server_port = number
  })
  default = null
}

output "control_plane_endpoint" { value = var.control_plane_endpoint }
output "failure_domains" {
  value = [{ name = "zone-a", control_plane = true, attributes = {} }]
}
output "exports" {
  value = { network_id = "net-1" }
}
output "health" {
  value = { state = "running", healthy = true, message = null, reasons = [] }
}
