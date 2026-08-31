package converge

// health_ownership_test.go — the boundary, driven through the SHIPPED sections
// with an index built the way production builds it.
//
// THE GAP THIS CLOSES. When the ownership boundary first landed, every converge
// test passed `health.OwnershipIndex{}` — the zero index, which owns nothing — so
// the demoting branch of the funnel never ran in any test in the repo. The rules
// were pinned in the health package against a COPY of the routing, which is the
// one thing a coupling test must never do: the copy stayed correct while the
// shipped path was free to break. Every case below starts from Argo Application
// JSON, parses it with the real ParseArgoApp, builds the real index, and drives
// the real check functions.

import (
	"errors"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
)

// instanceApp is one instance-owned Application as Argo reports it, declaring a
// Deployment and a CronWorkflow in a team namespace.
const instanceAppJSON = `{
  "metadata": {"name": "dispatch"},
  "spec": {"project": "instance-custom", "syncPolicy": {"automated": {}}},
  "status": {
    "sync": {"status": "Synced"}, "health": {"status": "Healthy"},
    "resources": [
      {"group": "apps", "kind": "Deployment", "namespace": "team-gsap", "name": "dispatch", "status": "Synced"},
      {"group": "batch", "kind": "Job", "namespace": "team-gsap", "name": "dispatch-migrate", "status": "Synced"},
      {"group": "argoproj.io", "kind": "WorkflowTemplate", "namespace": "team-gsap", "name": "docker-build-dispatch-main", "status": "Synced"}
    ]
  }}`

// platformAppJSON declares a workload in a namespace the platform owns.
const platformAppJSON = `{
  "metadata": {"name": "harbor"},
  "spec": {"project": "platform-support", "syncPolicy": {"automated": {}}},
  "status": {
    "sync": {"status": "Synced"}, "health": {"status": "Healthy"},
    "resources": [
      {"group": "apps", "kind": "Deployment", "namespace": "harbor", "name": "harbor-core", "status": "Synced"}
    ]
  }}`

func ownedIndex(t *testing.T) health.OwnershipIndex {
	t.Helper()
	var apps []health.ArgoApp
	for _, raw := range []string{instanceAppJSON, platformAppJSON} {
		a, err := health.ParseArgoApp([]byte(raw))
		if err != nil {
			t.Fatalf("ParseArgoApp: %v", err)
		}
		apps = append(apps, a)
	}
	return health.NewOwnershipIndex(apps)
}

// TestCheckPodsAppliesTheBoundary: eighteen ImagePullBackOff pods behind unseeded
// per-app PATs are what this boundary was built for, and a pod is only reachable
// through the controller an Application declares.
func TestCheckPodsAppliesTheBoundary(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		if a != "get pods -A -o json" {
			return nil, errors.New("nope")
		}
		return items(
			// An instance-owned app's pod, via ReplicaSet → Deployment/dispatch.
			`{"metadata":{"namespace":"team-gsap","name":"dispatch-76b5bb9749-abcde",
			  "ownerReferences":[{"kind":"ReplicaSet","name":"dispatch-76b5bb9749","apiVersion":"apps/v1","controller":true}]},
			  "status":{"phase":"Pending","containerStatuses":[{"name":"c","ready":false,"state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}`,
			// A PLATFORM pod in the same shape must still gate.
			`{"metadata":{"namespace":"harbor","name":"harbor-core-6d4f8b9c7-xyz12",
			  "ownerReferences":[{"kind":"ReplicaSet","name":"harbor-core-6d4f8b9c7","apiVersion":"apps/v1","controller":true}]},
			  "status":{"phase":"Pending","containerStatuses":[{"name":"c","ready":false,"state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}`,
		), nil
	})

	var r health.Report
	out := captureStdout(t, func() { checkPods(&r, ownedIndex(t), false) })

	if len(r.Failed) != 1 || !strings.Contains(r.Failed[0], "harbor-core") {
		t.Errorf("platform Failed = %v, want only the harbor pod — an app team's pod must not gate the platform", r.Failed)
	}
	if len(r.InstanceFailed) != 1 || !strings.Contains(r.InstanceFailed[0], "dispatch") {
		t.Errorf("InstanceFailed = %v, want the dispatch pod — the app scope is the gate it HAS", r.InstanceFailed)
	}
	if r.AppVerdict() != health.HardFailed {
		t.Errorf("app verdict = %v, want HardFailed", r.AppVerdict())
	}
	if !strings.Contains(out, "INSTANCE") {
		t.Errorf("a demoted finding must still be PRINTED, and as INSTANCE:\n%s", out)
	}
}

