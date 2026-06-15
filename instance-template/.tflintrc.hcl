# tflint rules for terraform-iac-bootstrap/cluster.
# Shared config — `llz lint` passes it as an absolute --config to each tflint --chdir.
# Docs: https://github.com/terraform-linters/tflint/blob/master/docs/user-guide/config.md

# The built-in terraform ruleset: validates naming, documented outputs/variables,
# required_version, required_providers, module source pinning, and deprecated syntax.
plugin "terraform" {
  enabled = true
  preset  = "recommended"
}
