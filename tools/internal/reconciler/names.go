package reconciler

// names.go — the two firewall-controller names, and WHY they live here.
//
// They are FACTS, not capabilities — the same call baoread.Namespace and
// docsguard.DeliveredDocs got. Four packages need to agree on them
// (ci_assert_reconciler.go, ci_converge.go, and this package's own
// discover-firewall lane), and the only way two callers can disagree about what
// a ConfigMap is called is if there are two copies of the name.
//
// They sit with the RECONCILER rather than with `llz ci firewall` because the
// discover-firewall lane is what writes the ConfigMap and rolls the Deployment;
// the CI command reads what this wrote.

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