// TestCheckJobsAppliesTheBoundary: checkPods deliberately skips Job-controlled
// pods and defers to this section, so a failed per-app migration Job is reachable
// through no other check. Before the boundary reached here it hard-failed the
// platform on an app team's credential — the exact coupling the change removes.
func TestCheckJobsAppliesTheBoundary(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		if a != "get jobs -A -o json" {
			return nil, errors.New("nope")
		}
		return items(
			`{"metadata":{"namespace":"team-gsap","name":"dispatch-migrate","creationTimestamp":"2026-08-01T00:00:00Z"},
			  "status":{"conditions":[{"type":"Failed","status":"True"}],"failed":1}}`,
			`{"metadata":{"namespace":"harbor","name":"harbor-core-init","creationTimestamp":"2026-08-01T00:00:00Z"},
			  "status":{"conditions":[{"type":"Failed","status":"True"}],"failed":1}}`,
		), nil
	})

	var r health.Report
	checkJobs(&r, ownedIndex(t), false)

	if len(r.Failed) != 1 || !strings.Contains(r.Failed[0], "harbor") {
		t.Errorf("platform Failed = %v, want only the harbor Job", r.Failed)
	}
	if len(r.InstanceFailed) != 1 || !strings.Contains(r.InstanceFailed[0], "dispatch-migrate") {
		t.Errorf("InstanceFailed = %v, want the app's migration Job", r.InstanceFailed)
	}
}

// TestBothScopesFillsEveryExitCode: healthResult.appCode's zero value is 0, which
// means Converged, so an early return that sets only `code` reports the app scope
// green for a cluster nobody read — an unreachable apiserver, a cluster that has
// not bootstrapped, a namespace list that failed.
func TestBothScopesFillsEveryExitCode(t *testing.T) {
	for _, code := range []int{2, 3} {
		got := bothScopes(code)
		if got.code != code || got.appCode != code {
			t.Errorf("bothScopes(%d) = code %d / appCode %d — a state neither scope could see past must not read as converged in either",
				code, got.code, got.appCode)
		}
	}
}

// TestGitAuthVetoIsPlatformOnly: r.GitAuthFailure vetoes the phase1 downgrade for
// the WHOLE report and prints an ::error:: naming the platform's values-repo
// credential. Set from an instance-owned Application, an app team's unseeded
// per-app PAT aborts the platform bootstrap and sends the operator to a
// credential that is fine — the coupling this boundary exists to break, arriving
// through a flag instead of through a verdict.
func TestGitAuthVetoIsPlatformOnly(t *testing.T) {
	const gitAuthErr = `"conditions":[{"type":"ComparisonError","message":"failed to list refs: authentication required: Unauthorized"}]`
	cases := []struct {
		name, project string
		wantVeto      bool
	}{
		{"platform Application", "platform-support", true},
		{"instance-owned Application", health.InstanceCustomProject, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withKubectl(t, func(a string) ([]byte, error) {
				if a != "-n argocd get applications.argoproj.io -o json" {
					return nil, errors.New("nope")
				}
				return items(`{"metadata":{"name":"gitops"},"spec":{"project":"` + c.project +
					`","syncPolicy":{"automated":{}}},"status":{"sync":{"status":"Unknown"},"health":{"status":"Healthy"},` + gitAuthErr + `}}`), nil
			})
			var r health.Report
			checkArgoApps(&r, mustArgoApps(t), true, true) // phase1
			if r.GitAuthFailure != c.wantVeto {
				t.Errorf("GitAuthFailure = %v, want %v — the phase1 veto is a PLATFORM signal", r.GitAuthFailure, c.wantVeto)
			}
		})
	}
}

// TestArgoAppFindingsPrintUnderTheirOwnHeader: the fetch runs early because the
// ownership index needs it, but printing from there put every Application verdict
// under whatever section header was last written — ten sections above the empty
// `== ArgoCD Applications ==` block a reader would go looking in.
func TestArgoAppFindingsPrintUnderTheirOwnHeader(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		if a != "-n argocd get applications.argoproj.io -o json" {
			return nil, errors.New("nope")
		}
		return items(`{"metadata":{"name":"harbor"},"spec":{"project":"platform-support","syncPolicy":{"automated":{}}},"status":{"sync":{"status":"Unknown"},"health":{"status":"Degraded"}}}`), nil
	})

	if out := captureStdout(t, func() { fetchArgoApps() }); out != "" {
		t.Errorf("the fetch must report nothing — it runs where its findings would be misfiled:\n%s", out)
	}
	var r health.Report
	out := captureStdout(t, func() { checkArgoApps(&r, mustArgoApps(t), true, false) })
	hdrAt, findingAt := strings.Index(out, "ArgoCD Applications"), strings.Index(out, "harbor")
	if hdrAt < 0 || findingAt < 0 || hdrAt > findingAt {
		t.Errorf("the header must print with, and before, its findings:\n%s", out)
	}
}

