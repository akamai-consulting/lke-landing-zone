package registry

// ownpaths_test.go — the one grant that was enforced against nothing.
//
// ────────────────────────────────────────────────────────────────────────────
// EVERY OTHER GRANT IS EITHER A HANDLE OR A RATCHET.
//
// cluster-read/write, cloud-read/mutate, secret-read/custody and read/write-repo
// all resolve to a handle in shared/capability that refuses what the binding did
// not declare, and the raw-transport ratchets stop code going around the handle.
// `own-paths` resolves to nothing at all: no handle, no ratchet, and — until this
// file — no comparison against the authority its own definition names.
//
// validate.go is specific about that authority: own-paths "is exactly the
// manifest's `owned` class", with .template-manifest as ADR 0014's ONE ownership
// authority, and the two states it may be held at (`scaffolded`, `upgraded`) are
// the two moments copier runs. So the grant has a territory, that territory is
// written down, and nothing ever put the two side by side.
//
// WHY THIS IS A VACUITY GUARD AND A RATCHET RATHER THAN A JOIN, which is the
// honest limit and is worth stating rather than working around. A path-level
// comparison is IMPOSSIBLE: `own-paths` is a bare Grant constant and carries no
// paths. There is nothing on the declaration to compare to a pattern, so "does
// import-brownfield own the right files?" cannot be asked in code.
//
// Giving the grant a `Paths []string` would make it askable and is exactly the
// vocabulary change this model refuses on one case — the same bar that kept
// write-repo out until a fourth extraction and that currently keeps
// `Binding.Component` uninvented. So the gap is recorded here, where the second
// case will be read, instead of being closed by inventing a field for the first.
//
// WHAT IS CHECKABLE, and both halves have caught nothing yet by design:
//
//  1. NEITHER SIDE MAY GO EMPTY. A grant nobody declares, or a territory with no
//     files in it, is a vocabulary word that means nothing — and it would look
//     exactly like a healthy one, because no test would fail. That is the
//     vacuous-green shape this tree refuses everywhere.
//  2. WHO DECLARES IT IS RATCHETED. One extension does. That is either right or it
//     is the under-declaration nobody has ruled on, and the ratchet makes the
//     second declarer a reviewed diff rather than a silent change of meaning.
// ────────────────────────────────────────────────────────────────────────────

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/manifest"
)

// ownPathsDeclarers is every extension declaring the copier fence. MEASURED, not
// chosen; it ratchets in both directions like its siblings.
//
// ONE ENTRY IS NOT SELF-EVIDENTLY RIGHT, and that is the point of writing it down.
// `.template-manifest` fences several trees an extension writes — `apl-values/*/**`
// comes from `llz render`, `landingzone.yaml` and `environments/*.yaml` from
// `environments` — and neither of those extensions holds the grant. That may be
// correct: validate.go draws own-paths as a FENCE rather than a permit ("copier
// must not render these bytes", said once, by whoever established it) and is
// explicit that generating a file is not grounds for it. It may also be an
// under-declaration. Nothing here rules on which; the ratchet only ensures the
// answer changes visibly.
var ownPathsDeclarers = map[string]bool{
	"import-brownfield": true,
}

func TestOwnPathsDeclarersAreRatcheted(t *testing.T) {
	live := map[string]bool{}
	for _, e := range All() {
		if e.HasGrant(extension.OwnPaths) {
			live[e.Name] = true
		}
	}

	// HALF ONE: the grant must be declared by someone. A vocabulary word no
	// declaration uses is indistinguishable from one that is working.
	if len(live) == 0 {
		t.Fatal("no extension declares own-paths — the grant now describes nothing, and the " +
			"copier fence it names has no owner in any declaration. Either a declaration lost " +
			"it, or the word should be retired from the vocabulary; both are decisions, and " +
			"neither should happen by nobody noticing.")
	}

	var appeared, banked []string
	for name := range live {
		if !ownPathsDeclarers[name] {
			appeared = append(appeared, name)
		}
	}
	for name := range ownPathsDeclarers {
		if !live[name] {
			banked = append(banked, name)
		}
	}
	sort.Strings(appeared)
	sort.Strings(banked)

	if len(appeared) > 0 {
		t.Errorf("%d extension(s) newly declare own-paths:\n\t%s\n"+
			"\tThat is a claim about the copier fence, not about writing files — validate.go's "+
			"own-paths block has the distinction. Add the line here so the second declarer is a "+
			"diff someone read rather than a change of what the word means.",
			len(appeared), strings.Join(appeared, "\n\t"))
	}
	if len(banked) > 0 {
		t.Errorf("%d extension(s) no longer declare own-paths — DELETE them here:\n\t%s\n"+
			"\tA stale entry says a fence has an owner when it does not.",
			len(banked), strings.Join(banked, "\n\t"))
	}
	t.Logf("extensions declaring own-paths: %d", len(live))
}

// HALF TWO: the TERRITORY must be non-empty.
//
// The grant means the manifest's copier-fenced class, so if the scaffold ever
// stops shipping a file in that class the grant is fencing nothing — and the
// declaration would still read as meaningful. This asks the manifest itself
// rather than re-parsing .template-manifest, so the classifier stays the one
// authority ADR 0014 makes it.
func TestTheCopierFencedTerritoryIsNotEmpty(t *testing.T) {
	root := scaffoldDir(t)

	m, err := manifest.Load(root)
	if err != nil {
		t.Fatalf("loading the template manifest at %s: %v", root, err)
	}
	files, err := manifest.ScaffoldFiles(root)
	if err != nil {
		t.Fatalf("listing the scaffold at %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatal("the scaffold lists no files — this guard would report a healthy territory it " +
			"never looked at")
	}

	var owned []string
	for _, rel := range files {
		if m.Classify(rel) == ownedClass {
			owned = append(owned, rel)
		}
	}
	if len(owned) == 0 {
		t.Fatalf("no file in the scaffold is classified %q, so the territory own-paths names is "+
			"empty — the grant would be a word with nothing behind it while every declaration "+
			"holding it still validated clean. Scanned %d scaffold file(s).", ownedClass, len(files))
	}
	t.Logf("copier-fenced scaffold files: %d of %d", len(owned), len(files))
}

// ownedClass is the copier-fenced class by name. A constant rather than a literal
// in two places, and deliberately NOT a second definition of the class: the
// manifest package owns what the class MEANS (Class.copierFenced), and this is
// only how the test spells the name it asks about.
const ownedClass = "owned"

// scaffoldDir walks up for instance-template/, the same way extensionsDir walks up
// for internal/extensions and for the same reason — a relative literal would point
// at nothing the first time this package moves.
func scaffoldDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		cand := filepath.Join(dir, "instance-template")
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find instance-template/ from the test's working directory")
	return ""
}
