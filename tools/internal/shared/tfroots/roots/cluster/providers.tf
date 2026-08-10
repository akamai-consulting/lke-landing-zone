# LKE Enterprise is only exposed on the v4beta API. This is not a knob: no
# caller could set it (it was never emitted into tfvars, and rendered tfvars are
# gitignored build artifacts), and a non-beta value simply breaks LKE-E.
provider "linode" {
  api_version = "v4beta"
}
