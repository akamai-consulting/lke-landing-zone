package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// template_removals.go closes a copier gap: `copier update` never deletes a file
// the template dropped between versions (it leaves orphans). The template instead
// declares obsolete paths in .template-removals, and `llz upgrade` applies them
// here AFTER the copier update. See instance-template/.template-removals.

const templateRemovalsFile = ".template-removals"

// removalRule is one `<mode>  <glob>` line of .template-removals.
type removalRule struct {
	mode string // "untrack" | "delete"
	glob string // filepath.Match pattern, rooted at the instance repo root
}

// readTemplateRemovals parses .template-removals (one `<mode>  <glob>` per line;
// blank lines and `#` comments ignored). A missing file → (nil, nil): an older
// instance simply has nothing to remove.
func readTemplateRemovals(path string) ([]removalRule, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rules []removalRule
	for i, ln := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(ln)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		f := strings.Fields(s)
		if len(f) != 2 {
			return nil, fmt.Errorf("%s:%d: want `<mode>  <glob>`, got %q", path, i+1, s)
		}
		if f[0] != "untrack" && f[0] != "delete" {
			return nil, fmt.Errorf("%s:%d: unknown mode %q (want untrack|delete)", path, i+1, f[0])
		}
		if _, err := filepath.Match(f[1], ""); err != nil {
			return nil, fmt.Errorf("%s:%d: bad glob %q: %w", path, i+1, f[1], err)
		}
		rules = append(rules, removalRule{mode: f[0], glob: f[1]})
	}
	return rules, nil
}

// applyTemplateRemovals removes the paths the current template version declares
// obsolete in .template-removals — run by `llz upgrade` after `copier update`.
// Two modes:
//   - untrack: `git rm --cached` — drop from the index, KEEP on disk (gitignored,
//     regenerated artifacts like the per-env <env>.tfvars).
//   - delete:  `git rm` — remove from the index AND the working tree (a file the
//     template no longer ships).
//
// Matches git-tracked files with filepath.Match (so `*` never spans '/'); delete
// wins when a path matches both modes. Idempotent — a no-op once nothing matches,
// so re-running `llz upgrade` is safe. Honors --dry-run (prints the plan only).
func applyTemplateRemovals(g globalOpts) error {
	rules, err := readTemplateRemovals(templateRemovalsFile)
	if err != nil || len(rules) == 0 {
		return err
	}
	mode := map[string]string{} // tracked path → resolved mode (delete beats untrack)
	for _, f := range strings.Split(gitOut("ls-files"), "\n") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		for _, r := range rules {
			if ok, _ := filepath.Match(r.glob, f); ok {
				if r.mode == "delete" || mode[f] == "" {
					mode[f] = r.mode
				}
			}
		}
	}
	var untrack, del []string
	for f, m := range mode {
		if m == "delete" {
			del = append(del, f)
		} else {
			untrack = append(untrack, f)
		}
	}
	sort.Strings(untrack)
	sort.Strings(del)

	if g.dryRun {
		if len(untrack) > 0 {
			fmt.Fprintf(os.Stderr, "→ (dry-run) would untrack %d file(s): %s\n", len(untrack), strings.Join(untrack, ", "))
		}
		if len(del) > 0 {
			fmt.Fprintf(os.Stderr, "→ (dry-run) would delete %d file(s): %s\n", len(del), strings.Join(del, ", "))
		}
		return nil
	}
	if len(untrack) > 0 {
		if err := execArgv(append([]string{"git", "rm", "--cached", "-q", "--"}, untrack...), ""); err != nil {
			return fmt.Errorf("untrack: %w", err)
		}
		fmt.Fprintf(os.Stderr, "%s untracked %d now-gitignored file(s) — commit the removal:\n  %s\n",
			dim("→"), len(untrack), strings.Join(untrack, "\n  "))
	}
	if len(del) > 0 {
		if err := execArgv(append([]string{"git", "rm", "-q", "--"}, del...), ""); err != nil {
			return fmt.Errorf("delete: %w", err)
		}
		fmt.Fprintf(os.Stderr, "%s deleted %d obsolete file(s) — commit the removal:\n  %s\n",
			dim("→"), len(del), strings.Join(del, "\n  "))
	}
	return nil
}
