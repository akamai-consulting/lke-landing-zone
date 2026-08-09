package capability_test

// RAW os.ReadFile IS THE HOLE THE read-repo FENCE CANNOT SEE, and unlike the
// kubectl case there was never a seam to miss: `read-repo` is declared by 40 of
// 61 extensions and every one of them reached the standard library directly —
// 124 os.ReadFile, 44 os.Stat, 12 os.ReadDir, 11 filepath.WalkDir. The grant was
// review metadata, and `llz ci gates`'s claim that a gate "touches nothing but
// files" was enforced by a check on the DECLARATION (extension.checkBindingCeiling)
// and by nothing at runtime.
//
// This guard watches what the grant cannot. It counts the raw filesystem calls
// per package and fails when a new one appears — and, like the in-degree and raw
// kubectl ratchets, it fails in BOTH directions, so a conversion has to be banked
// rather than left as room to regrow.
//
// ────────────────────────────────────────────────────────────────────────────
// IT WATCHES guards/ ONLY, AND THAT IS A MEASURED CEILING RATHER THAN A
// CONVENIENT ONE.
//
// The 40 declaring packages were converted one bucket at a time because a
// half-converted tree is the dangerous state: a fence that exists and does not
// hold reads as protection. guards/ went first because a gate is the thing that
// runs in a pre-commit hook on a developer's laptop, over a tree that may have
// arrived from a pull request — and because guardwalk/guardkit were already the
// shared file-walking layer for ten of the fifteen, so there WAS something to
// intercept.
//
// All fifteen are fenced, plus the three non-guard guardwalk callers the shared
// walk dragged along (atrest, manifestguard, assertobs). assertions/ and
// lifecycle/ are NOT yet, which is why widening this walk to internal/extensions
// would fail on day one for a reason everyone already knows — the shape the
// layering guard's registry exemption records as "a guard that failed on day one
// for a known reason would simply have been deleted".
//
// THE TWO INDIRECT HOLES ARE NOW CLOSED. This note used to name pincoherence and
// templatemanifest as reaching the filesystem through shared packages that were
// themselves unconverted (internal/shared/answers, internal/shared/manifest) —
// invisible to a ratchet that counts DIRECT calls. Both now read through
// answers.ReadFrom and manifest.RunFrom, and TestGuardsDoNotReadThroughUnfenced
// below keeps them there.
//
// Those two packages keep their unfenced string-taking entry points, and that is
// deliberate rather than half-done: their other callers are internal/verbs, which
// declares no bindings AT ALL by design (shared/extension/verbs_test.go pins it),
// plus cmd/llz. A verb has no binding to build a reader from and often no repo to
// be fenced to — `llz new` writes a tree that does not exist yet. The capability
// model does not reach the CLI's own surface, so the honest boundary is the one
// drawn here rather than a reader minted for callers the model does not cover.
// ────────────────────────────────────────────────────────────────────────────

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// allowedRawRead is every remaining direct filesystem call under
// internal/extensions/guards, by package, with why it is still raw.
//
// IT IS EMPTY, AND AN EMPTY MAP IS THE POINT. Every one of the fifteen guards now
// reads through capability.Repo. A new entry here is not a bookkeeping change —
// it is a gate that can read outside the repository, and it needs the reason
// written next to it.
var allowedRawRead = map[string]int{}

// rawReadCalls matches the four calls the census found. os.Open is included
// because it is the obvious way around a ReadFile ban, and filepath.Walk /
// WalkDir because the walk is where a symlink escape actually bites.
var rawReadCalls = regexp.MustCompile(
	`\bos\.(ReadFile|Stat|Lstat|ReadDir|Open|OpenFile)\(|\bfilepath\.(Walk|WalkDir)\(`)

