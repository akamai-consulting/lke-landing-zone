terraform {
  # Linode Object Storage is S3-compatible; use the S3 backend.
  #
  # All backend parameters are supplied at init time via -backend-config flags
  # or a backend.tfvars file (never committed).  The CI workflow provides them
  # from GitHub Actions secrets.
  #
  # Required -backend-config keys:
  #   bucket   = "platform-terraform-state"
  #   key      = "databases/<region>/terraform.tfstate"
  #   region   = "us-east-1"   (dummy; required by S3 backend)
  #
  # The key MUST start with `databases/`. This file was copied from the
  # object-storage root and inherited its key verbatim, which is a dangerous thing
  # for a state-key comment to get wrong: `terraform init` against
  # object-storage/<region>/terraform.tfstate loads THAT root's state, and since the
  # buckets in it do not appear in this configuration, the very next plan proposes
  # DESTROYING the registry and loki buckets in order to create a database.
  # CI is unaffected — .github/actions/terraform-init derives the key from the
  # module name — but the workflow jobs for this root do not exist yet, so applying
  # it by hand (as the runbooks describe) is currently the only way to run it.
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
