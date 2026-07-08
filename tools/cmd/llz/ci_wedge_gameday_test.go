package main

import (
	"sort"
	"strings"
	"testing"
)

// H is a healthy App status; U(x) an unhealthy one.
func hApp(h string) appHealth { return appHealth{sync: "Synced", health: h} }

// evalWedge: fault reaches the target while parent + siblings stay Healthy = contained.
func TestEvalWedgeContained(t *testing.T) {
	guarded := []string{"platform-bootstrap", "llz-externalsecrets", "llz-harbor"}
	snaps := []gamedaySnapshot{
		{"llz-observability": hApp("Healthy"), "platform-bootstrap": hApp("Healthy"), "llz-externalsecrets": hApp("Healthy"), "llz-harbor": hApp("Healthy")},
		{"llz-observability": hApp("Progressing"), "platform-bootstrap": hApp("Healthy"), "llz-externalsecrets": hApp("Healthy"), "llz-harbor": hApp("Healthy")},
	}
	v := evalWedge("llz-observability", guarded, snaps)
	if !v.contained() {
		t.Fatalf("expected contained, got %+v", v)
	}
	if v.targetStatus != "Progressing" {
		t.Errorf("target status should be Progressing, got %q", v.targetStatus)
	}
}

// A parent/sibling going non-Healthy during the window = containment breach (the
// fault cascaded), even though the target also degraded.
func TestEvalWedgeContainmentBreach(t *testing.T) {
	guarded := []string{"platform-bootstrap", "llz-harbor"}
	snaps := []gamedaySnapshot{
		{"llz-observability": hApp("Healthy"), "platform-bootstrap": hApp("Healthy"), "llz-harbor": hApp("Healthy")},
		{"llz-observability": hApp("Degraded"), "platform-bootstrap": hApp("Progressing"), "llz-harbor": hApp("Healthy")},
	}
	v := evalWedge("llz-observability", guarded, snaps)
	if v.contained() {
		t.Fatalf("a parent breach must NOT count as contained: %+v", v)
	}
	if v.containmentHeld {
		t.Error("containmentHeld should be false")
	}
	if len(v.breaches) != 1 || v.breaches[0] != "platform-bootstrap" {
		t.Errorf("breach should name platform-bootstrap, got %v", v.breaches)
	}
}

// The fault never surfaces in the target → inconclusive (not contained), even though
// nothing else broke.
func TestEvalWedgeFaultNeverPropagated(t *testing.T) {
	guarded := []string{"platform-bootstrap", "llz-harbor"}
	snaps := []gamedaySnapshot{
		{"llz-observability": hApp("Healthy"), "platform-bootstrap": hApp("Healthy"), "llz-harbor": hApp("Healthy")},
		{"llz-observability": hApp("Healthy"), "platform-bootstrap": hApp("Healthy"), "llz-harbor": hApp("Healthy")},
	}
	v := evalWedge("llz-observability", guarded, snaps)
	if v.faultPropagated {
		t.Error("fault should not have propagated")
	}
	if v.contained() {
		t.Error("no propagation → not contained")
	}
	if !v.containmentHeld {
		t.Error("containment held (nothing else broke)")
	}
}

// A guarded App MISSING from a snapshot (unreadable) is treated as a breach — we
// can't claim containment for an App we couldn't observe.
func TestEvalWedgeMissingGuardedIsBreach(t *testing.T) {
	guarded := []string{"platform-bootstrap", "llz-harbor"}
	snaps := []gamedaySnapshot{
		// llz-harbor absent from this snapshot.
		{"llz-observability": hApp("Progressing"), "platform-bootstrap": hApp("Healthy")},
	}
	v := evalWedge("llz-observability", guarded, snaps)
	if v.containmentHeld {
		t.Error("a missing guarded App must break containment")
	}
	if len(v.breaches) != 1 || v.breaches[0] != "llz-harbor" {
		t.Errorf("breach should name the missing llz-harbor, got %v", v.breaches)
	}
}

// siblingsOf excludes the target and returns the other carved Apps (registry-driven).
func TestSiblingsOf(t *testing.T) {
	sibs := siblingsOf("llz-observability")
	sort.Strings(sibs)
	want := []string{"llz-broad-pat-rotator", "llz-externalsecrets", "llz-harbor", "llz-reconciler"}
	if strings.Join(sibs, ",") != strings.Join(want, ",") {
		t.Errorf("siblingsOf(llz-observability) = %v, want %v", sibs, want)
	}
	// The target must never appear among its own siblings.
	for _, s := range sibs {
		if s == "llz-observability" {
			t.Error("target leaked into siblings")
		}
	}
}

func TestSplitNSName(t *testing.T) {
	if ns, n, ok := splitNSName("monitoring/loki-object-store"); !ok || ns != "monitoring" || n != "loki-object-store" {
		t.Errorf("parse failed: %q %q %v", ns, n, ok)
	}
	for _, bad := range []string{"", "no-slash", "/name", "ns/"} {
		if _, _, ok := splitNSName(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}
