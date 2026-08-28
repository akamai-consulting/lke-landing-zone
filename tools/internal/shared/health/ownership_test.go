package health

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// app builds an ArgoApp the way ParseArgoApp would, for the ownership tests.
func app(name, project string, res ...ResourceRef) ArgoApp {
	return ArgoApp{Name: name, Project: project, Resources: res}
}

func deployRef(ns, name string) ResourceRef {
	return ResourceRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: name}
}

func esRef(ns, name string) ResourceRef {
	return ResourceRef{Group: "external-secrets.io", Kind: "ExternalSecret", Namespace: ns, Name: name}
}

func boolp(b bool) *bool { return &b }

// TestIsInstanceOwnedApp_ProjectNotName is the regression. The managed-apps
// ApplicationSet names its Applications after the app ("dispatch") while stamping
// `project: instance-custom` on every one. Keying on the NAME classified all nine
// of akamai/gsap-apl's app Applications as platform, so an app team's missing
// credential hard-failed a platform release.
func TestIsInstanceOwnedApp_ProjectNotName(t *testing.T) {
	if !IsInstanceOwnedApp(app("dispatch", InstanceCustomProject)) {
		t.Error("an Application in the instance-custom PROJECT is instance-owned whatever it is named")
	}
	// The name-prefix signal still stands on its own, for an Application whose
	// project could not be read.
	if !IsInstanceOwnedApp(app("instance-custom-istio-system", "")) {
		t.Error("the escape-hatch name convention must keep working with no project")
	}
	// FAILS CLOSED: anything else is platform and gates.
	for _, a := range []ArgoApp{
		app("harbor", "platform-support"),
		app("openbao", "platform-support"),
		app("keycloak", ""),
		app("some-team-app", "team-gsap"),
	} {
		if IsInstanceOwnedApp(a) {
			t.Errorf("%s/%s must stay platform — an unrecognised project gates", a.Project, a.Name)
		}
	}
}

// TestInstanceCustomProjectMatchesTheShippedAppProject is the coupling test for
// the string the whole boundary turns on. The consumer is this constant; the
// producer is the AppProject the platform actually ships. Renaming the manifest
// without this would leave IsInstanceOwnedApp matching nothing and quietly return
// every instance-owned Application to the platform gate — the exact regression the
// project-over-name change fixed.
func TestInstanceCustomProjectMatchesTheShippedAppProject(t *testing.T) {
	const manifest = "../../../../platform-apl/manifest/instance-custom-project.yaml"
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read %s: %v", manifest, err)
	}
	m := regexp.MustCompile(`(?s)kind: AppProject.*?metadata:\s*\n\s*name:\s*(\S+)`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("could not find the AppProject's metadata.name in %s — if its shape changed, this test must be taught the new one rather than deleted", manifest)
	}
	if got := string(m[1]); got != InstanceCustomProject {
		t.Errorf("the shipped AppProject is named %q but health.InstanceCustomProject is %q — every instance-owned Application would gate the platform again", got, InstanceCustomProject)
	}
}

// TestOwnershipIndex_FollowsTheBoundaryDown: reclassifying the Application is not
// enough. Its ExternalSecrets, Deployments and Workflows are judged by their own
// sections, and on akamai/gsap-apl thirteen ExternalSecrets alone hard-failed the
// platform gate while their Applications were correctly reported non-gating.
func TestOwnershipIndex_FollowsTheBoundaryDown(t *testing.T) {
	idx := NewOwnershipIndex([]ArgoApp{
		app("dispatch", InstanceCustomProject,
			esRef("team-gsap", "git-credentials-dispatch"),
			deployRef("team-gsap", "dispatch")),
		// A PLATFORM app contributes nothing, even in the same namespace.
		app("harbor", "platform-support", esRef("harbor", "harbor-admin-password")),
	})

	if !idx.Owns(esRef("team-gsap", "git-credentials-dispatch")) {
		t.Error("a resource an instance-owned Application declares must be instance-owned")
	}
	if idx.Owns(esRef("harbor", "harbor-admin-password")) {
		t.Error("a platform Application's resource must never enter the index")
	}
	// Unclaimed → platform → gates. This is the fail-closed direction.
	if idx.Owns(esRef("team-gsap", "never-declared")) {
		t.Error("an undeclared resource must stay platform")
	}
}

// TestOwnershipIndex_PlatformClaimWins is the fail-closed arm the index did not
// have. The instance-custom AppProject is deliberately `namespace: '*'` with
// `group: '*', kind: '*'`, so an operator manifest CAN name a platform resource —
// and the index would have handed it the platform's exemption. Contested means
// platform.
func TestOwnershipIndex_PlatformClaimWins(t *testing.T) {
	idx := NewOwnershipIndex([]ArgoApp{
		app("instance-custom-istio-system", InstanceCustomProject,
			deployRef("istio-system", "gcp-oauth2-proxy"),
			deployRef("istio-system", "istiod")), // also declared by the platform below
		app("istio", "platform-support", deployRef("istio-system", "istiod")),
	})

	if idx.Owns(deployRef("istio-system", "istiod")) {
		t.Error("a resource a PLATFORM Application declares must keep gating, however many instance apps also claim it")
	}
	if !idx.Owns(deployRef("istio-system", "gcp-oauth2-proxy")) {
		t.Error("the operator's own uncontested workload is still instance-owned")
	}
	if idx.Contested() != 1 {
		t.Errorf("Contested() = %d, want 1 — a refused claim must be reported, not silently absorbed", idx.Contested())
	}
}