// TestUnreadableCorpusIsNotAnAppScopeConvergence: AppVerdict reads the demoted
// severities, and nothing can be demoted if the list never arrived — so an
// apiserver that answers the reachability probe and then throttles the section
// lists made the platform poll and the app scope report converged, over an estate
// it never examined.
func TestUnreadableCorpusIsNotAnAppScopeConvergence(t *testing.T) {
	withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("the connection to the server was refused") })

	var r health.Report
	checkPods(&r, health.OwnershipIndex{}, false)

	if !r.Inconclusive {
		t.Fatal("a list that failed after retries must mark the scan inconclusive")
	}
	if r.Verdict() == health.Converged {
		t.Error("platform verdict must not be Converged for an unreadable corpus")
	}
	if r.AppVerdict() == health.Converged {
		t.Error("app verdict must not be Converged either — the app estate was never examined")
	}
	// A hard failure still dominates: a scan that saw a broken app AND missed a
	// list is broken, not merely incomplete.
	r.AddInstanceOf(health.CatFail, "dispatch ImagePullBackOff")
	if r.AppVerdict() != health.HardFailed {
		t.Errorf("app verdict = %v, want HardFailed", r.AppVerdict())
	}
}

// TestConvergeAppsScopeReadsTheAppVerdictEverywhere is the regression for the
// sharpest self-inflicted bug in this branch. The poll's exit code was scoped and
// the HARD-FAIL RE-CHECK was not, so an apps-scope run whose app content
// hard-failed re-checked the PLATFORM code, found a healthy platform, and returned
// success — the gate reporting green on precisely the state it exists to catch,
// in the ordinary case where the platform is fine and one app is broken.
func TestConvergeAppsScopeReadsTheAppVerdictEverywhere(t *testing.T) {
	// Every poll: platform converged, app content hard-failed.
	calls := withConvergePoll(t,
		healthResult{code: 0, appCode: 1},
		healthResult{code: 0, appCode: 1},
	)
	err := runConverge(3600, 0, 0, ScopeApps)
	if err == nil {
		t.Fatal("runConverge(apps) = nil over a hard-failed app estate — a healthy platform must not clear the app gate")
	}
	if !strings.Contains(err.Error(), "apps scope") {
		t.Errorf("error = %q, want it to name the apps scope rather than blaming the cluster", err)
	}
	if *calls != 2 {
		t.Errorf("health scans = %d, want 2 (poll, hard-fail re-check)", *calls)
	}

	// The mirror: a hard-failed PLATFORM does not fail the apps lane.
	withConvergePoll(t, healthResult{code: 1, appCode: 0})
	if err := runConverge(3600, 0, 0, ScopeApps); err != nil {
		t.Errorf("runConverge(apps) = %v, want nil — a platform failure is not the app lane's verdict", err)
	}
}

// TestConvergeAppsScopeDoesNotRepairThePlatform: the self-heals restart the
// argocd-redis Deployment and strip CRD annotations. Those are platform repairs,
// and an apps-scope run gates an app team's content on the app team's behalf —
// letting it mutate the platform inverts the boundary the scope draws.
func TestConvergeAppsScopeDoesNotRepairThePlatform(t *testing.T) {
	var mutated bool
	withKubectl(t, func(string) ([]byte, error) { mutated = true; return nil, errors.New("no kubectl in this test") })
	// The annotation strip does NOT go through the kubectl fake — deps wires it as
	// its own seam whose package default returns nil. Without stubbing it the
	// half of this test named for it proved nothing: deleting the scope guard from
	// the annotation branch left this test passing, verified by mutation.
	prevStrip := deps.StripOversizedCRDLastApplied
	deps.StripOversizedCRDLastApplied = func() []string { mutated = true; return []string{"crd/x"} }
	defer func() { deps.StripOversizedCRDLastApplied = prevStrip }()

	withConvergePoll(t, healthResult{code: 0, appCode: 0, redisAuthSplit: true, annotationWedge: true})
	if err := runConverge(3600, 0, 0, ScopeApps); err != nil {
		t.Fatalf("runConverge(apps) = %v, want nil", err)
	}
	if mutated {
		t.Error("an apps-scope run reached into the platform to repair it")
	}

	// The control: a PLATFORM run must still repair both, or the guard has removed
	// the self-heal rather than scoped it.
	mutated = false
	withConvergePoll(t, healthResult{code: 0, appCode: 0, annotationWedge: true})
	if err := runConverge(3600, 0, 0, ScopePlatform); err != nil {
		t.Fatalf("runConverge(platform) = %v, want nil", err)
	}
	if !mutated {
		t.Error("a platform-scope run did not strip the oversized CRD annotation — the self-heal is gone, not scoped")
	}
}

