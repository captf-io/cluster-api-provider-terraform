# A .tofu-only copy of good/machine (regression fixture: the linter once
# rejected every .tofu-only module): terraform-config-inspect
# never opens a .tofu file, so this fixture proves the hcl/v2 pass's own
# declarations model is what input/* and output/* read when the module
# has no .tf/.tf.json file at all.

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
