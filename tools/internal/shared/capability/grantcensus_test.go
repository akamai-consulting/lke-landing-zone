package capability_test

// grantcensus_test.go — the numbers this package's headers are written around.
//
// ────────────────────────────────────────────────────────────────────────────
// EVERY HANDLE HEADER OPENS WITH A CENSUS, AND FOUR OF THEM HAD GONE STALE.
//
// The headers argue from measurement rather than assertion — "read-repo is declared
// by more extensions than any other grant", "20 declarations of cloud-read and 21
// of cloud-mutate". That is the right way to justify a capability, and exactly why
// the numbers cannot be left unchecked: an argument from a measurement is only as
// good as the measurement, and a hand-taken count goes stale against a registry
// that keeps growing.
//
// Measured against the live registry, repo.go said 40 of 61 (42 of 62), cloud.go
// said 20 cloud-read (19), secrets.go said sixteen secret-custody (18). None of
// the ARGUMENTS changed — read-repo is still the most-declared grant, cloud-mutate
// is still the most destructive — which is precisely what makes this the kind of
// drift nobody catches by reading.
//
// So the census is measured here and the headers cite it. A grant count moving is
// normal and this test failing is not a defect; it is the moment to update the
// sentence that quotes the number. The alternative — four hand-transcribed censuses
// nothing compares — is the shape that put "six named operations" above an
// eight-method interface.
// ────────────────────────────────────────────────────────────────────────────

import (
	"sort"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension/registry"
)

// citedCounts is every grant's census, keyed by where it is quoted. BINDING counts
// and EXTENSION counts both: a grant is per-binding, so the binding is the unit a
// handle is built for, but the model doc's distribution table counts extensions
// because "what does this extension touch?" is the question a reviewer asks.
//
// ALL NINE ARE HERE, WHICH THEY WERE NOT AT FIRST. The table started as the five
// grants the handle headers quote, and the model doc then grew a nine-row
// distribution table under a sentence saying "the counts are pinned by
// TestHandleHeaderCensusesMatchTheRegistry" — which was true of five of them. Four
// numbers presented as checked were not, in the paragraph arguing that an unchecked
// count is how a design doc comes to describe a system that no longer exists. A
// grant with no prose citing it still belongs here: it costs a line, and the next
// sentence quoting it starts out pinned instead of starting out unverified.
var citedCounts = map[extension.Grant]struct {
	bindings, extensions int
	cited                string
}{
	extension.ReadRepo:      {68, 55, "repo.go: \"declared by 55 of 75 extensions — more than any other grant\"; model doc distribution table"},
	extension.CloudRead:     {20, 17, "cloud.go: \"20 declarations of cloud-read\"; model doc distribution table"},
	extension.CloudMutate:   {21, 17, "cloud.go: \"21 of cloud-mutate\"; model doc distribution table"},
	extension.SecretRead:    {11, 9, "secrets.go: \"eleven declare secret-read\"; model doc distribution table"},
	extension.SecretCustody: {18, 12, "secrets.go: \"Eighteen bindings declare secret-custody\"; model doc distribution table"},
	extension.ClusterRead:   {52, 23, "model doc distribution table"},
	extension.ClusterWrite:  {22, 16, "model doc distribution table"},
	extension.WriteRepo:     {9, 6, "model doc distribution table"},
	extension.OwnPaths:      {1, 1, "model doc distribution table"},
}

func census() (bindings, extensions map[extension.Grant]int, total int) {
	bindings, extensions = map[extension.Grant]int{}, map[extension.Grant]int{}
	all := registry.All()
	for _, e := range all {
		for _, b := range e.Bindings {
			for _, g := range b.Grants {
				bindings[g]++
			}
		}
		for _, g := range e.Grants() {
			extensions[g]++
		}
	}
	return bindings, extensions, len(all)
}

func TestHandleHeaderCensusesMatchTheRegistry(t *testing.T) {
	byBinding, byExt, total := census()
	if total != 75 {
		t.Errorf("the registry holds %d extensions; repo.go's header says 75. Update the "+
			"header and the denominator below together.", total)
	}
	var grants []extension.Grant
	for g := range citedCounts {
		grants = append(grants, g)
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i] < grants[j] })
	for _, g := range grants {
		want := citedCounts[g]
		if byBinding[g] != want.bindings || byExt[g] != want.extensions {
			t.Errorf("%s declares %d binding(s) across %d extension(s); the header cites "+
				"%d/%d.\n  %s\nThe header's ARGUMENT is probably still sound — update the "+
				"number it quotes rather than deleting the measurement, which is what makes "+
				"the argument checkable at all.",
				g, byBinding[g], byExt[g], want.bindings, want.extensions, want.cited)
		}
	}
}

// read-repo being the most-declared grant is the LOAD-BEARING half of repo.go's
// argument — it is why the fence was worth building and why it was built last. The
// exact number may move; the ordering is the claim.
func TestReadRepoIsStillTheMostDeclaredGrant(t *testing.T) {
	_, byExt, _ := census()
	for _, g := range extension.Grants() {
		if g != extension.ReadRepo && byExt[g] >= byExt[extension.ReadRepo] {
			t.Errorf("%s is now held by %d extensions against read-repo's %d — repo.go opens "+
				"by calling read-repo the most-declared grant, and that is the reason it gives "+
				"for the fence mattering. Re-read that header before changing this test.",
				g, byExt[g], byExt[extension.ReadRepo])
		}
	}
}
