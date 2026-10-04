# Contract outputs of the machine role, v1alpha1.

# Stable per Machine. With no Node behind it, the e2e suite never expects a
# nodeRef; a real module emits the CCM's or kubelet's format (machine.md).
output "provider_id" {
  value = "noop:///${var.captf_object.namespace}/${var.machine_name}"
}

output "addresses" {
  value = [{ type = "InternalIP", address = "10.0.0.1" }]
}

output "failure_domain" {
  value = var.failure_domain
}

output "interruptible" {
  value = false
}

output "health" {
  value = { state = "running", healthy = true, message = null, reasons = [] }
}
