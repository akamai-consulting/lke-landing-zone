package clusterspec

// values.go resolves the values-repo identity that render.go stamps into the
// carved Application CRs (the repo the Apps sync from). The apl-core value-render
// pipeline that used to live here — RenderValues plus its object-store/identity/
// team wiring and the yaml.Node scalar setters — was RETIRED when LLZ moved to
// Linode's managed App Platform: apl-core owns its own values.yaml (LLZ no longer
// renders a per-env one — render_test asserts it never does), and LLZ drives
// apl-core's NATIVE config through the apl-overlay reconciler instead. See
// docs/adr/0005-managed-app-platform.md + docs/designs/apl-overlay-obj-native.md.

// ValuesIdentity carries the spec-derived values-repo coordinates render.go needs
// (currently just the repo URL the carved Applications point at). Build it with
// (*LandingZone).ValuesIdentity.
type ValuesIdentity struct {
	RepoURL string // the values repo the carved Application CRs sync from
}

// ValuesIdentity resolves the values-repo URL for env from the assembled spec. It
// defaults to the instance repo itself — the same literal the copier-rendered
// tfvars example carried ("https://github.com/<instance_repo>.git") — when the env
// spec omits aplValues.repoURL, so a carved App never points at an empty URL. An
// explicit spec value wins. Left empty only when spec.instance.repo is also unset,
// which Validate rejects.
func (lz *LandingZone) ValuesIdentity(env string) ValuesIdentity {
	e, _ := lz.Env(env)
	repoURL := e.Cluster.Bootstrap.AplValues.RepoURL
	if repoURL == "" && lz.Spec.Instance.Repo != "" {
		repoURL = "https://github.com/" + lz.Spec.Instance.Repo + ".git"
	}
	return ValuesIdentity{RepoURL: repoURL}
}
