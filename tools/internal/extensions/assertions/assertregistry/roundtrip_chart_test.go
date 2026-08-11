package assertregistry

// roundtrip_chart_test.go — "is llz-cert-automation deployed here" is answered by
// the chart's ExternalSecret, not by its namespace.
//
// THE REGRESSION THIS PINS. The lane used to read a present namespace as a
// deployed component, which was true until argoWorkflows began shipping
// llz-cert-automation EMPTY on managed clusters so argo-helm had somewhere to put
// the workflow RBAC that controller.workflowNamespaces asks for. With the
// namespace present and the chart absent, the lane stopped skipping and spent two
// minutes waiting for a Secret nothing would ever create — the only red in an
// otherwise green release-e2e.

import (
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/harborauth"
)

// stubChartProbes swaps the two deployment probes and the Secret read.
func stubChartProbes(t *testing.T, nsPresent, esPresent bool, secretReads *int) {
	t.Helper()
	oN, oE, oS := harborauth.NamespaceExists, harborauth.RobotExternalSecretExists, harborauth.ReadRobotSecret
	t.Cleanup(func() {
		harborauth.NamespaceExists, harborauth.RobotExternalSecretExists, harborauth.ReadRobotSecret = oN, oE, oS
	})
	harborauth.NamespaceExists = func(string) (bool, error) { return nsPresent, nil }
	harborauth.RobotExternalSecretExists = func(string, string) (bool, error) { return esPresent, nil }
	harborauth.ReadRobotSecret = func(string, string) ([]byte, error) { *secretReads++; return nil, nil }
}

// The managed shape: namespace present (argoWorkflows ships it), chart absent.
func TestRunAssertHarborRoundTripSkipsWithoutTheChart(t *testing.T) {
	reads := 0
	stubChartProbes(t, true, false, &reads)

	err := Run("llz-cert-automation", "harbor-docker-config", "", ProbeRepo, 2*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("an undeployed component must SKIP, not fail: %v", err)
	}
	if reads != 0 {
		t.Errorf("the Secret was read %d time(s) — with no ExternalSecret there is nothing to wait for, and "+
			"waiting is what cost two minutes and a red lane", reads)
	}
}

// ...and the finding the lane exists for must survive: chart deployed, Secret
// never materialized, means ESO is not doing its job. That still fails.
func TestRunAssertHarborRoundTripStillFailsWhenTheChartIsDeployed(t *testing.T) {
	reads := 0
	stubChartProbes(t, true, true, &reads)

	err := Run("llz-cert-automation", "harbor-docker-config", "", ProbeRepo, 60*time.Millisecond, 10*time.Millisecond)
	if err == nil {
		t.Fatal("ExternalSecret present + Secret absent is the real finding (ESO not materializing) and must fail")
	}
	if !strings.Contains(err.Error(), "secret/harbor/robot") {
		t.Errorf("the failure should name the OpenBao path so it is actionable, got: %v", err)
	}
	if reads == 0 {
		t.Error("the Secret was never read — absence must be retried across the settle budget")
	}
}

// An absent namespace still skips on its own, without needing the ExternalSecret
// probe (which would error against a namespace that is not there).
func TestRunAssertHarborRoundTripSkipsWithoutTheNamespace(t *testing.T) {
	reads := 0
	stubChartProbes(t, false, false, &reads)
	if err := Run("llz-cert-automation", "harbor-docker-config", "", ProbeRepo, time.Second, time.Millisecond); err != nil {
		t.Fatalf("an absent namespace must SKIP: %v", err)
	}
	if reads != 0 {
		t.Errorf("read the Secret %d time(s) despite no namespace", reads)
	}
}

// The flag set, which nothing exercised: a verb that resolves but whose flags do
// not is the shape TestDeliveredWorkflowCommands catches for delivered YAML and
// nothing catches here.
func TestAssertHarborRoundTripCmdWiring(t *testing.T) {
	c := AssertHarborRoundTripCmd()
	if c.Use != "assert-harbor-roundtrip" {
		t.Errorf("verb is spelled %q — the e2e assert suite calls assert-harbor-roundtrip", c.Use)
	}
	if c.RunE == nil {
		t.Fatal("the verb resolves but does nothing")
	}
	for _, f := range []string{"secret-namespace", "secret-name", "repo", "settle", "interval"} {
		if c.Flags().Lookup(f) == nil {
			t.Errorf("--%s is gone; the lane's callers set it", f)
		}
	}
	// The repo default must be the probe repository, not a real one: the round trip
	// requests a scope against it, and pointing it at something real would make a
	// read-only assertion look like a write.
	if f := c.Flags().Lookup("repo"); f != nil && f.DefValue != ProbeRepo {
		t.Errorf("--repo defaults to %q, want the throwaway probe repo %q", f.DefValue, ProbeRepo)
	}
	if err := c.Args(c, []string{"stray"}); err == nil {
		t.Error("a stray positional must be rejected")
	}
}

// A probe ERROR must fail the lane rather than skip it — "could not tell" is not
// "not deployed", and treating it as such would silently disable the gate on any
// cluster whose API blipped.
func TestRunAssertHarborRoundTripFailsWhenTheProbeErrors(t *testing.T) {
	orig := harborauth.NamespaceExists
	t.Cleanup(func() { harborauth.NamespaceExists = orig })
	harborauth.NamespaceExists = func(string) (bool, error) { return false, errTestProbe }

	if err := Run("ns", "name", "", ProbeRepo, time.Second, time.Millisecond); err == nil {
		t.Error("an unreadable namespace probe must fail, not skip")
	}
}

var errTestProbe = errProbe("probe unavailable")

type errProbe string

func (e errProbe) Error() string { return string(e) }
