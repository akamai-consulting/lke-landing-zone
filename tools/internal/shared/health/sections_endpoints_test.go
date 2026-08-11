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

func TestNoEndpointsAtAllStillFails(t *testing.T) {
	cat, msg := ClassifyServiceEndpoints("x/s", 0, 0, false)
	if cat != CatFail {
		t.Errorf("a Service with nothing backing it is the selector-drift case and must fail, got %v", cat)
	}
	if contains(msg, "still starting") {
		t.Errorf("a Service with no endpoints must not claim pods are starting: %q", msg)
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
