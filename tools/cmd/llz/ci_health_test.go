package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
)

// withKubectl stubs the execOutput seam to answer kubectl invocations via a
// handler keyed on the joined args; non-kubectl shell-outs error. An unstubbed
// kubectl call returns an error, which the section helpers treat as "empty".
func withKubectl(t *testing.T, h func(args string) ([]byte, error)) {
	t.Helper()
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		return h(strings.Join(args, " "))
	})
}

// items wraps item JSON blobs into a kubectl list response.
func items(blobs ...string) []byte {
	return []byte(`{"items":[` + strings.Join(blobs, ",") + `]}`)
}

func TestKItemsAndKExists(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		switch a {
		case "get pods -o json":
			return items(`{"metadata":{"name":"p"}}`), nil
		case "get crd present":
			return nil, nil
		default:
			return nil, errors.New("nope")
		}
	})
	if n := len(kItems("get", "pods")); n != 1 {
		t.Errorf("kItems = %d, want 1", n)
	}
	if len(kItems("get", "missing")) != 0 {
		t.Error("kItems on an errored call should be empty")
	}
	if !kExists("get", "crd", "present") {
		t.Error("kExists should be true on exit 0")
	}
	if kExists("get", "crd", "absent") {
		t.Error("kExists should be false on error")
	}
}

func TestCheckNodes(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		if a != "get nodes -o json" {
			return nil, errors.New("nope")
		}
		return items(
			`{"metadata":{"name":"good"},"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}]}}`,
			`{"metadata":{"name":"bad"},"spec":{"taints":[{"key":"dedicated","value":"gpu","effect":"NoSchedule"}]},"status":{"conditions":[{"type":"Ready","status":"False"}]}}`,
		), nil
	})
	var r health.Report
	checkNodes(&r)
	// bad node (NotReady) + its unexpected taint => 2 failures.
	if len(r.Failed) != 2 {
		t.Errorf("checkNodes failures = %v, want 2", r.Failed)
	}
}

func TestCheckNamespaces(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		return items(
			`{"metadata":{"name":"ok"},"status":{"phase":"Active"}}`,
			`{"metadata":{"name":"stuck"},"status":{"phase":"Terminating"}}`,
		), nil
	})
	var r health.Report
	checkNamespaces(&r)
	if len(r.Failed) != 1 || !strings.Contains(r.Failed[0], "stuck") {
		t.Errorf("checkNamespaces = %v, want 1 stuck", r.Failed)
	}
}

func TestCheckAPIServices(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		return items(
			`{"metadata":{"name":"v1.ok"},"status":{"conditions":[{"type":"Available","status":"True"}]}}`,
			`{"metadata":{"name":"v1.down"},"status":{"conditions":[{"type":"Available","status":"False","reason":"NoEndpoints","message":"down"}]}}`,
		), nil
	})
	var r health.Report
	checkAPIServices(&r)
	if len(r.Failed) != 1 {
		t.Errorf("checkAPIServices = %v, want 1", r.Failed)
	}
}

func TestCheckRequiredCRDsAndStorageClasses(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		switch {
		case a == "get crd applications.argoproj.io":
			return nil, nil // present
		case strings.HasPrefix(a, "get crd "):
			return nil, errors.New("absent")
		case a == "get storageclass block-storage-retain":
			return nil, nil
		case a == "get storageclass -o json":
			return items(`{"metadata":{"name":"block-storage-retain","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}`), nil
		}
		return nil, errors.New("nope")
	})
	var r health.Report
	checkRequiredCRDs(&r)
	// all but applications.argoproj.io are absent => many fails.
	if len(r.Failed) < 10 {
		t.Errorf("checkRequiredCRDs failures = %d, want most CRDs missing", len(r.Failed))
	}
	var r2 health.Report
	checkStorageClasses(&r2)
	if len(r2.Failed) != 0 {
		t.Errorf("checkStorageClasses with one default + present class should pass, got %v", r2.Failed)
	}
}