// TestScopedOutstandingItems: a red step must lead with the items ITS scope was
// waiting on. The measurement lists were scope-blind in both directions — the
// platform list carried instance findings that never gated it (and, on a cluster
// with 37 of them, could push the one item that DID gate past the report's
// 25-line cap), while an apps-scope run had no list of its own at all.
func TestScopedOutstandingItems(t *testing.T) {
	r := health.Report{
		Pending: []string{"platform: openbao sealed"},
		Failed:  []string{"platform: harbor-core 0/1"},
	}
	r.AddInstanceOf(health.CatFail, "app: dispatch ImagePullBackOff")
	r.AddInstanceOf(health.CatPending, "app: account-health 0/2")

	plat := longPoleCandidates(&r)
	if len(plat) != 2 {
		t.Errorf("platform list = %v, want only the platform's own Pending+Failed", plat)
	}
	for _, m := range plat {
		if strings.HasPrefix(m, "app:") {
			t.Errorf("platform list names %q — content that never held up platform convergence", m)
		}
	}

	apps := appLongPoleCandidates(&r)
	if len(apps) != 2 {
		t.Errorf("app list = %v, want the two demoted findings", apps)
	}
	for _, m := range apps {
		if strings.HasPrefix(m, "platform:") {
			t.Errorf("app list names %q — the app scope does not gate on it", m)
		}
	}
}

// TestCheckWorkflowsFollowsTheWorkflowTemplate is the run this boundary was built
// from, replayed. On akamai/gsap-apl's v0.0.48 promote the Applications, the
// ExternalSecrets, the Deployments and the app pods all demoted correctly — and
// the platform gate stayed red anyway, on four `docker-build-yakpurger-main-*`
// Workflows and a pod of one of them.
//
// A Workflow submitted with `argo submit --from workflowtemplate/<name>` — the
// documented way to run a managed-apps build — carries NO ownerReferences and a
// generated name. The Application declares the WorkflowTemplate; nothing declares
// the Workflow. So the Workflow resolved to itself, missed the index, and gated
// the platform; and its pods, which resolve no further than that same Workflow,
// gated with it. Only the CronWorkflow shape was covered.
func TestCheckWorkflowsFollowsTheWorkflowTemplate(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		switch a {
		case "get workflows.argoproj.io -A -o json":
			return items(
				// The gsap-apl shape: ownerless, generated name, workflowTemplateRef.
				`{"metadata":{"namespace":"team-gsap","name":"docker-build-dispatch-main-fh2sd"},
				  "spec":{"workflowTemplateRef":{"name":"docker-build-dispatch-main"}},
				  "status":{"phase":"Failed"}}`,
				// Same shape, but the template belongs to no instance Application.
				`{"metadata":{"namespace":"harbor","name":"harbor-scan-x1y2z"},
				  "spec":{"workflowTemplateRef":{"name":"harbor-scan"}},
				  "status":{"phase":"Failed"}}`,
			), nil
		case "get pods -A -o json":
			return items(
				// A pod of the demoted Workflow. Argo sets the Workflow as its
				// controller, so it resolves exactly one hop — to a name no
				// Application declares.
				`{"metadata":{"namespace":"team-gsap","name":"docker-build-dispatch-main-fh2sd-git-clone-359592251",
				  "ownerReferences":[{"kind":"Workflow","name":"docker-build-dispatch-main-fh2sd","apiVersion":"argoproj.io/v1alpha1","controller":true}]},
				  "status":{"phase":"Failed","containerStatuses":[{"name":"main","ready":false,"state":{"terminated":{"reason":"Error","exitCode":1}}}]}}`,
			), nil
		}
		return nil, errors.New("nope")
	})

	owned := ownedIndex(t)
	var r health.Report
	inv := &clusterInventory{crds: map[string]bool{"workflows.argoproj.io": true}, nsExists: map[string]bool{}}
	checkWorkflows(&r, inv, owned, false)
	// checkPods runs AFTER checkWorkflows in healthExitCodeState, which is what
	// makes the recorded hop available to it. Order is the contract.
	checkPods(&r, owned, false)

	if len(r.Failed) != 1 || !strings.Contains(r.Failed[0], "harbor-scan") {
		t.Errorf("platform Failed = %v, want only the harbor Workflow — an app team's build must not gate the platform", r.Failed)
	}
	if len(r.InstanceFailed) != 2 {
		t.Fatalf("InstanceFailed = %v, want the app's Workflow AND its pod", r.InstanceFailed)
	}
	if !strings.Contains(r.InstanceFailed[0], "docker-build-dispatch-main-fh2sd") {
		t.Errorf("InstanceFailed[0] = %q, want the ownerless Workflow", r.InstanceFailed[0])
	}
	if !strings.Contains(r.InstanceFailed[1], "git-clone") {
		t.Errorf("InstanceFailed[1] = %q, want the Workflow's pod — it resolves only through the hop checkWorkflows records", r.InstanceFailed[1])
	}
}

