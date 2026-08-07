package main

// spec_topology_test.go — the cross-cutting topology read.
//
// It stayed in package main when the `components`/`env show` UX tests moved to
// internal/clusterspec, because it reaches promoteDeps — one of main's fifteen
// deps assemblers, which is the dependency-injection layer main owns and no
// extension can see. The other two tests in its old file went; this one is about
// the wiring, not the capability.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/promote"
)

// #2: readTopology / promotionRanks read the SPEC, so role/peer/next stay correct
// even when no tfvars exist (a spec edit that wasn't rendered).
// #2: readTopology / promotionRanks read the SPEC, so role/peer/next stay correct
// even when no tfvars exist (a spec edit that wasn't rendered).
func TestReadTopologyFromSpec(t *testing.T) {
	chdirTempDir(t)
	writeSpecInstance(t, map[string]string{
		"east": clusterDef("east", "    ha: { role: active, group: prod }\n    promotionRank: 2\n"),
		"west": clusterDef("west", "    ha: { role: standby, group: prod }\n"),
		"lab":  clusterDef("lab", "    promotionRank: 1\n"),
	})

	deps, err := envtopology.ReadTopology("terraform-iac-bootstrap")
	if err != nil {
		t.Fatalf("readTopology: %v", err)
	}
	d, _ := envtopology.FindDeployment(deps, "east")
	if d.HARole != "active" || d.HAGroup != "prod" {
		t.Errorf("east from spec = %+v, want active/prod", d)
	}
	if peer, ok, err := envtopology.PeerOf(deps, "east"); err != nil || !ok || peer != "west" {
		t.Errorf("peerOf(east) = %q,%v,%v, want west", peer, ok, err)
	}
	if lab, _ := envtopology.FindDeployment(deps, "lab"); lab.HARole != "standalone" {
		t.Errorf("lab role = %q, want standalone default", lab.HARole)
	}

	ranks, err := promote.PromotionRanks(promoteDeps(), "terraform-iac-bootstrap")
	if err != nil {
		t.Fatalf("promotionRanks: %v", err)
	}
	if ranks["lab"] != 1 || ranks["east"] != 2 {
		t.Errorf("ranks from spec = %v, want lab=1 east=2", ranks)
	}
}
