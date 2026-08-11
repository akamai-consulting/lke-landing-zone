package converge

// health_starting_test.go — a workload the cluster is still bringing up must
// PEND, not FAIL.
//
// THE ROUND IT COST. A release-e2e aborted sixty seconds into a 1200-second
// convergence budget with "cluster hard-failed twice in a row — operator
// intervention required". The two polls saw a pod mid-ContainerCreating and two
// Services whose backing pods had not passed their probes yet, on a cluster four
// minutes old whose Applications were still flipping OutOfSync -> Synced. Both
// checks were reading a liveness predicate as a verdict.

import (
	"errors"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
)

func TestCheckPodsPendsAStartingPod(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		if a != "get pods -A -o json" {
			return nil, errors.New("nope")
		}
		return items(
			// Being created right now — the state the aborted run tripped on.
			`{"metadata":{"namespace":"otel","name":"platform-logs-collector-ccq5s"},"status":{"phase":"Pending","containerStatuses":[{"name":"otc-container","ready":false,"state":{"waiting":{"reason":"ContainerCreating"}}}]}}`,
			// Genuinely broken — must still FAIL at full speed, or the softening
			// has removed the gate rather than corrected it.
			`{"metadata":{"namespace":"x","name":"broken"},"status":{"phase":"Pending","containerStatuses":[{"name":"c","ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}`,
		), nil
	})
	// Inside a convergence budget a pod being created pends; outside one it is
	// terminal, because a pod wedged in ContainerCreating by a FailedMount never
	// leaves that state and must not read as "still starting" forever.
	prev := health.Budgeted
	health.Budgeted = true
	defer func() { health.Budgeted = prev }()

	var r health.Report
	checkPods(&r, false)

	if len(r.Failed) != 1 {
		t.Errorf("failed = %v, want only the CrashLoopBackOff pod", r.Failed)
	}
	for _, f := range r.Failed {
		if contains(f, "platform-logs-collector") {
			t.Errorf("a ContainerCreating pod was recorded as a hard failure: %q", f)
		}
	}
	var pending int
	for _, p := range r.Pending {
		if contains(p, "platform-logs-collector") && contains(p, "still starting") {
			pending++
		}
	}
	if pending != 1 {
		t.Errorf("pending = %v, want the ContainerCreating pod recorded as still starting", r.Pending)
	}

	// And the steady-state answer, which is the one that alerts.
	health.Budgeted = false
	var steady health.Report
	checkPods(&steady, false)
	var creatingFailed bool
	for _, f := range steady.Failed {
		if contains(f, "platform-logs-collector") {
			creatingFailed = true
		}
	}
	if !creatingFailed {
		t.Errorf("outside a convergence budget a ContainerCreating pod must FAIL — wedged by a "+
			"FailedMount it never leaves that state, and 'still starting' never alerts. failed = %v",
			steady.Failed)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The Service half of the same round: two Services in llz-observability reported
// "0 ready endpoints (selector drift or all backing pods NotReady)" — a message
// naming both causes while returning the verdict for only one of them. Endpoints
// that EXIST but are not Ready are a rollout, not drift.
func TestCheckServicesPendsAServiceWhoseEndpointsAreNotReadyYet(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		switch a {
		case "-n llz-observability get svc -o json":
			return items(
				`{"metadata":{"namespace":"llz-observability","name":"otel-collector"},"spec":{"type":"ClusterIP","clusterIP":"10.0.0.1"}}`,
				`{"metadata":{"namespace":"llz-observability","name":"orphan"},"spec":{"type":"ClusterIP","clusterIP":"10.0.0.2"}}`,
			), nil
		case "-n llz-observability get endpointslices -l kubernetes.io/service-name=otel-collector -o json":
			// Pods exist, none has passed its probes yet.
			return items(`{"endpoints":[{"conditions":{"ready":false}},{"conditions":{"ready":false}}]}`), nil
		case "-n llz-observability get endpointslices -l kubernetes.io/service-name=orphan -o json":
			// Nothing backs it at all — the real selector-drift case.
			return items(), nil
		}
		return nil, errors.New("nope")
	})
	// Drive it as a convergence poll: inside a budget the deferrable states pend,
	// outside one they are terminal (health.Budgeted — runConverge sets it).
	prev := health.Budgeted
	health.Budgeted = true
	defer func() { health.Budgeted = prev }()

	inv := &clusterInventory{nsExists: map[string]bool{"llz-observability": true}}
	var r health.Report
	checkServices(&r, inv, false)

	for _, f := range r.Failed {
		if contains(f, "otel-collector") {
			t.Errorf("a Service whose pods are still starting was recorded as a hard failure: %q", f)
		}
	}
	var pended bool
	for _, p := range r.Pending {
		if contains(p, "otel-collector") && contains(p, "still starting") {
			pended = true
		}
	}
	if !pended {
		t.Errorf("pending = %v, want otel-collector recorded as backing pods still starting", r.Pending)
	}
	// ...and the Service with NO endpoints is pending too, but under its own
	// words: nothing observable distinguishes "pods have no PodIP yet" from
	// "selector matches nothing", so the convergence budget decides and its
	// exhaustion report names the Service. See
	// TestNoEndpointsAtAllIsPendingAndSaysWhyItCannotTell.
	var orphanPended bool
	for _, p := range r.Pending {
		if contains(p, "orphan") && contains(p, "selector drift") {
			orphanPended = true
		}
	}
	if !orphanPended {
		t.Errorf("pending = %v, want the endpoint-less Service deferred to the budget while naming "+
			"selector drift as a possible cause", r.Pending)
	}
	if len(r.Failed) != 0 {
		t.Errorf("failed = %v, want nothing hard-failed on a young cluster", r.Failed)
	}
}
