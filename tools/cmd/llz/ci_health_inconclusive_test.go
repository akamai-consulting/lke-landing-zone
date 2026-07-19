package main

import (
	"errors"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
)

// TestSectionsRefuseSilentGreen pins the class of bug this file exists for: a
// section whose corpus comes back unreadable must NOT report the same clean run
// as one that listed everything and found it healthy.
//
// Report.Verdict() is default-Converged — it fails only on recorded Failed or
// Pending entries — so a section that iterates a nil list records nothing and
// votes "healthy". A bare kList/kItems returns nil on both "none exist" and
// "could not ask", which makes an RBAC denial or an apiserver blip on one
// resource type indistinguishable from a clean cluster.
//
// Each section here is driven with a kubectl that answers nothing at all. The
// assertion is deliberately about the VERDICT, not the message text: the
// contract is "an unreadable cluster is not converged", and any wording change
// should be free to happen without touching this test.
func TestSectionsRefuseSilentGreen(t *testing.T) {
	orig := execOutput
	t.Cleanup(func() { execOutput = orig })
	execOutput = func(_ string, _ ...string) ([]byte, error) {
		return nil, errors.New("the connection to the server was refused")
	}

	sections := []struct {
		name string
		run  func(*health.Report)
	}{
		{"Nodes", checkNodes},
		{"PVCs", checkPVCs},
		{"PVs", checkPVs},
		// phase1=false: the stricter reading, where a Job is expected to have
		// completed rather than being excused as still-bootstrapping.
		{"Jobs", func(r *health.Report) { checkJobs(r, false) }},
		{"PDBs", func(r *health.Report) { checkPDBs(r, false) }},
		{"Ingresses", func(r *health.Report) { checkIngresses(r, false) }},
		{"Pods", func(r *health.Report) { checkPods(r, false) }},
	}

	for _, s := range sections {
		t.Run(s.name, func(t *testing.T) {
			r := &health.Report{}
			s.run(r)
			if v := r.Verdict(); v == health.Converged {
				t.Fatalf("%s reported Converged against a cluster that answered nothing — "+
					"an unreadable corpus must not read as a healthy one (recorded %d failed, %d pending)",
					s.name, len(r.Failed), len(r.Pending))
			}
		})
	}
}

// TestAnsweredEmptyStaysGreen is the other half of the contract, and the reason
// the fix routes through kListOK rather than "nil means trouble": a cluster that
// answers with an empty list has genuinely told us there is nothing there. A
// fresh cluster with no PVCs is converged, not inconclusive. Without this, the
// obvious over-correction — treating every empty result as unreadable — would
// make health permanently pending on a healthy cluster.
func TestAnsweredEmptyStaysGreen(t *testing.T) {
	orig := execOutput
	t.Cleanup(func() { execOutput = orig })
	execOutput = func(_ string, _ ...string) ([]byte, error) {
		return []byte(`{"items":[]}`), nil
	}

	r := &health.Report{}
	checkPVCs(r)
	if v := r.Verdict(); v != health.Converged {
		t.Fatalf("an empty-but-answered PVC list reported %v; a cluster with no PVCs is converged, "+
			"not inconclusive (recorded %d failed, %d pending)", v, len(r.Failed), len(r.Pending))
	}
}
