# No-op machine module: implements the v1alpha1 machine role with no cloud.

# The stand-in for an instance. user_data decodes bootstrap_data the way a
# module feeding a plain user-data argument would, which proves the
# controller's base64 encoding round-trips (the value stays sensitive).
resource "terraform_data" "instance" {
  input = {
    cluster            = var.captf_cluster
    object             = var.captf_object
    machine_name       = var.machine_name
    tags               = var.captf_tags
    backend_id         = try(var.captf_cluster_outputs.backend_id, null)
    user_data          = base64decode(var.bootstrap_data)
    bootstrap_format   = var.bootstrap_format
    failure_domain     = var.failure_domain
    kubernetes_version = var.kubernetes_version
    control_plane      = var.control_plane
  }
}
