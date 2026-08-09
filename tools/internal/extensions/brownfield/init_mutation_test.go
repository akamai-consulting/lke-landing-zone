package brownfield

import (
	"strings"
	"testing"
)

// import_init_mutation_test.go closes the gaps mutation testing found in the
// report→spec mapping and the MIGRATION-TODO renderer. Import errors are
// DESTRUCTIVE rather than merely wrong: a mis-summarised report authors a spec
// that misrepresents live infrastructure, so "we detected nothing" must be as
// firmly asserted as "we detected this".
//
// Two shapes recur below:
//   - the ABSENT case. Nearly every section here is guarded by `len(x) > 0`;
//     nothing pinned the empty side, so a guard that always fired (or one that
//     dereferenced a nil `rep.Linode`) went unnoticed. Each section is asserted
//     both ways: present → the line appears, empty/absent → it does NOT.
//   - the TIE case. `largestPool` picks the biggest node pool with a strict `>`,
//     so equal-sized pools keep the FIRST. Nothing had two pools of the same
//     size, so "first wins" was never pinned.

// ── largestPool: ties, zero counts, and the nil-Linode fallbacks ─────────────

// TestLargestPoolTiePrefersFirst pins the tie-break: with equally sized pools the
// FIRST is kept, so the authored nodeType is stable across report re-scans rather
// than depending on the order the Linode API happened to list the pools in.
func TestLargestPoolTiePrefersFirst(t *testing.T) {
	linode := importReport{Linode: &importLinode{NodePools: []lkePool{
		{Type: "first-g6-standard-4", Count: 3},
		{Type: "second-g6-standard-8", Count: 3},
	}}}
	if nt, nc := largestPool(linode); nt != "first-g6-standard-4" || nc != 3 {
		t.Errorf("linode pool tie: got %q/%d, want first-g6-standard-4/3 (ties keep the first pool)", nt, nc)
	}

	kube := importReport{Cluster: importCluster{NodePools: []nodePool{
		{NodeType: "first-g6-standard-4", Count: 3},
		{NodeType: "second-g6-standard-8", Count: 3},
	}}}
	if nt, nc := largestPool(kube); nt != "first-g6-standard-4" || nc != 3 {
		t.Errorf("kube pool tie: got %q/%d, want first-g6-standard-4/3 (ties keep the first pool)", nt, nc)
	}

	// And the strict-greater ordering still holds when the LATER pool is bigger,
	// so "keep the first" is a tie-break and not a "never replace best".
	bigLast := importReport{Linode: &importLinode{NodePools: []lkePool{
		{Type: "small", Count: 3},
		{Type: "big", Count: 9},
	}}}
	if nt, nc := largestPool(bigLast); nt != "big" || nc != 9 {
		t.Errorf("got %q/%d, want big/9", nt, nc)
	}
}

// TestLargestPoolEmptyLinodePoolsFallsThrough pins that an EMPTY (not nil)
// Linode node-pool list falls through to the kube-derived pools instead of
// indexing pool [0].
func TestLargestPoolEmptyLinodePoolsFallsThrough(t *testing.T) {
	rep := importReport{
		Linode:  &importLinode{Region: "us-ord", NodePools: nil},
		Cluster: importCluster{NodePools: []nodePool{{NodeType: "g6-standard-2", Count: 4}}},
	}
	if nt, nc := largestPool(rep); nt != "g6-standard-2" || nc != 4 {
		t.Errorf("got %q/%d, want g6-standard-2/4 from the kube pools", nt, nc)
	}
}

// TestReportToEnvAddOptsZeroNodeCountLeftUnset pins that a pool with a node type
// but a zero count leaves nodeCount UNSET (the scaffold placeholder) rather than
// authoring a literal "0" — a spec that would provision a cluster with no nodes.
func TestReportToEnvAddOptsZeroNodeCountLeftUnset(t *testing.T) {
	rep := importReport{Linode: &importLinode{NodePools: []lkePool{{Type: "g6-standard-4", Count: 0}}}}
	o := reportToEnvSpec(Deps{}, rep)
	if o.NodeType != "g6-standard-4" {
		t.Errorf("nodeType=%q, want g6-standard-4", o.NodeType)
	}
	if o.NodeCount != "" {
		t.Errorf("nodeCount=%q, want \"\" — a zero count must leave the scaffold default, not author a 0-node pool", o.NodeCount)
	}
}