// TestScannedNamespacesDoesNotHandThePlatformTheAppEstate. The scan was widened to
// the namespaces instance apps declare into so `--scope=apps` can see Deployments,
// StatefulSets and Services at all. Those namespaces were previously examined by
// nobody — so without the namespace inference, everything in them that no
// Application declares (an operator's operand, a hand-applied manifest, anything a
// controller creates) arrives as PLATFORM and hard-fails the platform gate. That is
// a new way for app content to block a release, created by the change that exists
// to stop app content blocking a release.
func TestScannedNamespacesDoesNotHandThePlatformTheAppEstate(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		switch a {
		case "-n team-gsap get deploy -o json":
			return items(
				// Declared by no Application at all — the case the inference covers.
				`{"metadata":{"namespace":"team-gsap","name":"some-operator-operand"},"spec":{"replicas":1},"status":{"readyReplicas":0}}`,
			), nil
		case "-n harbor get deploy -o json":
			return items(
				// A platform namespace: the inference must not reach it.
				`{"metadata":{"namespace":"harbor","name":"harbor-undeclared"},"spec":{"replicas":1},"status":{"readyReplicas":0}}`,
			), nil
		case "-n team-gsap get sts -o json", "-n team-gsap get ds -o json",
			"-n harbor get sts -o json", "-n harbor get ds -o json":
			return items(), nil
		}
		return nil, errors.New("nope")
	})

	owned := ownedIndex(t).WithPlatformNamespaces([]string{"harbor"})
	if got := owned.InstanceNamespaces(); len(got) != 1 || got[0] != "team-gsap" {
		t.Fatalf("InstanceNamespaces() = %v, want [team-gsap]", got)
	}
	var r health.Report
	inv := &clusterInventory{crds: map[string]bool{}, nsExists: map[string]bool{"team-gsap": true, "harbor": true}}
	checkWorkloadsIn(&r, inv, owned, []string{"team-gsap", "harbor"}, false)

	if len(r.Failed) != 1 || !strings.Contains(r.Failed[0], "harbor-undeclared") {
		t.Errorf("platform Failed = %v, want only the harbor Deployment — an undeclared resource in a PLATFORM namespace still gates", r.Failed)
	}
	if len(r.InstanceFailed) != 1 || !strings.Contains(r.InstanceFailed[0], "some-operator-operand") {
		t.Errorf("InstanceFailed = %v, want the team-gsap Deployment", r.InstanceFailed)
	}
}

// TestScannedNamespacesIsTheAppEstateOnly. The widening exists so `--scope=apps`
// can see a Deployment at all — it must not quietly enlarge what the PLATFORM
// judges. An instance app that declares one ServiceMonitor into monitoring would,
// under `owned.Namespaces()`, pull that whole namespace into the per-namespace
// scan, and apl-core's loki Deployments — platform-owned and gating, never scanned
// here before — would start deciding the platform verdict off the back of one
// instance-owned side-car.
func TestScannedNamespacesIsTheAppEstateOnly(t *testing.T) {
	instance, err := health.ParseArgoApp([]byte(`{
	  "metadata": {"name": "dispatch"},
	  "spec": {"project": "instance-custom"},
	  "status": {"sync": {"status": "Synced"}, "health": {"status": "Healthy"}, "resources": [
	    {"group": "apps", "kind": "Deployment", "namespace": "team-gsap", "name": "dispatch", "status": "Synced"},
	    {"group": "monitoring.coreos.com", "kind": "ServiceMonitor", "namespace": "monitoring", "name": "dispatch", "status": "Synced"}
	  ]}}`))
	if err != nil {
		t.Fatalf("ParseArgoApp: %v", err)
	}
	loki, err := health.ParseArgoApp([]byte(`{
	  "metadata": {"name": "monitoring-loki"},
	  "spec": {"project": "platform", "destination": {"namespace": "monitoring"}},
	  "status": {"sync": {"status": "Synced"}, "health": {"status": "Healthy"}, "resources": [
	    {"group": "apps", "kind": "StatefulSet", "namespace": "monitoring", "name": "loki-ingester", "status": "Synced"}
	  ]}}`))
	if err != nil {
		t.Fatalf("ParseArgoApp: %v", err)
	}
	owned := health.NewOwnershipIndex([]health.ArgoApp{instance, loki}).WithPlatformNamespaces(healthNamespaces)

	var sawTeam, sawMonitoring bool
	for _, ns := range scannedNamespaces(owned) {
		switch ns {
		case "team-gsap":
			sawTeam = true
		case "monitoring":
			sawMonitoring = true
		}
	}
	if !sawTeam {
		t.Errorf("scannedNamespaces omitted team-gsap — the app scope cannot see the app estate without it")
	}
	if sawMonitoring {
		t.Errorf("scannedNamespaces added monitoring: a platform namespace must not enter the per-namespace scan because one instance-owned resource lives there")
	}
}

