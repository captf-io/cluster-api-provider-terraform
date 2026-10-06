terraform {
  # terraform_data needs Terraform 1.4; the variants' plantimestamp() needs
  # 1.5. Every OpenTofu release (1.6+) has both.
  required_version = ">= 1.5"
}
