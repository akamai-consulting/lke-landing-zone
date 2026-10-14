package health

// The webhook-race matcher's test followed it out of internal/extensions/kyverno.
// What it pins is the reason the matcher exists at all: a Kyverno webhook that is
// not up yet fails an apply with a message that looks exactly like a policy
// REJECTION, and treating a race as a rejection turns a retry into a hard stop.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

func TestIsKyvernoWebhookRace(t *testing.T) {
	races := []string{
		`Error from server (InternalError): failed calling webhook "mutate-policy.kyverno.svc"`,
		`dial tcp 10.0.0.1:443: connect: operation not permitted`,
		`connection refused`,
		`no endpoints available for service "kyverno-svc"`,
	}
	for _, s := range races {
		if !IsWebhookRace(s) {
			t.Errorf("should classify as race: %q", s)
		}
	}
	notRace := []string{
		`error validating "p.yaml": ClusterPolicy in version "v1" cannot be handled`,
		`the server could not find the requested resource`,
		``,
	}
	for _, s := range notRace {
		if IsWebhookRace(s) {
			t.Errorf("should NOT classify as race: %q", s)
		}
	}
}

// ArgoComparisonError and LokiConfigText arrived with no tests at all, from two
// different extensions. Both are cluster reads whose FAILURE mode is an empty
// string, which is the same value a healthy cluster returns — so a broken read
// and a clean bill of health are indistinguishable to the caller unless the
// distinction is deliberate. These pin that it is.
func TestArgoComparisonErrorReturnsEmptyWhenKubectlFails(t *testing.T) {
	var gotArgs []string
	d := cigate.Deps{Kubectl: func(args ...string) (string, bool) {
		gotArgs = args
		return "", false
	}}
	if got := ArgoComparisonError(d, "argocd", "platform-openbao"); got != "" {
		t.Errorf("failed kubectl = %q, want empty", got)
	}
	// The query must ask for the ComparisonError condition specifically: a bare
	// status read would return the whole object and never match.
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"argocd", "platform-openbao", "ComparisonError"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q missing %q", joined, want)
		}
	}
}

func TestArgoComparisonErrorTrimsTheMessage(t *testing.T) {
	d := cigate.Deps{Kubectl: func(...string) (string, bool) {
		return "  Failed to compare desired state\n", true
	}}
	// Trimmed because callers compare it against "" to decide whether there IS an
	// error; a lone newline would read as one.
	if got := ArgoComparisonError(d, "argocd", "app"); got != "Failed to compare desired state" {
		t.Errorf("ArgoComparisonError = %q, want it trimmed", got)
	}
}

func TestLokiConfigTextConcatenatesOnlyMatchingConfigMaps(t *testing.T) {
	// Stubbed at kubectlprobe.Exec: Items has no seam of its own, it bottoms out
	// in Probe, which uses this. Feeding real `kubectl get -o json` bytes also
	// exercises the decode rather than skipping past it.
	orig := kubectlprobe.Exec
	t.Cleanup(func() { kubectlprobe.Exec = orig })
	kubectlprobe.Exec = func(string, ...string) ([]byte, error) {
		return []byte(`{"items":[
			{"metadata":{"name":"loki-config"},"data":{"a":"chunk_store: s3"}},
			{"metadata":{"name":"unrelated"},"data":{"a":"NOPE"}}
		]}`), nil
	}
	got := LokiConfigText("^loki-")
	if !strings.Contains(got, "chunk_store: s3") {
		t.Errorf("matching ConfigMap missing from %q", got)
	}
	// A ConfigMap whose name does not match must not leak in: this text is fed to
	// an assertion about how Loki is configured, and one unrelated ConfigMap
	// mentioning s3 would make the check pass for the wrong reason.
	if strings.Contains(got, "NOPE") {
		t.Errorf("non-matching ConfigMap leaked into %q", got)
	}
}
