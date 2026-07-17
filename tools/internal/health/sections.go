package health

import (
	"fmt"
	"strings"
	"time"
)

// sections.go ports the remaining count/phase/condition checks (sections 3a–3i):
// PV hygiene, NetworkPolicy presence, Jobs, CronWorkflows, Service endpoints,
// PodDisruptionBudgets, Ingress addresses, Argo Workflows, and stuck-finalizer
// deletions. All reuse the established categories + deferred/phase-1 routing; the
// caller resolves phase1Pending (phase1 && MatchPrefix) where a name match applies.

// ClassifyPVPhase classifies a PersistentVolume by phase: Failed/Pending are hard
// failures; Bound/Available/Released are fine (Released is an expected orphan
// under Retain — the orchestrator emits a single aggregate warn when any exist);
// an unrecognized phase warns.
func ClassifyPVPhase(phase string) Category {
	switch phase {
	case "Failed", "Pending":
		return CatFail
	case "Bound", "Available", "Released":
		return CatOK
	default:
		return CatWarn
	}
}

// NetpolExemptNamespace reports whether a namespace owns no repo default-deny
// NetworkPolicy (argocd is operator-managed; kube-system is implicit-allow).
func NetpolExemptNamespace(ns string) bool { return ns == "argocd" || ns == "kube-system" }

// ClassifyNamespaceNetpol classifies a namespace's NetworkPolicy presence: ≥1 is
// healthy; zero defers when the namespace's NPs ride along with an operator-
// deferred Application, else fails (default-deny missing).
func ClassifyNamespaceNetpol(ns string, count int) (Category, string) {
	if count >= 1 {
		return CatOK, fmt.Sprintf("Namespace %s has %d NetworkPolicy(s)", ns, count)
	}
	if r, ok := MatchExternalDep(ns, NPExternalDepNamespaces()); ok {
		return CatDeferred, fmt.Sprintf("Namespace %s has NO NetworkPolicies — %s", ns, r)
	}
	return CatFail, fmt.Sprintf("Namespace %s has NO NetworkPolicies — default-deny missing (workloads run unrestricted)", ns)
}

// ClassifyJob classifies a Job: Complete passes; Failed fails (pending under a
// Phase-1 cascade); a never-started Job (no active/succeeded/failed pods) warns
// (pending under Phase 1); otherwise it's in progress.
func ClassifyJob(key string, complete, failed bool, active, succeeded, failedCount int, phase1Pending bool) (Category, string) {
	if complete {
		return CatOK, fmt.Sprintf("Job %s Complete (%d succeeded)", key, succeeded)
	}
	if failed {
		if phase1Pending {
			return CatPending, "Job " + key + " Failed — waiting on OpenBao bootstrap"
		}
		return CatFail, fmt.Sprintf("Job %s Failed (succeeded=%d failed=%d)", key, succeeded, failedCount)
	}
	if active == 0 && succeeded == 0 && failedCount == 0 {
		if phase1Pending {
			return CatPending, "Job " + key + " not started — waiting on OpenBao bootstrap"
		}
		return CatWarn, "Job " + key + " no active/succeeded/failed pods (stuck pre-admission?)"
	}
	return CatOK, fmt.Sprintf("Job %s active=%d succeeded=%d failed=%d (in progress)", key, active, succeeded, failedCount)
}

// JobRun is the subset of a Job the supersession analysis needs.
type JobRun struct {
	Key       string    // namespace/name
	CronOwner string    // owning CronJob name ("" if not CronJob-spawned)
	Created   time.Time // metadata.creationTimestamp
	Complete  bool
	Failed    bool
}

