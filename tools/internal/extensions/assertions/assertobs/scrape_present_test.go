package assertobs

// scrape_present_test.go — the evidence dump must actually discriminate between
// the three causes the failure line lists.
//
// A dump that prints something for every input is not evidence; it is noise with
// a heading. Each test below is one of the three causes, and asserts the reader
// can tell it from the other two.

import (
	"strings"
	"testing"
)

const certManagerSM = `{"spec":{
  "namespaceSelector":{"matchNames":["cert-manager"]},
  "selector":{"matchLabels":{"app.kubernetes.io/name":"cert-manager","app.kubernetes.io/component":"controller"}},
  "endpoints":[{"port":"tcp-prometheus-servicemonitor"}]}}`

// fakeKubectl answers by the resource being asked for.
func fakeKubectl(sm, svc string, err error) func(...string) (string, error) {
	return func(args ...string) (string, error) {
		if err != nil {
			return "", err
		}
		for _, a := range args {
			if a == "servicemonitor" {
				return sm, nil
			}
		}
		return svc, nil
	}
}

// CAUSE 1: the port name. The labels match, so the reader must be pointed at the
// ports — this is the cause the original message never mentions, and the one that
// actually bit on a release e2e.
func TestExplainStarsTheServiceWhoseLabelsMatchSoThePortIsTheMiss(t *testing.T) {
	svc := `{"items":[{"metadata":{"name":"cert-manager","namespace":"cert-manager",
	  "labels":{"app.kubernetes.io/name":"cert-manager","app.kubernetes.io/component":"controller"}},
	  "spec":{"ports":[{"name":"tcp-prometheus","port":9402}]}}]}`
	var b strings.Builder
	ExplainNoScrapeTarget(fakeKubectl(certManagerSM, svc, nil), "llz-observability/cert-manager", &b)
	got := b.String()

	if !strings.Contains(got, "* cert-manager/cert-manager") {
		t.Errorf("the label-matching Service is not starred, so the reader cannot see the port is the miss:\n%s", got)
	}
	// Both port names must be visible for the reader to diff them.
	if !strings.Contains(got, "tcp-prometheus-servicemonitor") || !strings.Contains(got, "tcp-prometheus:9402") {
		t.Errorf("wanted and actual port names are not both shown:\n%s", got)
	}
}

// CAUSE 2: the labels. Nothing is starred, so the miss is on the selector side.
func TestExplainStarsNothingWhenNoServiceMatchesTheLabels(t *testing.T) {
	svc := `{"items":[{"metadata":{"name":"cert-manager","namespace":"cert-manager",
	  "labels":{"app":"cert-manager"}},
	  "spec":{"ports":[{"name":"tcp-prometheus-servicemonitor","port":9402}]}}]}`
	var b strings.Builder
	ExplainNoScrapeTarget(fakeKubectl(certManagerSM, svc, nil), "llz-observability/cert-manager", &b)
	got := b.String()

	if strings.Contains(got, "* cert-manager/cert-manager") {
		t.Errorf("a Service whose labels do NOT match was starred:\n%s", got)
	}
	// The wanted labels and the actual labels both have to be on the page.
	if !strings.Contains(got, "app.kubernetes.io/name=cert-manager") || !strings.Contains(got, "app=cert-manager") {
		t.Errorf("wanted and actual labels are not both shown:\n%s", got)
	}
}

// CAUSE 3: the namespace. An empty namespace must say so in words — an empty list
// under a heading reads as "Services that do not match", which is a different
// repair.
func TestExplainSaysWhenTheNamespaceHasNoServicesAtAll(t *testing.T) {
	var b strings.Builder
	ExplainNoScrapeTarget(fakeKubectl(certManagerSM, `{"items":[]}`, nil), "llz-observability/cert-manager", &b)
	if got := b.String(); !strings.Contains(got, "NO Services at all") {
		t.Errorf("an empty namespace is not distinguished from a non-matching one:\n%s", got)
	}
}

// IT MUST LOOK WHERE THE namespaceSelector POINTS, not in the ServiceMonitor's
// own namespace. Getting that backwards is one of the three causes, so the dump
// cannot quietly assume it away — it would then "prove" the namespace is empty
// while the Services sit next door.
func TestExplainReadsTheNamespaceSelectorNotItsOwnNamespace(t *testing.T) {
	var asked []string
	kubectl := func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		asked = append(asked, joined)
		if strings.Contains(joined, "servicemonitor") {
			return certManagerSM, nil
		}
		return `{"items":[]}`, nil
	}
	var b strings.Builder
	ExplainNoScrapeTarget(kubectl, "llz-observability/cert-manager", &b)

	var listed string
	for _, a := range asked {
		if strings.Contains(a, "get svc") {
			listed = a
		}
	}
	if !strings.Contains(listed, "-n cert-manager") {
		t.Errorf("Services were listed with %q; the namespaceSelector says cert-manager, "+
			"not the monitor's own llz-observability", listed)
	}
	if !strings.Contains(b.String(), "cert-manager") {
		t.Errorf("the scope line does not name the selected namespace:\n%s", b.String())
	}
}

// ADVISORY, NEVER FATAL. The verdict is already decided by the caller; a cluster
// that will not answer must not escalate a precise assertion failure into a
// vaguer diagnostic one.
func TestExplainDegradesWhenKubectlFails(t *testing.T) {
	var b strings.Builder
	ExplainNoScrapeTarget(fakeKubectl("", "", errUnreachable{}), "llz-observability/cert-manager", &b)
	if got := b.String(); !strings.Contains(got, "could not read") {
		t.Errorf("an unreachable cluster must say so and stop, got:\n%s", got)
	}
}

type errUnreachable struct{}

func (errUnreachable) Error() string { return "connection refused" }

// A malformed monitor name is not this function's error to raise — it prints
// nothing rather than guessing a namespace.
func TestExplainIgnoresAMalformedMonitorName(t *testing.T) {
	var b strings.Builder
	ExplainNoScrapeTarget(fakeKubectl(certManagerSM, `{"items":[]}`, nil), "no-slash-here", &b)
	if b.Len() != 0 {
		t.Errorf("expected no output for a malformed monitor name, got:\n%s", b.String())
	}
}

// matchLabels semantics: a Service carrying EXTRA labels still matches, and an
// empty selector matches everything (Kubernetes' own rule). Getting either wrong
// would star the wrong rows and send the reader at the wrong cause.
func TestMatchesSelectorFollowsMatchLabelsSemantics(t *testing.T) {
	want := map[string]string{"a": "1", "b": "2"}
	for _, tc := range []struct {
		name string
		have map[string]string
		want bool
	}{
		{"exact", map[string]string{"a": "1", "b": "2"}, true},
		{"superset", map[string]string{"a": "1", "b": "2", "c": "3"}, true},
		{"missing one", map[string]string{"a": "1"}, false},
		{"wrong value", map[string]string{"a": "1", "b": "9"}, false},
		{"empty labels", map[string]string{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesSelector(tc.have, want); got != tc.want {
				t.Errorf("matchesSelector(%v, %v) = %t, want %t", tc.have, want, got, tc.want)
			}
		})
	}
	if !matchesSelector(map[string]string{"x": "y"}, nil) {
		t.Error("an empty selector must match everything, as Kubernetes does")
	}
}
