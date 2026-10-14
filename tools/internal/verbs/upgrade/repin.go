package upgrade

// repin.go — after an upgrade, every committed kustomize `?ref=` must name the
// NEW release.
//
// ── THE REGRESSION ────────────────────────────────────────────────────────────
//
// The committed apl-values kustomizations fetch the shared platform tree at
// `?ref=<the instance's pin>`, and the pin is what `copier update` has just
// rewritten. So the moment copier finishes, every one of those refs is stale —
// deterministically, on every upgrade, with no operator judgement involved. That
// is why `llz upgrade` re-renders (Lever 2) rather than leaving it to a step
// someone has to remember.
//
// It is not hypothetical. A live instance ran three releases behind on what
// ArgoCD DEPLOYS — v0.0.31 manifests under a v0.0.34 instance — because the
// re-render was manual and nobody ran it. The scaffold said one thing and the
// cluster ran another, and every gate was green: the pin was correct, the
// workflows were correct, and the only wrong thing was a URL fragment in a file
// nothing compared.
//
// ── WHY A SEPARATE CHECK AND NOT CONVERGENCE ──────────────────────────────────
//
// `apl-values/*/**` is `owned` in .template-manifest, so ConvergenceGaps ignores
// it BY DESIGN — the operator's overlay is theirs. That is correct for the
// overlay's content and blind to the one part of it the TEMPLATE controls: the
// ref the render stamps. Convergence cannot be relaxed to cover this without
// giving up the ownership rule, so the ref gets its own predicate.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// kustomizeRef matches the `?ref=<something>` in a remote kustomize base.
// Terminated by `&` or end-of-token so the `&timeout=` that follows is not
// swallowed into the ref.
var kustomizeRef = regexp.MustCompile(`\?ref=([^&"'\s]+)`)

// RefUsage is what a tree's committed refs point at: how many name the wanted
// release, and every file still naming something else.
type RefUsage struct {
	Wanted int                 // refs naming the new pin
	Stale  map[string][]string // file → the other refs it carries
}

// StaleFiles is the stale set as a sorted list, for a stable message.
func (u RefUsage) StaleFiles() []string {
	out := make([]string, 0, len(u.Stale))
	for f := range u.Stale {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// ScanKustomizeRefs walks `dir` and classifies every `?ref=` it finds against
// `want`.
//
// Pure over the filesystem and nothing else — no git, no network — so the
// predicate is exercisable from a table test.
func ScanKustomizeRefs(dir, want string) (RefUsage, error) {
	u := RefUsage{Stale: map[string][]string{}}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A missing apl-values dir is not this function's error to raise: the
			// caller decides whether an absent tree is a finding, because "no
			// overlay" and "an overlay full of stale refs" are different verdicts.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(p); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		for _, m := range kustomizeRef.FindAllStringSubmatch(string(b), -1) {
			if m[1] == want {
				u.Wanted++
				continue
			}
			if !contains(u.Stale[rel], m[1]) {
				u.Stale[rel] = append(u.Stale[rel], m[1])
			}
		}
		return nil
	})
	return u, err
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// CheckRepinned turns a scan into the gate's verdict.
//
// FAILS CLOSED ON AN EMPTY SCAN, which is the arm that matters most here. If the
// overlay carries no `?ref=` at all — the render did not run, the overlay was
// never written, the URL shape changed — then "no stale refs" is true and means
// nothing. Reporting that as a pass is precisely how a check goes quiet on the
// bug it exists to catch.
func CheckRepinned(from string, u RefUsage, want string) string {
	if u.Wanted == 0 && len(u.Stale) == 0 {
		return fmt.Sprintf("pin-repointed [from %s]: the upgraded instance's apl-values carry NO `?ref=` at all.\n"+
			"    Nothing was compared, so this is a failure rather than a pass: either the render did not\n"+
			"    run (upgrade's Lever 2), the overlay was never written, or the remote-base URL shape moved\n"+
			"    and this check no longer recognises it.", from)
	}
	if len(u.Stale) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "pin-repointed [from %s]: %d committed file(s) still fetch the platform tree at a ref that is not %s:",
		from, len(u.Stale), want)
	for _, f := range u.StaleFiles() {
		fmt.Fprintf(&b, "\n      %s → %s", f, strings.Join(u.Stale[f], ", "))
	}
	b.WriteString("\n    These kustomizations are what ArgoCD SYNCS. An upgrade that moves the pin without\n" +
		"    re-rendering them leaves the cluster deploying the previous release's manifests while\n" +
		"    every version string in the repo says otherwise — a live instance ran three releases\n" +
		"    behind exactly this way, with every other gate green.")
	return b.String()
}
