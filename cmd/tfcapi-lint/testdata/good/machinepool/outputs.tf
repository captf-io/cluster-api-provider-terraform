# Contract outputs of the machinepool role, v1alpha1.

locals {
  # Synthetic per-replica ids; unsorted here, sorted at each output that
  # needs the canonical order (provider_id_list must be sort()/distinct()
  # wrapped directly in its own output expression: output/provider-id-list-shape).
  raw_ids = [for i in range(var.replicas) : "noop:///${var.captf_object.namespace}/${var.captf_object.name}/${i}"]
}

# Stable per TerraformMachinePool. With no native scaling group behind it,
# this is a synthetic group id (machinepool.md "provider_id ... may stay
# null for group-less implementations"; this module chooses to set one).
output "provider_id" {
  value = "noop-group:///${var.captf_object.namespace}/${var.captf_object.name}"
}

output "provider_id_list" {
  value = sort(local.raw_ids)
}

output "replicas" {
  value = var.replicas
}

output "instances" {
  value = [for id in sort(local.raw_ids) : { provider_id = id, state = "running" }]
}

output "health" {
  value = { state = "running", healthy = true, message = null, reasons = [] }
}
