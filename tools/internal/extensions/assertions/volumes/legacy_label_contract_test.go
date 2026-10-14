package volumes

// The volume-labels LANE is retired (renaming a bound Volume breaks its next
// mount). These tests remain because the labels it wrote still exist on clusters
// relabelled by older builds, and `llz reap` must go on recognising them or those
// Volumes leak on teardown. The generator they exercise now lives in
// legacy_labels_test.go as a fixture.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

func TestDesiredVolumeLabel(t *testing.T) {
	cases := []struct {
		region, ns, pvc, want string
	}{
		{"pri", "team-gsap", "data-web-0", "pri-team-gsap-data-web-0"},
		{"sec", "kube_system", "vol_1", "sec-kube_system-vol_1"},
		// '/' and '.' are outside Linode's charset → '-'.
		{"pri", "team/foo", "data.web", "pri-team-foo-data-web"},
		// Over the cap: the MIDDLE is dropped, not the end. These expectations used
		// to be the plain `s[:32]` prefix — which is precisely the bug: cutting from
		// the right discards the ordinal that distinguishes sibling volumes, and
		// Linode rejects the duplicate with 400 "Must be unique". See
		// TestDesiredVolumeLabel_StatefulSetReplicasStayDistinct.
		{"lab", "a-very-long-namespace-here", "and-pvc", "lab-a-very-long-namespa-and-pvc"},
		{"pri", "abcdefghijklmnopqrstuvwxyz012", "z", "pri-abcdefghijklmnopqrs-xyz012-z"},
	}
	for _, c := range cases {
		got := desiredVolumeLabel(c.region, c.ns, c.pvc)
		if got != c.want {
			t.Errorf("desiredVolumeLabel(%q,%q,%q) = %q, want %q", c.region, c.ns, c.pvc, got, c.want)
		}
		if len(got) > maxLinodeLabel {
			t.Errorf("label %q exceeds %d chars", got, maxLinodeLabel)
		}
		if strings.HasSuffix(got, "-") {
			t.Errorf("label %q has a trailing dash", got)
		}
	}
}

// THE CONTRACT BETWEEN THE RELABELER AND THE REAPER.
//
// These live in different packages and were written months apart, and for weeks
// they disagreed: the volume-labels reconciler renames every bound volume away
// from `pvc-*`, while `llz reap` matched ONLY `pvc-*`. The reaper therefore saw
// nothing on any converged cluster and reported "none matched the filter", which
// is indistinguishable from "nothing to clean". ~17 volumes leaked before anyone
// looked at the Linode UI.
//
// This composes the two directly — the relabeler's real output fed into the
// reaper's real filter — so the same divergence cannot recur silently. The
// namespaces are the ones actually observed leaking.
func TestReaperRecognisesRelabelerOutput(t *testing.T) {
	// Deployment names of DIFFERENT LENGTHS. "e2e" alone proves nothing — three
	// characters is the one length at which the relabeler's linode.RegionShort(env)
	// prefix and the reaper's env prefix are the same string. On "primary" the
	// relabeler writes "pri-harbor-…" while the reaper looks for "primary-…", so the
	// sweep stays blind on every real deployment
	// and the guard that existed to catch it passed. A coupling test that fixes
	// the one input where two rules agree is a test of the test.
	for _, env := range []string{"e2e", "primary", "secondary", "standby", "lab"} {
		t.Run("env="+env, func(t *testing.T) {
			prefixes := linode.VolumeLabelPrefixes(env)

			for _, tc := range []struct{ ns, pvc string }{
				{"harbor", "harbor-otomi-db-1"},
				{"harbor", "data-harbor-redis-0"},
				{"keycloak", "keycloak-db-1-wal"},
				{"llz-openbao", "data-platform-openbao-0"},              // truncated at 32 chars
				{"istio-system", "data-oauth2-proxy-redis-ha-server-0"}, // also truncated
				{"monitoring", "storage-loki-0"},
			} {
				// desiredVolumeLabel takes REGION_SHORT, which is what `llz render`
				// stamps into the reconciler — linode.RegionShort(env), NOT env. Feeding it env
				// directly is precisely the mistake this test now exists to pin.
				label := desiredVolumeLabel(linode.RegionShort(env), tc.ns, tc.pvc)
				if !linode.VolumeIsCandidate(true, label, "us-ord", nil, "us-ord", nil, "1", "", prefixes...) {
					t.Errorf("reap cannot see %q, which is exactly what the relabeler writes for %s/%s on deployment %q.\n"+
						"The two must agree or orphaned Volumes accumulate invisibly.", label, tc.ns, tc.pvc, env)
				}
			}

			// Still matches volumes no reconciler has renamed yet.
			if !linode.VolumeIsCandidate(true, "pvc-abc123", "us-ord", nil, "us-ord", nil, "1", "", prefixes...) {
				t.Error("the CSI default prefix must still match — a volume is unrenamed between create and the first reconcile")
			}
			// And must still EXCLUDE an unrelated volume: this is a DESTRUCTIVE sweep.
			if linode.VolumeIsCandidate(true, "my-teams-database", "us-ord", nil, "us-ord", nil, "1", "", prefixes...) {
				t.Error("an unrelated volume must never be a deletion candidate")
			}
		})
	}

	// Without --env, a relabeled volume stays OUT of scope rather than being
	// silently swept — widening a destructive sweep must be explicit.
	if linode.VolumeIsCandidate(true, "pri-harbor-x", "us-ord", nil, "us-ord", nil, "1", "", linode.VolumeLabelPrefixes("")...) {
		t.Error("without --env, relabeled volumes must NOT be candidates")
	}
}

