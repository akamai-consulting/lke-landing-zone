package health

import "strings"

// foundations.go ports check-cluster-health.sh sections 0a–0e: node Ready/pressure,
// unexpected taints, stuck-Terminating namespaces, APIService availability, the
// required-CRD set, and the StorageClass default-class rule. The kubectl get calls
// live in cmd/llz; these are the pure classifications over the parsed JSON.

// ── Nodes (0a) ───────────────────────────────────────────────────────────────

// NodeCondition is one entry of a node's .status.conditions.
type NodeCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// Taint is one entry of a node's .spec.taints.
type Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

// Node is the subset of `kubectl get nodes -o json` the health checks read.
type Node struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Taints []Taint `json:"taints"`
	} `json:"spec"`
	Status struct {
		Conditions []NodeCondition `json:"conditions"`
	} `json:"status"`
}

// Name is the node's metadata.name.
func (n Node) Name() string { return n.Metadata.Name }

// Condition returns the status of a node condition by type, or "?" if absent —
// matching the jq `(.conds.<Type> // "?")`.
func (n Node) Condition(t string) string {
	for _, c := range n.Status.Conditions {
		if c.Type == t {
			return c.Status
		}
	}
	return "?"
}

// NodeHealthy reports whether the node is Ready with no Memory/Disk/PID pressure,
// returning the four condition values for the report line.
func NodeHealthy(n Node) (ok bool, ready, mem, disk, pid string) {
	ready = n.Condition("Ready")
	mem = n.Condition("MemoryPressure")
	disk = n.Condition("DiskPressure")
	pid = n.Condition("PIDPressure")
	ok = ready == "True" && mem == "False" && disk == "False" && pid == "False"
	return ok, ready, mem, disk, pid
}

// UnexpectedTaints returns a node's NoSchedule/NoExecute taints that are NOT
// Kubernetes-managed (node-role.kubernetes.io/* or node.kubernetes.io/*) — the
// operator/autoscaler taints that silently block scheduling.
func UnexpectedTaints(n Node) []Taint {
	var out []Taint
	for _, t := range n.Spec.Taints {
		if t.Effect != "NoSchedule" && t.Effect != "NoExecute" {
			continue
		}
		if strings.HasPrefix(t.Key, "node-role.kubernetes.io/") || strings.HasPrefix(t.Key, "node.kubernetes.io/") {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ── Namespaces (0b) ──────────────────────────────────────────────────────────

// NamespaceTerminating reports whether a namespace phase is the stuck-Terminating
// state that blocks fresh applies into it.
func NamespaceTerminating(phase string) bool { return phase == "Terminating" }

// ── APIServices (0c) ─────────────────────────────────────────────────────────

// APIServiceCondition is one entry of an APIService's .status.conditions.
type APIServiceCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// APIService is the subset of `kubectl get apiservices -o json` we read.
type APIService struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Conditions []APIServiceCondition `json:"conditions"`
	} `json:"status"`
}

// APIServiceUnavailable reports whether an APIService has an Available condition
// whose status is not True (aggregated API is down), with "reason: message" for
// the report.
func APIServiceUnavailable(a APIService) (bad bool, msg string) {
	for _, c := range a.Status.Conditions {
		if c.Type == "Available" && c.Status != "True" {
			return true, c.Reason + ": " + c.Message
		}
	}
	return false, ""
}

// ── Required CRDs (0d) ───────────────────────────────────────────────────────

// RequiredCRDs are the CRDs this repo ALWAYS depends on; a missing one means its
// owning ArgoCD Application never installed it. CRDs owned by OPTIONAL/ManagedSkip
// components (argo-workflows / argo-events) are NOT here — see ConditionalCRDs.
func RequiredCRDs() []string {
	return []string{
		"applications.argoproj.io",
		"appprojects.argoproj.io",
		"certificates.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"issuers.cert-manager.io",
		"externalsecrets.external-secrets.io",
		// NOTE (apl-core 6.x): clusterexternalsecrets.external-secrets.io is NOT
		// required. On 5.0.0 the landing zone ran its own ESO (installCRDs) which
		// shipped every ESO CRD; on 6.x apl-core's bundled ESO installs only the
		// kinds it uses (ExternalSecret / ClusterSecretStore / SecretStore / PushSecret),
		// not ClusterExternalSecret — and this repo does not use ClusterExternalSecret
		// anywhere. Requiring it hard-failed convergence for a CRD nothing needs.
		"clustersecretstores.external-secrets.io",
		"secretstores.external-secrets.io",
		"gateways.networking.istio.io",
		"virtualservices.networking.istio.io",
		"prometheusrules.monitoring.coreos.com",
	}
}

// ConditionalCRDs maps a CRD to the ArgoCD Application (in the argocd namespace) that
// owns it, for CRDs from OPTIONAL components. The argo-workflows/argo-events components
// are DefaultDisabled/ManagedSkip, so these CRDs are absent unless their app is deployed
// (an opt-in self-install, or managed with argo in managedApps). The health check only
// requires each CRD when its owning Application is present — so a managed cluster (argo
// skipped) and a self-install that never opted in both pass, while a deployed-but-broken
// argo app still hard-fails on its missing CRD.
func ConditionalCRDs() map[string]string {
	return map[string]string{
		"workflows.argoproj.io":         "argo-workflows",
		"workflowtemplates.argoproj.io": "argo-workflows",
		"cronworkflows.argoproj.io":     "argo-workflows",
		"eventbus.argoproj.io":          "argo-events",
		"eventsources.argoproj.io":      "argo-events",
		"sensors.argoproj.io":           "argo-events",
	}
}

// ── StorageClasses (0e) ──────────────────────────────────────────────────────

// RequiredStorageClasses are the repo-owned classes that must be present.
func RequiredStorageClasses() []string { return []string{"block-storage-retain"} }

const defaultStorageClassAnnotation = "storageclass.kubernetes.io/is-default-class"

// StorageClass is the subset of `kubectl get storageclass -o json` we read.
type StorageClass struct {
	Metadata struct {
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
}

// DefaultStorageClasses returns the names of the classes annotated as the
// cluster default. Exactly one is healthy; zero (PVCs without a class stay
// Pending) and more than one (non-deterministic selection) are both failures.
func DefaultStorageClasses(classes []StorageClass) []string {
	var out []string
	for _, sc := range classes {
		if sc.Metadata.Annotations[defaultStorageClassAnnotation] == "true" {
			out = append(out, sc.Metadata.Name)
		}
	}
	return out
}