// TestOwnershipIndex_UnresolvedApplicationsAreReported: Argo publishes an empty
// .status.resources until it has completed a comparison, so an Application that is
// new — or too broken to compare — claims nothing and its children gate the
// platform on that poll. That is the fail-closed direction and stays, but a
// converge that hard-fails on one poll and passes on the next has to be explicable.
func TestOwnershipIndex_UnresolvedApplicationsAreReported(t *testing.T) {
	idx := NewOwnershipIndex([]ArgoApp{
		app("dispatch", InstanceCustomProject), // ComparisonError: no resources yet
		app("account-health", InstanceCustomProject, deployRef("team-gsap", "account-health")),
	})
	got := idx.Unresolved()
	if len(got) != 1 || got[0] != "dispatch" {
		t.Errorf("Unresolved() = %v, want [dispatch] — an Application that claims nothing must be named", got)
	}
}

// TestGroupIsPartOfTheIdentity: Kind alone is not unique. `Certificate` is
// cert-manager's and also several other CRDs', and those are exactly the kinds the
// checks pass. Without the group an operator CRD would demote the platform's TLS
// certificate of the same name in the same namespace.
func TestGroupIsPartOfTheIdentity(t *testing.T) {
	idx := NewOwnershipIndex([]ArgoApp{
		app("mesh-addon", InstanceCustomProject,
			ResourceRef{Group: "mesh.example.com", Kind: "Certificate", Namespace: "istio-system", Name: "gateway"}),
	})
	if idx.Owns(ResourceRef{Group: "cert-manager.io", Kind: "Certificate", Namespace: "istio-system", Name: "gateway"}) {
		t.Error("a same-named Certificate in ANOTHER API group must not be claimed — the platform's cert would stop gating")
	}
}

// TestRoute_OnlyDemotesGatingCategories: the boundary changes what gates, not how
// anything is described. Drift stays drift, deferred stays deferred. Driven
// through Route — the funnel production uses — rather than a restatement of the
// rule, which would pass while the shipped routing was broken.
func TestRoute_OnlyDemotesGatingCategories(t *testing.T) {
	idx := NewOwnershipIndex([]ArgoApp{app("dispatch", InstanceCustomProject, deployRef("team-gsap", "dispatch"))})
	for _, cat := range []Category{CatFail, CatPending} {
		var r Report
		got, msg := idx.Route(&r, cat, deployRef("team-gsap", "dispatch"), "x")
		if got != CatInstance {
			t.Errorf("Route(%v) = %v, want CatInstance", cat, got)
		}
		if msg == "x" {
			t.Error("a demoted finding must say why it no longer gates")
		}
		if len(r.Instance) != 1 {
			t.Error("a demoted finding must still be REPORTED")
		}
	}
	for _, cat := range []Category{CatOK, CatWarn, CatDrift, CatDeferred} {
		var r Report
		if got, msg := idx.Route(&r, cat, deployRef("team-gsap", "dispatch"), "x"); got != cat || msg != "x" {
			t.Errorf("Route(%v) = %v/%q — already non-gating categories must pass through untouched", cat, got, msg)
		}
		if len(r.InstanceFailed)+len(r.InstancePending) != 0 {
			t.Errorf("Route(%v) invented a gate for the app scope", cat)
		}
	}
}

// TestRoute_TunnelOutageIsNotAnAppFailure pins the ORDER of the two remaps. A
// konnectivity outage is an apiserver→pod transport fault the contract polls
// through; applying ownership first banked it as a hard failure for the app lane
// and never raised TunnelDown, so the report lost the one line explaining why N
// unrelated components had all just failed.
func TestRoute_TunnelOutageIsNotAnAppFailure(t *testing.T) {
	const blocked = "ExternalSecret team-gsap/x not Ready: error dialing backend: No agent available"
	idx := NewOwnershipIndex([]ArgoApp{app("dispatch", InstanceCustomProject, esRef("team-gsap", "x"))})

	var inst Report
	idx.Route(&inst, CatFail, esRef("team-gsap", "x"), blocked)
	if !inst.TunnelDown {
		t.Error("a tunnel-blocked finding must raise TunnelDown whoever owns the resource")
	}
	if len(inst.InstanceFailed) != 0 || len(inst.InstancePending) != 1 {
		t.Errorf("tunnel outage recorded as failed=%d pending=%d — the app scope must poll it, not spend a hard strike",
			len(inst.InstanceFailed), len(inst.InstancePending))
	}
	if inst.AppVerdict() != InProgress {
		t.Errorf("app verdict = %v, want InProgress", inst.AppVerdict())
	}

	// The platform side is unchanged, which is the behaviour being matched.
	var plat Report
	idx.Route(&plat, CatFail, esRef("harbor", "x"), blocked)
	if !plat.TunnelDown || plat.Verdict() != InProgress {
		t.Errorf("platform: TunnelDown=%v verdict=%v, want true/InProgress", plat.TunnelDown, plat.Verdict())
	}
}

// TestRouteApp_ApplicationLevelFindingGatesTheAppScope is the hole the resource
// index could not cover. An Application in ComparisonError publishes an EMPTY
// .status.resources, so it claims no children and this line is the only signal
// there is — and it used to reach the report through Add(CatInstance), which
// records for display and remembers no severity. Both verdicts came back
// Converged for a cluster whose every app was broken.
func TestRouteApp_ApplicationLevelFindingGatesTheAppScope(t *testing.T) {
	var r Report
	broken := ArgoApp{Name: "dispatch", Project: InstanceCustomProject,
		Sync: "Unknown", Health: "Degraded", Automated: true,
		SpecErr: "ComparisonError: authentication required"}

	cat, msg := r.RouteApp(broken, false)
	if cat != CatInstance {
		t.Errorf("cat = %v, want CatInstance — an instance-owned App must not gate the platform", cat)
	}
	if msg == "" || len(r.Instance) != 1 {
		t.Fatalf("the finding must be reported: %d instance line(s)", len(r.Instance))
	}
	if r.Verdict() != Converged {
		t.Errorf("platform verdict = %v, want Converged", r.Verdict())
	}
	if r.AppVerdict() != HardFailed {
		t.Errorf("app verdict = %v, want HardFailed — the app scope is the gate this content HAS", r.AppVerdict())
	}

	// A platform Application in the same state still hard-fails the platform.
	var p Report
	p.RouteApp(ArgoApp{Name: "harbor", Project: "platform-support",
		Sync: "Unknown", Health: "Degraded", Automated: true,
		SpecErr: "ComparisonError: authentication required"}, false)
	if p.Verdict() != HardFailed {
		t.Errorf("platform Application verdict = %v, want HardFailed", p.Verdict())
	}
	if p.AppVerdict() != Converged {
		t.Error("a platform failure must not gate the apps scope")
	}
}

