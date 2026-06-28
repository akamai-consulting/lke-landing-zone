# Control Mapping & Auditor Evidence Pack

**Sources:** Org security requirements (e.g. SOG) via [linode-account-request-checklist.md](./linode-account-request-checklist.md), CIS Kubernetes Benchmark, Linode LKE-Enterprise shared-responsibility model
**Last Updated:** 2026-06-28

This document maps the controls your InfoSec team already signs off on (the SOG-style requirements in the [Linode Account Request Checklist](./linode-account-request-checklist.md#step-5--satisfy-your-orgs-security-requirements-eg-sog)) to:

1. the **CIS Kubernetes Benchmark** control(s) each one satisfies,
2. the **evidence source** that proves it, and
3. how that evidence is **collected** — automatically harvested into the signed evidence pack, or attested manually by the responsible team.

The checklist remains the spine of your sign-off process. This document and the generated **evidence pack** (`llz ci cis-evidence`) are the proof you attach to it. Nothing here replaces the existing approval workflow — it makes each row provable.

Replace `<org>`, `<project>`, and `<cluster_domain>` with your own values, consistent with the account-request checklist.

---

## Scope & Shared Responsibility

Linode **LKE-Enterprise runs the Kubernetes control plane** (API server, etcd, scheduler, controller-manager). Their configuration and audit logging are not user-auditable, so the corresponding CIS Benchmark sections are **out of scope by design** — see [§ Out-of-Scope CIS Controls](#out-of-scope-cis-controls-managed-control-plane) for the explicit carve-out and rationale. Auditors should treat those rows as the responsibility of the cloud provider under the shared-responsibility model, not as gaps.

**In scope** (the rest of this document): worker-node disk encryption, network segmentation, RBAC, Pod Security Standards, secrets management, admission control, and the org's account/identity/CSPM requirements.

**Collection legend:**

| Symbol | Meaning |
|--------|---------|
| 🟢 **Auto** | Harvested into the evidence pack by `llz ci cis-evidence` (CRD, JSON, kubectl, or Terraform state) |
| 🔵 **Manual** | Attested by the responsible team; the pack renders a sign-off line with a pointer to the external system of record |
| ⚪ **N/A** | Not applicable to this landing zone (e.g. no standalone VMs) — recorded with rationale |

---

## Section A — Org Security Requirements (SOG) → Evidence

The 16 requirements below are the ["Satisfy Your Org's Security Requirements"](./linode-account-request-checklist.md#step-5--satisfy-your-orgs-security-requirements-eg-sog) table from the account-request checklist, now backed by an evidence source and a CIS cross-reference.

| # | Requirement | CIS ref | Evidence source | Collection |
|---|-------------|---------|-----------------|------------|
| 1 | Approved account type (Production) | — | InfoSec approval ticket (org intake portal) | 🔵 Manual |
| 2 | Required account tags applied | — | Linode account tags via API / Cloud Manager | 🔵 Manual |
| 3 | Cloud Firewall with default DROP inbound | 5.3 (network segmentation) | `inbound_policy = "DROP"` in [terraform-modules/llz-node-firewall/main.tf](../../terraform-modules/llz-node-firewall/main.tf); Terraform state + Linode Firewall API | 🟢 Auto |
| 4 | Enroll all accounts in CSPM | — | Org CSPM enrollment record / inventory dashboard | 🔵 Manual |
| 5 | CSPM agent integrations on LKE | — | Presence of CSPM auto-enroll workload (`kubectl get` in target namespace) | 🟢 Auto (presence) + 🔵 Manual (enrollment) |
| 6 | Port exposure ticket per exposed port | 5.3 | Org port-exposure tickets; cross-checked against Istio Gateway / NodeBalancer exposure | 🔵 Manual |
| 7 | Managed SSH for all VM SSH access | — | No standalone production VMs in this landing zone (LKE-only) | ⚪ N/A |
| 8 | Token/key rotation (PAT 90d / bucket 120d / CSPM 180d) | 5.4 (secrets) | `llz ci cred-audit` JSON record (PAT expiry ≤ 90d, OBJ-key inventory) — see [#99] | 🟢 Auto |
| 9 | No sensitive data on Private LAN; encrypt at rest | 4.x, 5.3, 5.4 | Encrypted StorageClass ([block-storage-class.yaml](../../instance-template/terraform-iac-bootstrap/cluster-bootstrap/manifests/block-storage-class.yaml)) + default-deny NetworkPolicies ([network-policies.yaml](../../kubernetes-charts/llz-cluster-foundation/templates/network-policies.yaml)) | 🟢 Auto |
| 10 | LKE-Enterprise required | — | Cluster tier in Terraform / LKE API | 🟢 Auto |
| 11 | LKE secret rotation runbook | 5.4 | [docs/secrets.md](../secrets.md); rotation runbooks under `docs/runbooks/` | 🔵 Manual (doc existence) |
| 12 | LKE Control Plane IP ACL | 5.3 | LKE control-plane ACL config (Terraform / LKE API); CSPM scanner ranges whitelisted | 🟢 Auto |
| 13 | LKE disk encryption (LDE) | 4.x (data at rest) | `disk_encryption = "enabled"` enforced by the secure-by-default node-pool module ([terraform-modules/llz-pool/main.tf](../../terraform-modules/llz-pool/main.tf)); Terraform state + LKE node-pool API | 🟢 Auto |
| 14 | Account attribution format | — | Linode Company Name field via API / Cloud Manager | 🔵 Manual |
| 15 | SSO for org accounts | — | Org Control Center SSO enrollment | 🔵 Manual |
| 16 | Annual Linode Practitioner Training | — | Org training records | 🔵 Manual |

---

## Section B — CIS Kubernetes Benchmark (In-Scope Controls)

These controls are scored continuously by the **trivy-operator `ClusterComplianceReport`** (the standard CIS spec) and harvested into the evidence pack. The "LLZ control" column records the platform feature that satisfies each. The exact CIS Benchmark version is pinned to whatever trivy-operator ships at audit time and stamped into the pack header (typically CIS Kubernetes Benchmark v1.x).

### 4 — Worker Nodes / Data at Rest

| CIS § | Control (summary) | LLZ control | Evidence source | Collection |
|-------|-------------------|-------------|-----------------|------------|
| 4.1 | Worker node configuration files (kubelet config, perms) | Linode-managed node image (shared responsibility) | trivy `ClusterComplianceReport` (node checks) | 🟢 Auto |
| 4.2 | Kubelet arguments (anonymous-auth off, authz mode, etc.) | Linode-managed node image (shared responsibility) | trivy `ClusterComplianceReport` | 🟢 Auto |
| 4.x | Persistent data encrypted at rest | Encrypted block-storage CSI class + LDE node disks | [block-storage-class.yaml](../../instance-template/terraform-iac-bootstrap/cluster-bootstrap/manifests/block-storage-class.yaml), [llz-pool/main.tf](../../terraform-modules/llz-pool/main.tf) | 🟢 Auto |

### 5.1 — RBAC and Service Accounts

| CIS § | Control (summary) | LLZ control | Evidence source | Collection |
|-------|-------------------|-------------|-----------------|------------|
| 5.1.1 | Cluster-admin used only where required | Scoped Roles/RoleBindings (least privilege) | trivy report + `kubectl get clusterrolebindings -o json` dump | 🟢 Auto |
| 5.1.3 | Minimize wildcard use in Roles | Narrow verbs/resourceNames (e.g. [llz-cert-automation/templates/rbac.yaml](../../kubernetes-charts/llz-cert-automation/templates/rbac.yaml)) | trivy report + RBAC dump | 🟢 Auto |
| 5.1.5 | Default service accounts not actively used | Component-specific ServiceAccounts | trivy report | 🟢 Auto |
| 5.1.6 | SA token mounted only where necessary | `automountServiceAccountToken` per workload | trivy report | 🟢 Auto |

### 5.2 — Pod Security Standards

| CIS § | Control (summary) | LLZ control | Evidence source | Collection |
|-------|-------------------|-------------|-----------------|------------|
| 5.2.x | Restricted Pod Security Standard enforced | PSA `enforce: restricted` on platform namespaces | [namespaces.yaml](../../kubernetes-charts/llz-cluster-foundation/templates/namespaces.yaml); `kubectl get ns -L pod-security.kubernetes.io/enforce` | 🟢 Auto |
| 5.2.x | runAsNonRoot, drop capabilities, seccomp | securityContext on platform workloads (OpenBao, observability) | trivy report + PolicyReport | 🟢 Auto |

### 5.3 — Network Policies and CNI

| CIS § | Control (summary) | LLZ control | Evidence source | Collection |
|-------|-------------------|-------------|-----------------|------------|
| 5.3.2 | Default-deny NetworkPolicy on all namespaces | default-deny ingress+egress per namespace | [network-policies.yaml](../../kubernetes-charts/llz-cluster-foundation/templates/network-policies.yaml); `kubectl get netpol -A -o json` | 🟢 Auto |

### 5.4 — Secrets Management

| CIS § | Control (summary) | LLZ control | Evidence source | Collection |
|-------|-------------------|-------------|-----------------|------------|
| 5.4.1 | Prefer external secret store over env | OpenBao + External Secrets Operator | [llz-openbao-platform/values.yaml](../../kubernetes-charts/llz-openbao-platform/values.yaml); trivy report | 🟢 Auto |
| 5.4.x | Secrets access audited | OpenBao file audit device → Promtail → Loki | [llz-openbao-platform/values.yaml](../../kubernetes-charts/llz-openbao-platform/values.yaml) (audit stanza); Loki query | 🟢 Auto (config) + 🔵 Manual (log review) |

### 5.5 / 5.7 — Admission Control & General Policies

| CIS § | Control (summary) | LLZ control | Evidence source | Collection |
|-------|-------------------|-------------|-----------------|------------|
| 5.5.1 | Image provenance via admission controller | Kyverno cosign image-signature verification | [kyverno-verify-llz-image-signature.yaml](../../instance-template/apl-values/_shared/manifest/kyverno-policies/kyverno-verify-llz-image-signature.yaml); PolicyReport | 🟢 Auto |
| 5.7.x | Namespace isolation / policy coverage | Kyverno ClusterPolicies + policy-reporter | `PolicyReport`/`ClusterPolicyReport` CRDs via policy-reporter | 🟢 Auto |

---

## Out-of-Scope CIS Controls (Managed Control Plane)

Linode operates the LKE-Enterprise control plane. The following CIS Benchmark sections are the **provider's** responsibility under shared responsibility and are **not user-auditable**. They are recorded here as explicitly out of scope — not as findings or gaps.

| CIS § | Area | Why out of scope |
|-------|------|------------------|
| 1 | Control Plane Components (API server, controller-manager, scheduler) | Operated and hardened by Linode; node access and config files not exposed to tenants |
| 2 | etcd | Managed datastore; encryption-at-rest and peer TLS are Linode-operated |
| 3 | Control Plane Configuration (authn/authz, audit logging) | API server audit policy and config are Linode-managed |

> Auditor note: request Linode's LKE-Enterprise shared-responsibility / compliance attestation (e.g. SOC 2) to cover sections 1–3.

---

## How the Evidence Pack Is Generated

The evidence pack is produced by `llz ci cis-evidence` (also runnable on a schedule — see [.github/workflows/llz-scheduled-checks.yml](../../.github/workflows/llz-scheduled-checks.yml)). It:

1. **Harvests automated evidence** (🟢 rows): the standard CIS `ClusterComplianceReport`, the custom org-control `ClusterComplianceReport`, `PolicyReport`/`ClusterPolicyReport` CRDs, the `llz ci cred-audit` JSON record, and `kubectl`/Terraform dumps for PSS labels, NetworkPolicies, RBAC, StorageClass, IP ACL, and firewall policy.
2. **Renders manual rows** (🔵) as sign-off lines, each pointing at the external system of record (ticket, CSPM dashboard, training records) for the responsible team to attest.
3. **Emits** a machine-readable **JSON bundle** plus a human-readable **Markdown report** mirroring this document's tables.
4. **Signs and attests** the bundle with cosign (reusing the keyless OIDC signing already used for the `llz` image), producing a verifiable chain of custody the auditor can validate.

---

## Auditor Sign-Off

| Role | Name | Date | Signature / Approval ref |
|------|------|------|--------------------------|
| Project / Product owner | | | |
| InfoSec reviewer | | | |
| Evidence pack ref (attested bundle) | | | `cis-evidence-<cluster>-<timestamp>.json` (cosign-verified) |

---

## Related Docs

- [Linode Account Request — Checklist](./linode-account-request-checklist.md) — the SOG requirements spine
- [Secrets management](../secrets.md) — rotation, seal/recovery keys, service credentials
- [Adopter guide](../adopter-guide.md) — onboarding a `<project>` onto the landing zone

[#99]: https://github.com/akamai-consulting/lke-landing-zone/pull/99
