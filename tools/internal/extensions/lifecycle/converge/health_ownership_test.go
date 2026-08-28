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