// TestPodWorkloadOwner_ReplicaSetConvention pins the one naming convention in
// this file, including the shape check that keeps it from swallowing names it did
// not generate. Argo declares the Deployment; Kubernetes interposes a ReplicaSet
// named <deployment>-<podtemplatehash>. Every other controller IS the declared
// resource and must be returned unchanged.
func TestPodWorkloadOwner_ReplicaSetConvention(t *testing.T) {
	cases := []struct {
		name   string
		owners []OwnerRef
		want   ResourceRef
		ok     bool
	}{
		{"deployment via replicaset",
			[]OwnerRef{{Kind: "ReplicaSet", Name: "dispatch-76b5bb9749", APIVersion: "apps/v1"}},
			deployRef("team-gsap", "dispatch"), true},
		{"hyphenated app name keeps every segment but the hash",
			[]OwnerRef{{Kind: "ReplicaSet", Name: "gs-auto-tracker-8659556754", APIVersion: "apps/v1"}},
			deployRef("team-gsap", "gs-auto-tracker"), true},
		{"statefulset is itself declared",
			[]OwnerRef{{Kind: "StatefulSet", Name: "platform-openbao", APIVersion: "apps/v1"}},
			ResourceRef{Group: "apps", Kind: "StatefulSet", Namespace: "team-gsap", Name: "platform-openbao"}, true},
		{"job carries its own group",
			[]OwnerRef{{Kind: "Job", Name: "harbor-robot-provisioner-29796690", APIVersion: "batch/v1"}},
			ResourceRef{Group: "batch", Kind: "Job", Namespace: "team-gsap", Name: "harbor-robot-provisioner-29796690"}, true},
		// THE FAIL-OPEN THAT WAS: stripping the last segment of ANY hyphenated name
		// resolved a bare ReplicaSet to an unrelated Deployment that might be
		// instance-owned — exempting platform pods. `worker` and `canary` are not
		// pod-template-hash shaped (the alphabet has no vowels).
		{"bare replicaset with a word suffix is itself",
			[]OwnerRef{{Kind: "ReplicaSet", Name: "dispatch-worker", APIVersion: "apps/v1"}},
			ResourceRef{Group: "apps", Kind: "ReplicaSet", Namespace: "team-gsap", Name: "dispatch-worker"}, true},
		{"replicaset with no suffix at all is itself",
			[]OwnerRef{{Kind: "ReplicaSet", Name: "standalone", APIVersion: "apps/v1"}},
			ResourceRef{Group: "apps", Kind: "ReplicaSet", Namespace: "team-gsap", Name: "standalone"}, true},
		// The CONTROLLER ref is the answer, not the first one: ownerReferences carry
		// at most one controller and the API server preserves the writer's order.
		{"a non-controller ref ahead of the controller is ignored",
			[]OwnerRef{
				{Kind: "Cluster", Name: "platform-cnpg", APIVersion: "postgresql.cnpg.io/v1"},
				{Kind: "ReplicaSet", Name: "dispatch-76b5bb9749", APIVersion: "apps/v1", Controller: boolp(true)},
			},
			deployRef("team-gsap", "dispatch"), true},
		{"a half-populated ref is skipped, not obeyed",
			[]OwnerRef{{Kind: "", Name: "orphan"}, {Kind: "ReplicaSet", Name: "dispatch-76b5bb9749", APIVersion: "apps/v1"}},
			deployRef("team-gsap", "dispatch"), true},
		{"a bare pod is claimed by nobody", nil, ResourceRef{}, false},
		{"an entirely empty ref is not an owner",
			[]OwnerRef{{Kind: "ReplicaSet", Name: ""}}, ResourceRef{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := PodWorkloadOwner(c.owners, "team-gsap")
			if ok != c.ok || got != c.want {
				t.Errorf("PodWorkloadOwner = %+v/%v, want %+v/%v", got, ok, c.want, c.ok)
			}
		})
	}
}

