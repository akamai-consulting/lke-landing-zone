package converge

import (
	"errors"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
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
	orig := deps.Exec
	t.Cleanup(func() { deps.Exec = orig })
	deps.Exec = func(_ string, _ ...string) ([]byte, error) {
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
	// withExecOutput, not a bare deps.Exec swap: checkPVCs reads through
	// internal/kubectlprobe, which holds its OWN seam. Stubbing one leaves the
	// other shelling out to a real cluster — which is what a 6-second "empty list
	// reported inconclusive" failure actually was.
	withExecOutput(t, func(_ string, _ ...string) ([]byte, error) {
		return []byte(`{"items":[]}`), nil
	})

	r := &health.Report{}
	checkPVCs(r)
	if v := r.Verdict(); v != health.Converged {
		t.Fatalf("an empty-but-answered PVC list reported %v; a cluster with no PVCs is converged, "+
			"not inconclusive (recorded %d failed, %d pending)", v, len(r.Failed), len(r.Pending))
	}
}

// ── the three sections this file's own list did not cover ────────────────────
//
// TestSectionsRefuseSilentGreen above takes sections with no inventory argument.
// The per-namespace ones were left out, and all three carried the exact defect it
// exists to catch. They are driven here with a full inventory so the skip-if-
// absent guard cannot be what makes them quiet.

// allNamespacesPresent is an inventory in which every namespace the health tree
// knows about exists, so a section that records nothing did so because it could
// not read, not because it had nothing to read.
func allNamespacesPresent() *clusterInventory {
	ns := map[string]bool{}
	for _, n := range healthNamespaces {
		ns[n] = true
	}
	return &clusterInventory{crds: map[string]bool{}, nsExists: ns}
}

func TestPerNamespaceSectionsRefuseSilentGreen(t *testing.T) {
	withExecOutput(t, func(_ string, _ ...string) ([]byte, error) {
		return nil, errors.New("the connection to the server was refused")
	})
	prevRetries, prevDelay := kubectlprobe.Retries, kubectlprobe.Delay
	kubectlprobe.Retries, kubectlprobe.Delay = 1, 0
	t.Cleanup(func() { kubectlprobe.Retries, kubectlprobe.Delay = prevRetries, prevDelay })

	for _, s := range []struct {
		name string
		run  func(*health.Report)
	}{
		{"Leases", func(r *health.Report) { checkLeases(r, allNamespacesPresent()) }},
		{"NetworkPolicies", func(r *health.Report) { checkNetworkPolicies(r, allNamespacesPresent()) }},
		// phase1=false: the reading where Loki is expected to have settled.
		{"LokiObjStorage", func(r *health.Report) { checkLokiObjStorage(r, false) }},
	} {
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

// TestLeaseCheckUsesTheSharedNamespaceList is the join, and the join is the fix.
//
// checkLeases carried its OWN hand-written namespace list, still spelling the
// pre-rename "cert-automation" and "openbao". Both are skip-if-absent, so both
// were skipped in silence, `stale` stayed false, and the section recorded
// "all controller Leases renewed" having never looked at the OpenBao or
// cert-automation leader Leases at all. healthNamespaces' own header records that
// exact regression — it was fixed in the list and not in the copy, because nothing
// compared them.
//
// Asserting on the NAMESPACES ACTUALLY QUERIED rather than on the source text:
// restating the list here would be a third copy, and a third copy is the defect.
func TestLeaseCheckUsesTheSharedNamespaceList(t *testing.T) {
	queried := map[string]bool{}
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "leases.coordination.k8s.io") {
			for i, a := range args {
				if a == "-n" && i+1 < len(args) {
					queried[args[i+1]] = true
				}
			}
		}
		return []byte(`{"items":[]}`), nil
	})

	checkLeases(&health.Report{}, allNamespacesPresent())

	for _, ns := range healthNamespaces {
		if !queried[ns] {
			t.Errorf("checkLeases never asked for Leases in %q. It is in healthNamespaces, so a "+
				"leader-elected controller there can stop dead and this section still reports "+
				"\"all controller Leases renewed\" — which is what the pre-rename names did for "+
				"llz-openbao and llz-cert-automation.", ns)
		}
	}
	// And nothing outside the shared list: a second list is what this test exists
	// to prevent, in either direction.
	known := map[string]bool{}
	for _, ns := range healthNamespaces {
		known[ns] = true
	}
	for ns := range queried {
		if !known[ns] {
			t.Errorf("checkLeases asked for Leases in %q, which is not in healthNamespaces — "+
				"that is a second namespace list, and keeping two in step is the thing that failed", ns)
		}
	}
}

