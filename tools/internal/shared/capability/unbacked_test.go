package capability_test

// unbacked_test.go — the OTHER direction of "declare what you touch".
//
// ─────────────────────────────────────────────────────────────────────────────
// THE MODEL ENFORCES ONE HALF AND IMPLIES BOTH.
//
// A binding cannot USE what it did not declare: the handle refuses, and the raw
// ratchets in this package stop code going around the handle. That half is real.
//
// A binding may freely DECLARE what it never touches, and nothing notices. That is
// the half that erodes the declaration's worth, because the failure is silent and
// self-reinforcing: a binding that declares everything passes every check, reviews
// clean, and teaches the next author that the grant line is decoration. `llz
// extension list` then reports a blast radius larger than the code's.
//
// WHAT THIS MEASURES, STATED HONESTLY. It looks for evidence that a package
// ACQUIRES a handle for each mutating grant it declares. Absence of that evidence
// is not proof the grant is unused — a package can exercise one through a Deps
// seam assembled elsewhere, or through a transport this file does not know. So
// this is NOT a claim that the 21 entries below are over-declarations.
//
// It is a RATCHET, and that is all it claims to be: the number cannot grow without
// someone saying why, and every conversion moves it down. That is the same
// standing the raw-transport ratchets have, and they were worth having for the
// same reason — an unmeasured surface grows quietly.
//
// AND IT IS THE HONEST STOPPING POINT. Proving a grant unused needs whole-program
// analysis through Deps injection and interface satisfaction; inventing that off a
// list nobody has read yet would be building the answer before the question is
// understood. Read the entries, convert what converts, and let the shape of what
// resists tell you what the real check should be.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// grantHandleMarkers maps a mutating grant to the ways a package can be seen
// acquiring a handle for it. Several grants have more than one legal channel —
// cloud-mutate through the Linode client OR through `gh`, secret-custody through
// the Custodian OR the forge's repo-secret path OR OpenBao's admin surface — and
// missing one would manufacture a finding rather than record one.
var grantHandleMarkers = map[string][]string{
	// `.Cloud.` JOINED THIS ROW WHEN Handles GAINED THE FIELD, and the TRAILING DOT
	// is load-bearing rather than tidy.
	//
	// Written as `.Cloud` it matched `extension.CloudMutate` and
	// `extension.CloudRead` — the GRANT CONSTANTS, in the very declaration this
	// check reads to decide whether the grant is declared. Every package holding
	// cloud-mutate therefore contained its own evidence of backing it, and the row
	// went from 8 findings to 2 by asserting a tautology. A marker that matches the
	// declaration instead of the code is worse than a missing marker: it reports
	// the paydown as already done.
	//
	// `.Cloud.` is a field access followed by a method, which is how the handle is
	// ever used and is not how a constant is spelled. Assigning the handle to a
	// local first (`c := h.Cloud`) reads as unbacked, which is the safe direction —
	// an entry gets demanded rather than waived.
	// `capability.For` WAS LISTED FOR BOTH MUTATING KUBE/SECRET ROWS AND IT IS NOT A
	// MARKER FOR EITHER. It is how a binding acquires ALL of its handles, so its
	// presence says a package took SOMETHING — not that it took the one being
	// checked. That is the `.Cloud` defect one row up in a second costume: a marker
	// broader than the claim reports the paydown as already done.
	//
	// It cost exactly one entry, which is the useful part of the measurement.
	// identity-plane's only capability line is
	// `capability.For(configBinding()).BaoAdmin` — the OpenBao ADMIN surface, not a
	// Writer — and that single occurrence of the string credited it with backing
	// `cluster-write` too. Removing the marker moved it into the list below, where
	// the other twenty-one already sat.
	//
	// Each row now names handles that ONLY that grant yields. `.Forge` appears twice
	// deliberately: the forge is gated by three grants and a repo-secret write really
	// is custody (see forge.go).
	"CloudMutate":   {"capability.CloudFor", ".Forge", ".Cloud."},
	"SecretCustody": {".Custodian", ".Forge", ".BaoAdmin"},
	"ClusterWrite":  {".Writer", "capability.KubeFor", "capability.KubeAPI"},
	"WriteRepo":     {"capability.RepoFor", "capability.RepoAt", "capability.Repo"},
}

