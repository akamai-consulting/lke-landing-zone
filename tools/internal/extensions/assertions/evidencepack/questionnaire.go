package evidencepack

// questionnaire.go — the pre-filled answers to the org security checklist (the
// SOG table in docs/infosec/linode-account-request-checklist.md, Step 5), split
// by WHO satisfies each requirement:
//
//   - LLZ provides   — enforced by a module/chart/policy/gate in this template, on
//     every instance, with no adopter action;
//   - Instance configures — a spec/tfvars/secret value the instance must set for
//     the control to hold;
//   - Adopter process — organizational: tickets, tenancy, training, account
//     settings no template can reach.
//
// It is DATA, rendered into every evidence pack (so the answers travel with the
// signed evidence) and mirrored in docs/infosec/control-mapping.md. The two are
// held together by tests rather than by hope: every Ref must exist in the repo,
// every Gate must be a real `llz ci` verb (internal/cli), the doc's Section A
// must cite every Ref of its row, and each "hardcoded" claim below is pinned to
// the shipped file it describes (claims_test.go). An answer that drifts from the
// code fails at PR time instead of in front of an auditor.
//
// GAPS ARE ANSWERS TOO. Where LLZ falls short of the requirement as written, the
// row says so (Gap) instead of rounding up: an approval granted on an overstated
// answer is worse than a known exception.

// Requirement is one row of the org checklist, answered.
type Requirement struct {
	Num   int    `json:"num"`
	Title string `json:"title"`
	// LLZ is what the template enforces by default ("" = nothing).
	LLZ string `json:"llz_provides,omitempty"`
	// Instance is what the instance must configure ("" = nothing).
	Instance string `json:"instance_configures,omitempty"`
	// Adopter is the organizational process that remains ("" = none).
	Adopter string `json:"adopter_process,omitempty"`
	// Gap is a known shortfall against the requirement as written ("" = none).
	Gap string `json:"gap,omitempty"`
	// Refs are repo-relative paths implementing or documenting the control.
	Refs []string `json:"refs,omitempty"`
	// Gates are the `llz ci` verbs whose outcome is evidence for this row; when the
	// pack is built with --gate <verb>=<result>, the row shows it.
	Gates []string `json:"gates,omitempty"`
}

