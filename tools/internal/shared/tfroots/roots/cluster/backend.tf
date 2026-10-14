terraform {
  # Linode Object Storage is S3-compatible; use the S3 backend.
  #
  # All backend parameters are supplied at init time via -backend-config flags
  # or a backend.tfvars file (never committed).  The CI workflow provides them
  # from GitHub Actions secrets.
  #
  # Required -backend-config keys:
  #   bucket   = "platform-terraform-state"
  #   key      = "cluster/<region>/terraform.tfstate"   # e.g. cluster/us-lax/terraform.tfstate
  #   region   = "us-east-1"                            # dummy; required by S3 backend
  #   endpoints.s3 = "https://us-east-1.linodeobjects.com"  # or region-specific endpoint
  #
  # Note: endpoints.s3 cannot be passed via -backend-config CLI flags (it is a
  # nested block). Set the AWS_ENDPOINT_URL_S3 environment variable instead —
  # the AWS SDK picks it up automatically and Terraform's S3 backend respects it.
  #
  # Credentials and endpoint are injected via environment variables:
  #   AWS_ACCESS_KEY_ID     = Linode Object Storage access key  (GitHub secret: TF_STATE_ACCESS_KEY)
  #   AWS_SECRET_ACCESS_KEY = Linode Object Storage secret key  (GitHub secret: TF_STATE_SECRET_KEY)
  #   AWS_ENDPOINT_URL_S3   = Linode Object Storage endpoint    (GitHub var:    TF_STATE_ENDPOINT)
  #
  # TF_STATE_BUCKET is also a GitHub Actions variable and is passed via
  # -backend-config="bucket=$TF_STATE_BUCKET" at init time.
  backend "s3" {
    skip_credentials_validation = true
    skip_metadata_api_check     = true
    skip_region_validation      = true
    skip_requesting_account_id  = true
    skip_s3_checksum            = true
    use_path_style              = true
  }
}