// TestNetpolFailIsNotRewrittenWhenOwnershipIsUnknown. The managed-cluster skip
// turns a genuine missing-default-deny CatFail into CatOK when
// argocd/cluster-foundation is absent. With bare Exists, an apiserver that did not
// ANSWER also read as absent — so one unreadable probe did not weaken this
// section, it deleted its findings and reported them as passes.
func TestNetpolFailIsNotRewrittenWhenOwnershipIsUnknown(t *testing.T) {
	prevRetries, prevDelay := kubectlprobe.Retries, kubectlprobe.Delay
	kubectlprobe.Retries, kubectlprobe.Delay = 1, 0
	t.Cleanup(func() { kubectlprobe.Retries, kubectlprobe.Delay = prevRetries, prevDelay })

	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "application cluster-foundation"):
			// Not "NotFound" — an answer nobody gave.
			return nil, errors.New("Unable to connect to the server: dial tcp: i/o timeout")
		case strings.Contains(joined, "networkpolicies"):
			return []byte(`{"items":[]}`), nil // answered: this namespace has none
		}
		return []byte(`{"items":[]}`), nil
	})

	r := &health.Report{}
	checkNetworkPolicies(r, allNamespacesPresent())
	if v := r.Verdict(); v == health.Converged {
		t.Fatalf("a namespace with no default-deny was dismissed as \"apl-core owns NPs on managed\" "+
			"on the strength of a probe that never answered (recorded %d failed, %d pending)",
			len(r.Failed), len(r.Pending))
	}
}

// TestNetpolSkipStillAppliesWhenOwnershipIsAnswered pins the exclusion. The
// managed-cluster skip is real and load-bearing: without it every managed adopter
// fails this section, and a check that cries wolf gets deleted.
func TestNetpolSkipStillAppliesWhenOwnershipIsAnswered(t *testing.T) {
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "application cluster-foundation") {
			return nil, errors.New(`Error from server (NotFound): applications.argoproj.io "cluster-foundation" not found`)
		}
		return []byte(`{"items":[]}`), nil
	})
	r := &health.Report{}
	checkNetworkPolicies(r, allNamespacesPresent())
	if v := r.Verdict(); v != health.Converged {
		t.Fatalf("with cluster-foundation ANSWERED absent, the managed skip must still apply; got %v "+
			"(%d failed, %d pending)", v, len(r.Failed), len(r.Pending))
	}
}

// TestLokiUnreadableConfigMapsIsNotNotDeployed. LokiConfigText concatenates every
// matching ConfigMap and its source returns nil on ANY error, so an unreadable
// `get configmap -A` produced "" — which this section graded "Loki not deployed"
// and recorded as OK. The one state it exists to catch was reported as the state
// where it does not apply.
func TestLokiUnreadableConfigMapsIsNotNotDeployed(t *testing.T) {
	prevRetries, prevDelay := kubectlprobe.Retries, kubectlprobe.Delay
	kubectlprobe.Retries, kubectlprobe.Delay = 1, 0
	t.Cleanup(func() { kubectlprobe.Retries, kubectlprobe.Delay = prevRetries, prevDelay })

	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "secret obj-secrets") {
			return []byte(`{}`), nil // obj IS seeded: the section applies
		}
		if strings.Contains(joined, "configmap") {
			return nil, errors.New("the connection to the server was refused")
		}
		return []byte(`{"items":[]}`), nil
	})
	r := &health.Report{}
	checkLokiObjStorage(r, false)
	if v := r.Verdict(); v == health.Converged {
		t.Fatalf("unreadable ConfigMaps were graded \"Loki not deployed\" — converge would exit 0 with "+
			"Loki still filesystem-backed (recorded %d failed, %d pending)", len(r.Failed), len(r.Pending))
	}
}

// TestLokiAnsweredEmptyIsStillNotDeployed pins the exclusion: a cluster that
// answers and genuinely has no Loki ConfigMap is converged, not inconclusive.
func TestLokiAnsweredEmptyIsStillNotDeployed(t *testing.T) {
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "secret obj-secrets") {
			return []byte(`{}`), nil
		}
		return []byte(`{"items":[]}`), nil // answered, and there is no Loki ConfigMap
	})
	r := &health.Report{}
	checkLokiObjStorage(r, false)
	if v := r.Verdict(); v != health.Converged {
		t.Fatalf("an answered-empty ConfigMap list means Loki is genuinely not deployed; got %v "+
			"(%d failed, %d pending)", v, len(r.Failed), len(r.Pending))
	}
}

// ── from the code review of this PR ─────────────────────────────────────────

// TestAReleasedLeaseIsNotAStaleOne. Without this the lease widening added by this
// PR would abort EVERY converge on a healthy cluster.
//
// Kubernetes leader election clears holderIdentity on a graceful release and
// leaves renewTime FROZEN at the moment of release. LeaseStale is therefore
// permanently true for such a Lease — CatFail on every pass, which is
// runConverge's "hard-failed twice in a row — operator intervention required".
// apl-core's namespaces (harbor, istio-system, llz-observability) are exactly
// where a released lease sits, and exactly what the widening added.
func TestAReleasedLeaseIsNotAStaleOne(t *testing.T) {
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		if !strings.Contains(strings.Join(args, " "), "leases.coordination.k8s.io") {
			return []byte(`{"items":[]}`), nil
		}
		// A released lease: no holder, renewTime frozen years ago.
		return []byte(`{"items":[{"metadata":{"name":"cert-manager-controller"},` +
			`"spec":{"holderIdentity":"","renewTime":"2020-01-01T00:00:00Z","leaseDurationSeconds":15}}]}`), nil
	})
	r := &health.Report{}
	checkLeases(r, allNamespacesPresent())
	if v := r.Verdict(); v != health.Converged {
		t.Fatalf("a RELEASED lease (holderIdentity empty, renewTime frozen) is not a stopped controller — "+
			"failing on it aborts every converge on a healthy cluster; got %v with %d failed",
			v, len(r.Failed))
	}
}

