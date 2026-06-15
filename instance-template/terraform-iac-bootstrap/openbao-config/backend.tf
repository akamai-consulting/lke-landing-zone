terraform {
  # Linode Object Storage is S3-compatible; use the S3 backend.
  #
  # All backend parameters are supplied at init time via -backend-config flags
  # or a backend.tfvars file (never committed).  The CI workflow provides them
  # from GitHub Actions secrets.
  #
  # Required -backend-config keys:
  #   bucket   = "platform-terraform-state"
  #   key      = "openbao-config/<region>/terraform.tfstate"
  #   region   = "us-east-1"   (dummy; required by S3 backend)
  #
  # Credentials and endpoint are injected via environment variables:
  #   AWS_ACCESS_KEY_ID     = Linode Object Storage access key  (GitHub secret: TF_STATE_ACCESS_KEY)
  #   AWS_SECRET_ACCESS_KEY = Linode Object Storage secret key  (GitHub secret: TF_STATE_SECRET_KEY)
  #   AWS_ENDPOINT_URL_S3   = Linode Object Storage endpoint    (GitHub var:    TF_STATE_ENDPOINT)
  backend "s3" {
    skip_credentials_validation = true
    skip_metadata_api_check     = true
    skip_region_validation      = true
    skip_requesting_account_id  = true
    skip_s3_checksum            = true
    use_path_style              = true
  }
}