// SupersededFailedJobs returns the keys of Failed Jobs that a NEWER (or
// same-time) Completed sibling under the same CronJob owner has superseded —
// an early CronJob tick that failed before its backing service was up (e.g.
// harbor-robot-provisioner firing before harbor-core serves, or argo-resync-
// nudger before argocd is ready), later made moot by a successful tick.
//
// The gate would otherwise hard-fail on such a historical Failed Job even
// though the CronJob is healthy — a recurring false red (2026-07-02 e2e). The
// rule is deliberately narrow: a Failed Job is superseded ONLY by a success
// created at-or-after it, so a CURRENT regression (latest tick failing after an
// earlier success) still fails. Jobs with no CronJob owner are never masked.
func SupersededFailedJobs(jobs []JobRun) map[string]bool {
	latestOK := map[string]time.Time{}
	for _, j := range jobs {
		if j.Complete && j.CronOwner != "" {
			if t, ok := latestOK[j.CronOwner]; !ok || j.Created.After(t) {
				latestOK[j.CronOwner] = j.Created
			}
		}
	}
	out := map[string]bool{}
	for _, j := range jobs {
		if j.Failed && !j.Complete && j.CronOwner != "" {
			if t, ok := latestOK[j.CronOwner]; ok && !j.Created.After(t) {
				out[j.Key] = true
			}
		}
	}
	return out
}

// ClassifyCronWorkflow classifies a CronWorkflow: a SubmissionError fails; a
// suspended one warns; ageDays<0 (never scheduled) is informational; a
// lastScheduledTime older than staleDays fails (schedule isn't firing); else OK.
func ClassifyCronWorkflow(key, submissionErr string, suspended bool, ageDays, staleDays int) (Category, string) {
	switch {
	case submissionErr != "":
		return CatFail, "CronWorkflow " + key + " SubmissionError — " + submissionErr
	case suspended:
		return CatWarn, "CronWorkflow " + key + " suspended (spec.suspend=true)"
	case ageDays < 0:
		return CatOK, "CronWorkflow " + key + " has not yet scheduled a run"
	case ageDays > staleDays:
		return CatFail, fmt.Sprintf("CronWorkflow %s lastScheduledTime is %dd old (> %dd) — schedule isn't firing", key, ageDays, staleDays)
	default:
		return CatOK, fmt.Sprintf("CronWorkflow %s (%dd ago, no SubmissionError)", key, ageDays)
	}
}

// ClassifyServiceEndpoints classifies a Service by its ready endpoint count:
// >0 passes; zero routes through Phase-1 then operator-deferred before failing.
func ClassifyServiceEndpoints(key string, readyCount int, phase1Pending bool) (Category, string) {
	if readyCount > 0 {
		return CatOK, fmt.Sprintf("Service %s (%d ready endpoint(s))", key, readyCount)
	}
	if phase1Pending {
		return CatPending, "Service " + key + " has 0 ready endpoints — waiting on OpenBao bootstrap"
	}
	if r, ok := MatchExternalDep(key, ExternalDepWorkloads()); ok {
		return CatDeferred, "Service " + key + " has 0 ready endpoints — " + r
	}
	return CatFail, "Service " + key + " has 0 ready endpoints (selector drift or all backing pods NotReady)"
}

// ClassifyPDB classifies a PodDisruptionBudget. An orphan (expectedPods=0) is
// informational; a satisfied budget (currentHealthy≥desired with disruptions
// allowed, or single-replica / over-provisioned) passes; otherwise an operator-
// deferred match defers, Phase 1 pends (workloads still settling), else it fails
// (node drains will block).
func ClassifyPDB(key string, cur, des, allow, exp int, phase1 bool) (Category, string) {
	label := fmt.Sprintf("PDB %s (currentHealthy=%d desiredHealthy=%d disruptionsAllowed=%d)", key, cur, des, allow)
	switch {
	case exp == 0:
		return CatOK, "PDB " + key + " expectedPods=0 (selector matches nothing — orphan PDB?)"
	case cur >= des && allow != 0:
		return CatOK, label
	case cur >= des && exp == 1:
		return CatOK, "PDB " + key + " (single-replica, drain blocks until reschedule)"
	case cur >= des && des < exp:
		return CatOK, "PDB " + key + " (over-provisioned, permits drains once healed)"
	}
	if r, ok := MatchExternalDep(key, ExternalDepWorkloads()); ok {
		return CatDeferred, label + " — " + r
	}
	if phase1 {
		return CatPending, label + " — workloads still settling"
	}
	return CatFail, fmt.Sprintf("PDB %s (currentHealthy=%d desiredHealthy=%d disruptionsAllowed=%d expectedPods=%d) — node drains will block; check minAvailable vs replicas", key, cur, des, allow, exp)
}

