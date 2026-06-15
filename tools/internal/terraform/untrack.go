package terraform

import (
	"regexp"
	"strings"
)

// untrack.go holds the pure decision logic of untrack-cluster-bootstrap-on-destroy.sh:
// classifying the cluster API host, recognising a "no usable kubeconfig" state, and
// selecting which resource addresses to drop from TF state before a destroy. The
// terraform console / curl probe / state rm orchestration lives in cmd/llz.

// AplCoreChain is the CASE A drop set: the apl-core resources whose
// `helm uninstall` blocks on finalizer-heavy state (CNPG clusters, Argo apps,
// the apl-operator namespace) for zero benefit — the downstream cluster delete
// reaps them regardless. Dropping just these keeps `terraform destroy` from
// hanging on the uninstall while everything else is destroyed against the live API.
func AplCoreChain() []string {
	return []string{
		"helm_release.apl",
		"kubectl_manifest.apl_operator_namespace",
		"kubectl_manifest.apl_sops_secrets_placeholder",
		"kubectl_manifest.platform_app_storage_class",
	}
}

// clusterBackedRe matches resource addresses that need the cluster API to
// refresh/delete: helm_release.*, kubectl_manifest.*, and any kubernetes_*.
var clusterBackedRe = regexp.MustCompile(`^(helm_release\.|kubectl_manifest\.|kubernetes_)`)

// ClusterBackedAddrs returns the cluster-backed resource addresses in a
// `terraform state list` output — the CASE B drop set (every resource whose
// destroy would dial a gone/unreachable cluster API).
func ClusterBackedAddrs(stateList string) []string {
	var out []string
	for _, line := range strings.Split(stateList, "\n") {
		a := strings.TrimSpace(line)
		if a != "" && clusterBackedRe.MatchString(a) {
			out = append(out, a)
		}
	}
	return out
}

// StateHas reports whether addr appears as an exact line in a state list (the
// idempotent skip-if-absent guard for state rm).
func StateHas(stateList, addr string) bool {
	for _, line := range strings.Split(stateList, "\n") {
		if strings.TrimSpace(line) == addr {
			return true
		}
	}
	return false
}

// NoUsableKubeconfig reports whether the raw kubeconfig value from `terraform
// console` means there is no usable kubeconfig (cluster gone or never
// initialized): providers.tf normalizes it to "" and a present-but-null output
// prints as `null`. Either => CASE B.
func NoUsableKubeconfig(kubeconfigRaw string) bool {
	return kubeconfigRaw == `""` || kubeconfigRaw == "null"
}

// KubeHostSentinel is providers.tf's dummy endpoint used when the cluster
// workspace has no kubeconfig — its presence means the API is definitively gone.
const KubeHostSentinel = "https://cluster-already-destroyed.invalid"

// KubeHostKind classifies the cluster API host read from `terraform console`.
type KubeHostKind int

const (
	KubeHostProbe   KubeHostKind = iota // a real https:// host — probe /livez
	KubeHostGone                        // the no-kubeconfig sentinel — definitively unreachable
	KubeHostUnknown                     // unparseable — treat conservatively as reachable (CASE A)
)

// ClassifyKubeHost decides how to treat the cluster API host.
func ClassifyKubeHost(host string) KubeHostKind {
	switch {
	case host == KubeHostSentinel:
		return KubeHostGone
	case strings.HasPrefix(host, "https://"):
		return KubeHostProbe
	default:
		return KubeHostUnknown
	}
}
