# terraform-iac-bootstrap/ — Infrastructure as Code

> **Scope override.** This directory contains Terraform only — no Go tools, no application code.

## Layout

```
terraform-iac-bootstrap/
  cluster/            LKE-E cluster, VPC, node pool, node firewall — one apply per cluster
  cluster-bootstrap/  Installs Akamai App Platform (apl-core) via `helm install apl/apl`
                      and seeds the values-repo HTTPS PAT Secret. After this,
                      apl-core's in-cluster Argo CD takes over.
  openbao-config/     OpenBao secret engines, AppRole, policies, Kubernetes auth — run after cluster
  object-storage/     Loki S3 buckets (Linode Object Storage) — run once per region
  modules/
    llz-cluster/     Reusable LKE cluster module
    llz-pool/        Reusable node pool module
    llz-node-firewall/                 Node-level firewall rules module
```

## Apply order

`cluster` → `object-storage` → `cluster-bootstrap` → `openbao-config` (last, run via the
GitHub workflow `bootstrap-openbao.yml`, not from this directory).

For two-cluster deployments, complete the **primary** cluster fully before applying to secondary. The primary bootstrap seeds Harbor robot credentials and CA certs that the secondary consumes.

## tfvars pattern

Each root module has `primary.tfvars` and `secondary.tfvars` for the two production clusters, plus `*.example` files (committed, no real values) as operator templates. Never commit real credentials or API tokens — all secrets flow via GitHub Actions secrets and environment variables at CI time.

## State backend

State is stored in Linode Object Storage via the S3-compatible backend. Every `backend.tf` requires these flags — do not remove them; standard AWS S3 validation fails against Linode:

```hcl
backend "s3" {
  skip_credentials_validation = true
  skip_metadata_api_check     = true
  skip_region_validation      = true
  force_path_style            = true
  ...
}
```

The S3 endpoint cannot be passed via `-backend-config` CLI flags (it is a nested block). Set `AWS_ENDPOINT_URL_S3` as an environment variable instead — the AWS SDK picks it up and Terraform's S3 backend respects it.

Backend credentials are injected via:
- `AWS_ACCESS_KEY_ID` → GitHub secret `TF_STATE_ACCESS_KEY`
- `AWS_SECRET_ACCESS_KEY` → GitHub secret `TF_STATE_SECRET_KEY`
- `AWS_ENDPOINT_URL_S3` → GitHub variable `TF_STATE_ENDPOINT`

Never add these to `backend.tf` directly.

## Common commands

```bash
# Initialise (supply backend config from secrets — never commit backend.tfvars)
# The key's middle segment is the deployment discriminator (primary | secondary
# | lab), NOT a Linode region. In cluster-bootstrap/ the matching Terraform
# variable is `deployment` (renamed from `region`; CI passes TF_VAR_deployment
# — instances upgrading the template must rename `region` to `deployment` in
# their cluster-bootstrap *.tfvars). -var-file usage is otherwise unchanged.
terraform init \
  -backend-config="bucket=platform-terraform-state" \
  -backend-config="key=cluster/<deployment>/terraform.tfstate" \
  -backend-config="region=us-east-1"

# Plan / apply for primary cluster
terraform plan  -var-file=primary.tfvars
terraform apply -var-file=primary.tfvars

# Plan / apply for secondary cluster
terraform plan  -var-file=secondary.tfvars
terraform apply -var-file=secondary.tfvars

# Format check (CI enforces this)
terraform fmt -recursive -check
```

## Conventions

- Run `terraform fmt -recursive` before committing any `.tf` change. The CI `terraform.yml` validates formatting and fails on drift.
- The LKE control-plane ACL is seeded at create from `github_runner_*_cidrs` and then owned by the in-cluster cloud-firewall-controller (Linode API). github.com-hosted runners open their egress IP at runtime via `llz ci runner-acl open`.
- The `cluster` module writes a kubeconfig to a local path; `providers.tf` points the helm, kubernetes, and kubectl providers at that path. Do not hardcode kubeconfig paths — use the `local.kubeconfig_output_path` local.
- Do not run `terraform state rm`, `terraform state mv`, or `terraform init -reconfigure` without explicit user approval. State corruption is not recoverable without a backup.
- Do not add resources here that apl-core (or Argo CD on top of it) should own. Terraform bootstraps the cluster and installs apl-core; everything after that lives in `apl-values/` and is reconciled by apl-core's in-cluster Argo CD.

## What Terraform manages here vs apl-core

| Managed by Terraform | Managed by apl-core (or its Argo CD) |
|----------------------|--------------------------------------|
| LKE-E cluster, node pool, VPC | All Kubernetes workloads |
| Cluster-level Linode Cloud Firewall + node firewall | Helm releases (cert-manager, Harbor, Prometheus, Istio, Keycloak, OpenBao via `manifest/`, etc.) |
| `helm install apl/apl` (one-time, then upgrade by bumping `apl_chart_version`) | Argo CD self-management after bootstrap |
| OpenBao secret engines, policies, auth methods (`openbao-config/`) | ExternalSecret reconciliation |
| Linode Object Storage buckets (`object-storage/`) | Loki configuration |
| Values-repo HTTPS PAT Secret in argocd namespace | Everything reconciled from `apl-values/<env>/manifest/` |

Do not duplicate resources between the two systems.

## Rules that apply from root

- Never add `Co-Authored-By` to commits.
- Do not make changes without explicit user approval.
- Do not commit real secrets, API tokens, or kubeconfig files.
