package health

// routing.go — THE ONE FUNNEL every finding passes through.
//
// Two remaps stand between "a check decided something" and "the report records
// it", and they have to happen in this order:
//
//  1. THE KONNECTIVITY DOWNGRADE. A hard failure whose text is the tunnel
//     signature is an apiserver→pod transport outage, not a verdict on the
//     component. It becomes Pending so a converge polls its budget instead of
//     spending a hard strike, and it raises TunnelDown so the summary can name the
//     one cause behind N symptoms.
//  2. THE OWNERSHIP BOUNDARY. A gating finding on instance-owned content is
//     demoted to CatInstance: reported, excluded from the platform verdict, and
//     recorded at its ORIGINAL severity so AppVerdict() still gates the app scope.
//
// Order matters and was wrong before this file existed: converge applied
// ownership first and returned, so a tunnel blip on an instance-owned resource
// was banked as a hard failure for the app lane and never raised TunnelDown at
// all. A transport outage is not an app team's problem in either scope.
//
// The funnel lives HERE, in the package that owns both rules, rather than in the
// caller — the caller's only remaining job is to print what these return. That is
// also what makes the rules testable by calling the real functions: a copy of the
// routing in a test proves nothing about the routing that ships.

// normalizeTunnel applies remap (1). It is the single place that knows the
// signature, so every apiserver→pod surface (APIService discovery, exec probes,
// workload conditions) is covered without touching them individually.
func (r *Report) normalizeTunnel(cat Category, msg string) Category {
	if cat == CatFail && IsTunnelBlocked(msg) {
		r.TunnelDown = true
		return CatPending
	}
	return cat
}

// instanceDemotion applies remap (2) — the ONE statement of the boundary rule.
// owned is the ownership answer the caller resolved (an index hit, or an
// Application's own project). CatOK, CatWarn, CatDeferred and CatDrift are
// already non-gating and pass through untouched.
func instanceDemotion(cat Category, owned bool, msg string) (Category, string, bool) {
	if !owned || (cat != CatFail && cat != CatPending) {
		return cat, msg, false
	}
	return CatInstance, msg + InstanceOwnedSuffix, true
}

// Route records a finding about a cluster-wide fact — a node, a webhook, a lease —
// which has no resource identity to attribute to an owner. Returns the category
// and text the caller should print.
func (r *Report) Route(cat Category, msg string) (Category, string) {
	cat = r.normalizeTunnel(cat, msg)
	r.Add(cat, msg)
	return cat, msg
}

// Route records a finding about a NAMED resource, applying both remaps. Every
// check that judges one identifiable resource calls this instead of Report.Route,
// so the boundary cannot be applied in some sections and forgotten in others.
func (i OwnershipIndex) Route(r *Report, cat Category, ref ResourceRef, msg string) (Category, string) {
	cat = r.normalizeTunnel(cat, msg)
	if newCat, newMsg, demoted := i.demote(cat, ref, msg); demoted {
		r.AddInstanceOf(cat, newMsg)
		return newCat, newMsg
	}
	r.Add(cat, msg)
	return cat, msg
}

// RouteOwned is Route for a resource NO Application declares directly, because a
// controller generated it: a Pod (owned by a ReplicaSet/StatefulSet/DaemonSet), a
// Workflow (spawned by a CronWorkflow under a generated name), a CertificateRequest
// (created by cert-manager for a Certificate), a Job (created by a CronJob).
//
// Ownership is resolved through the controller that IS declared. self is the
// resource's own ref, used when it has no controller owner — for a Pod that is the
// zero ref (a bare pod is claimed by nobody), for a Job or Workflow created
// directly from a manifest it is the resource itself.
func (i OwnershipIndex) RouteOwned(r *Report, cat Category, owners []OwnerRef, self ResourceRef, msg string) (Category, string) {
	ref := self
	if owner, ok := PodWorkloadOwner(owners, self.Namespace); ok {
		ref = owner
	}
	return i.Route(r, cat, ref, msg)
}

// RouteApp records one Argo Application's own finding. It is the Application-level
// half of the same boundary, and it exists so that half records the severity too.
//
// It used to run through Report.Add(CatInstance, …), which appends to the display
// list and nothing else — so a broken instance-owned Application gated NEITHER
// scope, and the summary banner keyed on the demoted severities never fired for
// it. That is the worst case rather than a corner one: an Application in
// ComparisonError publishes an EMPTY .status.resources, so the ownership index
// claims no children for it and this line is the only signal that exists.
func (r *Report) RouteApp(a ArgoApp, phase1 bool) (Category, string) {
	cat, msg := classifyArgoApp(a, phase1)
	cat = r.normalizeTunnel(cat, msg)
	if newCat, newMsg, demoted := instanceDemotion(cat, IsInstanceOwnedApp(a), msg); demoted {
		r.AddInstanceOf(cat, newMsg)
		return newCat, newMsg
	}
	r.Add(cat, msg)
	return cat, msg
}

// RouteInconclusive records a corpus that could not be READ. It is Report.Route
// for CatPending plus the one fact neither verdict can infer afterwards: the scan
// has a hole in it. Both scopes then answer "in progress" rather than one of them
// answering "converged" about resources it never saw.
func (r *Report) RouteInconclusive(kind string) (Category, string) {
	r.Inconclusive = true
	return r.Route(CatPending, "could not list "+kind+" — cluster read failed after retries; treating as inconclusive rather than 'none found'")
}
