# LKE Secure-by-Default Terraform Modules

Reusable Terraform modules for bootstrapping a hardened LKE Enterprise cluster
with GitOps via ArgoCD.

## Module inventory

| Module | Purpose |
|---|---|
| [`llz-cluster`](llz-cluster/) | VPC + subnet + LKE-E cluster (no default node pool) |
| [`llz-object-storage`](llz-object-storage/) | Linode OBJ buckets + scoped keys for registry/log storage, with 120-day key rotation |

See [`../instance-template/terraform-iac-bootstrap/cluster/`](../instance-template/terraform-iac-bootstrap/cluster/) for a working composition of the cluster modules,
and [`RELEASING.md`](RELEASING.md) for the version/tag (`git::?ref=`) contract that
makes these publishable as Phase-3 reuse units.

---

## Bootstrap sequence

A new cluster bootstraps in a single apply. `kubectl_manifest` (gavinbunney/kubectl)
is used for the ArgoCD root Application instead of `kubernetes_manifest` because it
does not validate against CRD schemas at plan time, so `depends_on` alone is
sufficient to sequence the apply correctly.

```
terraform apply -var-file="<region>.tfvars"
```

After apply, get the deploy public key:

```
terraform output -raw argocd_deploy_public_key
```

Then register it on the GitOps repo (see below).

---

## Registering the ArgoCD deploy key

The TF config generates a per-cluster ED25519 SSH key and injects the private
key into a Kubernetes Secret that ArgoCD reads. The public half must be
registered as a **read-only deploy key** on the GitOps repository before ArgoCD
can clone it.

### Option A — Automated (github.com API)

The `apply-cluster` GitHub Actions job handles this automatically when
`GH_TOKEN` is set as a repository secret. It calls the github.com
Deploy Keys API after every successful `terraform apply`
and is idempotent — running it again when the key is unchanged is a no-op.

Required secret: `GH_TOKEN` — a github.com
personal access token with `repo` scope on `akamai-consulting/lke-landing-zone`.

### Option B — Manual (github.com UI)

If you are running Terraform locally or the API token is unavailable:

1. Get the public key:
   ```
   terraform output -raw argocd_deploy_public_key
   ```

2. Go to **github.com/akamai-consulting/lke-landing-zone → Settings → Repository → Deploy keys**.

3. Click **Add new deploy key**.
   - Title: `argocd-primary` (or `argocd-secondary`)
   - Key: paste the output from step 1
   - **Grant write permissions to this key**: leave unchecked

4. Click **Add key**.

ArgoCD will pick up the credential within its next refresh cycle (default 3 min).

### Option C — No github.com access (TBD)

If the GitOps repository is moved to a different SCM (GitHub, self-hosted
Gitea, etc.):

1. Update `argocd_repo_url` in the relevant `*.tfvars` file to the new SSH URL.
2. Update all `repoURL:` fields in the `kubernetes/` Application manifests to
   match.
3. Register the deploy key on the new SCM using its equivalent API or UI.

The TF config itself is SCM-agnostic — only the URL and the credential
registration step differ.

---

## Rotating the deploy key

If the key is compromised or you want to rotate it:

```
# Terraform: force a new key
terraform taint tls_private_key.argocd_repo
terraform apply

# Get the new public key
terraform output -raw argocd_deploy_public_key
```

The `apply-cluster` CI job will automatically replace the old deploy key on
github.com. If running manually, follow Option B above with the new key,
then delete the old one from the Deploy keys list.