func TestCheckArgoApps(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		if a != "-n argocd get applications.argoproj.io -o json" {
			return nil, errors.New("nope")
		}
		return items(
			`{"metadata":{"name":"ok"},"spec":{"syncPolicy":{"automated":{}}},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}}`,
			`{"metadata":{"name":"broken"},"spec":{"syncPolicy":{"automated":{}}},"status":{"sync":{"status":"OutOfSync"},"health":{"status":"Degraded"}}}`,
			`{"metadata":{"name":"external-dns-external-dns"},"spec":{},"status":{"sync":{"status":"Unknown"},"health":{"status":"Healthy"},"conditions":[{"type":"ComparisonError","message":"token not seeded"}]}}`,
		), nil
	})
	var r health.Report
	checkArgoApps(&r, false)
	if len(r.Failed) != 1 || len(r.Deferred) != 1 {
		t.Errorf("checkArgoApps = failed %v deferred %v, want 1 each", r.Failed, r.Deferred)
	}
}

func TestCheckWorkloads(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		switch {
		case a == "get ns openbao":
			return nil, nil
		case strings.HasPrefix(a, "get ns "):
			return nil, errors.New("absent")
		case a == "-n openbao get deploy -o json":
			return items(`{"metadata":{"name":"d"},"spec":{"replicas":2},"status":{"readyReplicas":1}}`), nil
		case a == "-n openbao get sts -o json":
			return items(`{"metadata":{"name":"s"},"spec":{"replicas":1},"status":{"readyReplicas":1}}`), nil
		case a == "-n openbao get ds -o json":
			return items(), nil
		}
		return nil, errors.New("nope")
	})
	var r health.Report
	checkWorkloads(&r, false)
	if len(r.Failed) != 1 {
		t.Errorf("checkWorkloads = %v, want 1 (the 1/2 deploy)", r.Failed)
	}
}

func TestCheckPVCsAndPVs(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		switch a {
		case "get pvc -A -o json":
			return items(
				`{"metadata":{"namespace":"x","name":"good"},"spec":{"storageClassName":"block-storage-retain"},"status":{"phase":"Bound"}}`,
				`{"metadata":{"namespace":"x","name":"bad"},"spec":{"storageClassName":"block-storage-retain"},"status":{"phase":"Pending"}}`,
			), nil
		case "get pv -o json":
			return items(`{"metadata":{"name":"pv1"},"status":{"phase":"Failed"}}`), nil
		}
		return nil, errors.New("nope")
	})
	var r health.Report
	checkPVCs(&r)
	checkPVs(&r)
	if len(r.Failed) != 2 {
		t.Errorf("checkPVCs+PVs = %v, want 2 (Pending PVC + Failed PV)", r.Failed)
	}
}

func TestCheckJobsAndWorkflows(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		switch a {
		case "get jobs -A -o json":
			return items(`{"metadata":{"namespace":"x","name":"j"},"status":{"failed":1,"conditions":[{"type":"Failed","status":"True"}]}}`), nil
		case "get crd workflows.argoproj.io":
			return nil, nil
		case "get workflows.argoproj.io -A -o json":
			return items(`{"metadata":{"namespace":"x","name":"w"},"status":{"phase":"Failed"}}`), nil
		}
		return nil, errors.New("nope")
	})
	var r health.Report
	checkJobs(&r, false)
	checkWorkflows(&r, false)
	if len(r.Failed) != 2 {
		t.Errorf("jobs+workflows = %v, want 2", r.Failed)
	}
}

func TestCheckPDBsAndIngresses(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		switch a {
		case "get pdb -A -o json":
			return items(`{"metadata":{"namespace":"x","name":"p"},"status":{"currentHealthy":1,"desiredHealthy":2,"disruptionsAllowed":0,"expectedPods":2}}`), nil
		case "get ingress -A -o json":
			return items(`{"metadata":{"namespace":"x","name":"i"},"status":{"loadBalancer":{}}}`), nil
		}
		return nil, errors.New("nope")
	})
	var r health.Report
	checkPDBs(&r, false)
	checkIngresses(&r, false)
	if len(r.Failed) != 2 {
		t.Errorf("pdb+ingress = %v, want 2", r.Failed)
	}
}