// TestAHeldButStaleLeaseStillFails is the exclusion: the widening must still
// catch the thing it was widened for.
func TestAHeldButStaleLeaseStillFails(t *testing.T) {
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		if !strings.Contains(strings.Join(args, " "), "leases.coordination.k8s.io") {
			return []byte(`{"items":[]}`), nil
		}
		return []byte(`{"items":[{"metadata":{"name":"cert-manager-controller"},` +
			`"spec":{"holderIdentity":"cert-manager-abc","renewTime":"2020-01-01T00:00:00Z","leaseDurationSeconds":15}}]}`), nil
	})
	r := &health.Report{}
	checkLeases(r, allNamespacesPresent())
	if v := r.Verdict(); v == health.Converged {
		t.Error("a lease still HELD but not renewed is a controller that stopped dead — that is the signal " +
			"this section exists for")
	}
}

// TestLeaseCheckDoesNotClaimNamespacesItCouldNotRead. checkLeases recorded "all
// controller Leases renewed" even when every list failed. Only sectionItems'
// incidental CatPending kept the report from reading green — and that is a BARE
// CatPending, so it lands on llz_convergence_state == 2 while
// LLZClusterNotConverged fires on == 1: in steady-state health it alerts nobody,
// contradicting this PR's own rule that every softened site goes through
// PendingIfBudgeted.
func TestLeaseCheckDoesNotClaimNamespacesItCouldNotRead(t *testing.T) {
	prevRetries, prevDelay := kubectlprobe.Retries, kubectlprobe.Delay
	kubectlprobe.Retries, kubectlprobe.Delay = 1, 0
	t.Cleanup(func() { kubectlprobe.Retries, kubectlprobe.Delay = prevRetries, prevDelay })
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "leases.coordination.k8s.io") {
			return nil, errors.New("the connection to the server was refused")
		}
		return []byte(`{"items":[]}`), nil
	})
	r := &health.Report{}
	// CatOK entries are printed, not accumulated on the Report, so the claim has
	// to be read off stdout — which is also where an operator reads it.
	out := captureStdout(t, func() { checkLeases(r, allNamespacesPresent()) })
	if strings.Contains(out, "all controller Leases renewed") {
		t.Errorf("checkLeases claimed every Lease was renewed having read none of them:\n%s", out)
	}
	if len(r.Pending) == 0 && len(r.Failed) == 0 {
		t.Error("an unreadable lease list must be recorded, not passed over")
	}
	// PendingIfBudgeted, not a bare CatPending: outside a budget this has to read
	// as a verdict, or LLZClusterNotConverged never sees it. RESTORED after —
	// Budgeted is a package global, and leaking it flips every later test in the
	// file into steady-state mode.
	prevBudgeted := health.Budgeted
	t.Cleanup(func() { health.Budgeted = prevBudgeted })
	health.Budgeted = false
	r2 := &health.Report{}
	checkLeases(r2, allNamespacesPresent())
	if len(r2.Failed) == 0 {
		t.Error("in steady-state health an unreadable lease list must be a FAILURE — a bare CatPending " +
			"lands on llz_convergence_state == 2 and LLZClusterNotConverged fires on == 1")
	}
}

// TestNetpolManagedSkipShortCircuitsBeforeTheListRead. On managed — the only
// supported mode — cluster-foundation is ManagedSkip, so every policy count
// resolves to CatOK and reading the list cannot change the verdict. Failing on an
// unreadable read there turns a throttled `get networkpolicies` into a red
// scheduled health run for a question already answered.
func TestNetpolManagedSkipShortCircuitsBeforeTheListRead(t *testing.T) {
	prevRetries, prevDelay := kubectlprobe.Retries, kubectlprobe.Delay
	kubectlprobe.Retries, kubectlprobe.Delay = 1, 0
	t.Cleanup(func() { kubectlprobe.Retries, kubectlprobe.Delay = prevRetries, prevDelay })
	listed := 0
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		j := strings.Join(args, " ")
		if strings.Contains(j, "application cluster-foundation") {
			return nil, errors.New(`Error from server (NotFound): applications.argoproj.io "cluster-foundation" not found`)
		}
		if strings.Contains(j, "networkpolicies") {
			listed++
			return nil, errors.New("the connection to the server was refused")
		}
		return []byte(`{"items":[]}`), nil
	})
	r := &health.Report{}
	checkNetworkPolicies(r, allNamespacesPresent())
	if v := r.Verdict(); v != health.Converged {
		t.Errorf("on managed the policy count cannot change the verdict, so an unreadable list must not "+
			"fail the run; got %v (%d failed, %d pending)", v, len(r.Failed), len(r.Pending))
	}
	if listed != 0 {
		t.Errorf("the list was read %d time(s) on a cluster where its answer is irrelevant", listed)
	}
}