func TestNoNewRawFilesystemReadsInGuards(t *testing.T) {
	root := filepath.FromSlash("../../extensions/guards")
	got := map[string]int{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// COMMENTS DO NOT COUNT. Several guards' headers narrate the conversion
		// ("it was `b, _ := os.ReadFile(f)`, which…"), and counting that prose
		// would make the ratchet fail on a documentation change — the fastest way
		// to teach people to delete a guard.
		n := 0
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			n += len(rawReadCalls.FindAllString(line, -1))
		}
		if n > 0 {
			// .../guards/<pkg>/file.go — take the second-to-last segment. The raw
			// kubectl ratchet records why: a path index is only as stable as the
			// layout it assumes, and taking the FIRST segment silently started
			// counting per bucket when the tree sub-divided.
			rel, _ := filepath.Rel(root, path)
			seg := strings.Split(filepath.ToSlash(rel), "/")
			got[seg[len(seg)-2]] += n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for pkg, n := range got {
		want, ok := allowedRawRead[pkg]
		if !ok {
			t.Errorf("guards/%s makes %d direct filesystem call(s) — that bypasses the read-repo "+
				"fence entirely, so this gate can read ~/.aws/credentials while declaring it "+
				"touches nothing but the repo. Route it through capability.Repo (RepoForGate, "+
				"then ReadFile/Stat/ReadDir/WalkDir), or add it here with the reason it cannot be.",
				pkg, n)
			continue
		}
		if n > want {
			t.Errorf("guards/%s: %d raw filesystem calls, allowed %d — a NEW one appeared. A gate "+
				"runs in the pre-commit path on a developer's machine; an unfenced read there is "+
				"unbounded by anything.", pkg, n, want)
		}
		if n < want {
			t.Errorf("guards/%s: %d raw filesystem calls but %d allowed — LOWER IT to %d in this "+
				"commit, so the paydown is banked instead of left as room to regrow", pkg, n, want, n)
		}
	}
	var gone []string
	for pkg := range allowedRawRead {
		if _, still := got[pkg]; !still {
			gone = append(gone, pkg)
		}
	}
	sort.Strings(gone)
	if len(gone) > 0 {
		t.Errorf("these guards no longer read the filesystem directly — delete them from "+
			"allowedRawRead: %s", strings.Join(gone, ", "))
	}
}

// EVERY GATE MUST BUILD ITS READER FROM ITS OWN DECLARATION. The fence is only
// as good as where the reader comes from: a guard that constructs
// `capability.RepoAt(extension.Binding{Grants: …}, root)` inline has granted
// itself the capability, which is precisely the "grants annotate rather than
// constrain" state this whole layer exists to end.
//
// So the constructors that take a binding from a DECLARATION (RepoForGate,
// RepoContaining with a looked-up binding) are the sanctioned door, and a
// hand-built extension.Binding inside guards/ is not.
func TestGuardsDoNotMintTheirOwnBindings(t *testing.T) {
	root := filepath.FromSlash("../../extensions/guards")
	mintsBinding := regexp.MustCompile(`capability\.RepoAt\(\s*extension\.Binding\{`)

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if mintsBinding.Match(b) {
			t.Errorf("%s builds a reader from a binding it constructed inline. Look the binding up "+
				"from Extension() instead (capability.RepoForGate, or an accessor like objenc's "+
				"seedBinding) — a capability minted at the call site is not a declared one.", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// A GUARD CAN LAUNDER A READ THROUGH A SHARED PACKAGE, and the direct-call
// ratchet above cannot see it. That is not hypothetical — it is exactly how
// pincoherence and templatemanifest stayed unfenced through the first pass while
// reporting zero raw calls, because their reads happened inside
// internal/shared/answers and internal/shared/manifest.
//
// So the unfenced entry points of those packages are named, and guards/ may not
// call them. The fenced twin of each takes a reader and is the only door a gate
// should use.
func TestGuardsDoNotReadThroughUnfencedHelpers(t *testing.T) {
	// call -> what to use instead.
	unfenced := map[string]string{
		"answers.Read(":           "answers.ReadFrom(repo, dir)",
		"manifest.Load(":          "manifest.LoadFrom(repo, root)",
		"manifest.Run(":           "manifest.RunFrom(repo, root, …)",
		"manifest.ScaffoldFiles(": "manifest.ScaffoldFilesFrom(repo, root)",
	}

	root := filepath.FromSlash("../../extensions/guards")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for call, instead := range unfenced {
				// The fenced twins are prefixes of nothing — ReadFrom does not
				// contain "Read(" — so a plain match is exact enough.
				if strings.Contains(line, call) {
					t.Errorf("%s calls %s, which reads the disk unfenced. A gate that launders its "+
						"reads through a shared package reports zero raw calls to the ratchet above "+
						"while being exactly as unbounded. Use %s.", path, call, instead)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