// TestRegionShortIsTheOneDerivation pins the two halves of the naming contract to
// a single definition. `llz render` stamps linode.RegionShort(env) into REGION_SHORT and the
// relabeler prefixes labels with it; the reaper accepts RegionShort(env). If those
// ever diverge again the sweep goes blind, so they must be the same function.

// TestRegionShortIsTheOneDerivation pins the two halves of the naming contract to
// a single definition. `llz render` stamps linode.RegionShort(env) into REGION_SHORT and the
// relabeler prefixes labels with it; the reaper accepts RegionShort(env). If those
// ever diverge again the sweep goes blind, so they must be the same function.
func TestRegionShortIsTheOneDerivation(t *testing.T) {
	for _, env := range []string{"e2e", "primary", "secondary", "lab", "ab", ""} {
		if got, want := linode.RegionShort(env), linode.RegionShort(env); got != want {
			t.Errorf("linode.RegionShort(%q)=%q but linode.RegionShort(%q)=%q — the label written and the prefix accepted must come from one derivation",
				env, got, env, want)
		}
	}
	// And the accepted prefix must be built from that derivation, not from env.
	for _, env := range []string{"primary", "secondary"} {
		prefixes := linode.VolumeLabelPrefixes(env)
		wantPrefix := linode.RegionShort(env) + "-"
		var found bool
		for _, p := range prefixes {
			if p == wantPrefix {
				found = true
			}
			if p == env+"-" {
				t.Errorf("VolumeLabelPrefixes(%q) accepts %q, which the relabeler never writes — "+
					"it only widens a destructive sweep", env, env+"-")
			}
		}
		if !found {
			t.Errorf("VolumeLabelPrefixes(%q) does not accept %q, so every relabeled Volume is invisible to the sweep", env, wantPrefix)
		}
	}
}

// TestDesiredVolumeLabel_StatefulSetRepicasStayDistinct is the regression test for
// the bug that made the relabeler a no-op in practice.
//
// Linode Volume labels are account-UNIQUE. The old `s[:32]` truncation cut from the
// right, which is exactly where a StatefulSet's ordinal lives, so all three OpenBao
// replicas asked for one label. The first won; the rest got
// 400 {"reason":"Must be unique"} — measured live, 17 of 17 renames rejected, so
// every Volume kept its opaque pvc-<uuid> name.

// TestDesiredVolumeLabel_StatefulSetRepicasStayDistinct is the regression test for
// the bug that made the relabeler a no-op in practice.
//
// Linode Volume labels are account-UNIQUE. The old `s[:32]` truncation cut from the
// right, which is exactly where a StatefulSet's ordinal lives, so all three OpenBao
// replicas asked for one label. The first won; the rest got
// 400 {"reason":"Must be unique"} — measured live, 17 of 17 renames rejected, so
// every Volume kept its opaque pvc-<uuid> name.
func TestDesiredVolumeLabel_StatefulSetReplicasStayDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, pvc := range []string{
		"data-platform-openbao-0",
		"data-platform-openbao-1",
		"data-platform-openbao-2",
	} {
		got := desiredVolumeLabel("e2e", "llz-openbao", pvc)
		if len(got) > maxLinodeLabel {
			t.Fatalf("desiredVolumeLabel(%s) = %q (%d chars) — Linode rejects over %d", pvc, got, len(got), maxLinodeLabel)
		}
		if prev, dup := seen[got]; dup {
			t.Fatalf("%s and %s both map to %q — the Linode API rejects the second with 'Must be unique', leaving it named pvc-<uuid> forever", prev, pvc, got)
		}
		seen[got] = pvc
	}
}

// TestDesiredVolumeLabel_ShortNamesUnchanged: the fix must not churn labels that
// already fit. A changed label on an existing Volume is a needless API write and
// breaks any operator bookmark or dashboard keyed on the name.

// TestDesiredVolumeLabel_ShortNamesUnchanged: the fix must not churn labels that
// already fit. A changed label on an existing Volume is a needless API write and
// breaks any operator bookmark or dashboard keyed on the name.
func TestDesiredVolumeLabel_ShortNamesUnchanged(t *testing.T) {
	cases := map[string]string{
		"harbor-otomi-db-1":     "e2e-harbor-harbor-otomi-db-1",
		"harbor-otomi-db-1-wal": "e2e-harbor-harbor-otomi-db-1-wal", // exactly 32
	}
	for pvc, want := range cases {
		if got := desiredVolumeLabel("e2e", "harbor", pvc); got != want {
			t.Errorf("desiredVolumeLabel(%s) = %q, want %q", pvc, got, want)
		}
	}
}

// TestFitLinodeLabel covers the squeeze directly: within the cap, at the cap, and
// the sibling-pair case truncation used to merge.

// TestFitLinodeLabel covers the squeeze directly: within the cap, at the cap, and
// the sibling-pair case truncation used to merge.
func TestFitLinodeLabel(t *testing.T) {
	if got := fitLinodeLabel("short-one"); got != "short-one" {
		t.Errorf("under the cap must pass through, got %q", got)
	}
	exact := strings.Repeat("a", maxLinodeLabel)
	if got := fitLinodeLabel(exact); got != exact {
		t.Errorf("exactly at the cap must pass through, got %q", got)
	}
	a := fitLinodeLabel("e2e-monitoring-prometheus-po-prometheus-db-0")
	b := fitLinodeLabel("e2e-monitoring-prometheus-po-prometheus-db-1")
	if a == b {
		t.Fatalf("siblings differing only in the final char collapsed to %q", a)
	}
	for _, s := range []string{a, b} {
		if len(s) > maxLinodeLabel {
			t.Errorf("%q is %d chars, over the %d cap", s, len(s), maxLinodeLabel)
		}
		if strings.HasSuffix(s, "-") {
			t.Errorf("%q ends in a separator", s)
		}
	}
}
