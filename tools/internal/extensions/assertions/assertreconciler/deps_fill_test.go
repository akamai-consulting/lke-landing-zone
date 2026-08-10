package assertreconciler

// deps_fill_test.go — an Install that omits a field must not leave it nil.
//
// THE DEFECT THIS PINS reached e2e. Install used to be `deps = d`, so every field
// a caller's struct literal omitted became Go's zero value rather than the
// fail-closed default declared beside it. internal/cli omitted WithPrometheus, so
// `llz ci assert-reconciler` called a nil func and died with a SIGSEGV — the
// scrape-reconciler lane exited rc=2 with a stack trace instead of a verdict.
//
// The nil crash was the LUCKY outcome. The default it should have fallen back to
// was `return nil` — a no-op that queries nothing, leaving the probe zero-valued.
// And a zero-valued reconcilerProbe reports healthy(): every failWhy is empty and
// staleLanes() is empty. So the "safe" fallback would have passed the lane having
// asked Prometheus nothing at all. Both halves are fixed: the default errors, and
// Install fills rather than replaces.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestInstallFillsOmittedSeams(t *testing.T) {
	orig := deps
	t.Cleanup(func() { deps = orig })

	// The exact shape internal/cli shipped: WithPrometheus omitted.
	Install(Deps{
		Cluster:               capability.For(extension.Binding{}).Cluster,
		FirewallConfigMapName: "x",
	})

	if deps.WithPrometheus == nil {
		t.Fatal("Install left WithPrometheus nil — calling it is a SIGSEGV, which is how " +
			"the scrape-reconciler lane died mid-e2e")
	}
	err := deps.WithPrometheus("ns/svc:9090", func(func(string) ([]byte, error)) error { return nil })
	if err == nil {
		t.Fatal("the filled default must FAIL CLOSED. Returning nil leaves the probe zero-valued, " +
			"and a zero-valued reconcilerProbe reports healthy() — the lane would go green having " +
			"queried nothing")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("the error should name the un-installed seam, got %v", err)
	}
}

// TestZeroProbeIsNotHealthy is the other half, stated directly: it is the reason
// a no-op default is unsafe here, and it must stay true for that argument to hold.
func TestZeroProbeIsNotHealthy(t *testing.T) {
	var p reconcilerProbe
	if !p.healthy() {
		t.Skip("a zero probe already reports unhealthy — the no-op-default hazard is gone")
	}
	t.Log("a zero-valued probe reports healthy(), which is why WithPrometheus must fail closed " +
		"rather than no-op: an un-queried lane would otherwise pass")
}
