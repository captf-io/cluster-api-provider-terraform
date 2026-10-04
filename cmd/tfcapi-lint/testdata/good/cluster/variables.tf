# Contract inputs of the cluster role, v1alpha1 (https://captf.io/docs/module-author/contract/v1alpha1/common.html
# and cluster.html). The controller sets every one; the no-op module
# records them in terraform_data so a plan shows what it was given.

variable "captf_contract" {
  type = string
}

variable "captf_cluster" {
  type = object({
    name      = string
    namespace = string
  })
}

variable "captf_object" {
  type = object({
    kind      = string
    name      = string
    namespace = string
  })
}

# No captf_cluster_outputs: the cluster role never receives it (see the
# contract CHANGELOG). The skeleton's defaulted declaration is optional.

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
  type = bool
}

variable "cluster_network" {
  type = object({
    pods            = optional(list(string), [])
    services        = optional(list(string), [])
    service_domain  = optional(string)
    api_server_port = optional(number)
  })
  default = null
}
