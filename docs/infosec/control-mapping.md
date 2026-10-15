# Control Mapping & Auditor Evidence Pack

**Sources:** Your org's production-account security checklist (see [linode-account-request-checklist.md](./linode-account-request-checklist.md)), the Linode LKE-Enterprise shared-responsibility model, apl-core's trivy-operator compliance specs
**Last Updated:** 2026-10-15

This document explains how an instance answers the security checklist your InfoSec team already signs off on, and how `llz ci cis-evidence` packages those answers with live evidence from the cluster. For each requirement the answers say:

1. **who satisfies it**: what LLZ enforces on every instance, what your instance must configure, and what stays with your organization's process;
2. **what implements it**: the module, chart, policy or gate in this repo;
3. **where the gaps are**: stated plainly rather than rounded up.

**The answers are instance data, not template code.** Every org's checklist has different rows, numbering and process. So the template ships a generic, org-neutral starting point, [`instance-template/infosec/questionnaire.yaml.example`](../../instance-template/infosec/questionnaire.yaml.example). Your instance copies it to `infosec/questionnaire.yaml` and re-answers it for your own checklist. The pack carries whatever that file says. [Section A](#section-a--the-generic-example-answered) documents the generic example row by row.

<!-- toc -->
- [Scope & Shared Responsibility](#scope--shared-responsibility)
- [The questionnaire file](#the-questionnaire-file)
- [Section A — The Generic Example, Answered](#section-a--the-generic-example-answered)
- [Section B — CIS Kubernetes Benchmark: Not Scored on Managed App Platform](#section-b--cis-kubernetes-benchmark-not-scored-on-managed-app-platform)
- [Out-of-Scope CIS Controls (Managed Control Plane)](#out-of-scope-cis-controls-managed-control-plane)
- [How the Evidence Pack Is Generated](#how-the-evidence-pack-is-generated)
- [Auditor Sign-Off](#auditor-sign-off)
- [Related Docs](#related-docs)
<!-- /toc -->

---

## Scope & Shared Responsibility

Linode **LKE-Enterprise runs the Kubernetes control plane**: the API server, etcd, the scheduler and the controller-manager. Tenants cannot audit their configuration or audit logging, so the matching CIS Benchmark sections are **out of scope by design**. [§ Out-of-Scope CIS Controls](#out-of-scope-cis-controls-managed-control-plane) records that carve-out. Under the shared-responsibility model those rows belong to the cloud provider and are not gaps.

**Who satisfies a requirement:**

| Column | Meaning |
|--------|---------|
| **LLZ provides** | Enforced by this template on every instance with no adopter action: a hardcoded module value, a default-on component, or a gate |
| **Instance configures** | A spec, tfvars or secret value your instance must set for the control to hold |
| **Adopter process** | Organizational work no template can reach: tickets, tenancy, account settings, training |

---

## The questionnaire file

`llz ci cis-evidence` reads `infosec/questionnaire.yaml` from the instance checkout. Use `--questionnaire <path>` (or `EVIDENCE_QUESTIONNAIRE`) to point it somewhere else; the path must be relative and inside the tree. Validate the file without touching a cluster:

```bash
llz ci cis-evidence --check-questionnaire
```

```yaml
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: EvidenceQuestionnaire
metadata:
  name: my-org-production-checklist        # lowercase kebab
  source: My Org Production Account Policy # how your approver knows the checklist
requirements:
  - num: 8                                 # your checklist's row number
    title: Credential rotation schedule
    llz: What the template enforces (empty = nothing).
    instance: What this instance must set.
    adopter: What remains your org's process.
    gap: A known shortfall, stated.        # optional
    refs: [docs/runbooks/linode-credential-rotation.md]
    gates: [assert-rotation-health]        # `llz ci` verbs whose outcome the pack records
```

The file is **validated strictly**, and a file that fails validation fails the pack:

- An unknown key is an error, so a typo cannot silently drop an answer.
- `apiVersion` and `kind` must match the values above, and `metadata.name` is required.
- There must be at least one requirement. Row numbers must be unique and positive, and every row needs a title.
- Every row must set at least one of `llz`, `instance` or `adopter`. A row nobody satisfies has to say so honestly, in `gap`.
- Each `gates` entry must be a bare, **registered** `llz ci` verb. At runtime the pack checks it against the CLI that is running.

A **missing** file is not an error: the pack is still built and records that no questionnaire was provided.

**Cite only the gates the pack records.** The scheduled job passes the outcomes of `token-inventory`, `assert-rotation-health` and `alert-eval`. A cited gate with no recorded outcome shows *not run* on its row, never passed. Name build-time and bootstrap-time gates (`at-rest-guard`, `assert-volume-encryption`, `assert-k8s-version`) in the prose instead.

---

## Section A — The Generic Example, Answered

These rows document [`questionnaire.yaml.example`](../../instance-template/infosec/questionnaire.yaml.example). They are **org-neutral**: no intake process, vendor, attribution format or training programme is named. Add those rows in your own copy. Tests keep this table and the example in step:

- every path and gate the example cites appears here, and every gap is stated;
- every cited path exists;
- every cited gate is a registered verb that the scheduled job records;
- every "hardcoded" claim is checked against the file it describes, parsed so that re-formatting the source (`tofu fmt`) does not break the check.

| # | Requirement | LLZ provides | Instance configures | Adopter process | Implemented by / evidence |
|---|-------------|--------------|---------------------|-----------------|---------------------------|
| 1 | Approved production account | — | — | Request a production cloud account; obtain security approval through your org's intake | Your org's intake record |
| 2 | Required resource tags | Every tag in `spec.cluster.tags` is stamped onto the LKE cluster, its node pool and the node Cloud Firewall | `spec.cluster.tags`: your ownership / environment / data-classification tags | Account-level tags in the cloud console (LLZ manages resources, not the account) | `terraform-modules/llz-cluster/main.tf`, `terraform-modules/llz-cluster/firewall.tf`, `tools/internal/shared/tfroots/roots/cluster/main.tf`. **Gap:** no validator requires a tag; an empty list passes |
| 3 | Cloud firewall with default deny | Node Cloud Firewall created with `inbound_policy = "DROP"` (hardcoded) and attached to the node pool; every other inbound rule is scoped to the VPC subnet, the LKE control-plane range or the NodeBalancer range (plus `apiServerAllowCIDRs`, see #10) | — | Operator ingress (bastion SSH, admin HTTPS) is added and reviewed under your own firewall process; the baseline ships none | `terraform-modules/llz-cluster/firewall.tf`. **Gap:** the baseline allows ICMP from `0.0.0.0/0` and `::/0`. Record an exception or narrow the rule |
| 4 | Cloud accounts enrolled in CSPM | — | — | Enroll each cloud account with your CSPM provider | Your CSPM enrollment record |
| 5 | CSPM cluster integration | — | — | Install your CSPM vendor's Kubernetes integration as an instance-owned Argo CD Application (`kubernetes-custom/`), with its credential delivered through OpenBao + External Secrets | **Gap:** LLZ ships no CSPM connector. Vendor connectors usually hold cluster-wide read; review the ClusterRole they install, including whether it reads Secrets |
| 6 | Port exposure review | Beyond the VPC and LKE control-plane ranges, the node firewall exposes only the NodePort range, and only to the NodeBalancer range (plus `apiServerAllowCIDRs` when set) | — | Review each externally reachable port (every LoadBalancer Service a team creates) before go-live | `terraform-modules/llz-cluster/firewall.tf` |
| 7 | Administrative SSH access | Not applicable to nodes: no standalone instances are declared, and the node firewall admits TCP/22 only from the cluster's own VPC subnet; there is no external SSH rule | — | — | `terraform-modules/llz-cluster/firewall.tf` |
| 8 | Credential rotation schedule | Credential single pane: the daily `token-inventory` measures every CI token's expiry, `assert-rotation-health` fails any credential past the 90-day SLA, and `alert-eval` reads the credential alerts. Scheduled rotation covers the lke-admin token, the Linode PAT and the object-storage keys | Credentials as GitHub environment secrets; optionally `broadPatRotator` | Set and track the cadence of every credential LLZ does not hold (third-party service accounts, CSPM tokens) | `instance-template/.github/workflows/llz-scheduled-checks.yml`, `instance-template/.github/workflows/llz-secret-rotation.yml`, `docs/runbooks/linode-credential-rotation.md`; gates `llz ci token-inventory`, `llz ci assert-rotation-health`, `llz ci alert-eval`. **Gap:** one 90-day SLA for every credential LLZ measures; credentials outside LLZ are not measured |
| 9 | Encryption at rest and network segmentation | Encrypted node disks and block volumes (the encrypted default StorageClass), secrets in OpenBao delivered by External Secrets, encrypted Terraform state, and default-deny NetworkPolicies on platform namespaces. Enforced at PR time by `llz ci at-rest-guard` and at bootstrap by `llz ci assert-volume-encryption`; the daily pack reports the StorageClass and NetworkPolicy signals live | `objProxy` to encrypt Object Storage (SSE-C); keep workloads on the encrypted StorageClass | Classify the data | `tools/internal/extensions/lifecycle/bootstrapcluster/manifests/block-storage-class.yaml`, `kubernetes-charts/llz-cluster-foundation/templates/network-policies.yaml`, `platform-apl/components/objProxy/kustomization.yaml`. **Gap:** LKE-E's control plane reaches nodes over a shared private range, so the node firewall admits that whole range on the kubelet/DNS/BGP ports rather than individual addresses |
| 10 | Kubernetes control-plane IP allowlist | Cluster created with the control-plane ACL enabled (hardcoded); open-world (`/0`) entries are refused by the CIDR validator that `llz env add` and the spec readiness scan apply | `spec.cluster.apiServerAllowCIDRs`: the operator and CI egress ranges that must reach the API. A connector-style integration that dials out (most CSPM brokers) needs no entry; only a scanner that calls the API server inbound does | — | `terraform-modules/llz-cluster/main.tf`, `tools/internal/shared/validate/validate.go`. **Gap:** `apiServerAllowCIDRs` also opens the NodePort range on the node firewall to the same CIDRs, and there is no ACL-only list. After creation Terraform ignores ACL drift |
| 11 | Managed Kubernetes tier | Cluster created with `tier = "enterprise"` (hardcoded); the pinned version is checked against the account's Enterprise catalog before every apply by `llz ci assert-k8s-version` | — | — | `terraform-modules/llz-cluster/main.tf` |
| 12 | Node disk encryption | Node pools created with `disk_encryption = "enabled"` (hardcoded); `llz ci at-rest-guard` fails a pull request that adds a node pool or instance without it | — | — | `tools/internal/shared/tfroots/roots/cluster/main.tf`, `tools/internal/extensions/lifecycle/atrest/atrest.go` |
| 13 | Secret rotation runbook | Rotation runbooks ship with every instance (lke-admin token, Linode credentials) alongside the secrets reference | — | Accept or remediate the documented position on the OpenBao seal and recovery keys, which are static by design | `docs/secrets.md`, `docs/runbooks/lke-admin-rotation.md`, `docs/runbooks/linode-credential-rotation.md` |
| 14 | Single sign-on | — | — | Cloud-console access through your org's SSO. Federate App Platform's Keycloak to your identity provider if your checklist covers in-cluster logins | Your SSO configuration |

**On control-plane allowlists and CSPM.** An outbound connector (a broker that dials the vendor's SaaS from inside the cluster) never needs an `apiServerAllowCIDRs` entry. Adding scanner ranges "just in case" is not free: it also opens the NodePort range on the node firewall to those ranges (gap #10).

---

## Section B — CIS Kubernetes Benchmark: Not Scored on Managed App Platform

**This pack does not score the CIS Kubernetes Benchmark on a managed App Platform cluster today, and makes no CIS §4/§5 claim.** The reason is specific.

The scanner that would score it is trivy-operator's **cluster-compliance scanner**. apl-core installs trivy-operator with that scanner turned off. In apl-core's `values/trivy-operator/trivy-operator.gotmpl`:

- `operator.clusterComplianceEnabled: false`;
- `operator.infraAssessmentScannerEnabled: false`, so there is no node or infra assessment;
- `targetNamespaces` is limited to the `team-*` namespaces, so platform namespaces are never config-audited.

The chart still **creates** its compliance-report objects: `k8s-cis-1.23`, `k8s-nsa-1.0`, `k8s-pss-baseline-0.1` and `k8s-pss-restricted-0.1`, from `charts/trivy-operator/templates/specs/`. With the scanner off, those objects never get a `status`.

**LLZ cannot turn the scanner on.** On managed App Platform the only channel to an apl-core app's chart values is `apps.<name>._rawValues` on the AplApp object, through LLZ's apl-overlay (`apl-values/_shared/apl-overlay/appvalues.yaml`). apl-core's helmfile injects `_rawValues` only into the release named after the app (`helmfile.d/snippets/common.gotmpl` reads `apps.<release>._rawValues`). trivy-operator's release is named `trivy-operator` while the app is `trivy`, and its values file reads no `_rawValues` of its own, so `apps.trivy._rawValues` is accepted by the schema and **reaches no chart**. Setting it would be exactly the "key that names nothing" failure the apl-overlay README warns about.

What the pack does instead:

- It **harvests apl-core's own report objects** rather than shipping duplicates. An earlier draft shipped its own `cis` report with `controls: []`, which scored nothing.
- It **fails closed on hollow reports.** A report with no `status.updateTimestamp`, with 0 pass / 0 fail, or older than `--max-report-age` (default 24h) is **NOT_COLLECTED**.
- It **checks the scanner directly.** It reads `OPERATOR_CLUSTER_COMPLIANCE_ENABLED` from the `trivy-operator-config` ConfigMap. While that is not `true`, the whole pack is **NOT_COLLECTED**, whatever the gates beside it say, and `--strict` (which the scheduled job uses) fails the step. When apl-core exposes the switch, or a self-managed instance turns it on, the same pack starts scoring the specs above with no LLZ change.

The controls an auditor would map to CIS §4/§5 are still **implemented and gated**. The evidence for them is the named LLZ gate, not a CIS score:

| Area | LLZ control | Evidence |
|------|-------------|----------|
| Data at rest | Encrypted block-storage StorageClass + LDE node disks | `llz ci at-rest-guard` (PR), `llz ci assert-volume-encryption` (bootstrap); pack: default StorageClass encrypted |
| Network segmentation | Default-deny ingress+egress on platform namespaces | `llz ci assert-network-enforcement` (assert-suite); pack: NetworkPolicy count |
| Pod Security | PSA `enforce: restricted` on LLZ namespaces | [namespaces.yaml](../../kubernetes-charts/llz-cluster-foundation/templates/namespaces.yaml); pack: restricted-namespace list |
| Secrets management | OpenBao + External Secrets Operator; the audit device ships to Loki | `llz ci externalsecret-paths`, `llz ci assert-openbao-audit` |
| Image provenance | Kyverno cosign verification of the first-party image | `llz ci assert-admission-enforcement` |

---

## Out-of-Scope CIS Controls (Managed Control Plane)

Linode operates the LKE-Enterprise control plane. The CIS Benchmark sections below are the **provider's** responsibility under shared responsibility, and tenants cannot audit them. They are recorded here as explicitly out of scope, not as findings or gaps.

| CIS § | Area | Why out of scope |
|-------|------|------------------|
| 1 | Control Plane Components (API server, controller-manager, scheduler) | Operated and hardened by Linode; node access and config files are not exposed to tenants |
| 2 | etcd | Managed datastore; encryption at rest and peer TLS are operated by Linode |
| 3 | Control Plane Configuration (authn/authz, audit logging) | API server audit policy and config are managed by Linode |

> Auditor note: request Linode's LKE-Enterprise shared-responsibility / compliance attestation (e.g. SOC 2) to cover sections 1–3.

---

## How the Evidence Pack Is Generated

`llz ci cis-evidence` runs daily in the `credential-single-pane` job of [llz-scheduled-checks.yml](../../instance-template/.github/workflows/llz-scheduled-checks.yml), after that job's own gates. It does four things:

1. **Harvests** the compliance-scanner switch, apl-core's ClusterComplianceReports (`--report` overrides the list), NetworkPolicy coverage, the namespaces enforcing restricted PSS, and the default StorageClass's encryption parameter.
2. **Records the gates that ran beside it**: `token-inventory`, the credential funnel (`assert-rotation-health`) and the credential alerts (`alert-eval`). Each is recorded as PASS, FAIL or SKIPPED on the rows that cite it.
3. **Carries the instance's questionnaire** (`infosec/questionnaire.yaml`), so the answers and their evidence travel together.
4. **Emits** a JSON bundle, a Markdown report and the bundle's sha256. These are uploaded as the `cis-evidence-<env>` workflow artifact with 90-day retention.

**Result.** The pack is **FAIL** when a compliance report fails or a recorded gate fails. It is **PASS** only when the compliance scanner is on and every expected report is fresh, non-empty and passing. Anything else is **NOT_COLLECTED**. Gate outcomes alone never make a pack PASS. A harvest that could not reach the cluster is recorded as *not collected*.

> **Signing.** The scheduled job records a sha256 digest instead of a cosign attestation, because keyless signing needs `id-token: write` and the delivered caller stub does not grant it. An instance that wants a signed bundle can add that permission to its `scheduled-checks.yml` stub and sign the uploaded artifact (`cosign attest-blob`) in its own step.

---

## Auditor Sign-Off

| Role | Name | Date | Signature / Approval ref |
|------|------|------|--------------------------|
| Project / Product owner | | | |
| InfoSec reviewer | | | |
| Evidence pack ref | | | `cis-evidence-<env>-<timestamp>.json` + its `.sha256` |

---

## Related Docs

- [Linode Account Request — Checklist](./linode-account-request-checklist.md): the account-request process
- [Secrets management](../secrets.md): rotation, seal/recovery keys, service credentials
- [Landing-zone spec](../landing-zone-spec.md): `spec.cluster.tags`, `apiServerAllowCIDRs`, components
- [apl-overlay README](../../instance-template/apl-values/_shared/apl-overlay/README.md): the `_rawValues` channel and its limits
- [Adopter guide](../adopter-guide.md): onboarding a `<project>` onto the landing zone
