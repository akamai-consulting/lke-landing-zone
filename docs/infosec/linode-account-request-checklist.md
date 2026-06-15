# Linode Account Request — Checklist

**Sources:** Internal account-creation process docs, Linode VM Policies & Best Practices, your org's security requirements (e.g. SOG), InfoSec Approval Process
**Last Updated:** 2026-05-05

This checklist walks a `<project>` team through requesting a Linode account for a secure-by-default LKE-Enterprise + apl-core landing zone, and obtaining the InfoSec approval required before production go-live. Linode and LKE-Enterprise are hard givens for this landing zone. Replace `<org>`, `<project>`, and `<cluster_domain>` with your own values; substitute your organization's equivalents for any org-specific tools, distribution lists, or portals named below.

---

## Step 1 — Determine Account Type

| Type | Who | Resource Limits | InfoSec Approval? |
|------|-----|----------------|------------------|
| **Personal** | Individual employees for personal learning | Low | No |
| **Demo & Learning** | Teams for non-production demos | Medium; firewall template required | No (exception process if needed) |
| **Production** | Org products/services | Unlimited (request capacity separately) | **Yes — required** |

For a production supporting-systems account (the use case that covers product supporting systems running on Linode), proceed through the full checklist below.

---

## Step 2 — Pre-Submission Prerequisites

- [ ] Identify the **executive sponsor** (e.g. VP-level) who will provide written approval (required for all production accounts)
- [ ] Confirm the correct **company name format** per your org's convention: `<org> - <project> - [Workload] Prod`
- [ ] Confirm billing contact fields (country, state/region, postal code) for your org
- [ ] Identify the team **distribution list** (DL) email for billing; create one if it doesn't exist
- [ ] Complete any **mandatory Linode/compute training** required by your org before account access is granted
- [ ] Determine the two geographic regions for HA deployment (if applicable)

---

## Step 3 — Submit the Account Request Form

1. Navigate to your org's internal compute account request form
2. Fill out the questionnaire — key sections typically include:
   - **Use Case** (select the production supporting-systems use case)
   - **Billing contact** and **company name** (see format above)
   - **Data classification** — does the account store sensitive/confidential data?
   - **Executive approval** — attach or forward the sponsor's approval email
   - **Security contact** — team DL for security escalations
3. Submit the completed form to your org's account-creation intake
4. CC the relevant account-creation team when forwarding executive approval

---

## Step 4 — InfoSec Production Approval

For projects requiring a production security review:

- [ ] Submit the architecture overview via your org's InfoSec intake portal
- [ ] Include: architecture diagram, data flow, port exposure table, threat model
- [ ] Your security org creates a tracking ticket for the production review
- [ ] Establish primary contacts on the product-security / architecture team
- [ ] Follow your org's published guidance for working with the product-security architecture team

---

## Step 5 — Satisfy Your Org's Security Requirements (e.g. SOG)

The requirements below are generic production-security controls (these map to a typical "SOG"-style requirements set). All must be met before production go-live. Items marked ⬜ require action.

| # | Requirement | Action |
|---|-------------|--------|
| 1 | Approved account type | ✅ Production (supporting-systems use case) |
| 2 | Required account tags applied | Apply your org's ownership/production/data-classification tags |
| 3 | Cloud Firewall with default DROP | Configure CFW on all Linodes/LKE node pools — no 0.0.0.0/0 rules |
| 4 | Enroll all accounts in CSPM (Dev/Test/QA/Prod) | Enroll each account with your org's CSPM team |
| 5 | CSPM agent integrations | Install the CSPM LKE auto-enroll integration on LKE; install the host agent on any standalone VMs |
| 6 | Port exposure ticket | File a port-exposure review ticket for each exposed port before go-live |
| 7 | Managed SSH access for all VM SSH | Enroll any VMs in your org's SSH access gateway; no direct key-only SSH to production VMs |
| 8 | Token/key rotation schedule | User/service tokens: 90 days; bucket keys: 120 days; CSPM tokens: 180 days |
| 9 | No sensitive data on Private LAN | Encrypt all secrets at rest; no cross-customer PII on the shared private network (e.g. `192.168.128.0/17`) |
| 10 | LKE-Enterprise required | Use LKE-Enterprise for production clusters (a hard given for this landing zone) |
| 11 | LKE secret rotation runbook | Document the process for rotating secrets-manager unseal keys + service credentials (see [docs/secrets.md](../secrets.md)) |
| 12 | LKE Control Plane IP ACL | Enable IP ACL; whitelist your CSPM scanner IP ranges |
| 13 | LKE disk encryption (LDE) | Automated via Terraform — `disk_encryption = "enabled"` is enforced by the secure-by-default node-pool module at cluster bootstrap; no manual LKE engineering request required |
| 14 | Account attribution format | Set Company Name: `<org> - <project> - [Workload] Prod` |
| 15 | SSO for org accounts | All team members must authenticate via your org's Control Center SSO |
| 16 | Annual Linode Practitioner Training | Completed by all team members with account access |

