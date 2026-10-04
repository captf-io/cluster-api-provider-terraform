# Contract outputs of the cluster role, v1alpha1.

# A user or control-plane endpoint wins; otherwise a stable, valid endpoint
# derived from the object name (.invalid never resolves, RFC 2606).
output "control_plane_endpoint" {
  value = var.control_plane_endpoint != null ? var.control_plane_endpoint : {
    host = "noop-${var.captf_object.name}.invalid"
    port = 6443
  }
}

output "failure_domains" {
  value = [{ name = "fd1", control_plane = true }]
}

output "exports" {
  value = { backend_id = "noop-backend-${terraform_data.load_balancer.id}" }
}

output "health" {
  value = { state = "running", healthy = true, message = null, reasons = [] }
}