// TestRouteOwned_ResolvesThroughController: Argo never declares a pod, and never
// declares the Workflow a CronWorkflow spawns or the Job a CronJob creates — those
// carry generated names that appear in no manifest. They are only reachable
// through the controller that IS declared.
func TestRouteOwned_ResolvesThroughController(t *testing.T) {
	idx := NewOwnershipIndex([]ArgoApp{
		app("dispatch", InstanceCustomProject,
			deployRef("team-gsap", "dispatch"),
			ResourceRef{Group: "argoproj.io", Kind: "CronWorkflow", Namespace: "team-gsap", Name: "nightly-sync"}),
	})

	// A pod of an instance-owned Deployment.
	var pod Report
	cat, _ := idx.RouteOwned(&pod, CatFail,
		[]OwnerRef{{Kind: "ReplicaSet", Name: "dispatch-76b5bb9749", APIVersion: "apps/v1", Controller: boolp(true)}},
		ResourceRef{Namespace: "team-gsap"}, "ImagePullBackOff")
	if cat != CatInstance || len(pod.InstanceFailed) != 1 {
		t.Errorf("pod: cat=%v instanceFailed=%d — a pod of an instance-owned Deployment is instance-owned", cat, len(pod.InstanceFailed))
	}

	// A Workflow the CronWorkflow spawned under a generated name: the name itself
	// is in no .status.resources, so a direct lookup could never match.
	var wf Report
	cat, _ = idx.RouteOwned(&wf, CatFail,
		[]OwnerRef{{Kind: "CronWorkflow", Name: "nightly-sync", APIVersion: "argoproj.io/v1alpha1", Controller: boolp(true)}},
		ResourceRef{Group: "argoproj.io", Kind: "Workflow", Namespace: "team-gsap", Name: "nightly-sync-vrvbr"}, "Failed")
	if cat != CatInstance {
		t.Errorf("workflow: cat=%v — a generated Workflow resolves through its CronWorkflow", cat)
	}

	// Same pod shape, different namespace → not owned. Fails closed.
	var other Report
	cat, _ = idx.RouteOwned(&other, CatFail,
		[]OwnerRef{{Kind: "ReplicaSet", Name: "dispatch-76b5bb9749", APIVersion: "apps/v1", Controller: boolp(true)}},
		ResourceRef{Namespace: "kube-system"}, "ImagePullBackOff")
	if cat != CatFail || len(other.Failed) != 1 {
		t.Errorf("ownership is namespaced — a same-named workload elsewhere is not claimed (cat=%v)", cat)
	}

	// A bare pod belongs to whoever created it and gates.
	var bare Report
	if cat, _ = idx.RouteOwned(&bare, CatFail, nil, ResourceRef{Namespace: "team-gsap"}, "ImagePullBackOff"); cat != CatFail {
		t.Errorf("a bare pod is claimed by no Application and must gate (cat=%v)", cat)
	}
}

// TestZeroIndexOwnsNothing: an index that could not be built must classify
// everything as platform. Fail-closed is the whole safety property here — a
// dropped Application fetch must never silently exempt the cluster.
func TestZeroIndexOwnsNothing(t *testing.T) {
	var idx OwnershipIndex
	if idx.Owns(esRef("team-gsap", "anything")) {
		t.Error("the zero index must own nothing")
	}
	var r Report
	if cat, _ := idx.Route(&r, CatFail, esRef("team-gsap", "anything"), "x"); cat != CatFail {
		t.Error("the zero index must demote nothing — an unbuilt index gates everything")
	}
	if r.Verdict() != HardFailed {
		t.Errorf("verdict = %v, want HardFailed", r.Verdict())
	}
}

// TestAppVerdict_KeepsTheAppLaneGated is the counterweight to the whole change.
// Excluding app content from the platform verdict must not make it unowned: the
// same report still hard-fails the apps scope, and a merely-settling resource
// keeps that scope polling rather than collapsing to "broken".
func TestAppVerdict_KeepsTheAppLaneGated(t *testing.T) {
	idx := NewOwnershipIndex([]ArgoApp{app("dispatch", InstanceCustomProject, esRef("team-gsap", "git-credentials-dispatch"))})

	var r Report
	idx.Route(&r, CatFail, esRef("team-gsap", "git-credentials-dispatch"), "boom")
	if r.Verdict() != Converged {
		t.Errorf("platform verdict = %v, want Converged — instance-owned content must not gate it", r.Verdict())
	}
	if r.AppVerdict() != HardFailed {
		t.Errorf("app verdict = %v, want HardFailed — the app lane keeps its own gate", r.AppVerdict())
	}
	if len(r.Failed) != 0 {
		t.Errorf("nothing should have reached the platform Failed bucket, got %v", r.Failed)
	}

	// Pending is a distinct answer: a caller polling the app lane has to be able to
	// tell "still coming" from "broken", and the demoted severity is the only
	// record of which it was.
	var p Report
	idx.Route(&p, CatPending, esRef("team-gsap", "git-credentials-dispatch"), "not ready")
	if p.AppVerdict() != InProgress {
		t.Errorf("app verdict = %v, want InProgress", p.AppVerdict())
	}

	// A platform resource routes unchanged and still gates.
	var plat Report
	idx.Route(&plat, CatFail, esRef("harbor", "harbor-admin-password"), "boom")
	if plat.Verdict() != HardFailed {
		t.Error("a platform resource must still hard-fail the platform verdict")
	}
	if plat.AppVerdict() != Converged {
		t.Error("a platform failure must not gate the apps scope")
	}
}

// TestRouteInconclusive_NeitherScopeConverges: an unread corpus is not an empty
// one. The platform half always had this — the failed read records a Pending —
// but the app half reads only the demoted severities, and nothing can be demoted
// from a list that never arrived, so it answered "converged" about an estate it
// never examined.
func TestRouteInconclusive_NeitherScopeConverges(t *testing.T) {
	var r Report
	cat, msg := r.RouteInconclusive("Pods")
	if cat != CatPending || msg == "" {
		t.Errorf("RouteInconclusive = %v/%q, want CatPending with an explanation", cat, msg)
	}
	if !r.Inconclusive {
		t.Error("the scan must be marked inconclusive, which is the fact neither verdict can infer afterwards")
	}
	if r.Verdict() != InProgress {
		t.Errorf("platform verdict = %v, want InProgress", r.Verdict())
	}
	if r.AppVerdict() != InProgress {
		t.Errorf("app verdict = %v, want InProgress — a gate must not report success having examined nothing", r.AppVerdict())
	}
	// A hard failure dominates: a scan that saw a broken app AND missed a list is
	// broken, not merely incomplete.
	r.AddInstanceOf(CatFail, "dispatch ImagePullBackOff")
	if r.AppVerdict() != HardFailed {
		t.Errorf("app verdict = %v, want HardFailed", r.AppVerdict())
	}
}