---

## Step 6 — Post-Approval Setup

After the account is provisioned and InfoSec approval received:

### Cloud Firewall
- [ ] Create Cloud Firewall with default DROP inbound policy in Cloud Manager
- [ ] Apply firewall to all Linodes and LKE node pools
- [ ] Permit only the specific ports/sources documented in your port-exposure ticket

### LKE Clusters
- [ ] Install **Cloud Firewall Controller** via Helm (required for LKE inbound control):
  ```bash
  helm repo add linode-cfw https://linode.github.io/cloud-firewall-controller
  helm repo update
  helm install cloud-firewall-controller linode-cfw/cloud-firewall-controller \
    --namespace kube-system \
    --set linodeToken=<service-PAT>
  ```
- [ ] Enable **LKE Control Plane IP ACL** and add your CSPM scanner IP ranges
- [ ] Confirm **LKE disk encryption (LDE)** is enabled — automated by the secure-by-default node-pool Terraform module (`disk_encryption = "enabled"`); verify the node pool was created with disk encryption (check `terraform show` or Cloud Manager node pool details)
- [ ] Install your CSPM **LKE auto-enroll** Helm chart on each cluster

### Account & Identity
- [ ] Set **Company Name** in Cloud Manager → Account → Billing Contact
- [ ] Apply your org's account tags (ownership / production / data-classification)
- [ ] Ensure all team members access via your org's **Control Center SSO** (cloud.linode.com)
- [ ] Create PATs with expiry ≤ 90 days; add rotation reminders to the team calendar (see [docs/secrets.md](../secrets.md))

### CSPM & Compliance
- [ ] Enroll account in your org's CSPM platform
- [ ] Verify enrollment in your org's account-inventory dashboard (typically refreshed every ~24 hours)
- [ ] File a port-exposure ticket for all externally-reachable ports

### Private LAN Hardening
- [ ] Confirm no CFW rules allow the shared private range (e.g. `192.168.128.0/17`) broadly — only specific `/32` IPs (NodeBalancer health: `192.168.255.0/24`, LKE control plane) are permitted
- [ ] ICMP: permit from your org's IP ranges only (a full block can cause GRE/MTU failures with some DDoS-mitigation paths)

---

## Related Docs

- [Secrets management](../secrets.md) — secret rotation, unseal keys, and service credentials
- [Adopter guide](../adopter-guide.md) — onboarding a `<project>` onto the landing zone
- [apl-core migration runbook](../apl-core-migration-runbook.md) — migrating workloads onto apl-core

---

## Key Contacts

Replace these with your org's equivalents:

| Purpose | Contact |
|---------|---------|
| Account creation questions | Org account-creation intake DL |
| Account creation team | Org compute/account creation team DL |
| Security best practices / firewall exceptions | Org security best-practices channel |
| CSPM onboarding | Org CSPM onboarding DL |
| InfoSec production approval | Org InfoSec intake portal |
| Product-security architecture review | Org product-security architecture team |
| Security escalations | Org security escalation request form |
| Live security Q&A | Org security best-practices channel |
