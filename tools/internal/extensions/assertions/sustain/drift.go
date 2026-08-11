package sustain

// drift.go is the template-drift check (formerly template-scripts/
// check-template-drift.sh, now retired). It resolves this instance's provenance
// (ResolveTemplateVersion — copier's .copier-answers.yml, no committed stamp),
// resolves the template repo's current branch head, and reports whether the
// instance is behind. Report-only by default; --strict exits 1 on drift so a
// scheduled job can gate. The Scheduled Checks template-drift job runs this via
// the llz baked into TF_IMAGE.

import (
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

func RunDrift(d Deps, branch, repoURL string, strict bool) error {
	if branch == "" {
		branch = "main"
	}
	tv := ResolveTemplateVersion(d)
	if tv.TemplateRepo == "" || tv.TemplateSHA == "" {
		return fmt.Errorf("cannot determine this instance's template provenance — run from an instance checkout (one with .copier-answers.yml, written by `llz new`)")
	}

	slug := githubSlug(tv.TemplateRepo)
	if repoURL == "" {
		switch {
		case strings.Contains(tv.TemplateRepo, "://") || strings.HasPrefix(tv.TemplateRepo, "git@"):
			repoURL = tv.TemplateRepo
		case strings.Contains(tv.TemplateRepo, "/"):
			repoURL = "https://github.com/" + strings.TrimSuffix(tv.TemplateRepo, ".git") + ".git"
		default:
			return fmt.Errorf("cannot derive a fetch URL from template_repo=%q — pass --repo-url", tv.TemplateRepo)
		}
	}

	latest := gitOut(d, "ls-remote", repoURL, "refs/heads/"+branch)
	if i := strings.IndexAny(latest, " \t"); i > 0 {
		latest = latest[:i]
	}
	if latest == "" {
		return fmt.Errorf("could not read %s head from %s (check access / --repo-url / --branch)", branch, repoURL)
	}

	short := func(s string) string {
		if len(s) > 8 {
			return s[:8]
		}
		return s
	}
	var compareURL string
	if slug != "" {
		compareURL = fmt.Sprintf("https://github.com/%s/compare/%s...%s", slug, tv.TemplateSHA, latest)
	}

	// Compare COMMITS, not spellings. copier records `_commit:` as whatever ref
	// the upgrade named, which on the normal adopter path is a release TAG
	// (`v0.0.42`) — never a 40-hex sha. Comparing that string against a branch
	// head can only be unequal, so an instance sitting exactly ON main's head was
	// told it was behind, with a compare link of a commit against itself showing
	// no commits. Every tag-pinned instance saw a permanent drift warning, which
	// is how a real one stops being read (and --strict fails a scheduled gate).
	pinned := resolvePin(d, repoURL, tv.TemplateSHA)

	if pinned == latest {
		fmt.Printf("%s Up to date with %s@%s (%s).\n", color.Green("✓"), tv.TemplateRepo, branch, short(latest))
		emitDriftSummary(tv, branch, latest, "", "✅ up to date")
		return nil
	}

	behind := ""
	if commitReachable(d, pinned) && commitReachable(d, latest) {
		behind = gitOut(d, "rev-list", "--count", pinned+".."+latest)
	}
	msg := fmt.Sprintf("behind %s@%s", tv.TemplateRepo, branch)
	if behind != "" {
		msg = behind + " commit(s) " + msg
	}
	fmt.Printf("%s instance at %s, %s head at %s — %s.\n",
		color.Yellow("Template drift:"), short(tv.TemplateSHA), branch, short(latest), msg)
	if compareURL != "" {
		fmt.Printf("%s %s\n", color.Dim("Compare:"), color.Cyan(compareURL))
	}
	if os.Getenv("GITHUB_ACTIONS") != "" {
		fmt.Printf("::warning title=Template drift::Instance is %s. Sync upstream + re-stamp with `llz upgrade`.\n", msg)
	}
	emitDriftSummary(tv, branch, latest, behind, "⚠️ drifted ("+msg+")")

	if strict {
		return fmt.Errorf("template drift detected (--strict)")
	}
	return nil
}

// githubSlug returns owner/repo when template_repo is a github reference, else "".
func githubSlug(repo string) string {
	switch {
	case strings.HasPrefix(repo, "git@github.com:"):
		return strings.TrimSuffix(strings.TrimPrefix(repo, "git@github.com:"), ".git")
	case strings.Contains(repo, "github.com/"):
		return strings.TrimSuffix(repo[strings.Index(repo, "github.com/")+len("github.com/"):], ".git")
	case strings.Contains(repo, "://") || strings.HasPrefix(repo, "git@"):
		return "" // some other host — no github compare link
	case strings.Contains(repo, "/"):
		return strings.TrimSuffix(repo, ".git") // bare owner/repo (assume github)
	default:
		return ""
	}
}

// resolvePin turns the recorded pin into a commit sha so it can be compared with
// a branch head. A pin that already looks like a sha, or a tag the remote does
// not have, is returned unchanged — an unresolvable pin is still worth reporting
// as drift, and guessing would be worse than the honest inequality.
//
// `^{}` first: an ANNOTATED tag's own object is the tag, not the commit it points
// at, so refs/tags/<t> alone would compare a tag object against a commit and
// never match — the same class of mismatch this function exists to remove.
func resolvePin(d Deps, repoURL, pin string) string {
	if isHexSHA(pin) {
		return pin
	}
	for _, ref := range []string{"refs/tags/" + pin + "^{}", "refs/tags/" + pin} {
		out := gitOut(d, "ls-remote", repoURL, ref)
		if i := strings.IndexAny(out, " \t"); i > 0 {
			out = out[:i]
		}
		if out != "" {
			return out
		}
	}
	return pin
}

// isHexSHA reports whether s is a full 40-character hex object name.
func isHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func commitReachable(d Deps, sha string) bool {
	_, err := d.Exec("git", "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func emitDriftSummary(tv TemplateVersion, branch, latest, behind, status string) {
	f := os.Getenv("GITHUB_STEP_SUMMARY")
	if f == "" {
		return
	}
	short := func(s string) string {
		if len(s) > 8 {
			return s[:8]
		}
		return s
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Template drift — %s\n\n", tv.TemplateRepo)
	sb.WriteString("| Field | Value |\n|---|---|\n")
	fmt.Fprintf(&sb, "| Instance template ref | `%s` (%s) |\n", tv.TemplateRef, short(tv.TemplateSHA))
	fmt.Fprintf(&sb, "| Template %s head | %s |\n", branch, short(latest))
	if behind != "" {
		fmt.Fprintf(&sb, "| Commits behind | %s |\n", behind)
	}
	fmt.Fprintf(&sb, "| Status | %s |\n", status)
	fh, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer fh.Close()
	_, _ = fh.WriteString(sb.String())
}