// ClassifyIngress classifies an Ingress by whether its loadBalancer address is
// programmed: ≥1 address passes; none pends under Phase 1, else fails.
func ClassifyIngress(key string, addressCount int, phase1 bool) (Category, string) {
	if addressCount > 0 {
		return CatOK, "Ingress " + key + " programmed"
	}
	if phase1 {
		return CatPending, "Ingress " + key + " has no address — ingress controller may not yet be Ready"
	}
	return CatFail, "Ingress " + key + " has no .status.loadBalancer.ingress (controller down / IngressClass mismatch / webhook rejection)"
}

// ClassifyWorkflowPhase classifies a (persisted) Argo Workflow by phase:
// Failed/Error fails (pends under Phase 1, where cert-automation is gated on
// OpenBao); Succeeded passes; Pending/Running are in-flight (OK/info).
// IsEphemeralE2EProbe reports whether a Workflow/Pod name is an ephemeral e2e
// health probe (submitted by `llz ci assert-health-workflow`, generateName
// "e2e-assert-health-"). These are TEST SCAFFOLDING, not platform components: a
// Failed one lingering on a REUSED e2e cluster must never gate convergence — the
// assert-health-workflow step checks its OWN workflow by name, and converge
// scanning all Workflows/Pods would otherwise inherit a prior run's dead probe.
func IsEphemeralE2EProbe(name string) bool {
	return strings.HasPrefix(name, "e2e-assert-health-")
}

func ClassifyWorkflowPhase(key, phase string, phase1 bool) (Category, string) {
	// key is "namespace/name" — ignore ephemeral e2e probes regardless of phase.
	if name := key[strings.LastIndex(key, "/")+1:]; IsEphemeralE2EProbe(name) {
		return CatOK, "Workflow " + key + " (ephemeral e2e probe — not a convergence signal)"
	}
	switch phase {
	case "Failed", "Error":
		if phase1 {
			return CatPending, fmt.Sprintf("Workflow %s phase=%s — cert-automation gated on OpenBao bootstrap", key, phase)
		}
		return CatFail, fmt.Sprintf("Workflow %s phase=%s", key, phase)
	case "Succeeded":
		return CatOK, "Workflow " + key + " Succeeded"
	default:
		return CatOK, fmt.Sprintf("Workflow %s %s (in flight)", key, phase)
	}
}

// StuckFinalizer reports whether a resource is a dead, un-GC-able object: it has
// a deletionTimestamp, a non-empty finalizer set, and has been Terminating for
// more than 5 minutes.
func StuckFinalizer(hasDeletionTimestamp bool, finalizerCount int, ageSeconds float64) bool {
	return hasDeletionTimestamp && finalizerCount > 0 && ageSeconds > 300
}

// StuckResourceKinds are the kinds swept for stuck-finalizer deletions, in
// "kind|scope" form (scope "-A" = namespaced, "" = cluster-scoped).
func StuckResourceKinds() []string {
	return []string{
		"pv|",
		"pvc|-A",
		"certificates.cert-manager.io|-A",
		"certificaterequests.cert-manager.io|-A",
		"workflows.argoproj.io|-A",
		"externalsecrets.external-secrets.io|-A",
		"clustersecretstores.external-secrets.io|",
		"clusterissuers.cert-manager.io|",
		"issuers.cert-manager.io|-A",
	}
}
