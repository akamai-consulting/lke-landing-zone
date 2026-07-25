package main

// upgrade_churn_guard.go is the regression gate for a class of bug that is
// invisible in review: making the TEMPLATE re-render a version string into the
// delivered scaffold, so every instance's `llz upgrade` diff grows by that many
// lines per release for zero functional change.
//
// It cost a real instance 45 of 53 changed lines on the v0.0.31 -> v0.0.32 upgrade
// (gsap-apl) — 27 version-pinned doc permalinks, 10 `template-ref:` inputs, and the
// rest prose and a provenance stamp — all restating one fact copier already records
// once in .copier-answers.yml. Nothing in CI objected, because each individual
// addition looked reasonable at the time.
//
// The invariant this locks in: THE DELIVERED SCAFFOLD CONTAINS NO VERSION AT ALL.
// The pin lives in .copier-answers.yml (copier writes it) and is read at runtime
// (pinnedTemplateRef); the one deliberate exception is the pinned docs pointer,
// which `llz ci deliver-docs` generates at render time rather than storing in the
// template. That makes a content-free version bump touch exactly one file by
// construction, not by luck.
//
// It runs on BOTH sides of the delivery. In the template repo it stops a version
// from being re-rendered into the scaffold. In an instance it catches the same
// strings having actually landed — which is not hypothetical: gsap-apl's v0.0.33
// upgrade left 27 permalinks pinned at v0.0.32 because the delivery ran with an
// llz predating the fix, and nothing in the instance's own CI objected.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// churnPattern is one banned shape plus the reason, so a failure explains the
// class rather than just pointing at a line.
type churnPattern struct {
	re     *regexp.Regexp
	what   string
	remedy string
}

var churnPatterns = []churnPattern{
	{
		// The copier version token. Rendering it into any delivered file means that
		// file changes on every release.
		re:   regexp.MustCompile(`<@\s*llz_version\s*@>`),
		what: "the copier `llz_version` token rendered into the delivered scaffold",
		remedy: "read the pin at runtime instead — `pinnedTemplateRef()` in Go, or\n" +
			"      .copier-answers.yml directly. The scaffold carries no version (PR #323).",
	},
	{
		// The retired workflow input. Re-adding it puts a hand-maintained third copy
		// of the pin next to TF_IMAGE, which is the skew assert-image-fresh exists to
		// catch.
		re:   regexp.MustCompile(`(?m)^\s*template-ref:`),
		what: "a `template-ref:` workflow input (retired in PR #323)",
		remedy: "the two real consumers (`llz ci assert-image-fresh`, `llz ci\n" +
			"      bootstrap-cluster`) resolve the ref from the instance checkout themselves.",
	},
	{
		// A permalink pinned to a concrete release. deliver-docs generates the ONE
		// pinned pointer at render time; a stored one churns every release.
		re:   regexp.MustCompile(`lke-landing-zone/(?:blob|tree)/v\d+\.\d+\.\d+/`),
		what: "a version-pinned template permalink stored in a source file",
		remedy: "link relatively (kept docs) or let `llz ci deliver-docs` rewrite it —\n" +
			"      it owns the single pinned pointer in docs/README.md.",
	},
}

// churnGuardRoots are the trees whose contents are DELIVERED to an instance: the
// scaffold itself, and the docs keep-set that ships inside it. Everything else in
// the template repo (its own .github/, the maintainer docs under docs/workflows/,
// designs, ADRs) can name versions freely — it never reaches an instance.
var churnGuardRoots = []string{
	"instance-template",
	"docs/quickstart.md",
	"docs/runbooks",
	"docs/playbooks",
}

// churnGuardInstanceRoots is the same surface as it lands INSIDE an instance: the
// vendored workflows plus the locally-kept docs. docs/README.md is deliberately
// absent — it carries the one pinned pointer, which is the whole point of it.
var churnGuardInstanceRoots = []string{
	".github/workflows",
	"docs/quickstart.md",
	"docs/runbooks",
	"docs/playbooks",
}

// churnGuardScope picks the delivered surface for the checkout we are standing in:
// the template repo (identified by instance-template/), an instance (identified by
// copier's answers file), or neither — in which case there is nothing to guard.
func churnGuardScope() (roots []string, instance bool) {
	if fi, err := os.Stat("instance-template"); err == nil && fi.IsDir() {
		return churnGuardRoots, false
	}
	if _, err := os.Stat(".copier-answers.yml"); err == nil {
		return churnGuardInstanceRoots, true
	}
	return nil, false
}

// stepUpgradeChurnGuard fails the lint gate when a banned pattern is present in the
// delivered surface — about to ship it (template repo) or carrying it (instance).
func stepUpgradeChurnGuard(_ globalOpts) error {
	roots, instance := churnGuardScope()
	if roots == nil {
		return nil // neither a template repo nor an instance — nothing to guard
	}
	var hits []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // a missing optional root is not this check's concern
			}
			// .copier-answers.yml is copier's own record — the one place the version
			// belongs. Its template carries copier's jinja, not ours.
			if strings.HasSuffix(p, ".copier-answers.yml") {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil || len(data) == 0 {
				return nil //nolint:nilerr // unreadable/empty — skip, as the marker scan does
			}
			for _, cp := range churnPatterns {
				for _, ln := range matchedLines(string(data), cp.re) {
					hits = append(hits, fmt.Sprintf("%s:%d — %s\n      fix: %s", p, ln, cp.what, cp.remedy))
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("upgrade-churn guard: walking %s: %w", root, err)
		}
	}
	if len(hits) > 0 {
		fmt.Fprintf(os.Stderr, "upgrade-churn guard: %d version reference(s) in the DELIVERED surface:\n", len(hits))
		for _, h := range hits {
			fmt.Fprintf(os.Stderr, "  • %s\n", h)
		}
		if instance {
			return fmt.Errorf("this instance must carry no version outside .copier-answers.yml and the " +
				"docs/README.md pointer — these are stale strings from a delivery run by an older llz; " +
				"`llz self-update` then re-run `llz upgrade` to re-deliver them")
		}
		return fmt.Errorf("the delivered scaffold must carry no version — each of these adds a line to " +
			"EVERY instance's upgrade diff, every release, for no functional change")
	}
	return nil
}

// matchedLines returns the 1-indexed lines of content matching re.
func matchedLines(content string, re *regexp.Regexp) []int {
	var out []int
	for i, ln := range strings.Split(content, "\n") {
		if re.MatchString(ln) {
			out = append(out, i+1)
		}
	}
	return out
}