// ── The compared-but-empty Application ───────────────────────────────────────
//
// THE CORPUS IS THE FAILED RUN. akamai/gsap-apl on LLZ v0.0.49, job
// 99517902800: four platform Applications reported Synced/Healthy with an EMPTY
// .status.resources — apl-core's global/team gitops shells, values repos that
// render no manifests and report that same state on every poll forever — plus
// the escape hatch's instance-custom-istio-system declaring an oauth2-proxy
// Deployment and two ExternalSecrets in istio-system. Reading `len(Resources)
// == 0` as "Argo has not compared this app" put all four in platformUnresolved,
// so platformMayClaim returned true permanently and the per-resource boundary
// that shipped to demote exactly that istio-system content never fired once.
//
// WHY THE GATE IS HERE AND NOT ONLY IN health/ownership_test.go. This is
// docs/e2e-gates.md ②, a split contract: ParseArgoApp reads
// `.status.sync.status` into ArgoApp.Sync (one copy of the rule) and ownership's
// argoCompared decides which of those values means "Argo finished comparing"
// (the other). A test that constructs `ArgoApp{Sync: "Synced"}` by hand restates
// the producer's output instead of exercising it — it stays green if
// ParseArgoApp stops populating Sync, or normalises it, while the shipped
// boundary goes permanently blind again. These drive the REAL fetch into the
// REAL index construction health.go performs.

// emptyGitopsShell is one of apl-core's values-only Applications as Argo
// publishes it: a completed comparison (Synced) declaring nothing, and NO
// destination namespace — the shape that also set platformUnresolvedAnywhere and
// widened the old veto to every platform namespace at once.
func emptyGitopsShell(name string) string {
	return `{"metadata":{"name":"` + name + `"},"spec":{"project":"default","syncPolicy":{"automated":{}}},` +
		`"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}}`
}

// uncomparedGitopsShell is the same Application before Argo has diffed it — the
// state a ComparisonError also leaves it in, and argo.go's own worked example
// (`gitops-global (Unknown/Healthy) — ComparisonError: failed to list refs`).
func uncomparedGitopsShell(name string) string {
	return `{"metadata":{"name":"` + name + `"},"spec":{"project":"default","syncPolicy":{"automated":{}}},` +
		`"status":{"sync":{"status":"Unknown"},"health":{"status":"Healthy"}}}`
}

// hatchIstioSystemJSON is the escape hatch's Application, declaring the
// istio-system resources that hard-failed the platform gate in the failed run.
const hatchIstioSystemJSON = `{"metadata":{"name":"instance-custom-istio-system"},` +
	`"spec":{"project":"instance-custom","syncPolicy":{"automated":{}},"destination":{"namespace":"istio-system"}},` +
	`"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"},"resources":[` +
	`{"group":"apps","kind":"Deployment","namespace":"istio-system","name":"gcp-oauth2-proxy","status":"Synced"},` +
	`{"group":"external-secrets.io","kind":"ExternalSecret","namespace":"istio-system","name":"gcp-oauth2-client","status":"Synced"},` +
	`{"group":"external-secrets.io","kind":"ExternalSecret","namespace":"istio-system","name":"gcp-oauth2-cookie","status":"Synced"}` +
	`]}}`

// boundaryIndex runs converge's REAL Application fetch against a stubbed kubectl
// and builds the index exactly as runHealthChecks does
// (NewOwnershipIndex(...).WithPlatformNamespaces(healthNamespaces)), so the gate
// cannot drift from the shipped wiring by construction.
func boundaryIndex(t *testing.T, apps ...string) health.OwnershipIndex {
	t.Helper()
	withKubectl(t, func(a string) ([]byte, error) {
		if a != "-n argocd get applications.argoproj.io -o json" {
			return nil, errors.New("unstubbed")
		}
		return items(apps...), nil
	})
	return health.NewOwnershipIndex(mustArgoApps(t)).WithPlatformNamespaces(healthNamespaces)
}

// oauthProxyRef / oauthSecretRef are two of the four instance-custom findings the
// failed run hard-failed the platform on.
var (
	oauthProxyRef  = health.ResourceRef{Group: "apps", Kind: "Deployment", Namespace: "istio-system", Name: "gcp-oauth2-proxy"}
	oauthSecretRef = health.ResourceRef{Group: "external-secrets.io", Kind: "ExternalSecret", Namespace: "istio-system", Name: "gcp-oauth2-client"}
)

