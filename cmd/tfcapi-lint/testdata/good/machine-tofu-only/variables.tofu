# Contract inputs of the machine role, v1alpha1 (https://captf.io/docs/module-author/contract/v1alpha1/common.html
# and machine.html).

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

# The cluster module's exports. The controller always sets it; the default
# follows the contract skeleton (machine.md).
variable "captf_cluster_outputs" {
  type    = any
  default = null
}

variable "captf_tags" {
  type = map(string)
}

variable "machine_name" {
  type = string
}

# Base64 of the bootstrap Secret's value.
variable "bootstrap_data" {
  type      = string
  sensitive = true
}

variable "bootstrap_format" {
  type = string
}

variable "failure_domain" {
  type    = string
  default = null
}

variable "kubernetes_version" {
  type    = string
  default = null
}

variable "control_plane" {
  type = bool
}