// TestReportRoute_ClusterWideFactsStillNormalizeTheTunnel: the funnel for findings
// with no resource identity (nodes, webhooks, leases) has to keep the konnectivity
// downgrade, or the sections that use it spend hard strikes on a transport blip.
func TestReportRoute_ClusterWideFactsStillNormalizeTheTunnel(t *testing.T) {
	var r Report
	cat, _ := r.Route(CatFail, "APIService v1beta1.metrics unavailable: error dialing backend: No agent available")
	if cat != CatPending || !r.TunnelDown {
		t.Errorf("Route = %v (TunnelDown=%v), want CatPending/true", cat, r.TunnelDown)
	}
	var plain Report
	if cat, _ := plain.Route(CatFail, "node worker-1 NotReady"); cat != CatFail || plain.TunnelDown {
		t.Errorf("an ordinary cluster-wide failure must route unchanged, got %v", cat)
	}
}

// TestAddCatInstanceRemembersNothingToGate: the display-only sink is gone, so the
// one remaining way to record an already-demoted finding cannot silently drop the
// severity — it records a category that gates nothing, explicitly.
func TestAddCatInstanceRemembersNothingToGate(t *testing.T) {
	var r Report
	r.Add(CatInstance, "instance-custom-istio-system: Degraded"+InstanceOwnedSuffix)
	if len(r.Instance) != 1 {
		t.Errorf("the finding must be reported, got %d line(s)", len(r.Instance))
	}
	if len(r.InstanceFailed)+len(r.InstancePending) != 0 {
		t.Error("a caller that never said what it demoted FROM must not invent a gate")
	}
	if r.Verdict() != Converged || r.AppVerdict() != Converged {
		t.Errorf("verdicts = %v/%v, want Converged/Converged", r.Verdict(), r.AppVerdict())
	}
}

// TestOwns_PlatformReservedNamespace: the contested pass only protects what an
// Argo Application declares, and LKE ships cilium, coredns, csi-linode-node and
// konnectivity-agent into kube-system with no Application at all — so an operator
// manifest naming one of them was an uncontested claim on a resource the platform
// cannot live without.
func TestOwns_PlatformReservedNamespace(t *testing.T) {
	idx := NewOwnershipIndex([]ArgoApp{
		app("cni-tweak", InstanceCustomProject,
			ResourceRef{Group: "apps", Kind: "DaemonSet", Namespace: "kube-system", Name: "cilium"}),
	})
	if idx.Owns(ResourceRef{Group: "apps", Kind: "DaemonSet", Namespace: "kube-system", Name: "cilium"}) {
		t.Error("kube-system is the platform's — a claim there must never demote, declared by Argo or not")
	}
}

// TestOwns_PlatformAppThatDeclaresNothingProtectsItsNamespaces: an Application
// Argo has not compared publishes an EMPTY .status.resources, so a PLATFORM app in
// that state defends nothing and an instance claim on its resources would win.
// The instance side already refuses to claim in this state; the platform side has
// to make the same state safe.
func TestOwns_PlatformAppThatDeclaresNothingProtectsItsNamespaces(t *testing.T) {
	ref := deployRef("istio-system", "istiod")
	apps := []ArgoApp{
		app("instance-custom-istio-system", InstanceCustomProject, ref),
		app("istio", "platform-support"), // ComparisonError: declares nothing
	}
	idx := NewOwnershipIndex(apps).WithPlatformNamespaces([]string{"istio-system"})
	if idx.Owns(ref) {
		t.Error("a platform namespace is not demotable while a platform Application's contents are unknown")
	}
	if got := idx.PlatformUnresolved(); len(got) != 1 || got[0] != "istio" {
		t.Errorf("PlatformUnresolved() = %v, want [istio] — the state must be reported, not silently absorbed", got)
	}
	// A TEAM namespace is unaffected: that is where the app estate lives, and
	// disabling the boundary there during any platform hiccup would put app content
	// back on the platform gate — the thing this whole boundary removes.
	team := deployRef("team-gsap", "dispatch")
	idx2 := NewOwnershipIndex([]ArgoApp{
		app("dispatch", InstanceCustomProject, team),
		app("istio", "platform-support"),
	}).WithPlatformNamespaces([]string{"istio-system"})
	if !idx2.Owns(team) {
		t.Error("a team namespace stays demotable — the guard is scoped to the platform's own namespaces")
	}
}

// TestOwns_GeneratedSecondHop: a CronWorkflow's Workflow carries a generated name
// no Application declares, and a POD of that Workflow resolves only as far as the
// generated name — so the Workflow was demoted while its pods gated the platform.
func TestOwns_GeneratedSecondHop(t *testing.T) {
	cron := ResourceRef{Group: "argoproj.io", Kind: "CronWorkflow", Namespace: "team-gsap", Name: "nightly-sync"}
	wf := ResourceRef{Group: "argoproj.io", Kind: "Workflow", Namespace: "team-gsap", Name: "nightly-sync-vrvbr"}
	idx := NewOwnershipIndex([]ArgoApp{app("dispatch", InstanceCustomProject, cron)})

	if idx.Owns(wf) {
		t.Fatal("precondition: the generated Workflow is in no .status.resources")
	}
	idx.RecordGenerated(wf, cron)
	if !idx.Owns(wf) {
		t.Error("a generated child of a declared parent is instance-owned")
	}
	// The link is shared by every copy of the index, which is what lets a later
	// section (checkPods) see what an earlier one (checkWorkflows) learned.
	if !copyOf(idx).Owns(wf) {
		t.Error("the recorded link must survive the value copy the sections are handed")
	}
}

func copyOf(i OwnershipIndex) OwnershipIndex { return i }

