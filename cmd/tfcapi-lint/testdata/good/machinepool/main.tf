# No-op machinepool module: implements the v1alpha1 machinepool role with
# no cloud and no native scaling group. autoscaling is declared (the
# contract input always exists) but not consumed: this module always sets
# the group's desired capacity from var.replicas, so it has nothing to
# ignore_changes on (machinepool.md "autoscaling").

# The stand-in for the scaling group. user_data decodes bootstrap_data the
# way a module feeding a plain user-data argument would, which proves the
# controller's base64 encoding round-trips (the value stays sensitive).
resource "terraform_data" "group" {
  input = {
    cluster                 = var.captf_cluster
    object                  = var.captf_object
    machinepool_name        = var.machinepool_name
    tags                    = var.captf_tags
    backend_id              = try(var.captf_cluster_outputs.backend_id, null)
    replicas                = var.replicas
    user_data               = base64decode(var.bootstrap_data)
    bootstrap_format        = var.bootstrap_format
    failure_domains         = var.failure_domains
    cluster_failure_domains = var.cluster_failure_domains
    kubernetes_version      = var.kubernetes_version
    node_labels             = var.node_labels
  }
}
