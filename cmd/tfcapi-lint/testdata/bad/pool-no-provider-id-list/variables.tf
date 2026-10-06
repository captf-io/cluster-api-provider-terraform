# Contract inputs of the machinepool role, v1alpha1 (https://captf.io/docs/module-author/contract/v1alpha1/common.html
# and machinepool.html).

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
# follows the contract skeleton (machinepool.md).
variable "captf_cluster_outputs" {
  type    = any
  default = null
}

variable "captf_tags" {
  type = map(string)
}

variable "machinepool_name" {
  type = string
}

# The group's desired capacity.
variable "replicas" {
  type = number
}

# Base64 of the bootstrap Secret's value.
variable "bootstrap_data" {
  type      = string
  sensitive = true
}

variable "bootstrap_format" {
  type = string
}

variable "failure_domains" {
  type = list(string)
}

variable "cluster_failure_domains" {
  type = list(string)
}

variable "kubernetes_version" {
  type    = string
  default = null
}

variable "node_labels" {
  type = map(string)
}

variable "autoscaling" {
  type = object({
    enabled = bool
    min     = number
    max     = number
  })
}