// TestOwns_StatefulSetVolumeClaimTemplatePVC: a volumeClaimTemplate PVC carries NO
// ownerReferences under the default Retain policy and appears in no Application's
// declared set under its generated name — the commonest instance-owned PVC shape,
// and it gated the platform.
func TestOwns_StatefulSetVolumeClaimTemplatePVC(t *testing.T) {
	sts := ResourceRef{Group: "apps", Kind: "StatefulSet", Namespace: "team-gsap", Name: "dispatch-db"}
	idx := NewOwnershipIndex([]ArgoApp{app("dispatch", InstanceCustomProject, sts)})

	pvc := func(name string) ResourceRef {
		return ResourceRef{Kind: "PersistentVolumeClaim", Namespace: "team-gsap", Name: name}
	}
	if !idx.Owns(pvc("data-dispatch-db-0")) {
		t.Error("a volumeClaimTemplate PVC resolves to the StatefulSet the Application declares")
	}
	if !idx.Owns(pvc("logs-dispatch-db-11")) {
		t.Error("any template name and any ordinal")
	}
	// FAILS CLOSED on everything that is not that shape.
	for _, n := range []string{"data-other-db-0", "dispatch-db-0", "data-dispatch-db", "data-dispatch-db-x"} {
		if idx.Owns(pvc(n)) {
			t.Errorf("%q is not a volumeClaimTemplate PVC of dispatch-db and must gate", n)
		}
	}
}

// TestOwns_RefWithNoIdentityMatchesNothing: checkPods passes a namespace-only ref
// for a pod with no controller. One malformed .status.resources entry would
// otherwise make every such pod in that namespace instance-owned — fail-open, in
// the index whose whole posture is fail-closed.
func TestOwns_RefWithNoIdentityMatchesNothing(t *testing.T) {
	idx := NewOwnershipIndex([]ArgoApp{app("broken", InstanceCustomProject, ResourceRef{Namespace: "team-gsap"})})
	if idx.Owns(ResourceRef{Namespace: "team-gsap"}) {
		t.Error("a ref with no Kind and no Name identifies nothing and must never match")
	}
}

// TestNamespaces_ReachesTheAppEstate: the per-namespace sections iterate the
// PLATFORM's namespaces, so without this an instance app in a team namespace was
// examined by neither scope — its Service's endpoints in particular, which no
// other check looks at and which Argo calls Healthy unconditionally.
func TestNamespaces_ReachesTheAppEstate(t *testing.T) {
	idx := NewOwnershipIndex([]ArgoApp{
		app("dispatch", InstanceCustomProject, deployRef("team-gsap", "dispatch"), esRef("team-b", "x")),
		app("harbor", "platform-support", esRef("harbor", "admin")), // platform contributes nothing
	})
	got := idx.Namespaces()
	if len(got) != 2 || got[0] != "team-b" || got[1] != "team-gsap" {
		t.Errorf("Namespaces() = %v, want the instance-owned namespaces, sorted", got)
	}
	if len(NewOwnershipIndex(nil).Namespaces()) != 0 {
		t.Error("an empty index adds no namespaces to the scan")
	}
}

// TestStuckResourceRef_MapsTheSweptPlurals: the stuck-finalizer sweep names a
// resource by its kubectl plural, and the index is keyed on (group, Kind). A
// plural with no mapping must fail closed — gate, rather than be demoted on a
// guess.
func TestStuckResourceRef_MapsTheSweptPlurals(t *testing.T) {
	for _, spec := range StuckResourceKinds() {
		plural, _, _ := strings.Cut(spec, "|")
		ref, ok := StuckResourceRef(plural, "team-gsap", "x")
		if !ok {
			t.Errorf("%q is swept but has no (group, Kind) mapping, so the boundary cannot be asked about it", plural)
			continue
		}
		if ref.Kind == "" || ref.Name != "x" || ref.Namespace != "team-gsap" {
			t.Errorf("StuckResourceRef(%q) = %+v", plural, ref)
		}
	}
	if _, ok := StuckResourceRef("widgets.example.com", "ns", "n"); ok {
		t.Error("an unmapped plural must not resolve — a wrong Kind would demote the wrong resource")
	}
	// The group half matters: the index keys on it, and Kind alone is ambiguous.
	if ref, _ := StuckResourceRef("externalsecrets.external-secrets.io", "ns", "n"); ref.Group != "external-secrets.io" {
		t.Errorf("group = %q, want external-secrets.io", ref.Group)
	}
	if ref, _ := StuckResourceRef("pvc", "ns", "n"); ref.Group != "" || ref.Kind != "PersistentVolumeClaim" {
		t.Errorf("core-group plural mapped to %+v", ref)
	}
}

// TestRecordGenerated_IgnoresRefsThatIdentifyNothing keeps the alias map from
// acquiring a key Owns() would have to special-case.
func TestRecordGenerated_IgnoresRefsThatIdentifyNothing(t *testing.T) {
	idx := NewOwnershipIndex([]ArgoApp{app("dispatch", InstanceCustomProject, deployRef("team-gsap", "dispatch"))})
	idx.RecordGenerated(ResourceRef{Namespace: "team-gsap"}, deployRef("team-gsap", "dispatch"))
	if idx.Owns(ResourceRef{Namespace: "team-gsap"}) {
		t.Error("a link from a ref that identifies nothing must not be recorded")
	}
	var zero OwnershipIndex // no map: must not panic
	zero.RecordGenerated(deployRef("a", "b"), deployRef("a", "c"))
}

