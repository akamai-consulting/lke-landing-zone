variable "region_suffix" {
  description = "Deployment suffix appended to every database label — the lowercase deployment/env name (e.g. primary, secondary, staging, lab, e2e). The deployment discriminator; must match the cluster workspace deployment. Format validation lives in the llz-databases module this root passes the value through to."
  type        = string
}

# ── 0-n database clusters ─────────────────────────────────────────────────────
# A map, not a list: the key is the cluster name AND its address in state
# (module.databases["<name>"]), so adding, removing or reordering entries never
# re-plans — let alone destroys and recreates — the clusters that did not change.
# A list would key on position and do exactly that.
#
# Empty (the default) is a first-class case, and the common one: the root applies
# cleanly and provisions nothing. That is what makes `databases` opt-in without
# needing a separate enabled flag or a guard in CI.
#
# The name becomes the middle label segment: "<label_prefix>-<name>-<region_suffix>",
# e.g. "platform-postgres-primary". `region` must be the region of `vpc_id`.
variable "databases" {
  description = "Database clusters to provision for this deployment, keyed by cluster name. Empty (default) provisions nothing. Each entry is a VPC-attached Linode Managed PostgreSQL cluster; region must match the region of its vpc_id."
  type = map(object({
    region    = string
    vpc_id    = number
    subnet_id = number
    # Optional with the module's own defaults restated, so `terraform plan` shows
    # the effective value rather than a null the module fills in later.
    # engine_version: when MIGRATING data in, set it to at least the source
    # cluster's major version — pg_restore is forward-compatible only, so a lower
    # target fails at restore time, after the cluster is provisioned and billing.
    engine_version = optional(string, "16")
    db_type        = optional(string, "g6-dedicated-2")
    cluster_size   = optional(number, 2)
  }))
  default = {}

  validation {
    condition     = alltrue([for _, d in var.databases : contains([1, 2, 3], d.cluster_size)])
    error_message = "cluster_size must be 1 (single) or 2/3 (HA) for every entry in databases."
  }

  validation {
    # vpc_id/subnet_id are required attributes, so the failure mode is not an
    # absent value but a ZERO one — a scaffolded tfvars nobody filled in. VPC 0
    # is a valid-looking id that points at nothing, so catch it at plan rather
    # than at apply.
    condition     = alltrue([for _, d in var.databases : d.vpc_id > 0 && d.subnet_id > 0])
    error_message = "every databases entry needs a real vpc_id and subnet_id (`linode-cli vpcs list`; `linode-cli vpcs subnets-list <vpc_id>`) — 0 is the unscaffolded placeholder."
  }
}

# Per-instance namespace on the database cluster label, exactly as in the
# object-storage root. The module defaults this to "platform" and its own doc
# calls the result "the global database label", so leaving it at the default gave
# every LLZ instance the same `platform-<name>-<env>` and the second one to
# declare a database for a given deployment name fails at apply.
#
# No default: `llz render` always emits it from spec.instance.objLabelPrefix.
variable "label_prefix" {
  description = "Per-instance prefix for the database cluster label (spec.instance.objLabelPrefix). The label becomes <label_prefix>-<name>-<region_suffix>."
  type        = string

  validation {
    # Rejects the SHIPPED PLACEHOLDER as well as the grammar, mirroring
    # region_suffix's `!= "your-env"`. Without it two adopters hand-authoring
    # tfvars from the example both create `your-instance-…` — the identical
    # collision this variable exists to remove, under a new shared name.
    condition     = can(regex("^[a-z0-9]([a-z0-9-]*[a-z0-9])?$", var.label_prefix)) && var.label_prefix != "your-instance"
    error_message = "label_prefix must be lowercase alphanumerics and hyphens, not starting or ending with a hyphen, and must not be the shipped placeholder \"your-instance\" — `llz render` fills it from spec.instance.objLabelPrefix."
  }
}