// ── linodeRegion / subnet CIDR: the nil-Linode and empty-subnet sides ────────

// TestLinodeRegionPrefersLinodeOverKube pins both sides of the `rep.Linode != nil`
// guard: present → the Linode API region wins; absent → the kube-derived region is
// used and nothing dereferences the nil report section.
func TestLinodeRegionPrefersLinodeOverKube(t *testing.T) {
	withLinode := importReport{
		Linode:  &importLinode{Region: "us-ord"},
		Cluster: importCluster{Region: "us-east"},
	}
	if got := linodeRegion(withLinode); got != "us-ord" {
		t.Errorf("linodeRegion=%q, want us-ord", got)
	}
	if got := reportToEnvSpec(Deps{}, withLinode).Region; got != "us-ord" {
		t.Errorf("region=%q, want the Linode region us-ord", got)
	}

	noLinode := importReport{Cluster: importCluster{Region: "us-east"}}
	if got := linodeRegion(noLinode); got != "" {
		t.Errorf("linodeRegion with no Linode section=%q, want \"\"", got)
	}
	if got := reportToEnvSpec(Deps{}, noLinode).Region; got != "us-east" {
		t.Errorf("region=%q, want the kube-derived us-east", got)
	}
}

// TestReportToEnvAddOptsSubnetCIDR walks the three-part VPC guard: only a Linode
// section WITH a VPC WITH at least one subnet authors subnetCIDR; every other
// shape leaves it empty (and must not index an empty subnet list).
func TestReportToEnvAddOptsSubnetCIDR(t *testing.T) {
	cases := []struct {
		name string
		rep  importReport
		want string
	}{
		{"vpc with subnets", importReport{Linode: &importLinode{VPC: &lkeVPC{Subnets: []string{"10.200.0.0/14", "10.204.0.0/14"}}}}, "10.200.0.0/14"},
		{"vpc with no subnets", importReport{Linode: &importLinode{VPC: &lkeVPC{Label: "vpc-1"}}}, ""},
		{"linode but no vpc", importReport{Linode: &importLinode{Region: "us-ord"}}, ""},
		{"no linode section", importReport{Cluster: importCluster{Region: "us-ord"}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reportToEnvSpec(Deps{}, c.rep).SubnetCIDR; got != c.want {
				t.Errorf("subnetCIDR=%q, want %q", got, c.want)
			}
		})
	}
}

// ── MIGRATION-TODO: every optional section, present AND absent ──────────────

// emptyTodoReport is the "scan found nothing optional" report: no Linode section,
// no warnings, no databases, no helm releases, no disabled apps. Every optional
// TODO section must be silent for it.
func emptyTodoReport() importReport {
	return importReport{
		Cluster: importCluster{KubernetesVersion: "v1.30.0", Region: "us-ord", NodeCount: 3},
		DNS:     importDNS{DomainSuffix: "example.com"},
		Summary: importSummary{PVCs: 0, TotalStorage: ""},
	}
}

// TestBuildMigrationTodoOmitsEmptySections is the absent half of every optional
// section. A guard that always fires would emit a heading claiming "0 database(s)"
// or an empty disabled-apps list — a TODO that misdescribes the source.
func TestBuildMigrationTodoOmitsEmptySections(t *testing.T) {
	md := buildMigrationTodo(Deps{}, emptyTodoReport(), "prod")
	mustNotContain := []string{
		"disabled in the source",   // :298 — no APL signals at all
		"Platform differences",     // :306 — no warnings
		"database(s) — migrate",    // :330 — no databases
		"object-storage bucket(s)", // :340 — no Linode section
		"Installed Helm releases",  // :351 — no helm releases
	}
	for _, s := range mustNotContain {
		if strings.Contains(md, s) {
			t.Errorf("empty report still emitted %q — an always-firing guard misdescribes the source\n---\n%s", s, md)
		}
	}
	// The unconditional sections must still be there, so this isn't passing by
	// rendering nothing at all.
	for _, s := range []string{"# Migration TODO — prod", "k8s_version", "Data — migrate"} {
		if !strings.Contains(md, s) {
			t.Errorf("MIGRATION-TODO missing the unconditional %q\n---\n%s", s, md)
		}
	}
}