// unbackedGrants is every <package>/<grant> whose handle acquisition is not
// visible in that package, with the count fixed at what it was when measured.
//
// EDITING THIS DOWN IS THE POINT. A conversion deletes its line in the same commit.
var unbackedGrants = map[string]bool{
	// bootstrapcluster/ClusterWrite WAS HERE AND HAS BEEN PAID DOWN. The package
	// mutated through a raw kubectl runner, so its cluster-write grant entitled a
	// handle nobody built. The brownfield migration's recreate goes through
	// capability.MustWriter/DeleteOrphan instead — which is what makes the one
	// destructive call in this package a named operation a reviewer can grep for,
	// rather than an argv one flag away from taking the ingest path down.
	"bootstrapcluster/SecretCustody": true,
	"chartpublish/CloudMutate":       true,
	"clusteraccess/ClusterWrite":     true,
	"clusteraccess/SecretCustody":    true,
	"credrotate/SecretCustody":       true,
	"firewall/ClusterWrite":          true,
	"gameday/ClusterWrite":           true,
	"harbor/CloudMutate":             true,
	// SURFACED BY REMOVING THE `capability.For` MARKER, not by new code. This package
	// reaches the cluster through the kubectlprobe seam (counted at 1 in
	// allowedSeamCalls) and takes only .BaoAdmin from capability.For, so its
	// cluster-write has no visible Writer. It was never backed; the marker said it
	// was.
	"identityconfig/ClusterWrite":   true,
	"kyverno/ClusterWrite":          true,
	"openbao/CloudMutate":           true,
	"openbao/ClusterWrite":          true,
	"openbao/SecretCustody":         true,
	"reconcilelanes/CloudMutate":    true,
	"reconcilelanes/SecretCustody":  true,
	"reconciler/ClusterWrite":       true,
	"reconciler/SecretCustody":      true,
	"releasepublish/CloudMutate":    true,
	"statepassphrase/CloudMutate":   true,
	"statepassphrase/SecretCustody": true,
	"teardown/ClusterWrite":         true,
}

func TestUnbackedGrantsAreRatcheted(t *testing.T) {
	root := filepath.FromSlash("../../extensions")

	// The scan must find its subject — the class rawcloud_test.go documents. If the
	// marker set or the declaration spelling drifts, everything looks backed and
	// this file reports a clean tree it never read.
	if len(grantHandleMarkers) == 0 {
		t.Fatal("no grant markers configured")
	}

	live := map[string]bool{}
	var scanned int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return err
		}
		declB, derr := os.ReadFile(filepath.Join(path, "extension.go"))
		if derr != nil {
			return nil // not an extension package
		}
		scanned++
		body := nonCommentSource(t, path)
		for grant, markers := range grantHandleMarkers {
			if !strings.Contains(string(declB), "extension."+grant) {
				continue
			}
			backed := false
			for _, m := range markers {
				if strings.Contains(body, m) {
					backed = true
					break
				}
			}
			if !backed {
				live[filepath.Base(path)+"/"+grant] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned == 0 {
		t.Fatal("no extension.go found under internal/extensions — the walk lost its subject " +
			"and would report every grant backed")
	}

	var appeared, banked []string
	for k := range live {
		if !unbackedGrants[k] {
			appeared = append(appeared, k)
		}
	}
	for k := range unbackedGrants {
		if !live[k] {
			banked = append(banked, k)
		}
	}
	sort.Strings(appeared)
	sort.Strings(banked)

	if len(appeared) > 0 {
		t.Errorf("%d grant declaration(s) with no visible handle acquisition:\n\t%s\n"+
			"\tThe model enforces that a binding cannot USE what it did not declare; nothing "+
			"stops it DECLARING what it never touches, and a binding that declares everything "+
			"passes every check. Acquire the handle, or add the entry here.",
			len(appeared), strings.Join(appeared, "\n\t"))
	}
	if len(banked) > 0 {
		t.Errorf("%d entr(ies) are backed now — DELETE them from unbackedGrants:\n\t%s\n"+
			"\tThat is the paydown this ratchet exists to bank.",
			len(banked), strings.Join(banked, "\n\t"))
	}
	t.Logf("mutating grant declarations with no visible handle: %d across %d extension packages",
		len(live), scanned)
}

// nonCommentSource concatenates a package's production Go with comment lines
// dropped, for the reason every scanner here gives: several headers narrate a
// conversion, and matching that prose would report a handle the code does not take.
func nonCommentSource(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var b strings.Builder
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("reading %s: %v", n, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			fmt.Fprintln(&b, line)
		}
	}
	return b.String()
}
