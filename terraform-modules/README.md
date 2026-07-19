# LKE Secure-by-Default Terraform Modules

Reusable Terraform modules for standing up a hardened LKE Enterprise cluster and
its object storage. Everything above the infrastructure layer — ArgoCD, apl-core,
the platform apps — is bootstrapped by `llz`, not by Terraform (see
[ADR 0002](../docs/adr/0002-thin-terraform-native-bootstrap.md)).

## Module inventory

| Module | Purpose |
|---|---|
| [`llz-cluster`](llz-cluster/) | VPC + subnet + node Cloud Firewall + LKE-E cluster (no node pool) |
| [`llz-object-storage`](llz-object-storage/) | Linode OBJ buckets for registry/log storage (buckets only — no keys) |

The node firewall used to be a separate `llz-node-firewall` module and the node
pool an `llz-pool` module. Both were single-consumer wrappers and are now inlined
— the firewall into `llz-cluster` (`firewall.tf`), the pool into the cluster root
as a plain `linode_lke_node_pool`.

**A working composition** is the generated `cluster` root:
[`../tools/internal/tfroots/roots/cluster/`](../tools/internal/tfroots/roots/cluster/)
(alongside `object-storage/` and `vpc/`). An instance commits **zero** Terraform —
`llz render` writes these roots and their `<env>.tfvars` as gitignored build
artifacts, so the roots live once, in the binary.

See [`RELEASING.md`](RELEASING.md) for the version/tag (`git::?ref=`) contract that
makes these publishable reuse units, and for why each module's README
Inputs/Outputs tables are the SemVer surface.

---

## What Terraform does, and where it stops

Terraform owns the infrastructure and nothing else:

```
terraform apply -var-file="<env>.tfvars"
```

That creates the VPC and subnet, the node Cloud Firewall with a **bootstrap
baseline** rule set, the LKE-E cluster with a locked-down control-plane ACL, and
the node pool (`disk_encryption` enabled, firewall attached — both non-negotiable).

It then hands off. Two owners take over what Terraform seeded:

- The **cloud-firewall-controller** owns the node firewall rules and the
  control-plane ACL from the first reconcile onward, resolving EAA/bastion CIDRs
  from the Linode firewall template via the API. `llz-cluster` sets
  `ignore_changes` on both so later applies do not overwrite live controller
  state. Terraform seeds only enough ACL for the bootstrapping runner to reach the
  API server.
- **apl-core** owns the ArgoCD repo credential (its `argocd-repo-creds`
  ExternalSecret). No deploy key is generated, registered, or rotated by Terraform.

In-cluster bootstrap runs natively via `llz ci bootstrap-cluster` — there is no
Terraform workspace for it, and no `kubectl_manifest`/`kubernetes_manifest`
resource anywhere in these modules.

## Object Storage credentials

`llz-object-storage` creates **buckets only**. The scoped Loki/Harbor access keys
are minted at bootstrap by `llz ci mint-bootstrap-objkeys` and rotated in-cluster
by the `linodeCredRotator` CronJob. No key material transits Terraform state or
GitHub secrets, and there is no rotation clock in this module — see
[`../docs/designs/linode-credential-rotator.md`](../docs/designs/linode-credential-rotator.md).