// TestWorkflowDeclaredOwner covers all four shapes a Workflow reaches a cluster
// in. The third is the one that leaked: `argo submit --from workflowtemplate/X`
// creates an ownerless Workflow with a generated name, so before this the Workflow
// resolved to itself, was claimed by no Application, and hard-failed the PLATFORM
// gate on an app team's build — four of them on the run this boundary comes from.
func TestWorkflowDeclaredOwner(t *testing.T) {
	yes := true
	cron := []OwnerRef{{Kind: "CronWorkflow", Name: "nightly", APIVersion: "argoproj.io/v1alpha1", Controller: &yes}}
	cases := []struct {
		name        string
		owners      []OwnerRef
		templateRef string
		cluster     bool
		labels      map[string]string
		want        ResourceRef
		wantOK      bool
	}{
		{
			name: "a CronWorkflow's Workflow resolves to the CronWorkflow", owners: cron,
			want: ResourceRef{Group: "argoproj.io", Kind: "CronWorkflow", Namespace: "team-gsap", Name: "nightly"}, wantOK: true,
		},
		{
			// The controller ref WINS: a CronWorkflow that itself uses a
			// workflowTemplateRef stamps both, and the CronWorkflow is what the
			// Application declares.
			name: "the controller ref beats a template ref", owners: cron, templateRef: "docker-build-dispatch-main",
			want: ResourceRef{Group: "argoproj.io", Kind: "CronWorkflow", Namespace: "team-gsap", Name: "nightly"}, wantOK: true,
		},
		{
			name: "argo submit --from workflowtemplate resolves to the template", templateRef: "docker-build-dispatch-main",
			want: ResourceRef{Group: "argoproj.io", Kind: "WorkflowTemplate", Namespace: "team-gsap", Name: "docker-build-dispatch-main"}, wantOK: true,
		},
		{
			name:   "the label is the fallback when the spec carries the templates inline",
			labels: map[string]string{WorkflowTemplateLabel: "docker-build-dispatch-main"},
			want:   ResourceRef{Group: "argoproj.io", Kind: "WorkflowTemplate", Namespace: "team-gsap", Name: "docker-build-dispatch-main"}, wantOK: true,
		},
		{
			// Cluster-scoped: Argo reports a cluster-scoped resource in
			// .status.resources with an empty namespace, so the ref must match.
			name: "a ClusterWorkflowTemplate carries no namespace", templateRef: "shared-build", cluster: true,
			want: ResourceRef{Group: "argoproj.io", Kind: "ClusterWorkflowTemplate", Name: "shared-build"}, wantOK: true,
		},
		{
			// A Workflow applied straight from a manifest IS declared under its own
			// name, so the caller routes on that and this must not invent a parent.
			name: "no owner and no template resolves to nothing", wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := WorkflowDeclaredOwner(c.owners, c.templateRef, c.cluster, c.labels, "team-gsap")
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && got != c.want {
				t.Errorf("owner = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestOwns_InstanceNamespaceInference. The scan reaches team namespaces now, so an
// undeclared resource there must not arrive as platform — but the inference is the
// only rule here that is not a direct claim, so it is fenced four ways.
//
// THE THIRD FENCE IS THE ONE THAT NEARLY GOT AWAY. "Platform namespace" cannot mean
// the caller's list: converge's healthNamespaces is nine entries, while apl-core's
// platform runs in monitoring, keycloak, kyverno and cnpg-system, all judged by the
// -A sections. One instance-owned resource in monitoring would have re-labelled the
// whole namespace as app estate — and loki's volumeClaimTemplate PVCs, generated so
// declared by no Application and therefore unprotected by the platformDeclared veto,
// would have dropped out of the platform gate. A namespace a platform Application
// actually has a resource in is the platform's, list or no list.
func TestOwns_InstanceNamespaceInference(t *testing.T) {
	instance := ArgoApp{Name: "dispatch", Project: InstanceCustomProject, Resources: []ResourceRef{
		{Group: "apps", Kind: "Deployment", Namespace: "team-gsap", Name: "dispatch"},
		// The instance reaching INTO a namespace apl-core's platform occupies.
		{Group: "monitoring.coreos.com", Kind: "ServiceMonitor", Namespace: "monitoring", Name: "dispatch"},
	}}
	platform := ArgoApp{Name: "monitoring-loki", Project: "platform", DestNamespace: "monitoring", Resources: []ResourceRef{
		{Group: "apps", Kind: "StatefulSet", Namespace: "monitoring", Name: "loki-ingester"},
	}}
	idx := NewOwnershipIndex([]ArgoApp{instance, platform}).WithPlatformNamespaces([]string{"harbor", "istio-system"})

	if got := idx.InstanceNamespaces(); len(got) != 1 || got[0] != "team-gsap" {
		t.Fatalf("InstanceNamespaces() = %v, want [team-gsap] — monitoring is the platform's, whatever the caller's list says", got)
	}

	undeclared := ResourceRef{Group: "apps", Kind: "Deployment", Namespace: "team-gsap", Name: "some-operand"}
	if !idx.Owns(undeclared) {
		t.Errorf("an undeclared resource in a namespace only instance apps occupy must be instance-owned — otherwise widening the scan hands the platform gate the app estate")
	}
	// loki's volumeClaimTemplate PVC: generated, so no Application declares it and
	// the contested pass cannot defend it. Only the namespace answer protects it.
	if idx.Owns(ResourceRef{Kind: "PersistentVolumeClaim", Namespace: "monitoring", Name: "data-loki-ingester-0"}) {
		t.Errorf("a generated PVC in a namespace a PLATFORM Application occupies must still gate the platform")
	}
	if idx.Owns(ResourceRef{Group: "apps", Kind: "Deployment", Namespace: "harbor", Name: "undeclared"}) {
		t.Errorf("the inference must not reach a namespace the caller named as the platform's")
	}
	if idx.Owns(ResourceRef{Group: "apps", Kind: "Deployment", Namespace: "kube-system", Name: "undeclared"}) {
		t.Errorf("the inference must not reach a reserved namespace")
	}

	// One unresolved platform Application and the inference has nothing to veto
	// with, so it switches off — while the DIRECT claim survives, because that one
	// is evidence.
	blind := NewOwnershipIndex([]ArgoApp{instance, platform, {Name: "mid-compare", Project: "platform"}}).
		WithPlatformNamespaces([]string{"harbor", "istio-system"})
	if blind.Owns(undeclared) {
		t.Errorf("the inference must be off while a platform Application has published nothing")
	}
	if !blind.Owns(ResourceRef{Group: "apps", Kind: "Deployment", Namespace: "team-gsap", Name: "dispatch"}) {
		t.Errorf("a DIRECT claim in a team namespace must survive an unresolved platform app — that app names no destination there")
	}
}

// TestPlatformMayClaim_ScopedToTheDestination. The blanket form meant ONE
// Application mid-comparison — routine while Argo works through the tree during a
// bootstrap — switched the boundary off across every platform namespace, including
// istio-system where the instance's own oauth2-proxy Deployment and ExternalSecrets
// live. The demotion then flipped poll to poll for a reason the report could not
// show. An app that names a destination bounds the veto to it; one that names none
// still bounds nothing.
func TestPlatformMayClaim_ScopedToTheDestination(t *testing.T) {
	instance := ArgoApp{Name: "instance-custom-istio-system", Project: InstanceCustomProject, Resources: []ResourceRef{
		{Group: "apps", Kind: "Deployment", Namespace: "istio-system", Name: "gcp-oauth2-proxy"},
	}}
	oauth := ResourceRef{Group: "apps", Kind: "Deployment", Namespace: "istio-system", Name: "gcp-oauth2-proxy"}
	ns := []string{"harbor", "istio-system", "argocd"}

	bounded := NewOwnershipIndex([]ArgoApp{instance, {Name: "harbor-harbor", Project: "platform", DestNamespace: "harbor"}}).WithPlatformNamespaces(ns)
	if !bounded.Owns(oauth) {
		t.Errorf("an unresolved platform app destined for harbor must not veto istio-system")
	}
	if bounded.Owns(ResourceRef{Group: "apps", Kind: "Deployment", Namespace: "harbor", Name: "anything"}) {
		t.Errorf("it must still veto its OWN destination")
	}

	unbounded := NewOwnershipIndex([]ArgoApp{instance, {Name: "mystery", Project: "platform"}}).WithPlatformNamespaces(ns)
	if unbounded.Owns(oauth) {
		t.Errorf("an unresolved platform app with NO destination bounds nothing, so the veto must still cover every platform namespace")
	}
}

// TestMisprojected names the residual coupling. Ownership keys on
// `.spec.project`, so an instance that generates Applications without setting it
// — a new ApplicationSet left on `default`, a copy-pasted manifest — puts its apps
// straight back on the platform gate and the report reads exactly as it did before
// the boundary existed. Worse, such an Application marks its own namespace
// platformOccupied, switching OFF the namespace inference for the estate around
// it. The noise floor is the whole difficulty: apl-core shares istio-system and
// argocd with the escape hatch by design.
func TestMisprojected(t *testing.T) {
	apps := []ArgoApp{
		// Correctly projected — establishes team-gsap as app estate.
		{Name: "dispatch", Project: InstanceCustomProject, Resources: []ResourceRef{
			{Group: "apps", Kind: "Deployment", Namespace: "team-gsap", Name: "dispatch"}}},
		// The escape hatch reaching into a platform namespace, as designed.
		{Name: "instance-custom-istio-system", Project: InstanceCustomProject, Resources: []ResourceRef{
			{Group: "apps", Kind: "Deployment", Namespace: "istio-system", Name: "gcp-oauth2-proxy"}}},
		// THE BUG: an app in the estate's namespace, on the default project.
		{Name: "dispatch-v2", Project: "default", Resources: []ResourceRef{
			{Group: "apps", Kind: "Deployment", Namespace: "team-gsap", Name: "dispatch-v2"}}},
		// Platform Applications that must stay silent: one sharing istio-system
		// with the hatch, one in a namespace the app estate never touches.
		{Name: "istio-system-oauth2-proxy", Project: "platform", Resources: []ResourceRef{
			{Group: "apps", Kind: "Deployment", Namespace: "istio-system", Name: "platform-oauth2-proxy"}}},
		{Name: "monitoring-loki", Project: "platform", Resources: []ResourceRef{
			{Group: "apps", Kind: "StatefulSet", Namespace: "monitoring", Name: "loki-ingester"}}},
	}
	idx := NewOwnershipIndex(apps).WithPlatformNamespaces([]string{"istio-system", "argocd", "harbor"})

	got := idx.Misprojected()
	if len(got) != 1 || got[0] != "dispatch-v2" {
		t.Fatalf("Misprojected() = %v, want [dispatch-v2] — the platform's own apps share istio-system with the hatch by design and must stay silent", got)
	}
	// And the thing the line explains: dispatch-v2 made team-gsap look occupied by
	// the platform, so the estate's namespace inference is now off around it.
	if idx.Owns(ResourceRef{Group: "apps", Kind: "Deployment", Namespace: "team-gsap", Name: "undeclared-operand"}) {
		t.Errorf("a misprojected Application marks its namespace platformOccupied — the inference must be off there, which is exactly why the report has to name it")
	}
}

// TestMisprojected_QuietWhenTheProjectIsSet is the noise-floor half: with the
// project set on everything, a report must not carry this line at all.
func TestMisprojected_QuietWhenTheProjectIsSet(t *testing.T) {
	apps := []ArgoApp{
		{Name: "dispatch", Project: InstanceCustomProject, Resources: []ResourceRef{
			{Group: "apps", Kind: "Deployment", Namespace: "team-gsap", Name: "dispatch"}}},
		{Name: "dispatch-v2", Project: InstanceCustomProject, Resources: []ResourceRef{
			{Group: "apps", Kind: "Deployment", Namespace: "team-gsap", Name: "dispatch-v2"}}},
		{Name: "monitoring-loki", Project: "platform", Resources: []ResourceRef{
			{Group: "apps", Kind: "StatefulSet", Namespace: "monitoring", Name: "loki-ingester"}}},
	}
	idx := NewOwnershipIndex(apps).WithPlatformNamespaces([]string{"istio-system", "harbor"})
	if got := idx.Misprojected(); len(got) != 0 {
		t.Errorf("Misprojected() = %v, want none", got)
	}
	if !idx.Owns(ResourceRef{Group: "apps", Kind: "Deployment", Namespace: "team-gsap", Name: "undeclared-operand"}) {
		t.Errorf("with nothing misprojected the estate's namespace inference must be on")
	}
}