// TestBoundary_ComparedEmptyPlatformAppDoesNotVetoTheInstanceEstate fails if the
// compared/uncompared discriminator stops being applied — and its inverse arm
// fails if the guard is simply deleted, so the fix cannot pass by removing it.
func TestBoundary_ComparedEmptyPlatformAppDoesNotVetoTheInstanceEstate(t *testing.T) {
	shells := []string{
		emptyGitopsShell("gitops-global"),
		emptyGitopsShell("istio-system-istio-artifacts"),
		emptyGitopsShell("team-admin-values-gitops"),
		emptyGitopsShell("team-platform-values-gitops"),
	}
	idx := boundaryIndex(t, append([]string{hatchIstioSystemJSON}, shells...)...)

	// FAIL CLOSED ON VACUITY. "Not owned" is this index's default answer, so a
	// fixture that never parsed — a renamed JSON field, a fetch returning nothing
	// — would make every demotion assertion below pass having examined nothing.
	// Prove the instance claim actually arrived through ParseArgoApp first.
	if got := idx.Namespaces(); len(got) != 1 || got[0] != "istio-system" {
		t.Fatalf("the escape hatch's .status.resources never reached the index: Namespaces() = %v, want [istio-system] — every assertion below would otherwise pass on an empty corpus", got)
	}

	if got := idx.PlatformUnresolved(); len(got) != 0 {
		t.Errorf("PlatformUnresolved() = %v, want none — all four are Synced, and an empty .status.resources on a COMPARED Application is the answer ('I own nothing'), not missing evidence", got)
	}
	for _, ref := range []health.ResourceRef{oauthProxyRef, oauthSecretRef} {
		if !idx.Owns(ref) {
			t.Errorf("%s/%s in %s did not demote — this is the gsap-apl regression: four permanently Synced/empty gitops shells vetoed every platform namespace on every poll, so the per-resource boundary never fired once",
				ref.Kind, ref.Name, ref.Namespace)
		}
	}

	// THE INVERSE. The same four apps mid-comparison publish the same empty
	// .status.resources and must STILL veto: zero resources is then genuinely
	// missing evidence, and any of them could own this Deployment. None names a
	// destination, so this also holds the platformUnresolvedAnywhere arm.
	uncompared := []string{
		uncomparedGitopsShell("gitops-global"),
		uncomparedGitopsShell("istio-system-istio-artifacts"),
		uncomparedGitopsShell("team-admin-values-gitops"),
		uncomparedGitopsShell("team-platform-values-gitops"),
	}
	blind := boundaryIndex(t, append([]string{hatchIstioSystemJSON}, uncompared...)...)
	if got := blind.PlatformUnresolved(); len(got) != 4 {
		t.Errorf("PlatformUnresolved() = %v, want all four — an Application Argo has NOT compared must still be named and must still veto", got)
	}
	for _, ref := range []health.ResourceRef{oauthProxyRef, oauthSecretRef} {
		if blind.Owns(ref) {
			t.Errorf("%s/%s demoted while a platform Application is UNCOMPARED and names no destination — it has published nothing, so it could own this resource and the instance claim would win uncontested",
				ref.Kind, ref.Name)
		}
	}

	// THE CONTESTED PASS is one `continue` away from the arm being changed, and
	// is the invariant most likely to break with it: a resource a PLATFORM
	// Application actually declares stays platform, whatever else is compared.
	contested := boundaryIndex(t, hatchIstioSystemJSON, emptyGitopsShell("gitops-global"),
		`{"metadata":{"name":"istio"},"spec":{"project":"default","syncPolicy":{"automated":{}}},`+
			`"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"},"resources":[`+
			`{"group":"apps","kind":"Deployment","namespace":"istio-system","name":"gcp-oauth2-proxy","status":"Synced"}]}}`)
	if contested.Owns(oauthProxyRef) {
		t.Error("a resource a PLATFORM Application declares must stay platform — relaxing the empty-app veto must not relax the contested pass with it")
	}
	if contested.Contested() != 1 {
		t.Errorf("Contested() = %d, want 1 — the refused claim must still be reported", contested.Contested())
	}
}

