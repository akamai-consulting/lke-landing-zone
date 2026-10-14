# Control Mapping & Auditor Evidence Pack

**Sources:** Org security requirements (e.g. SOG) via [linode-account-request-checklist.md](./linode-account-request-checklist.md), CIS Kubernetes Benchmark, Linode LKE-Enterprise shared-responsibility model
**Last Updated:** 2026-10-14

This document answers the controls your InfoSec team already signs off on — the SOG-style requirements in the [Linode Account Request Checklist](./linode-account-request-checklist.md#step-5--satisfy-your-orgs-security-requirements-eg-sog) — for an account that runs this landing zone. For each requirement it says:

1. **who satisfies it** — what LLZ enforces on every instance, what your instance must configure, and what remains your organization's process;
2. **what implements it** — the module, chart, policy or gate in this repo;
3. **where the gaps are** — stated, not rounded up;
4. the **CIS Kubernetes Benchmark** control(s) it satisfies, and the evidence that proves it.

The checklist remains the spine of your sign-off. This document and the generated **evidence pack** (`llz ci cis-evidence`) are what you attach to it: the pack carries the same pre-filled answers as Section A below, next to live evidence from the cluster. The goal is that your production-account questionnaire is mostly answered before you open it, and that what is left is your organization's own process.

Replace `<org>`, `<project>`, and `<team>` with your own values, consistent with the account-request checklist.

<!-- toc -->
- [Scope & Shared Responsibility](#scope--shared-responsibility)
- [Section A — Org Security Requirements (SOG), Answered](#section-a--org-security-requirements-sog-answered)
- [Enrolling a cluster in CSPM (SOG #5)](#enrolling-a-cluster-in-cspm-sog-5)
- [Section B — CIS Kubernetes Benchmark (In-Scope Controls)](#section-b--cis-kubernetes-benchmark-in-scope-controls)
- [Out-of-Scope CIS Controls (Managed Control Plane)](#out-of-scope-cis-controls-managed-control-plane)
- [How the Evidence Pack Is Generated](#how-the-evidence-pack-is-generated)
- [Auditor Sign-Off](#auditor-sign-off)
- [Related Docs](#related-docs)
<!-- /toc -->

---

## Scope & Shared Responsibility

Linode **LKE-Enterprise runs the Kubernetes control plane** (API server, etcd, scheduler, controller-manager). Their configuration and audit logging are not user-auditable, so the corresponding CIS Benchmark sections are **out of scope by design** — see [§ Out-of-Scope CIS Controls](#out-of-scope-cis-controls-managed-control-plane) for the explicit carve-out and rationale. Auditors should treat those rows as the responsibility of the cloud provider under the shared-responsibility model, not as gaps.

**In scope** (the rest of this document): worker-node and volume encryption, network segmentation, RBAC, Pod Security Standards, secrets management, admission control, CSPM enrollment, and the org's account/identity requirements.

**Who satisfies a requirement:**

| Column | Meaning |
|--------|---------|
| **LLZ provides** | Enforced by this template on every instance, with no adopter action — a hardcoded module value, a default-on component, a gate |
| **Instance configures** | A spec / tfvars / secret value your instance must set for the control to hold |
| **Adopter process** | Organizational: tickets, tenancy, account settings, training — nothing a template can reach |

---

## Section A — Org Security Requirements (SOG), Answered

The 16 requirements are the ["Satisfy Your Org's Security Requirements"](./linode-account-request-checklist.md#step-5--satisfy-your-orgs-security-requirements-eg-sog) table. The same answers are compiled into the evidence pack; a test fails if this table stops citing what the pack cites, if a cited path disappears, or if a "hardcoded" claim stops being true of the file it names.

| # | Requirement | LLZ provides | Instance configures | Adopter process | Implemented by / evidence |
|---|-------------|--------------|---------------------|-----------------|---------------------------|
| 1 | Approved account type | — | — | Request a Production account; obtain InfoSec approval through the org intake | [linode-account-request-checklist.md](./linode-account-request-checklist.md) (`docs/infosec/linode-account-request-checklist.md`) |
| 2 | Required account tags | Every tag in `spec.cluster.tags` is stamped onto the LKE cluster, its node pool and the node Cloud Firewall | `spec.cluster.tags` — your ownership / production / data-classification tags | Account-level tags in Cloud Manager (LLZ manages resources, not the account) | `terraform-modules/llz-cluster/variables.tf` (`tags`), `tools/internal/shared/tfroots/roots/cluster/main.tf`. **Gap:** no validator requires a tag; an empty list passes |
| 3 | Cloud Firewall with default DROP | Node Cloud Firewall created with `inbound_policy = "DROP"` (hardcoded) and attached to the node pool; inbound rules scoped to the VPC subnet, the LKE control-plane range and the NodeBalancer range | — | Operator ingress (bastion SSH / HTTPS) reconciled by your firewall controller from the org firewall template | `terraform-modules/llz-cluster/firewall.tf`. CIS 5.3. **Gap:** the baseline allows ICMP from `0.0.0.0/0` and `::/0`; the checklist asks for no `0.0.0.0/0` rules and ICMP from org ranges only — record an exception or narrow the rule |
| 4 | Enroll all accounts in CSPM | — (LLZ enrolls clusters, see #5) | — | Enroll each account (dev/test/QA/prod) with the org CSPM team | Org CSPM enrollment record |
| 5 | CSPM agent integrations | Optional `wiz` component: the upstream Wiz connector, its credential delivered from OpenBao by ExternalSecret, a default-deny namespace, and a gate that fails unless the cluster is registered and the broker tunnel is Ready | `components.wiz.enabled` + `secretPath` under a declared team subtree; seed `clientId` + `clientToken` there ([below](#enrolling-a-cluster-in-cspm-sog-5)) | Provision the Wiz tenant and a connector service account. No standalone VMs exist, so no host agent | `platform-apl/components/wiz/kustomization.yaml`, `platform-apl/components/wiz/wiz/application.yaml`; gate `llz ci assert-wiz-enrollment`. **Gap:** the connector's upstream ClusterRole reads every resource, including Secrets |
| 6 | Port exposure ticket | Beyond the VPC and LKE control-plane ranges, the node firewall exposes only the NodePort range, and only to the NodeBalancer range (plus `apiServerAllowCIDRs` when set) | — | File a port-exposure ticket per externally reachable port | `terraform-modules/llz-cluster/firewall.tf`. CIS 5.3 |
| 7 | Managed SSH for VM SSH | Not applicable: no standalone Linode instances are declared; the node firewall has no TCP/22 rule | — | — | `terraform-modules/llz-cluster/firewall.tf` |
| 8 | Token/key rotation (90d / 120d / 180d) | Credential single pane: daily `token-inventory` measures every CI token's expiry, `assert-rotation-health` fails any credential past the 90-day SLA, `alert-eval` reads the credential alerts; scheduled rotation for the lke-admin token, the Linode PAT and the object-storage keys | Credentials as environment secrets; optionally `broadPatRotator` for in-cluster PAT rotation | Set and track the CSPM (Wiz) service-account rotation cadence | `instance-template/.github/workflows/llz-scheduled-checks.yml`, `instance-template/.github/workflows/llz-secret-rotation.yml`, `docs/runbooks/linode-credential-rotation.md`; gates `llz ci token-inventory`, `llz ci assert-rotation-health`, `llz ci alert-eval`. CIS 5.4. **Gap:** one 90-day SLA for every credential (stricter than the 120-day bucket-key requirement); the 180-day CSPM token is not measured |
| 9 | No sensitive data on Private LAN; encrypt at rest | Encrypted node disks and block volumes (the encrypted default StorageClass), secrets in OpenBao delivered by ESO, encrypted Terraform state, default-deny NetworkPolicies on platform namespaces | `objProxy` to encrypt Object Storage (SSE-C); keep workloads on the encrypted StorageClass | Classify the data; keep cross-customer PII off the shared private network | `tools/internal/extensions/lifecycle/bootstrapcluster/manifests/block-storage-class.yaml`, `kubernetes-charts/llz-cluster-foundation/templates/network-policies.yaml`, `platform-apl/components/objProxy/kustomization.yaml`; gates `llz ci at-rest-guard`, `llz ci assert-volume-encryption`, `llz ci assert-obj-encryption`, `llz ci assert-network-enforcement`. CIS 4.x, 5.3, 5.4. **Gap:** LKE-E's control plane reaches nodes over the shared private range, so the node firewall admits that whole range on the kubelet/DNS/BGP ports rather than individual /32s |
| 10 | LKE-Enterprise required | Cluster created with `tier = "enterprise"` (hardcoded); the pinned Kubernetes version is checked against the account's Enterprise catalog before apply | — | — | `terraform-modules/llz-cluster/main.tf`; gate `llz ci assert-k8s-version` |
| 11 | LKE secret rotation runbook | Rotation runbooks ship with every instance (lke-admin token, Linode credentials) alongside the secrets reference | — | Accept or remediate the documented position on the OpenBao seal and recovery keys, which are static by design | `docs/secrets.md`, `docs/runbooks/lke-admin-rotation.md`, `docs/runbooks/linode-credential-rotation.md`. CIS 5.4 |
| 12 | LKE control-plane IP ACL | ACL enabled (hardcoded); open-world (`/0`) entries refused by the CIDR validator and the readiness scan | Add the CSPM scanner ranges to `spec.cluster.apiServerAllowCIDRs` | Obtain the current scanner ranges from the CSPM team | `terraform-modules/llz-cluster/main.tf`, `tools/internal/shared/validate/validate.go`. CIS 5.3. **Gap:** `apiServerAllowCIDRs` also opens the NodePort range on the node firewall to those CIDRs; there is no ACL-only list |
| 13 | LKE disk encryption (LDE) | Node pools created with `disk_encryption = "enabled"` (hardcoded); `at-rest-guard` fails any node pool or instance without it and accepts no exemption for it | — | — | `tools/internal/shared/tfroots/roots/cluster/main.tf`, `tools/internal/extensions/lifecycle/atrest/atrest.go`; gate `llz ci at-rest-guard`. CIS 4.x |
| 14 | Account attribution format | — | — | Set the account Company Name to the org's format in Cloud Manager | Linode Company Name field |
| 15 | SSO for org accounts | In-cluster human access is OIDC through Keycloak (team login to OpenBao) | — | Cloud Manager access through the org SSO | `docs/runbooks/openbao-team-login.md` |
| 16 | Annual Linode Practitioner Training | — | — | Training records for every team member with account access | Org training records |

**What is left for the adopter**, in one list: the account itself (#1, #2 account tags, #14), CSPM account enrollment and the Wiz tenant (#4, #5), port-exposure tickets (#6), the CSPM token cadence (#8), data classification (#9), a decision on the static seal keys (#11), the scanner CIDRs (#12), SSO and training (#15, #16) — plus a recorded position on each **Gap** above.

---

## Enrolling a cluster in CSPM (SOG #5)

The `wiz` component replaces a hand-carried Argo Application + ExternalSecret + NetworkPolicy in an instance's `kubernetes-custom/` overlay. If you carry one, **delete it when you enable the component** — two Applications releasing into one namespace fight over the same objects.

1. **Declare a team** whose OpenBao subtree will hold the credential (`spec.teams: [{name: <team>, openbaoSubtree: secret/<team>}]`). The ESO identity reads every declared team subtree; a platform path would need a policy change that only a bootstrap-time `bao-configure` applies. A team declared after bootstrap needs the retrofit in [openbao-team-login.md](../runbooks/openbao-team-login.md).
2. **Seed the credential** (a Wiz service account with the connector role) as that team:

   ```bash
   eval "$(llz openbao login --team <team>)"
   llz openbao set secret/<team>/wiz/connector clientId=<id> clientToken=<secret> --yes
   ```

   `--yes` matters: without it `llz openbao set` dry-runs and exits 0. The property names are the upstream chart's contract — `clientSecret` (the Wiz console's word) is ignored.
3. **Enable the component** for the env, and render:

   ```yaml
   # environments/<env>.yaml
   spec:
     components:
       wiz: { enabled: true, secretPath: secret/<team>/wiz/connector }
   ```

   `llz render` points the ExternalSecret at that path and names the connector after the env. Validate refuses a `secretPath` outside every declared team subtree.
4. **Verify**: `llz ci assert-wiz-enrollment --region <env>` fails unless the ExternalSecret is Ready, the Secret carries both keys, the Wiz API has registered the connector, and the broker's tunnel is Ready. It names the first broken link.

Only the connector is installed. The Wiz runtime sensor (a privileged DaemonSet) and admission controller (a cluster-wide webhook) are separate decisions with their own blast radius and are not part of this component.

---

## Section B — CIS Kubernetes Benchmark (In-Scope Controls)

These controls are scored continuously by the **trivy-operator `ClusterComplianceReport`s** the `complianceReports` component ships (the standard CIS spec, plus `llz-org-controls` — the trivy-checkable subset of Section A), and harvested into the evidence pack. trivy-operator is opt-in on managed App Platform, so the component emits only when `trivy` is declared in `cluster.bootstrap.managedApps`; without it the pack records both reports as **not collected**, never as passing. The CIS Benchmark version is whatever the deployed trivy-operator ships, stamped into the report.

### 4 — Worker Nodes / Data at Rest

| CIS § | Control (summary) | LLZ control | Evidence source |
|-------|-------------------|-------------|-----------------|
| 4.1 / 4.2 | Worker node config files, kubelet arguments | Linode-managed node image (shared responsibility) | trivy `ClusterComplianceReport` (node checks) |
| 4.x | Persistent data encrypted at rest | Encrypted block-storage StorageClass + LDE node disks | [block-storage-class.yaml](../../tools/internal/extensions/lifecycle/bootstrapcluster/manifests/block-storage-class.yaml), [cluster root](../../tools/internal/shared/tfroots/roots/cluster/main.tf); `llz ci assert-volume-encryption` |

### 5.1 — RBAC and Service Accounts

| CIS § | Control (summary) | LLZ control | Evidence source |
|-------|-------------------|-------------|-----------------|
| 5.1.1 / 5.1.3 | Cluster-admin only where required; minimize wildcards | Scoped Roles/RoleBindings per component | trivy report (`llz-org-controls` SOG-RBAC) |
| 5.1.5 / 5.1.6 | Default SAs unused; SA tokens mounted only where needed | Component-specific ServiceAccounts | trivy report |

### 5.2 — Pod Security Standards

| CIS § | Control (summary) | LLZ control | Evidence source |
|-------|-------------------|-------------|-----------------|
| 5.2.x | Restricted Pod Security Standard enforced | PSA `enforce: restricted` on LLZ namespaces | [namespaces.yaml](../../kubernetes-charts/llz-cluster-foundation/templates/namespaces.yaml); pack: restricted-namespace list |
| 5.2.x | runAsNonRoot, drop capabilities, seccomp | securityContext on platform workloads | trivy report (`llz-org-controls` SOG-9-PSS) |

### 5.3 — Network Policies and CNI

| CIS § | Control (summary) | LLZ control | Evidence source |
|-------|-------------------|-------------|-----------------|
| 5.3.2 | Default-deny NetworkPolicy per namespace | Default-deny ingress+egress on platform namespaces | [network-policies.yaml](../../kubernetes-charts/llz-cluster-foundation/templates/network-policies.yaml); `llz ci assert-network-enforcement`; pack: NetworkPolicy count |

### 5.4 — Secrets Management

| CIS § | Control (summary) | LLZ control | Evidence source |
|-------|-------------------|-------------|-----------------|
| 5.4.1 | Prefer an external secret store | OpenBao + External Secrets Operator | [llz-openbao-platform/values.yaml](../../kubernetes-charts/llz-openbao-platform/values.yaml); `llz ci externalsecret-paths` |
| 5.4.x | Secrets access audited | OpenBao audit device shipped to Loki | `llz ci assert-openbao-audit` (round trip: OpenBao → Loki) |

### 5.5 / 5.7 — Admission Control & General Policies

| CIS § | Control (summary) | LLZ control | Evidence source |
|-------|-------------------|-------------|-----------------|
| 5.5.1 | Image provenance via admission controller | Kyverno cosign signature verification of the first-party image | [kyverno-verify-llz-image-signature.yaml](../../platform-apl/components/imageSignature/kyverno-verify-llz-image-signature.yaml); `llz ci assert-admission-enforcement` |

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

`llz ci cis-evidence` runs daily in the `credential-single-pane` job of [llz-scheduled-checks.yml](../../instance-template/.github/workflows/llz-scheduled-checks.yml), after that job's own gates. It:

1. **Harvests** the two `ClusterComplianceReport`s, NetworkPolicy coverage, the namespaces enforcing restricted PSS, and the default StorageClass's encryption parameter.
2. **Records the gates that ran beside it** — the credential funnel (`assert-rotation-health`), the credential alerts (`alert-eval`) and the Wiz enrollment (`assert-wiz-enrollment`) — as PASS / FAIL / SKIPPED on the Section A rows that cite them. A cited gate with no outcome says *not run*, never passed.
3. **Carries the Section A answers** so the questionnaire and its evidence travel together.
4. **Emits** a JSON bundle, a Markdown report, and the bundle's sha256, uploaded as the `cis-evidence-<env>` workflow artifact.

Nothing here passes by omission: a harvest that could not reach the cluster is recorded as *not collected*, and a pack that collected nothing reports **NOT COLLECTED** rather than PASS. The pack FAILs when a compliance report fails or a recorded gate fails.

> **Signing.** The scheduled job records a sha256 digest rather than a cosign attestation: keyless signing needs `id-token: write`, which the delivered caller stub does not grant. An instance that wants a signed bundle can add that permission to its `scheduled-checks.yml` stub and sign the uploaded artifact (`cosign attest-blob`) in its own step.

---

## Auditor Sign-Off

| Role | Name | Date | Signature / Approval ref |
|------|------|------|--------------------------|
| Project / Product owner | | | |
| InfoSec reviewer | | | |
| Evidence pack ref | | | `cis-evidence-<env>-<timestamp>.json` + its `.sha256` |

---

## Related Docs

- [Linode Account Request — Checklist](./linode-account-request-checklist.md) — the SOG requirements spine
- [Secrets management](../secrets.md) — rotation, seal/recovery keys, service credentials
- [Landing-zone spec](../landing-zone-spec.md) — `components.wiz`, `complianceReports`, `spec.teams`
- [Adopter guide](../adopter-guide.md) — onboarding a `<project>` onto the landing zone
