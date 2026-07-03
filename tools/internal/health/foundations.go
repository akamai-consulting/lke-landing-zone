package health

import (
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

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

// RequiredCRDs are the CRDs this repo depends on; a missing one means its owning
// ArgoCD Application never installed it.
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
		"workflows.argoproj.io",
		"workflowtemplates.argoproj.io",
		"cronworkflows.argoproj.io",
		"eventbus.argoproj.io",
		"eventsources.argoproj.io",
		"sensors.argoproj.io",
		"gateways.networking.istio.io",
		"virtualservices.networking.istio.io",
		"prometheusrules.monitoring.coreos.com",
	}
}

// ── StorageClasses (0e) ──────────────────────────────────────────────────────

// RequiredStorageClasses are the repo-owned classes that must be present.
func RequiredStorageClasses() []string { return []string{"block-storage-retain"} }

const defaultStorageClassAnnotation = "storageclass.kubernetes.io/is-default-class"

// StorageClass is the subset of `kubectl get storageclass -o json` we read.
// provisioner + parameters are TOP-LEVEL on a StorageClass (not under spec).
type StorageClass struct {
	Metadata struct {
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Provisioner string            `json:"provisioner"`
	Parameters  map[string]string `json:"parameters"`
}

// Linode block-storage CSI parameter keys the driver actually HONORS. The driver
// silently ignores unknown keys — no error, no warning — so a misspelling leaves
// Volumes unencrypted and/or untagged while the StorageClass still looks correct.
// That exact regression shipped once (keys were `encryption`/`volume-tags`). This
// audit pins the live class to the honored spelling and flags the known-ignored
// misspellings by name so a future typo can't silently regress the fleet.
const (
	blockStorageProvisioner = "linodebs.csi.linode.com"
	csiEncryptedKey         = "linodebs.csi.linode.com/encrypted"
	csiVolumeTagsKey        = "linodebs.csi.linode.com/volumeTags"
	csiEncryptedKeyIgnored  = "linodebs.csi.linode.com/encryption"  // silently ignored
	csiVolumeTagsKeyIgnored = "linodebs.csi.linode.com/volume-tags" // silently ignored
	blockStorageSweepTag    = "block-storage"                       // destroy-time sweep filters on this
	reapOwnershipTagPrefix  = "lke"                                 // `lke<id>` — reap's liveness gate keys on this

	// canonicalBlockStorageClass is the platform's encrypting + tagging default class
	// (manifests/block-storage-class.yaml, applied by ci bootstrap-cluster with the
	// lke<id> volumeTag rendered in) — the one every PVC should land on.
	canonicalBlockStorageClass = "block-storage-retain"
)

// lkeStockStorageClasses are the LKE-shipped classes: present but not for platform
// use (demoted from default, and PVCs redirected off them by the
// kyverno-pvc-redirect-untagged-storage-class policy). They carry no platform
// volumeTags by design, so the audit acknowledges them rather than flagging them.
var lkeStockStorageClasses = map[string]bool{
	"linode-block-storage":        true,
	"linode-block-storage-retain": true,
}

// StorageClassParamFinding is one assertion about block-storage-retain's params.
type StorageClassParamFinding struct {
	Cat Category
	Msg string
}

// ClassifyBlockStorageParams audits block-storage-retain's provisioner + params for
// the honored CSI keys: encryption on, volumeTags carrying the sweep tag and an
// `lke<id>` ownership tag, and NONE of the silently-ignored misspellings. Returns
// one finding per assertion (CatOK included so the caller can print passes).
func ClassifyBlockStorageParams(sc StorageClass) []StorageClassParamFinding {
	var f []StorageClassParamFinding
	add := func(c Category, m string) { f = append(f, StorageClassParamFinding{c, m}) }

	if sc.Provisioner != blockStorageProvisioner {
		add(CatFail, "provisioner "+q(sc.Provisioner)+" — expected "+q(blockStorageProvisioner))
	} else {
		add(CatOK, "provisioner "+blockStorageProvisioner)
	}

	// Encryption. Flag the ignored misspelling first — its presence means someone
	// intended encryption but the driver dropped it.
	if _, ok := sc.Parameters[csiEncryptedKeyIgnored]; ok {
		add(CatFail, "parameter "+csiEncryptedKeyIgnored+" is SILENTLY IGNORED (use "+csiEncryptedKey+") — Volumes provisioned UNENCRYPTED")
	}
	switch sc.Parameters[csiEncryptedKey] {
	case "true":
		add(CatOK, csiEncryptedKey+"=true")
	case "":
		add(CatFail, csiEncryptedKey+" unset — Volumes provisioned unencrypted")
	default:
		add(CatFail, csiEncryptedKey+"="+q(sc.Parameters[csiEncryptedKey])+` — driver only honors "true"`)
	}

	// volumeTags. Same ignored-misspelling guard, then the tag-content assertions.
	if _, ok := sc.Parameters[csiVolumeTagsKeyIgnored]; ok {
		add(CatFail, "parameter "+csiVolumeTagsKeyIgnored+" is SILENTLY IGNORED (use "+csiVolumeTagsKey+") — Volumes provisioned UNTAGGED")
	}
	tags := splitTags(sc.Parameters[csiVolumeTagsKey])
	if len(tags) == 0 {
		add(CatFail, csiVolumeTagsKey+" unset — Volumes untagged; reap can neither attribute nor sweep them")
		return f
	}
	if containsTag(tags, blockStorageSweepTag) {
		add(CatOK, csiVolumeTagsKey+" carries "+blockStorageSweepTag)
	} else {
		add(CatFail, csiVolumeTagsKey+" missing "+q(blockStorageSweepTag)+" — the destroy-time Volume sweep filters on it")
	}
	// Reuse reap's OWN attribution parser (linode.LKEIDFromTags, `^lke-?[0-9]+$`)
	// rather than a loose "lke" prefix match, so the audit green-lights a tag iff
	// reap can actually attribute it — a malformed `lke-foo`/`lkexyz` must FAIL here
	// exactly as it would be unattributable there.
	if linode.LKEIDFromTags(tags) != "" {
		add(CatOK, csiVolumeTagsKey+" carries an "+reapOwnershipTagPrefix+"<id> ownership tag")
	} else {
		add(CatFail, csiVolumeTagsKey+" has no "+reapOwnershipTagPrefix+"<id> ownership tag — reap can't attribute these Volumes; the cluster_id likely rendered empty (params are immutable — recreate the class)")
	}
	return f
}

// AuditLinodeStorageClasses inspects EVERY StorageClass using the Linode block-storage
// CSI provisioner, so a PVC can't be born on a linodebs class that lacks the `lke<id>`
// ownership tag without the check noticing:
//   - the canonical block-storage-retain class gets the full param audit
//     (ClassifyBlockStorageParams: encryption + sweep tag + lke<id> tag, misspelling traps);
//   - the LKE stock classes are acknowledged (untagged by design; Kyverno redirects
//     PVCs off them, so they never back a Volume);
//   - any OTHER linodebs class is a coverage risk — Volumes provisioned on it carry no
//     `lke<id>` tag, so reap can't attribute them (bleed #4). Warned, not failed: a
//     custom class may be intentional, but it must be seen.
//
// Findings are prefixed with the class name. Non-linodebs classes are ignored.
func AuditLinodeStorageClasses(classes []StorageClass) []StorageClassParamFinding {
	var out []StorageClassParamFinding
	for _, sc := range classes {
		if sc.Provisioner != blockStorageProvisioner {
			continue
		}
		name := sc.Metadata.Name
		switch {
		case name == canonicalBlockStorageClass:
			for _, f := range ClassifyBlockStorageParams(sc) {
				out = append(out, StorageClassParamFinding{f.Cat, name + ": " + f.Msg})
			}
		case lkeStockStorageClasses[name]:
			out = append(out, StorageClassParamFinding{CatOK,
				name + ": LKE stock class present (untagged by design; PVCs redirected off it by kyverno-pvc-redirect-untagged-storage-class)"})
		case linode.LKEIDFromTags(splitTags(sc.Parameters[csiVolumeTagsKey])) != "":
			out = append(out, StorageClassParamFinding{CatOK,
				name + ": non-default linodebs class carries an " + reapOwnershipTagPrefix + "<id> ownership tag"})
		default:
			out = append(out, StorageClassParamFinding{CatWarn,
				name + ": non-default linodebs class has no " + reapOwnershipTagPrefix + "<id> volumeTags — PVCs on it get Volumes reap can't attribute; add the tag or ensure nothing uses it"})
		}
	}
	return out
}

// splitTags parses a Linode volumeTags CSV, trimming spaces and dropping empties.
func splitTags(csv string) []string {
	var out []string
	for _, t := range strings.Split(csv, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func containsTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func q(s string) string { return `"` + s + `"` }

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
