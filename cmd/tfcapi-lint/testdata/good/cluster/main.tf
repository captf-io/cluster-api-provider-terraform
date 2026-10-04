# No-op cluster module: implements the v1alpha1 cluster role with no cloud.
# Every "resource" is a terraform_data holding the inputs it was given, so
# plans, state and outputs are real while nothing is provisioned.

# The stand-in for a load balancer and network: exports carry its id, so
# machines receive a value that only exists after this module applied.
resource "terraform_data" "load_balancer" {
  input = {
    cluster                   = var.captf_cluster
    object                    = var.captf_object
    tags                      = var.captf_tags
    control_plane_endpoint    = var.control_plane_endpoint
    kubernetes_version        = var.kubernetes_version
    control_plane_initialized = var.control_plane_initialized
    cluster_network           = var.cluster_network
  }
}