func TestCheckPods(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		if a != "get pods -A -o json" {
			return nil, errors.New("nope")
		}
		return items(
			`{"metadata":{"namespace":"x","name":"ok"},"status":{"phase":"Running","containerStatuses":[{"name":"c","ready":true}]}}`,
			`{"metadata":{"namespace":"x","name":"bad"},"status":{"phase":"Pending","containerStatuses":[{"name":"c","ready":false,"state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}`,
			`{"metadata":{"namespace":"external-dns","name":"external-dns-1"},"status":{"phase":"Pending"}}`,
			// Ephemeral CronJob pod (owned by a Job) caught mid-ContainerCreating —
			// must be SKIPPED, not counted as a failing workload (the flake we fixed).
			`{"metadata":{"namespace":"argocd","name":"argo-resync-nudger-29706490-n6n7n","ownerReferences":[{"kind":"Job","controller":true}]},"status":{"phase":"Pending","containerStatuses":[{"name":"nudger","ready":false,"state":{"waiting":{"reason":"ContainerCreating"}}}]}}`,
			// harbor-registry stranded on the harbor-registry-s3 ExternalSecret (not yet
			// synced) — CreateContainerConfigError is in-progress (PENDING), not a hard
			// fail, so converge keeps polling instead of aborting in the post-store-Ready window.
			`{"metadata":{"namespace":"harbor","name":"harbor-registry-1"},"status":{"phase":"Pending","containerStatuses":[{"name":"registry","ready":false,"state":{"waiting":{"reason":"CreateContainerConfigError"}}},{"name":"registryctl","ready":true,"state":{"running":{}}}]}}`,
		), nil
	})
	var r health.Report
	checkPods(&r, false)
	// 1 failed (bad) + 1 deferred (external-dns) + 1 pending (harbor-registry config-error);
	// the Job-owned nudger pod is skipped.
	if len(r.Failed) != 1 || len(r.Deferred) != 1 || len(r.Pending) != 1 {
		t.Errorf("checkPods = failed %v deferred %v pending %v, want 1 each (Job pod must be skipped)", r.Failed, r.Deferred, r.Pending)
	}
	if len(r.Pending) == 1 && !strings.Contains(r.Pending[0], "harbor-registry") {
		t.Errorf("checkPods pending = %v, want the harbor-registry config-error pod", r.Pending)
	}
	for _, f := range r.Failed {
		if strings.Contains(f, "nudger") {
			t.Errorf("checkPods flagged an ephemeral Job pod: %q", f)
		}
	}
}

func TestSecretPlaneSettling(t *testing.T) {
	ready := `{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`
	notReady := `{"status":{"conditions":[{"type":"Ready","status":"False","reason":"SecretSyncedError"}]}}`

	// All critical ExternalSecrets Ready → not settling (fail fast resumes).
	withKubectl(t, func(string) ([]byte, error) { return []byte(ready), nil })
	if secretPlaneSettling() {
		t.Error("all-Ready critical secrets must not be settling")
	}

	// harbor-docker-config not Ready (the observed e2e failure) → settling.
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.Contains(a, "harbor-docker-config") {
			return []byte(notReady), nil
		}
		return []byte(ready), nil
	})
	if !secretPlaneSettling() {
		t.Error("an unsynced critical ExternalSecret must count as settling")
	}

	// Absent secret (kubectl errors) must NOT wedge the grace open forever.
	withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("NotFound") })
	if secretPlaneSettling() {
		t.Error("absent critical secrets must not be treated as settling")
	}
}

func TestSecretPresentWithRetry(t *testing.T) {
	prevDelay := phase1ProbeDelay
	phase1ProbeDelay = 0 // no real sleeps in the test
	t.Cleanup(func() { phase1ProbeDelay = prevDelay })

	// Present on the first try → true, no retries needed.
	calls := 0
	withExecOutput(t, func(string, ...string) ([]byte, error) { calls++; return nil, nil })
	if !secretPresentWithRetry("-n", "cert-manager", "get", "secret", "platform-app-ca") || calls != 1 {
		t.Errorf("present-first: got false or calls=%d, want true in 1 call", calls)
	}

	// Transient blip then success → present wins (a one-off error must not read
	// as "absent" / flip phase1). This is fix #3.
	calls = 0
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("transient: connection refused")
		}
		return nil, nil
	})
	if !secretPresentWithRetry("x") || calls != 2 {
		t.Errorf("blip-then-ok: want true after 2 calls, got calls=%d", calls)
	}

	// Genuinely absent → false after exhausting all attempts.
	calls = 0
	withExecOutput(t, func(string, ...string) ([]byte, error) { calls++; return nil, errors.New("NotFound") })
	if secretPresentWithRetry("x") || calls != phase1ProbeRetries {
		t.Errorf("absent: want false after %d calls, got true or calls=%d", phase1ProbeRetries, calls)
	}
}

