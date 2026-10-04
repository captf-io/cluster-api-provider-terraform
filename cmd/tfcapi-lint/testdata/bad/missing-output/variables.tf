# A minimal machine module that satisfies contract v1alpha1 with zero
# findings under --strict (a stand-in fixture, independent of
# the noop machine module).

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

variable "captf_cluster_outputs" {
  type = any
}

variable "machine_name" {
  type = string
}

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
