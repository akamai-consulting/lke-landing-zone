package main

// drift.go is the template-drift check (formerly template-scripts/
// check-template-drift.sh, now retired). It reads the committed
// .template-version (written by stampTemplateVersion / `llz env add` / `llz
// upgrade`), resolves the template repo's current branch head, and reports
// whether the instance is behind. Report-only by default; --strict exits 1 on
// drift so a scheduled job can gate. The Scheduled Checks template-drift job
// runs this via the llz baked into TF_IMAGE.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func runDrift(branch, repoURL string, strict bool) error {
	if branch == "" {
		branch = "main"
	}
	// Extension output drift is a SEPARATE delivery channel from the copier template, so it
	// is reported first and unconditionally — an instance can carry extensions without a
	// .template-version (e.g. one not created via `llz new`), and its scaffolded files
	// still drift. Report-only.
	lifecycleDrift(gopts, ".")

	b, err := os.ReadFile(".template-version")
	if err != nil {
		return fmt.Errorf("no .template-version found — run `llz env add` or `llz upgrade` first")
	}
	var tv templateVersion
	if err := json.Unmarshal(b, &tv); err != nil {
		return fmt.Errorf("malformed .template-version: %w", err)
	}
	if tv.TemplateRepo == "" || tv.TemplateSHA == "" {
		return fmt.Errorf("malformed .template-version (missing template_repo/template_sha)")
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

	latest := gitOut("ls-remote", repoURL, "refs/heads/"+branch)
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

	if tv.TemplateSHA == latest {
		fmt.Printf("%s Up to date with %s@%s (%s).\n", green("✓"), tv.TemplateRepo, branch, short(latest))
		emitDriftSummary(tv, branch, latest, "", "✅ up to date")
		return nil
	}

	behind := ""
	if commitReachable(tv.TemplateSHA) && commitReachable(latest) {
		behind = gitOut("rev-list", "--count", tv.TemplateSHA+".."+latest)
	}
	msg := fmt.Sprintf("behind %s@%s", tv.TemplateRepo, branch)
	if behind != "" {
		msg = behind + " commit(s) " + msg
	}
	fmt.Printf("%s instance at %s, %s head at %s — %s.\n",
		yellow("Template drift:"), short(tv.TemplateSHA), branch, short(latest), msg)
	if compareURL != "" {
		fmt.Printf("%s %s\n", dim("Compare:"), cyan(compareURL))
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

func commitReachable(sha string) bool {
	_, err := execOutput("git", "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func emitDriftSummary(tv templateVersion, branch, latest, behind, status string) {
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
	fmt.Fprintf(&sb, "| Instance stamped at | %s |\n", firstNonEmpty(tv.StampedAt, "unknown"))
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
