package health

// sections_endpoints_test.go — "0 ready endpoints" has two causes and they need
// opposite verdicts.
//
// The old message said so out loud — "selector drift or all backing pods
// NotReady" — and returned CatFail for both. Selector drift is a spec error that
// waiting never fixes; pods that are not Ready YET is what every Service looks
// like mid-rollout. On a four-minute-old cluster that cost a release-e2e round:
// two such Services, sixty seconds apart, tripped converge's hard-failure abort
// while the cluster was still installing.

import "testing"

func TestEndpointsExistButNoneReadyIsPending(t *testing.T) {
	cat, msg := ClassifyServiceEndpoints("llz-observability/otel-collector", 0, 3, false)
	if cat != CatPending {
		t.Errorf("3 endpoints with none Ready is a rollout in progress, got %v (%s)", cat, msg)
	}
	if msg == "" || !contains(msg, "still starting") {
		t.Errorf("the message should say the pods are starting, got %q", msg)
	}
}

// ZERO endpoints is also PENDING, and the reason is that nothing observable
// distinguishes the two causes. A pod only appears in an EndpointSlice once it
// HAS a PodIP, so a Service whose pods are still Pending/ContainerCreating has no
// endpoints AT ALL — identical, from here, to a Service whose selector matches
// nothing. An earlier draft failed this case and would have kept aborting young
// clusters while asserting "selector drift" with more confidence than it had.
//
// The verdict is deferred to the convergence BUDGET, which is only honest
// because the exhaustion report names what was outstanding
// (reportConvergePending) — so a genuinely drifted selector is still reported,
// by name, a few minutes later instead of aborting a cluster that is merely young.
func TestNoEndpointsAtAllIsPendingAndSaysWhyItCannotTell(t *testing.T) {
	// Outside a budget it is terminal — scheduled cluster-health has nothing left
	// to wait with, and reporting a permanently broken Service as "still
	// settling" is how an alert stops firing.
	if cat, msg := ClassifyServiceEndpoints("x/s", 0, 0, false); cat != CatFail {
		t.Errorf("a steady-state check must FAIL an endpoint-less Service, got %v (%s)", cat, msg)
	}
	prev := Budgeted
	Budgeted = true
	defer func() { Budgeted = prev }()

	cat, msg := ClassifyServiceEndpoints("x/s", 0, 0, false)
	if cat != CatPending {
		t.Errorf("a Service with no endpoints cannot be told from one whose pods have no IP yet; "+
			"got %v (%s)", cat, msg)
	}
	if !contains(msg, "selector drift") || !contains(msg, "budget decides") {
		t.Errorf("the message should name BOTH causes and say what resolves them, got %q", msg)
	}
	if contains(msg, "still starting") {
		t.Errorf("with no endpoints at all we cannot claim the pods are starting: %q", msg)
	}
}

func TestReadyEndpointsStillPass(t *testing.T) {
	if cat, _ := ClassifyServiceEndpoints("x/s", 2, 3, false); cat != CatOK {
		t.Errorf("2 ready endpoints must pass, got %v", cat)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