// TestBoundary_ParseArgoAppFeedsTheComparisonDiscriminator closes the split
// contract directly: the value ParseArgoApp extracts from the Application JSON
// must be the value the boundary keys on. Without this, a change that stopped
// populating ArgoApp.Sync — or normalised it to a different vocabulary — would
// send every platform Application back to "uncompared" and switch the boundary
// permanently off again, while every hand-built unit test stayed green.
func TestBoundary_ParseArgoAppFeedsTheComparisonDiscriminator(t *testing.T) {
	// Every value Argo's SyncStatusCode defines, plus the state a fetch that
	// could not read the field produces. `demotes` is what the boundary must
	// conclude for a platform Application in that state that declares nothing.
	cases := []struct {
		name    string
		blob    string
		want    string
		demotes bool
		why     string
	}{
		{"Synced", `"sync":{"status":"Synced"},`, "Synced", true, "a completed comparison that found nothing to own"},
		{"OutOfSync", `"sync":{"status":"OutOfSync"},`, "OutOfSync", true, "also a completed comparison — Argo diffed and published what it found"},
		{"Unknown", `"sync":{"status":"Unknown"},`, "Unknown", false, "Argo has not compared it, so zero resources is missing evidence"},
		{"absent", ``, "", false, "no .status.sync at all — fail closed on a value the predicate cannot recognise"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shell := `{"metadata":{"name":"gitops-global"},"spec":{"project":"default","syncPolicy":{"automated":{}}},` +
				`"status":{` + tc.blob + `"health":{"status":"Healthy"}}}`

			// The producer half on its own, so a failure names which side broke
			// rather than only that the two disagree.
			withKubectl(t, func(a string) ([]byte, error) {
				if a != "-n argocd get applications.argoproj.io -o json" {
					return nil, errors.New("unstubbed")
				}
				return items(shell), nil
			})
			parsed := mustArgoApps(t)
			if len(parsed) != 1 {
				t.Fatalf("fixture parsed to %d Applications, want 1", len(parsed))
			}
			if parsed[0].Sync != tc.want {
				t.Fatalf("ParseArgoApp put Sync=%q in ArgoApp, want %q — the boundary keys on this field, so the producer changing it silently changes the boundary", parsed[0].Sync, tc.want)
			}

			// The consumer half, over that same real output.
			idx := boundaryIndex(t, hatchIstioSystemJSON, shell)
			if got := idx.Namespaces(); len(got) != 1 {
				t.Fatalf("vacuity: the instance claim never indexed (Namespaces() = %v)", got)
			}
			if got := idx.Owns(oauthProxyRef); got != tc.demotes {
				t.Errorf("Owns(%s/%s) = %v with a platform Application at sync=%q, want %v — %s",
					oauthProxyRef.Namespace, oauthProxyRef.Name, got, tc.want, tc.demotes, tc.why)
			}
			// The report line and the veto must agree: an app is named in
			// PlatformUnresolved exactly when it is the reason nothing demoted.
			if named := len(idx.PlatformUnresolved()) > 0; named == tc.demotes {
				t.Errorf("PlatformUnresolved() = %v at sync=%q while demotion=%v — the report must name an Application exactly when it is vetoing",
					idx.PlatformUnresolved(), tc.want, tc.demotes)
			}
		})
	}
}

// TestBoundary_ReportNamesOnlyUncomparedApplications is the reporting half, and
// the instance side's equivalent of the gate above. Both boundary lines tell the
// reader the state is transient ("on this poll"), and that is only true of an
// Application Argo has not compared yet — one that really does resolve on a
// later poll. A permanently Synced/empty shell listed there sends the reader to
// wait for something that will never change; a compared instance app listed
// under "THEIR content still gates the platform" sends them looking for content
// that does not exist.
func TestBoundary_ReportNamesOnlyUncomparedApplications(t *testing.T) {
	idx := boundaryIndex(t,
		hatchIstioSystemJSON,
		emptyGitopsShell("gitops-global"),                     // compared, owns nothing → silent
		uncomparedGitopsShell("istio-system-istio-artifacts"), // not compared → named, vetoing
		// An instance-owned Application Argo HAS compared that declares nothing.
		`{"metadata":{"name":"account-health-values"},"spec":{"project":"instance-custom","syncPolicy":{"automated":{}}},`+
			`"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}}`,
		// One Argo has NOT compared: this one is transient and its content is
		// genuinely gating meanwhile.
		`{"metadata":{"name":"dispatch"},"spec":{"project":"instance-custom","syncPolicy":{"automated":{}}},`+
			`"status":{"sync":{"status":"Unknown"},"health":{"status":"Healthy"}}}`,
	)

	if got := idx.PlatformUnresolved(); len(got) != 1 || got[0] != "istio-system-istio-artifacts" {
		t.Errorf("PlatformUnresolved() = %v, want [istio-system-istio-artifacts] only — gitops-global is Synced and waiting on nothing, so the line's 'on this poll' would be a lie about it", got)
	}
	if got := idx.Unresolved(); len(got) != 1 || got[0] != "dispatch" {
		t.Errorf("Unresolved() = %v, want [dispatch] only — a COMPARED instance Application that declares nothing owns nothing, so 'THEIR content still gates the platform' points the reader at content that does not exist", got)
	}

	// Asserted on what printHealthSummary actually PRINTS, not on the accessors
	// twice: the report is the only place an operator meets this state, and the
	// two lines are the reason the accessors have to be precise.
	out := captureStdout(t, func() { printHealthSummary(&health.Report{}, idx) })
	if !strings.Contains(out, "istio-system-istio-artifacts") || !strings.Contains(out, "dispatch") {
		t.Errorf("the boundary lines did not name the uncompared Applications:\n%s", out)
	}
	if strings.Contains(out, "gitops-global") || strings.Contains(out, "account-health-values") {
		t.Errorf("the report named a COMPARED Application that owns nothing — nothing is waiting on it and nothing of its is gating:\n%s", out)
	}
	// The lines must name the CONDITION, because that is what makes their
	// transience claim true. Guards the wording against a rewrite that
	// re-conflates the two states in prose while the code keeps them apart.
	for _, want := range []string{"have not been compared by Argo yet", "Argo has not compared them"} {
		if !strings.Contains(out, want) {
			t.Errorf("boundary line no longer says %q — it must name the condition (not compared), not just 'declares no resources':\n%s", want, out)
		}
	}
}
