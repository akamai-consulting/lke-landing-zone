// Package ghapi reads the GitHub API as JSON, through the `gh` CLI.
//
// IT IS DELIBERATELY NOT internal/shared/ghcli. That package's own doc opens
// "Package ghcli is the `gh` command line as llz SHOWS it, not as llz runs it" --
// it owns the argv a dry-run prints and the quoting that makes it copy-pasteable,
// and execution lives elsewhere on purpose. This executes, so it is its own
// package rather than a violation of that split.
//
// IT CAME OUT OF internal/extensions/buildpreflight, where three peers were
// reaching for it. Reading a paginated API response is not a build-preflight
// concern; preflight was simply the first thing that needed it.
//
// THE --slurp FALLBACK IS THE PART WITH A SCAR IN IT, preserved verbatim below: an
// older `gh` rejects the flag at parse time with exit 1 and NO stdout, which used
// to fall through to a decode of empty output and surface as "could not determine
// whether the passphrase exists" -- with no way forward for an operator on a
// distro-packaged gh.
package ghapi

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghcli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// GHAPIJSON runs `gh api <path>` and decodes the response into out. Package var
// so tests substitute the whole GitHub round-trip.
var GHAPIJSON = func(path string, out any) error {
	b, err := kubectlprobe.Exec("gh", "api", path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// GHAPIJSONPaged is GHAPIJSON over every page. `--paginate --slurp` yields an
// ARRAY of page objects, so the caller's shape is decoded per page and the named
// list fields concatenated — a single-object decode would silently keep page 1.
var GHAPIJSONPaged = func(path string, out any) error {
	b, err := kubectlprobe.Exec("gh", "api", "--paginate", "--slurp", path)
	if err != nil {
		// `--slurp` is a recent `gh api` flag. An older gh rejects it at parse time
		// — exit 1, NO stdout — so this is the only place that case is visible. It
		// used to fall through to a decode of empty output and surface as "could not
		// determine whether the passphrase exists", with no way forward for an
		// operator on a distro-packaged gh. Retry unpaged: one page is worse than a
		// hard stop, and the list this serves is short.
		if strings.Contains(err.Error(), "unknown flag") || strings.Contains(err.Error(), "unknown shorthand") {
			return GHAPIJSON(path, out)
		}
		return err
	}
	// --slurp ALWAYS wraps, including a single page, so a non-array here means the
	// shape changed rather than "this gh did not slurp".
	var pages []json.RawMessage
	if err := json.Unmarshal(b, &pages); err != nil {
		return fmt.Errorf("gh api --slurp %s: expected an array of pages: %w", path, err)
	}
	return MergeJSONPages(pages, out)
}

// MergeJSONPages decodes each page into a fresh copy of out's type, appends every
// SLICE field, and takes the LAST non-zero value of every other field.
//
// Reflection rather than a per-caller merge because the alternative is each caller
// hand-rolling pagination, which is how page-1 truncation gets reintroduced.
//
// Non-slice fields are carried rather than dropped: the first cut only appended
// slices, which silently zeroed `total_count` — the one field you would use to
// CHECK pagination was complete. Scalars are page-invariant on GitHub list
// endpoints (every page repeats the same total), so last-non-zero is well defined.
//
// Limitation, stated rather than discovered later: out must point at a STRUCT, so
// this cannot serve the gh endpoints returning a top-level array. Those need a
// slice-shaped variant; there is no caller for one yet.
func MergeJSONPages(pages []json.RawMessage, out any) error {
	dst := reflect.ValueOf(out)
	if dst.Kind() != reflect.Ptr || dst.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("MergeJSONPages: out must be a pointer to struct, got %T", out)
	}
	for _, p := range pages {
		page := reflect.New(dst.Elem().Type())
		if err := json.Unmarshal(p, page.Interface()); err != nil {
			return err
		}
		for i := 0; i < dst.Elem().NumField(); i++ {
			f, pf := dst.Elem().Field(i), page.Elem().Field(i)
			if !f.CanSet() {
				continue // unexported
			}
			if f.Kind() == reflect.Slice {
				f.Set(reflect.AppendSlice(f, pf))
				continue
			}
			if !pf.IsZero() {
				f.Set(pf)
			}
		}
	}
	return nil
}

// ── DOES THIS REPO EXIST, AND WHAT TO DO WHEN IT DOES NOT ────────────────────
//
// Out of internal/extensions/onboard, where `newinstance` was importing the whole
// wizard to ask whether a repo is reachable.
//
// THE REMEDIATION MOVED WITH THE CHECK, deliberately. GitHub 404s a private repo
// you cannot see exactly as it 404s one that does not exist, so "create it" is the
// wrong first move for an operator simply authed as the wrong account -- and an
// absent OWNER needs a different first step again, because `gh repo create` makes
// a repository and never the org that holds it. That reasoning is what makes the
// boolean useful, and a caller handed only the boolean would have to reinvent it.

// RepoStatus is RepoExists with the third answer kept: (false, nil) means GitHub
// answered 404, while a non-nil error means llz could not ask at all — `gh`
// absent, unauthenticated, offline, rate-limited. Collapsing those two into
// "false" is fine for a readiness table (every row reads "missing" either way)
// but wrong for a preflight that then blames the repo for gh's problem.
//
// A 404 is "not there, OR not visible to this login" — GitHub hides private
// repos behind the same status rather than admitting they exist. Callers must
// word it that way: an operator authed as the wrong account, or with a token
// missing the repo scope, is told to create a repository that is already there,
// and `gh repo create` then dead-ends on "Name already exists on this account".
func RepoStatus(repo string) (bool, error) {
	// kubectlprobe.LookPathFn, not exec.LookPath directly: this used to sit behind
	// onboard's own execLookPath seam, and calling the stdlib would have removed a
	// swap point three tests rely on. The shared seam is the one every other
	// package here already stubs.
	if _, err := kubectlprobe.LookPathFn("gh"); err != nil {
		return false, fmt.Errorf("the GitHub CLI is not on PATH: %w", err)
	}
	if _, err := kubectlprobe.Exec("gh", "api", "repos/"+repo, "--silent"); err != nil {
		if ghcli.NotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// RepoExists reports whether the GitHub repo is reachable (it exists and the
// authenticated token can see it). The usual cause of a doctor/tokens run where
// every secret reads "missing" is that the natural `llz new` (without --push) →
// `llz tokens` sequence never created the remote repo.
func RepoExists(repo string) bool {
	found, err := RepoStatus(repo)
	return found && err == nil
}

// RequireInstanceRepo gates `llz tokens` / `llz doctor` on the live instance repo
// — both read and write it, so neither means anything without it. It keeps the
// two failures apart for the same reason `llz new`'s preflight does: a `gh` that
// cannot answer (missing, unauthenticated, offline, rate-limited) used to be
// reported as "instance repo not found on GitHub", sending the operator off to
// re-create a repo that was never missing. Skipped entirely when gh is absent —
// doctor's own tooling table is where that belongs, and `llz tokens` fails on it
// soon enough.
func RequireInstanceRepo(instanceRepo string) error {
	if !kubectlprobe.Lookable("gh") {
		return nil
	}
	found, err := RepoStatus(instanceRepo)
	switch {
	case err != nil:
		return ghcli.UnreachableErr(instanceRepo, err,
			"This does NOT mean the repo is missing — nothing was checked. Re-run once gh can answer")
	case !found:
		RemediateMissingRepo(instanceRepo)
		return fmt.Errorf("instance repo %s is not visible to your `gh` login (absent, or private to an account you are not authed as)", instanceRepo)
	}
	return nil
}

// RemediateMissingRepo prints the exact fix for an absent instance repo so the
// failure is actionable instead of an all-missing readiness table.
func RemediateMissingRepo(repo string) {
	fmt.Fprintf(os.Stderr, "\n%s instance repo %q is not reachable on GitHub.\n", color.Red("✗"), repo)
	fmt.Fprintln(os.Stderr, "  `llz tokens` and `llz doctor` read/write the live repo, so it must exist and be pushed first.")
	// GitHub 404s a private repo you cannot see exactly as it 404s one that does
	// not exist, so "create it" is the wrong first move for an operator who is
	// simply authed as the wrong account — `gh repo create` would then dead-end
	// on "Name already exists on this account".
	fmt.Fprintf(os.Stderr, "  If it DOES exist, you are authed as an account that cannot see it: gh auth status --hostname %s\n", ghcli.Host())
	// An absent OWNER is the more common cause and needs a different first step —
	// `gh repo create` makes a repository, never the org that holds it, so
	// printing the create line alone sends the operator into a bare
	// "does not have the correct permissions to execute `CreateRepository`".
	// Spelling comes first: a user owner they can log in as always exists, so an
	// absent one is far more often a typo in instance_repo than an uncreated org.
	if owner, _, ok := strings.Cut(repo, "/"); ok {
		if kind, err := ghcli.OwnerKindFn(owner); err == nil && kind == "" {
			fmt.Fprintf(os.Stderr, "  The OWNER %q does not exist either — check how it is spelled in .copier-answers.yml,\n", owner)
			fmt.Fprintf(os.Stderr, "  or, if that org is simply not created yet: https://%s/organizations/new\n", ghcli.Host())
		}
	}
	// `--source . --push` pushes whatever branch is checked out, and `git init`
	// still names the first one `master` unless init.defaultBranch says otherwise.
	// `llz new --push` normalises that itself (ensureScaffoldBranch); a remediation
	// the operator types by hand in some existing checkout does not, and a repo
	// whose content lands on `master` leaves the platform-bootstrap Application
	// asking Argo CD for a revision that is not there.
	fmt.Fprintln(os.Stderr, "  Create + push it from the instance directory — on `main`, which the platform-bootstrap")
	fmt.Fprintln(os.Stderr, "  Application tracks (apps_repo_revision); `git branch -M main` first if you are on master:")
	fmt.Fprintf(os.Stderr, "    gh repo create %s --private --source . --remote origin --push\n", repo)
	fmt.Fprintln(os.Stderr, "  …or re-scaffold with push next time: `llz new <name> --push --yes`.")
}
