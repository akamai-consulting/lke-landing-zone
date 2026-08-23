package upgrade

// convergence_test.go — the checks `llz ci upgrade-test` gained, at unit speed.
//
// The gate itself needs copier, git tags and ~2.5 minutes, and self-skips without
// them. These run on every `go test`, so the arms that decide whether it is a real
// gate — what it asserts on, what it excludes, and what it refuses to call a pass —
// cannot be lost and rediscovered by an adopter.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/manifest"
)

// classOf stands in for the .template-manifest classifier.
func fixedClasses(m map[string]string) func(string) string {
	return func(p string) string { return m[p] }
}

func TestConvergenceGapsFindsEachKind(t *testing.T) {
	classes := fixedClasses(map[string]string{
		"AGENTS.md":       "managed",
		"never-delivered": "managed",
		"workflow.yml":    "merge",
		"dropped.md":      "managed",
	})
	fresh := map[string]string{
		"AGENTS.md":       "aaa",
		"never-delivered": "bbb",
		"workflow.yml":    "ccc",
	}
	upgraded := map[string]string{
		"AGENTS.md":    "OLD", // the release-before's bytes, still here
		"workflow.yml": "ccc",
		"dropped.md":   "ddd", // template no longer ships it, copier left it
	}

	got := ConvergenceGaps(fresh, upgraded, classes)
	want := []ConvergenceGap{
		{Path: "never-delivered", Class: "managed", Kind: GapMissing},
		{Path: "dropped.md", Class: "managed", Kind: GapOrphaned},
		{Path: "AGENTS.md", Class: "managed", Kind: GapStale},
	}
	if len(got) != len(want) {
		t.Fatalf("ConvergenceGaps = %+v, want %d gaps", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("gap %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// THE EXCLUSION, and it has to stay one. `owned` files are seeded on scaffold and
// never updated by design, so an instance built at an older release legitimately
// keeps that release's provider lockfile. Asserting equality on them would turn
// this gate red the first time a lockfile changed upstream — a false alarm in a
// gate whose whole value is being believed.
func TestConvergenceGapsIgnoresOwnedFiles(t *testing.T) {
	classes := fixedClasses(map[string]string{
		"terraform-iac-bootstrap/cluster/.terraform.lock.hcl": "owned",
		"kubernetes-custom/global/x.yaml":                     "owned",
	})
	fresh := map[string]string{
		"terraform-iac-bootstrap/cluster/.terraform.lock.hcl": "new-hashes",
	}
	upgraded := map[string]string{
		"terraform-iac-bootstrap/cluster/.terraform.lock.hcl": "old-hashes",
		"kubernetes-custom/global/x.yaml":                     "operator wrote this",
	}
	if got := ConvergenceGaps(fresh, upgraded, classes); len(got) != 0 {
		t.Errorf("ConvergenceGaps = %+v; `owned` files differing is what the class MEANS, not a finding", got)
	}
}

// A `merge` file is held to the same equality, and that is deliberate rather than
// sloppy: the probe instance carries no local edits, so `ours` equals the merge
// base and a correct 3-way merge must yield `theirs` exactly. Relaxing this would
// blind the gate to a merge that resolved against the wrong base — which is the
// failure that shipped invalid YAML in the gsap-apl v0.0.24 upgrade.
func TestConvergenceGapsAssertsMergeClass(t *testing.T) {
	classes := fixedClasses(map[string]string{".github/workflows/llz-terraform.yml": "merge"})
	got := ConvergenceGaps(
		map[string]string{".github/workflows/llz-terraform.yml": "new"},
		map[string]string{".github/workflows/llz-terraform.yml": "half-merged"},
		classes)
	if len(got) != 1 || got[0].Kind != GapStale {
		t.Errorf("ConvergenceGaps = %+v; a merge-class file that did not reach the new render is a finding", got)
	}
}

// An UNCLASSIFIED path is asserted, not skipped. .template-manifest is required to
// cover the whole scaffold, so a file matching no rule is a manifest bug — and if
// this skipped it, a file could drop out of the strongest check in the gate by
// having no rule at all, which is also the easiest way to end up unclassified.
func TestConvergenceGapsAssertsUnclassifiedPaths(t *testing.T) {
	got := ConvergenceGaps(
		map[string]string{"stray.txt": "new"},
		map[string]string{"stray.txt": "old"},
		fixedClasses(nil))
	if len(got) != 1 || got[0].Kind != GapStale {
		t.Errorf("ConvergenceGaps = %+v; a path no manifest rule covers must still be compared", got)
	}
}

// FAIL CLOSED ON VACUITY is the caller's arm, so this pins the half that lives
// here: comparing against nothing yields nothing, which is precisely why
// RunUpgradeTest refuses to proceed on an empty reference scaffold. If this ever
// starts returning gaps for an empty `fresh`, that guard can be reconsidered —
// until then it is load-bearing.
func TestConvergenceGapsAgainstAnEmptyScaffoldFindsNothing(t *testing.T) {
	if got := ConvergenceGaps(nil, map[string]string{"a": "b"}, fixedClasses(map[string]string{"a": "managed"})); len(got) != 1 {
		t.Errorf("ConvergenceGaps = %+v; an upgraded file absent from an empty reference is an orphan", got)
	}
	if got := ConvergenceGaps(nil, nil, fixedClasses(nil)); len(got) != 0 {
		t.Errorf("ConvergenceGaps = %+v; two empty trees produce no gaps — hence the emptiness guard in the caller", got)
	}
}

// A SPLIT CONTRACT: which classes this check asserts on is decided by
// .template-manifest's class table, not by a list of names copied into
// convergence.go. Both sides' real code is called here — manifest.LookupClass for
// the rule, convergenceAsserted for the consumer — so a fourth class, or a renamed
// one, cannot quietly fall out of the comparison. The names above ("managed",
// "merge", "owned") are the same strings the manifest ships, which is what makes
// those tests real and not a private vocabulary.
func TestConvergenceAssertionTracksTheManifestClassTable(t *testing.T) {
	names := strings.Split(manifest.ClassNames(), "|")
	if len(names) < 3 {
		t.Fatalf("manifest.ClassNames() = %q — this gate would compare against a table it cannot read", names)
	}
	restoreSeen := false
	for _, name := range names {
		c, ok := manifest.LookupClass(name)
		if !ok {
			t.Fatalf("manifest.ClassNames() lists %q but LookupClass does not know it", name)
		}
		want := c.Upgrade != manifest.UpgradeRestore
		if got := convergenceAsserted(name); got != want {
			t.Errorf("convergenceAsserted(%q) = %v, want %v — the class declares Upgrade=%q, and the "+
				"convergence check must follow the manifest rather than a second opinion about ownership",
				name, got, want, c.Upgrade)
		}
		if c.Upgrade == manifest.UpgradeRestore {
			restoreSeen = true
		}
	}
	if !restoreSeen {
		t.Error("no class in the manifest declares Upgrade=restore, so this test asserted the exclusion " +
			"arm against nothing — the one arm that keeps the gate from crying wolf on a lockfile")
	}
}

func TestDigestTreeSkipsGitAndDigestsContent(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("AGENTS.md", "hello")
	write("docs/x.md", "hello")
	// The gate commits the scaffold before upgrading, so the upgraded side has a
	// .git and the reference side does not. Digesting it would report thousands of
	// gaps about the harness and none about the upgrade.
	write(".git/objects/ab/cdef", "gitguts")
	write(".terraform/plugin", "provider blob")

	got, err := DigestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("DigestTree = %v, want exactly AGENTS.md and docs/x.md", got)
	}
	if got["AGENTS.md"] != got["docs/x.md"] {
		t.Error("identical content produced different digests")
	}
	// Slash-separated, so the map keys match .template-manifest globs on every OS.
	if _, ok := got["docs/x.md"]; !ok {
		t.Errorf("DigestTree keys are not slash-separated: %v", got)
	}
}

func TestDigestTreeDigestsSymlinksAsLinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("/nonexistent/target", filepath.Join(root, "dangling")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := DigestTree(root)
	if err != nil {
		t.Fatalf("a dangling symlink must be a recorded difference, not a read error: %v", err)
	}
	if !strings.HasPrefix(got["dangling"], "symlink:") {
		t.Errorf("DigestTree[dangling] = %q, want the link target — reading THROUGH a symlink either "+
			"errors on a dangling one or silently compares the same target twice", got["dangling"])
	}
}

// assertTasksRan reads the TREE. Its first cut scanned copier's output for the
// fallback message, which copier also prints while echoing the task it is about to
// run — so the gate failed on every render, including the ones that worked. A log
// line saying a fallback EXISTS is not evidence that it was TAKEN.
func TestAssertTasksRanReadsTheTreeNotTheLog(t *testing.T) {
	tmpl, inst := t.TempDir(), t.TempDir()
	docs := func(root string, n int) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
			t.Fatal(err)
		}
		for i := range n {
			if err := os.WriteFile(filepath.Join(root, "docs", string(rune('a'+i))+".md"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("pruned docs are a pass", func(t *testing.T) {
		docs(tmpl, 8)
		docs(inst, 3)
		if err := assertTasksRan(tmpl, inst); err != nil {
			t.Errorf("assertTasksRan = %v; a pruned docs/ is deliver-docs having run", err)
		}
	})

	t.Run("unpruned docs are a harness failure", func(t *testing.T) {
		full := t.TempDir()
		docs(full, 8)
		err := assertTasksRan(tmpl, full)
		if err == nil {
			t.Fatal("an instance carrying the template's ENTIRE docs/ means `llz` was not on PATH and the " +
				"tasks degraded — two equally undelivered instances match each other, so convergence would " +
				"pass having measured nothing")
		}
		if !strings.Contains(err.Error(), "--llz") {
			t.Errorf("the failure does not name the flag that fixes it: %v", err)
		}
	})

	t.Run("no docs at all is a failure, not an empty pass", func(t *testing.T) {
		if err := assertTasksRan(tmpl, t.TempDir()); err == nil {
			t.Error("an instance that received no docs/ passed — 'nothing there' and 'nothing to prune' " +
				"are the same count and opposite verdicts")
		}
	})

	t.Run("a template with no docs cannot measure delivery", func(t *testing.T) {
		if err := assertTasksRan(t.TempDir(), inst); err == nil {
			t.Error("a template with no docs/ passed — the comparison has no denominator")
		}
	})
}

func TestPutOnPATHIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", "/usr/bin")
	for range 3 {
		if err := putOnPATH(dir); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := os.Getenv("PATH"), dir+string(os.PathListSeparator)+"/usr/bin"; got != want {
		t.Errorf("PATH = %q, want %q — a re-entrant gate must not grow PATH per call", got, want)
	}
	// Empty is a no-op rather than a PATH of "", which would make every child
	// process unable to find git, copier or llz.
	t.Setenv("PATH", "/usr/bin")
	if err := putOnPATH(""); err != nil || os.Getenv("PATH") != "/usr/bin" {
		t.Errorf("putOnPATH(\"\") changed PATH to %q (%v)", os.Getenv("PATH"), err)
	}
}

// The gate must drive `llz upgrade`, not `copier update`. Stopping at copier is
// what left the manifest-policy pass and the removals pass — two of the three
// things an upgrade does — ungated under a gate named for the upgrade path.
func TestUpgradeUnderTestArgvDrivesTheRealCommand(t *testing.T) {
	argv := UpgradeUnderTestArgv("/opt/llz", "abc123")
	if len(argv) < 2 || argv[0] != "/opt/llz" || argv[1] != "upgrade" {
		t.Fatalf("UpgradeUnderTestArgv = %v; want the llz binary under test running `upgrade`", argv)
	}
	for _, want := range []string{"--ref", "abc123", "--no-doctor"} {
		if !containsArgv(argv, want) {
			t.Errorf("UpgradeUnderTestArgv is missing %q: %v", want, argv)
		}
	}
	// --no-render USED TO BE REQUIRED HERE, and dropping it is the point rather
	// than a relaxation. It was passed to keep the gate offline, but render reads a
	// spec and writes files — it was never online — and while it was skipped the
	// upgrade's Lever 2 (re-render every `?ref=` the new pin invalidates) was
	// exercised by nothing. TestUpgradeUnderTestArgvRenders in repin_test.go now
	// holds the inverse.
	// --commit would leave the probe's upgrade in a commit and change what the
	// conflict scan sees; the gate inspects the WORKING TREE.
	if containsArgv(argv, "--commit") {
		t.Errorf("UpgradeUnderTestArgv commits the upgrade: %v", argv)
	}
}

func containsArgv(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}
