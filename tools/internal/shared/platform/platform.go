// Package platform holds the well-known names this platform's own resources
// carry: namespaces, annotations, and the objects llz creates and then reads back.
//
// THEY ARE FACTS, NOT CAPABILITIES, and that phrasing is lifted from
// internal/extensions/reconciler/names.go, which had already made the argument and
// then drawn the wrong conclusion from it: "the same call baoread.Namespace and
// docsguard's DeliveredDocs got. Four packages need to agree on them ... the only
// way two callers can disagree about what a ConfigMap is called is if there are
// two copies of the name." Correct -- and it then placed them "with the RECONCILER
// rather than with `firewall`", which answers WHICH EXTENSION when the
// answer is NEITHER. A name four packages must agree on is substrate.
//
// The rule for adding here: it is a string that two or more packages must spell
// identically, and getting it wrong is silent. A ConfigMap read under the wrong
// name is empty, not an error; a StorageClass annotation misspelled simply never
// matches.
package platform

// apl-core 6.x ships ESO as a core app in the `external-secrets` namespace; the
// landing zone no longer runs its own controller. (The `openbao`
// ClusterSecretStore probed below is cluster-scoped, so the -n is cosmetic, but
// keep it pointed at the live ESO namespace.)
const ESONamespace = "external-secrets"

const SCDefaultAnnotation = "storageclass.kubernetes.io/is-default-class"

// FirewallConfigMapName is the controller's config ConfigMap (kube-system). It
// MUST match the chart's fullname-derived name (release llz-linode-cidr-firewall
// → <fullname>-config), which is what the Deployment's env reads, so the dynamic
// LINODE_FIREWALL_ID / LKE_CLUSTER_ID we patch here land in the ConfigMap the
// controller actually consumes. The controller + chart live in the private
// lke-landing-zone-internal repo; these llz subcommands are the integration hook
// that bootstraps and health-checks it. The Application ignoreDifferences those
// two keys so selfHeal keeps our patch (the chart renders them empty placeholders).
const FirewallConfigMapName = "llz-linode-cidr-firewall-config"

// FirewallDeploymentName is the controller Deployment (chart fullname). After
// patching the ConfigMap we roll it: env injected via configMapKeyRef is read
// once at pod creation, so a Deployment ArgoCD already created from the empty
// placeholders would crashloop on the stale values until restarted.
const FirewallDeploymentName = "llz-linode-cidr-firewall"

// ── THE DELIVERED DOC SET ────────────────────────────────────────────────────
//
// DeliveredDocs came from internal/extensions/docsguard. It is the third name
// reconciler/names.go cited when it argued these are FACTS -- "the same call
// baoread.Namespace and docsguard's DeliveredDocs got" -- so it was already
// recognised as one and left in an extension anyway. deliver-docs consumes it to
// decide what to keep; docs-guard consumes it to decide what to check. Two
// consumers that must agree exactly is the whole argument.
// DeliveredDocs is the day-to-day operator set an instance carries locally.
// Everything else in docs/ is referenced at the template repo. Names are entries
// directly under docs/ — a file keeps its extension, a directory does not.
var DeliveredDocs = map[string]bool{
	"quickstart.md": true, // stand up + operate this instance
	"runbooks":      true, // incident recovery
	"playbooks":     true, // routine operational how-tos
	"README.md":     true, // the pointer deliver-docs writes (kept if it already exists)
}

// RenderTimeArtifact names paths that exist in a RENDERED instance but not in the
// template, because the render itself creates them. They are not dead links — the
// guard simply cannot see them from here. Keep this list tiny and cite the creator,
// so it stays a statement of fact rather than a place to bury real breakage.
// IT LIVES HERE FOR THE REASON DeliveredDocs DOES, and an architecture guard is
// what said so: source-ref-guard needed it too, importing docsguard to get it, and
// TestNoNewExtensionToExtensionImports refused the edge — "extension packages must
// not import each other; the library half moves to internal/shared". Which path is
// written by a render rather than committed is a FACT two consumers must agree on
// exactly, not a capability either of them owns. docs-guard skips these links;
// source-ref-guard skips these literals; a second copy is how one of them grows.
var RenderTimeArtifact = map[string]bool{
	// runDeliverDocs writes it (docsPointer) after pruning docs/ to the keep-set.
	"docs/README.md": true,
	// `llz render` writes the per-root tfvars; terraform-iac-bootstrap/.gitignore
	// excludes the whole rendered tree, which is why the instance commits zero
	// Terraform. Cited by shared/terraform as the variable contract to stay in
	// step with.
	"instance-template/terraform-iac-bootstrap/cluster/variables.tf": true,
	// `llz scaffold` writes it from landingzone.yaml.example; only the example is
	// committed. e2e-instantiate rm -f's it to force a clean re-scaffold.
	"instance-template/landingzone.yaml": true,
}