// TestBuildMigrationTodoEmptyOptionalSubsections covers the sections whose
// CONTAINER is non-empty but whose optional inner list is empty: an APL repo with
// no disabled apps, a Linode section with no buckets, a database with no clients.
func TestBuildMigrationTodoEmptyOptionalSubsections(t *testing.T) {
	rep := emptyTodoReport()
	rep.Repos = []repoInventory{{Role: "apl", APL: &aplSignals{AplVersion: "v4.14.1", DisabledApps: nil}}}
	rep.Linode = &importLinode{Region: "us-ord", ObjectStorage: nil}
	rep.Storage = importStorage{Databases: []dbInfo{{Namespace: "gitea", Name: "gitea-db", Engine: "postgres", Kind: "CNPG", Clients: nil}}}

	md := buildMigrationTodo(Deps{}, rep, "prod")
	if strings.Contains(md, "disabled in the source") {
		t.Errorf("APL signals with NO disabled apps must not emit the disabled-apps section\n---\n%s", md)
	}
	if strings.Contains(md, "object-storage bucket(s)") {
		t.Errorf("a Linode section with NO buckets must not emit the bucket line\n---\n%s", md)
	}
	// The database itself IS listed, but with the no-client wording — an empty
	// client list must never render as "client: " with nothing after it.
	if !strings.Contains(md, "gitea/gitea-db (postgres, CNPG) — no client found") {
		t.Errorf("a database with no clients must render \"no client found\"\n---\n%s", md)
	}
	if strings.Contains(md, "client: ") {
		t.Errorf("empty client list rendered as a client list\n---\n%s", md)
	}
}

// TestBuildMigrationTodoPresentSections is the present half: each optional section
// appears, with its real content, when the report has something to say. This is
// what a negated guard drops.
func TestBuildMigrationTodoPresentSections(t *testing.T) {
	rep := emptyTodoReport()
	rep.Repos = []repoInventory{{Role: "apl", APL: &aplSignals{DisabledApps: []string{"alertmanager"}}}}
	rep.Warnings = []string{"in-cluster Gitea detected"}
	rep.Linode = &importLinode{Region: "us-ord", ObjectStorage: []lkeBucket{
		{Label: "loki", Region: "us-ord-1"},
		{Label: "harbor", Region: "us-ord-1"},
	}}
	rep.Storage = importStorage{Databases: []dbInfo{
		{Namespace: "gitea", Name: "gitea-db", Engine: "postgres", Kind: "CNPG", Clients: []string{"gitea", "gitea-runner"}},
	}}
	rep.Platform.HelmReleases = []helmRelease{{Namespace: "harbor", Name: "harbor", Chart: "harbor", ChartVersion: "1.13.0"}}

	md := buildMigrationTodo(Deps{}, rep, "prod")
	mustContain := []string{
		"disabled in the source",                                        // :298
		"- [ ] alertmanager",                                            // the actual list
		"## Platform differences (from the scan)",                       // :306
		"- [ ] in-cluster Gitea detected",                               // the actual warning
		"- [ ] 1 database(s)",                                           // :330
		"gitea/gitea-db (postgres, CNPG) — client: gitea, gitea-runner", // :334 present side
		"- [ ] 2 object-storage bucket(s)",                              // :340
		"## Installed Helm releases (reference)",                        // :351
		"harbor/harbor — harbor 1.13.0",
	}
	for _, s := range mustContain {
		if !strings.Contains(md, s) {
			t.Errorf("MIGRATION-TODO missing %q\n---\n%s", s, md)
		}
	}
	if strings.Contains(md, "no client found") {
		t.Errorf("a database WITH clients must not render \"no client found\"\n---\n%s", md)
	}
}
