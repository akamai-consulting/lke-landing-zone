package clusterspec

// component_emits.go — "does this component's content actually reach the
// cluster for this environment?", as ONE function.
//
// It exists because a preflight needed the answer and there was no way to ask
// it. The repo-level Secrets capability check demands a GitHub grant whose only
// consumer is the harbor-robot-provisioner CronJob, which ships inside the
// `harbor` component — and a component reaches a cluster only if BOTH halves
// agree: the env's toggle (`spec.components.harbor.enabled`, default true) and,
// on the Managed App Platform, the operator's declared app list
// (`spec.cluster.bootstrap.managedApps`, via ManagedConditionalOn). An instance
// that opts out either way runs no provisioner and needs no grant.
//
// THE CONJUNCTION IS THE POINT, and asking half of it is how this goes wrong:
// ComponentEnabled alone says "yes" for a managed cluster whose managedApps list
// omits harbor, and EmitOnManaged alone says "yes" for a self-managed cluster
// that switched the component off. kustomize.go and overlay.go both apply the
// pair; a third caller spelling it out again is the split-contract shape
// docs/e2e-gates.md warns about, so they all call this.

// ComponentEmits reports whether the named component's content is PRESENT on a
// cluster built from this environment.
//
// MANDATORY IS ALWAYS PRESENT, whatever the toggles say. This is the one place
// the answer differs from RenderManifestKustomization's loop, and the difference
// is not a discrepancy: that loop skips Mandatory components because they are
// emitted by a DIFFERENT path, not because they are absent — "cannot be disabled
// (the cluster does not converge without them)". Asking the toggles about them
// gives the wrong answer in the worst direction for a preflight: clusterFoundation
// is Mandatory AND ManagedSkip, so the conjunction alone calls it "not emitted" on
// every managed cluster, and a check keyed on it would silently stop asking about
// a component guaranteed to be there. A first cut of this function did exactly
// that, and only harbor's not being Mandatory kept it from mattering.
//
// Otherwise it is the conjunction both renderers apply: the env's toggle
// (spec.components.<name>.enabled) AND, on the Managed App Platform, the
// operator's declared app list (bootstrap.managedApps, via ManagedConditionalOn).
//
// An unknown name is false: a caller asking about a component that does not
// exist is asking about content nothing will ever emit.
func ComponentEmits(components map[string]ComponentToggle, boot Bootstrap, name string) bool {
	c, ok := componentByName[name]
	if !ok {
		return false
	}
	if c.Mandatory {
		return true
	}
	return ComponentEnabled(components, name) && c.EmitOnManaged(boot, components)
}

// DisabledComponents names every component that will NOT emit for env, as a set.
//
// IT REPORTS THE OFF SET, NOT THE ON SET, because the consumer's default has to
// be "assume it runs". A caller that cannot read a spec at all gets an empty map
// and therefore treats every component as present — which for a permissions
// preflight means it still asks for the grant, i.e. it fails toward the check
// that has evidence behind it rather than toward silence. Inverting this to an
// on-set would make an unreadable spec silently switch every conditional check
// off, which is the failure mode the check was written to end.
//
// A missing spec, an unknown env, or a parse error is not an error here for the
// same reason: `llz doctor` runs in trees at every stage of setup, and a
// preflight that refuses to run without a complete spec is a preflight that does
// not run when it is most needed.
//
// THAT FAIL-OPEN IS ALSO THE RISK, and it is why TestDisabledComponentsReadsARealSpec
// exists. A swallowed load error yields an empty off-set, every conditional check
// runs, and nothing anywhere says the spec went unread — so a schema bump or a
// layout move would turn the conditionality inert with every test still green.
// That gate loads a REAL spec off disk and asserts a component the fixture
// disables actually comes back disabled, so "the loader stopped working" and
// "nothing is disabled" stop looking alike.
//
// THE ROOT COMES FROM Detected(), not a parameter. It is "the single entry every
// command uses to become spec-aware"; all three call sites passed "." and would
// have gone on passing it, which duplicates a decision that already has a home
// and would diverge the day an instance layout changes under one of them.
func DisabledComponents(env string) map[string]bool {
	off := map[string]bool{}
	lz, present, err := Detected()
	if err != nil || !present || lz == nil {
		return off
	}
	e, ok := lz.Env(env)
	if !ok {
		return off
	}
	for _, c := range Components {
		if !ComponentEmits(e.Components, e.Cluster.Bootstrap, c.Name) {
			off[c.Name] = true
		}
	}
	return off
}
