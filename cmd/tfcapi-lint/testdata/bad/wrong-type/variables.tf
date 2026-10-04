# A minimal cluster module that satisfies contract v1alpha1 with zero
# findings under --strict (a stand-in fixture, independent of
# the noop cluster module).

variable "captf_contract" {
  type = string
}

variable "captf_cluster" {
  type = any
}

variable "captf_object" {
  type = any
}

variable "captf_tags" {
  type = map(string)
}

variable "control_plane_endpoint" {
  type = object({
    host = string
    port = number
  })
  default = null
}

variable "kubernetes_version" {
  type    = string
  default = null
}

variable "control_plane_initialized" {
  type = string
}

variable "cluster_network" {
  type = object({
    pods            = list(string)
    services        = list(string)
    service_domain  = string
    api_server_port = number
  })
  default = null
}