func TestPhase1OpenBaoBootstrapPending(t *testing.T) {
	prevDelay := phase1ProbeDelay
	phase1ProbeDelay = 0 // no real sleeps in the test
	t.Cleanup(func() { phase1ProbeDelay = prevDelay })

	withKubectl(t, func(a string) ([]byte, error) {
		switch a {
		case "-n cert-manager get secret platform-app-ca":
			return nil, errors.New("NotFound")
		case "get clustersecretstore openbao -o json":
			return []byte(`{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`), nil
		default:
			return nil, fmt.Errorf("unexpected kubectl args %q", a)
		}
	})
	if phase1OpenBaoBootstrapPending() {
		t.Error("openbao ClusterSecretStore Ready should end phase1 even when platform-app-ca is absent")
	}

	withKubectl(t, func(a string) ([]byte, error) {
		switch a {
		case "-n cert-manager get secret platform-app-ca":
			return nil, errors.New("NotFound")
		case "get clustersecretstore openbao -o json":
			return []byte(`{"status":{"conditions":[{"type":"Ready","status":"False"}]}}`), nil
		default:
			return nil, fmt.Errorf("unexpected kubectl args %q", a)
		}
	})
	if !phase1OpenBaoBootstrapPending() {
		t.Error("platform-app-ca absent and openbao ClusterSecretStore not Ready should remain phase1")
	}
}

func TestCheckReadyResources(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		switch a {
		case "get clusterissuers.cert-manager.io -o json":
			return items(`{"metadata":{"name":"platform-app-ca"},"status":{"conditions":[{"type":"Ready","status":"False","reason":"IssuerNotReady"}]}}`), nil
		case "get externalsecrets.external-secrets.io -A -o json":
			return items(`{"metadata":{"namespace":"x","name":"es"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`), nil
		}
		return items(), nil // certs, CRs, CSS empty
	})
	// phase1=false so platform-app-ca isn't excused => a failure.
	var r health.Report
	checkReadyResources(&r, false)
	if len(r.Failed) != 1 {
		t.Errorf("checkReadyResources = %v, want 1 (issuer NotReady, not phase1)", r.Failed)
	}
	// phase1=true => the platform-app-ca issuer is pending, not failed.
	var r2 health.Report
	checkReadyResources(&r2, true)
	if len(r2.Failed) != 0 || len(r2.Pending) != 1 {
		t.Errorf("checkReadyResources phase1 = failed %v pending %v, want 0/1", r2.Failed, r2.Pending)
	}
}

func TestCheckOpenBaoSkips(t *testing.T) {
	// No StatefulSet => skip (warn), not a failure.
	withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("absent") })
	var r health.Report
	checkOpenBao(&r, false)
	if len(r.Failed) != 0 {
		t.Errorf("checkOpenBao with no STS should skip, got failures %v", r.Failed)
	}
}

func TestHealthExitCodePaths(t *testing.T) {
	// Unreachable apiserver => 3 (infrastructure transient, not a hard strike).
	withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("refused") })
	if ec := healthExitCode(); ec != 3 {
		t.Errorf("unreachable => exit %d, want 3", ec)
	}
	// Reachable but Phase 0 (applications CRD missing) => 2.
	withKubectl(t, func(a string) ([]byte, error) {
		if a == "version --request-timeout=10s" {
			return nil, nil
		}
		return nil, errors.New("absent") // CRD/app missing => phase 0
	})
	if ec := healthExitCode(); ec != 2 {
		t.Errorf("phase 0 => exit %d, want 2", ec)
	}
}

func TestRunConvergeUnreachableExhaustsBudget(t *testing.T) {
	// A persistently unreachable apiserver must drain the budget and exit 1 via
	// the unreachable branch — never the twice-in-a-row hard-fail abort. budget=0
	// trips the deadline immediately; retry-delay=0 keeps it from sleeping.
	withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("refused") })
	if ec := runConverge(0, 0, 0); ec != 1 {
		t.Errorf("unreachable + exhausted budget => exit %d, want 1", ec)
	}
}

func TestPrintHealthSummaryAndRecord(t *testing.T) {
	var r health.Report
	record(&r, health.CatOK, "fine")
	record(&r, health.CatFail, "broken")
	record(&r, health.CatDeferred, "later")
	if len(r.Failed) != 1 || len(r.Deferred) != 1 {
		t.Fatalf("record routing wrong: %+v", r)
	}
	printHealthSummary(&r) // exercises the summary formatting (HardFailed branch)
}
