terraform {
  # State for the in-cluster bootstrap resources (ArgoCD, controllers, root app).
  # Lives in its own S3 backend key so the cluster workspace can be destroyed
  # without touching this state, and so the cluster module no longer needs the
  # kubernetes/helm/kubectl providers.
  #
  # Required -backend-config keys (matching the cluster workspace pattern;
  # the key's middle segment is the deployment discriminator — var.deployment —
  # supplied by CI at init time):
  #   bucket   = "platform-terraform-state"
  #   key      = "cluster-bootstrap/<deployment>/terraform.tfstate"
  #   region   = "us-east-1"                          # dummy; required by S3 backend
  #
  # The S3 endpoint is provided via AWS_ENDPOINT_URL_S3 (set in the workflow env).
  backend "s3" {
    skip_credentials_validation = true
    skip_metadata_api_check     = true
    skip_region_validation      = true
    skip_requesting_account_id  = true
    skip_s3_checksum            = true
    use_path_style              = true
  }
}
