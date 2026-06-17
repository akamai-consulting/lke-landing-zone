package health

// kstatus.go adopts the upstream kstatus library (sigs.k8s.io/cli-utils) as the
// generic readiness primitive for standard Kubernetes workload resources. Before
// this, "is this Deployment/StatefulSet/Pod/Job ready?" was answered by either a
// `kubectl rollout status` shell-out or hand-rolled replica/phase parsing. kstatus
// computes the same Current/InProgress/Failed verdict from the object's status
// using the conventions the kubectl/cli-utils ecosystem already standardises on
// (observedGeneration, replica counts, the Progressing/Available conditions, Job
// completions, …), so we don't re-derive it per resource here.
//
// This is deliberately scoped to the CORE controller kinds kstatus has
// first-class rollout rules for (Deployment / StatefulSet / DaemonSet / Job).
// Note one kstatus semantic to be aware of: a Pod in a terminal phase
// (Failed/Succeeded) is reported as Current ("done, won't change"), not Failed —
// so this primitive is for waiting on controller rollouts, where a bad Pod
// surfaces as an under-replicated controller, not for gating on bare Pods.
//
// The CR-specific predicates in this package — Argo Applications
// (ClassifyArgoApp), cert-manager Certificates (ClassifyCertificateRequest),
// ExternalSecrets, OpenBao seal state, the Linode firewall/control-plane-ACL
// checks — are NOT routed through kstatus: kstatus's generic CR fallback keys off
// the Reconciling/Stalled convention, which those resources do not follow (they
// use a `Ready` condition), so it would mis-report them as Current. Those keep
// their dedicated classifiers.

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"
)

// ResourceStatus computes the convergence-contract Category for a single workload
// resource from its raw JSON (as emitted by `kubectl get <ref> -o json`). It maps
// the kstatus verdict onto the same three-bucket contract the rest of this package
// uses:
//
//	CurrentStatus                                  → CatOK    (converged)
//	InProgress / Terminating / Unknown / NotFound  → CatPending (reconcile in flight; poll)
//	FailedStatus                                   → CatFail  (operator intervention)
//
// NotFound maps to CatPending rather than CatFail on purpose: in a wait/poll loop a
// not-yet-created object is "still coming up", not a terminal failure. The returned
// string is a human-readable label (kstatus's own message when it has one).
func ResourceStatus(raw []byte) (Category, string) {
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(raw); err != nil {
		return CatFail, fmt.Sprintf("unparseable resource JSON: %v", err)
	}
	return resourceStatusFromUnstructured(u)
}

func resourceStatusFromUnstructured(u *unstructured.Unstructured) (Category, string) {
	res, err := status.Compute(u)
	if err != nil {
		return CatFail, fmt.Sprintf("%s %s: kstatus compute failed: %v", kindKey(u), nameKey(u), err)
	}
	label := fmt.Sprintf("%s %s", kindKey(u), nameKey(u))
	if res.Message != "" {
		label += " — " + res.Message
	}
	switch res.Status {
	case status.CurrentStatus:
		return CatOK, label
	case status.FailedStatus:
		return CatFail, label
	default:
		// InProgressStatus, TerminatingStatus, UnknownStatus, NotFoundStatus —
		// all "time may fix this without operator action", i.e. keep polling.
		return CatPending, label
	}
}

// kindKey / nameKey produce a stable "<kind> <ns>/<name>" label without panicking
// on a partial object (an unstructured resource is allowed to omit any field).
func kindKey(u *unstructured.Unstructured) string {
	if k := u.GetKind(); k != "" {
		return k
	}
	return "resource"
}

func nameKey(u *unstructured.Unstructured) string {
	ns, name := u.GetNamespace(), u.GetName()
	switch {
	case ns != "" && name != "":
		return ns + "/" + name
	case name != "":
		return name
	default:
		return "<unnamed>"
	}
}