// Questionnaire is the answered checklist, in checklist order.
var Questionnaire = []Requirement{
	{
		Num: 1, Title: "Approved account type",
		Adopter: "Request a Production account and obtain InfoSec approval through the org intake.",
		Refs:    []string{"docs/infosec/linode-account-request-checklist.md"},
	},
	{
		Num: 2, Title: "Required account tags applied",
		LLZ:      "Every tag in spec.cluster.tags is stamped onto the LKE cluster, its node pool and the node Cloud Firewall.",
		Instance: "Set spec.cluster.tags to the org's ownership / production / data-classification tags.",
		Adopter:  "Apply the account-level tags in Cloud Manager (LLZ manages resources, not the account).",
		Gap:      "No validator requires any tag; an empty tags list passes.",
		Refs:     []string{"terraform-modules/llz-cluster/variables.tf", "tools/internal/shared/tfroots/roots/cluster/main.tf"},
	},
	{
		Num: 3, Title: "Cloud Firewall with default DROP",
		LLZ:     "The node Cloud Firewall is created with inbound_policy DROP (hardcoded, not a variable) and attached to the node pool; inbound rules are scoped to the VPC subnet, the LKE control-plane range and the NodeBalancer range.",
		Adopter: "Operator ingress (bastion SSH / HTTPS) is reconciled by the org's firewall controller from the org firewall template.",
		Gap:     "The baseline allows ICMP from 0.0.0.0/0 and ::/0; the checklist asks for no 0.0.0.0/0 rules and ICMP from org ranges only. Record an exception or narrow the rule.",
		Refs:    []string{"terraform-modules/llz-cluster/firewall.tf"},
	},
	{
		Num: 4, Title: "Enroll all accounts in CSPM",
		Adopter: "Enroll each Linode account (dev/test/QA/prod) with the org CSPM team; LLZ enrolls clusters (#5), not accounts.",
	},
	{
		Num: 5, Title: "CSPM agent integrations",
		LLZ:      "Optional `wiz` component: the upstream Wiz connector, its credential delivered from OpenBao by ExternalSecret, a default-deny namespace, and a gate that fails unless the cluster is actually registered and the broker tunnel is Ready.",
		Instance: "Enable components.wiz with secretPath under a declared team subtree, and seed clientId + clientToken there.",
		Adopter:  "Provision the Wiz tenant and a service account for the connector. No standalone VMs exist, so no host agent is needed.",
		Gap:      "The connector's ClusterRole (upstream) reads every resource, including Secrets.",
		Refs:     []string{"platform-apl/components/wiz/kustomization.yaml", "platform-apl/components/wiz/wiz/application.yaml"},
		Gates:    []string{"assert-wiz-enrollment"},
	},
	{
		Num: 6, Title: "Port exposure ticket",
		LLZ:     "Beyond the VPC and LKE control-plane ranges, the node firewall exposes only the NodePort range, and only to the NodeBalancer range (plus spec apiServerAllowCIDRs when set).",
		Adopter: "File a port-exposure ticket for each externally reachable port before go-live.",
		Refs:    []string{"terraform-modules/llz-cluster/firewall.tf"},
	},
	{
		Num: 7, Title: "Managed SSH access for VM SSH",
		LLZ:  "Not applicable: the landing zone declares no standalone Linode instances, and the node firewall has no TCP/22 rule.",
		Refs: []string{"terraform-modules/llz-cluster/firewall.tf"},
	},
	{
		Num: 8, Title: "Token/key rotation schedule",
		LLZ:      "Credential single pane: daily token-inventory measures every CI token's expiry, assert-rotation-health fails any credential past the 90-day SLA, alert-eval reads the credential alerts; scheduled rotation for the lke-admin token, the Linode PAT and the object-storage keys.",
		Instance: "Provide the credentials as environment secrets; optionally enable broadPatRotator for in-cluster PAT rotation.",
		Adopter:  "Set and track the CSPM (Wiz) service-account rotation cadence.",
		Gap:      "The in-cluster SLA is 90 days for all credentials (stricter than the 120-day bucket-key requirement); the 180-day CSPM token is not measured.",
		Refs:     []string{"instance-template/.github/workflows/llz-scheduled-checks.yml", "instance-template/.github/workflows/llz-secret-rotation.yml", "docs/runbooks/linode-credential-rotation.md"},
		Gates:    []string{"token-inventory", "assert-rotation-health", "alert-eval"},
	},
	{
		Num: 9, Title: "No sensitive data on Private LAN; secrets encrypted at rest",
		LLZ:      "Node disks and block volumes are encrypted (disk_encryption, the encrypted default StorageClass), secrets live in OpenBao and reach pods through ESO, Terraform state is encrypted, platform namespaces run default-deny NetworkPolicies.",
		Instance: "Enable objProxy to encrypt Object Storage (SSE-C); keep workloads on the encrypted StorageClass.",
		Adopter:  "Classify the data; keep cross-customer PII off the shared private network.",
		Gap:      "LKE-E's control plane reaches nodes over the shared private range, so the node firewall admits that whole range on the kubelet/DNS/BGP ports rather than individual /32s.",
		Refs:     []string{"tools/internal/extensions/lifecycle/bootstrapcluster/manifests/block-storage-class.yaml", "kubernetes-charts/llz-cluster-foundation/templates/network-policies.yaml", "platform-apl/components/objProxy/kustomization.yaml"},
		Gates:    []string{"at-rest-guard", "assert-volume-encryption", "assert-obj-encryption", "assert-network-enforcement"},
	},
	{
		Num: 10, Title: "LKE-Enterprise required",
		LLZ:   "The cluster is created with tier enterprise (hardcoded); the pinned Kubernetes version is checked against the account's Enterprise catalog before apply.",
		Refs:  []string{"terraform-modules/llz-cluster/main.tf"},
		Gates: []string{"assert-k8s-version"},
	},
	{
		Num: 11, Title: "LKE secret rotation runbook",
		LLZ:     "Rotation runbooks ship with every instance (lke-admin token, Linode credentials) alongside the secrets reference.",
		Adopter: "Accept or remediate the documented position on the OpenBao seal and recovery keys, which are static by design.",
		Refs:    []string{"docs/secrets.md", "docs/runbooks/lke-admin-rotation.md", "docs/runbooks/linode-credential-rotation.md"},
	},
	{
		Num: 12, Title: "LKE control-plane IP ACL",
		LLZ:      "The control-plane ACL is enabled (hardcoded); open-world (/0) entries are refused by the CIDR validator and the readiness scan.",
		Instance: "Add the CSPM scanner ranges to spec.cluster.apiServerAllowCIDRs.",
		Adopter:  "Obtain the current scanner ranges from the CSPM team.",
		Gap:      "apiServerAllowCIDRs also opens the NodePort range on the node firewall to those CIDRs; there is no ACL-only list.",
		Refs:     []string{"terraform-modules/llz-cluster/main.tf", "tools/internal/shared/validate/validate.go"},
	},
	{
		Num: 13, Title: "LKE disk encryption (LDE)",
		LLZ:   "Node pools are created with disk_encryption enabled (hardcoded); at-rest-guard fails any node pool or instance without it and does not accept an exemption for it.",
		Refs:  []string{"tools/internal/shared/tfroots/roots/cluster/main.tf", "tools/internal/extensions/lifecycle/atrest/atrest.go"},
		Gates: []string{"at-rest-guard"},
	},
	{
		Num: 14, Title: "Account attribution format",
		Adopter: "Set the account Company Name to the org's attribution format in Cloud Manager.",
	},
	{
		Num: 15, Title: "SSO for org accounts",
		LLZ:     "In-cluster human access is OIDC through Keycloak (team login to OpenBao).",
		Adopter: "Cloud Manager access through the org SSO.",
		Refs:    []string{"docs/runbooks/openbao-team-login.md"},
	},
	{
		Num: 16, Title: "Annual Linode Practitioner Training",
		Adopter: "Training records for every team member with account access.",
	},
}
