package buildpreflight

// build_preflight.go — check that the deployment exists WHERE THE BUILD LOOKS.
//
// `llz build` is a `gh workflow run terraform.yml` dispatch: GitHub runs the
// workflow from the repo's default branch, and the job renders the tfvars from
// the spec IN THAT CHECKOUT. So the only spec that matters at build time is the
// pushed one — the local tree is never read.
//
// The quickstart flow ends `llz env add lab` → `llz doctor --env lab` →
// `llz up lab --yes`, and `env add` COMMITS the spec but deliberately does not
// push (pushing is the operator's call). Nothing between there and the dispatch
// looks at the remote, so the default path for a first-time adopter — commit
// made, push forgotten — reaches GitHub as a build for a deployment the checked-
// out tree has never heard of. It fails minutes later, in CI, with a render
// error about a missing environments/<env>.yaml, having burned an Actions run
// and (on a partial apply) real Linode resources.
//
// So ask the remote the one question that decides the outcome: is this
// deployment on the branch the build will run from? Best-effort — without `gh`,
// a resolvable repo, or a spec, this says nothing and the dispatch proceeds
// exactly as before.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// GHAPIJSON runs `gh api <path>` and decodes the response into out. Package var
// so tests substitute the whole GitHub round-trip.
var GHAPIJSON = func(path string, out any) error {
	b, err := execOutput("gh", "api", path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// GHAPIJSONPaged is GHAPIJSON over every page. `--paginate --slurp` yields an
// ARRAY of page objects, so the caller's shape is decoded per page and the named
// list fields concatenated — a single-object decode would silently keep page 1.
var GHAPIJSONPaged = func(path string, out any) error {
	b, err := execOutput("gh", "api", "--paginate", "--slurp", path)
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

// Run verifies the deployment the dispatch names is present, locally
// and on the branch CI builds from. A returned error blocks the dispatch; every
// unknown degrades to nil (or an advisory line) so this can only ever fail a
// build that was already going to fail.
func Run(env string) error {
	tfDir, aplDir, _ := instancelayout.Detect()
	specRoot := filepath.Dir(tfDir)
	if !clusterspec.InstancePresent(specRoot) {
		// No spec: either not an instance checkout, or a pre-spec instance whose
		// tracked tfvars are the source of truth. Nothing to compare.
		return nil
	}
	known, err := knownLocally(tfDir, env)
	if err != nil {
		return err
	}
	if !known {
		// Absent from THIS checkout is not the same as absent from the build.
		// A colleague can add a deployment, push it, and be building it while your
		// clone has never seen it — and the build reads the pushed tree, not
		// yours. Refusing that on local evidence alone would block a build that
		// works, and "run `llz env add`" would then author a duplicate.
		return unknownDeploymentErr(tfDir, env)
	}
	if err := remoteDeploymentPresent(specRoot, env); err != nil {
		return err
	}
	warnUnpublishedEdits(specRoot, aplDir, env)
	return nil
}

// warnUnpublishedEdits names spec/overlay files that are edited but not committed.
//
// remoteDeploymentPresent compares the two SPEC files against the build branch,
// which catches "committed but never pushed" for them. It cannot catch the state
// the quickstart actually walks an operator into: `llz env add` commits the spec
// AND the overlay up front, and everything that comes after it — filling the
// placeholders `env add` just listed, `llz env set`, `llz spec set`, `llz env
// edit` — writes files that NOTHING commits. Those edits sit in the working tree
// while the dispatch builds the pushed tree, so the fix the operator just made is
// silently absent from the cluster they get. Nothing failed; it just built the
// wrong thing, which is the expensive way to find out.
//
// Advisory, like the stale-file warning above and for the same reason: building
// an older revision on purpose is legitimate. Name what will be left behind and
// get out of the way.
func warnUnpublishedEdits(specRoot, aplDir, env string) {
	// THIS deployment's files only. The whole environments/ directory would flag a
	// half-drafted environments/other.yaml while building `env` — and then
	// prescribe `git add -A && git commit && git push`, which publishes that
	// unfinished deployment into the spec `llz-discover-deployments.yml`
	// enumerates, pulling it into rotation and the scheduled health checks.
	// remoteDeploymentPresent already scopes to environments/<env>.yaml; match it.
	//
	// _shared/apl-overlay IS in scope: `llz render` writes it (obj.yaml, apps.yaml)
	// and the build reads the pushed copy, so a re-render left uncommitted there is
	// the same silent-stale-build class as the per-env overlay.
	paths := []string{
		filepath.Join(specRoot, clusterspec.LandingZoneFile),
		filepath.Join(specRoot, clusterspec.EnvironmentsDir, env+".yaml"),
		filepath.Join(aplDir, env),
		filepath.Join(aplDir, "_shared"),
	}
	dirty := GitOut(append([]string{"status", "--porcelain", "--"}, paths...)...)
	if dirty == "" {
		// Also the answer outside a git repo, or when git is unavailable: no
		// evidence of an unpublished edit is not evidence of one.
		return
	}
	fmt.Fprintf(os.Stderr, "%s uncommitted spec/overlay changes — the build reads the COMMITTED + pushed tree, so these are NOT in it:\n", color.Yellow("!"))
	for _, line := range strings.Split(dirty, "\n") {
		fmt.Fprintf(os.Stderr, "      %s\n", color.Dim(strings.TrimSpace(line)))
	}
	// Stage exactly the paths that were scanned. `git add -A` is worktree-wide
	// while this warning is scoped, so prescribing it would sweep in whatever else
	// happens to be dirty — including, on the environments/ side, a half-drafted
	// deployment that publishing pulls into rotation and the scheduled checks.
	// existingPaths, because `git status --porcelain -- <missing>` ignores an
	// unmatched pathspec while `git add -- <missing>` is fatal (exit 128) and stages
	// NOTHING. apl-values/<env> is genuinely absent after an HA-deferred `env add`,
	// so the prescribed fix would have aborted in exactly the state that most needs
	// it. Same guard scaffold.go already applies before committing generated files.
	if stage := existingPaths(paths); len(stage) > 0 {
		fmt.Fprintf(os.Stderr, "  publish them first: %s\n",
			color.Cyan(fmt.Sprintf(`git add -- %s && git commit -m "llz: fill deployment values" && git push`,
				strings.Join(stage, " "))))
	}
}

// knownLocally reports whether the local spec declares env. The error case is a
// spec that fails to PARSE: it yields an empty deployment list, and reporting
// that as "no deployment <env>" would blame the argument for a broken file — the
// same wrong-diagnosis-from-a-failed-lookup mistake ghFileSHA made.
func knownLocally(tfDir, env string) (bool, error) {
	if _, _, err := clusterspec.Detected(); err != nil {
		return false, fmt.Errorf("the LandingZone spec does not load, so the deployment set is unknown: %w\n"+
			"  fix it (`llz doctor --env %s` reports the details), then re-run", err, env)
	}
	names, err := envtopology.ListDeployments(tfDir)
	if err != nil {
		return true, nil // unreadable tree — not this gate's business
	}
	for _, n := range names {
		if n == env {
			return true, nil
		}
	}
	return false, nil
}

// unknownDeploymentErr decides what an unknown-here deployment means by asking
// the branch the build reads. Present there → a stale checkout, which is not an
// error: the dispatch will work, and the operator is told why they can't see it.
// Absent there (or unanswerable) → the typo `validate.EnvName` cannot catch,
// because deployment names are free-form and the dispatch input is a plain
// string that GitHub accepts unconditionally.
func unknownDeploymentErr(tfDir, env string) error {
	if repo, branch, ok := buildBranch(); ok {
		if _, found, answered := ghFileSHA(repo, clusterspec.EnvironmentsDir+"/"+env+".yaml", branch); answered && found {
			fmt.Fprintf(os.Stderr, "%s %q is not in your checkout but IS on %s's %s branch — the build uses the pushed tree, so this dispatch is fine (%s to see it locally).\n",
				color.Yellow("!"), env, repo, branch, color.Cyan("git pull"))
			return nil
		}
	}
	names, _ := envtopology.ListDeployments(tfDir)
	have := "none are scaffolded yet"
	if len(names) > 0 {
		have = "this instance has: " + strings.Join(names, ", ")
	}
	return fmt.Errorf("no deployment %q in this instance (%s), and none on the branch the build reads.\n"+
		"  • building an existing one?  check the name — `llz env list`\n"+
		"  • someone else added it?     %s\n"+
		"  • adding a new one?          %s",
		env, have, color.Cyan("git pull"),
		color.Cyan("llz env add "+env+" --region <region> --obj-cluster <obj-cluster>"))
}

// buildBranch resolves the repo + default branch a dispatch would run from.
// ok=false whenever that cannot be established — no gh, no resolvable repo, an
// unreachable or not-yet-created repo — in which case callers must not draw any
// conclusion from the absence of an answer.
func buildBranch() (repo, branch string, ok bool) {
	if !kubectlprobe.Lookable("gh") {
		return "", "", false
	}
	repo, err := answers.ResolveInstanceRepo("", false)
	if err != nil {
		return "", "", false
	}
	if b := ghDefaultBranch(repo); b != "" {
		return repo, b, true
	}
	return repo, "", false
}

// remoteDeploymentPresent checks the spec files the apply renders from —
// landingzone.yaml AND environments/<env>.yaml — on the instance repo's default
// branch, which is the tree the dispatched workflow checks out.
//
// BOTH files, not just the deployment's: the apply renders the tfvars from the
// assembled spec, so an unpushed `llz spec set` (acmeEmail, teams, shared
// networks) builds the OLD instance-level values just as silently as an unpushed
// deployment fails outright.
func remoteDeploymentPresent(specRoot, env string) error {
	repo, branch, ok := buildBranch()
	if !ok {
		if repo == "" {
			return nil // no gh, or no repo to resolve — nothing to say
		}
		// The repo did not answer. Overwhelmingly the reason is that it does not
		// exist yet — `llz new` without --push leaves exactly this state — so name
		// THAT rather than "could not resolve a default branch", which sounds like
		// a transient and sends the operator looking in the wrong place.
		if repoMissing(repo) {
			fmt.Fprintf(os.Stderr, "%s instance repo %s does not exist, or your `gh` login cannot see it — the build dispatches to it either way.\n",
				color.Yellow("!"), repo)
			fmt.Fprintf(os.Stderr, "  create + push it (%s), or fix the auth `llz doctor` reports.\n",
				color.Cyan("gh repo create "+repo+" --private --source . --remote origin --push"))
			return nil
		}
		fmt.Fprintf(os.Stderr, "%s could not resolve %s's default branch — dispatching without the spec check.\n",
			color.Yellow("!"), repo)
		return nil
	}
	// The deployment's own file first: when a first-ever instance has pushed
	// NEITHER, "deployment lab is not on main" is the message that matches what
	// the operator just did, and blaming landingzone.yaml would read as a
	// different, more alarming problem.
	for _, rel := range []string{
		// Slash-joined: these are repo paths for the GitHub API, not disk paths.
		clusterspec.EnvironmentsDir + "/" + env + ".yaml",
		clusterspec.LandingZoneFile,
	} {
		remoteSHA, found, ok := ghFileSHA(repo, rel, branch)
		if !ok {
			// The question could not be asked — offline, rate-limited, a 5xx, an
			// unauthenticated gh. That is NOT evidence of absence, and blocking a
			// legitimate build on it would be worse than the failure this gate
			// prevents. Say so and get out of the way.
			fmt.Fprintf(os.Stderr, "%s could not check whether %s is on %s's %s branch — dispatching anyway.\n",
				color.Yellow("!"), rel, repo, branch)
			return nil
		}
		if !found {
			return notOnBuildBranchErr(rel, repo, branch, env)
		}
		// Present but stale: the build will run the pushed spec, not the edits in
		// the working tree. Advisory — building an older revision on purpose is
		// legitimate, and this is exactly what a promotion flow looks like.
		local := GitOut("hash-object", filepath.Join(specRoot, filepath.FromSlash(rel)))
		if local != "" && remoteSHA != "" && local != remoteSHA {
			fmt.Fprintf(os.Stderr, "%s %s on %s differs from your working copy — the build uses the PUSHED one (%s).\n",
				color.Yellow("!"), rel, branch, publishHint(branch))
		}
	}
	return nil
}

// notOnBuildBranchErr explains an absent spec file and — the part that has to be
// right — how to get it onto the branch that matters.
func notOnBuildBranchErr(rel, repo, branch, env string) error {
	what := fmt.Sprintf("the instance spec (%s)", rel)
	if rel != clusterspec.LandingZoneFile {
		what = fmt.Sprintf("deployment %q (%s)", env, rel)
	}
	//lint:ignore ST1005 multi-line operator diagnostic: the trailing period closes an embedded remediation block, not a sentence fragment
	return fmt.Errorf("%s is not on %s's %s branch — that is the tree the build checks out.\n"+
		"  `llz env add` commits the spec locally; publishing it is still yours to do:\n"+
		"      %s\n"+
		"  (the apply renders the tfvars from the PUSHED spec, so a dispatch without it fails in CI.)\n"+
		"  Dispatching anyway is `%s`.",
		what, repo, branch, publishHint(branch),
		"llz build "+env+" --yes --skip-preflight")
}

// publishHint is the command that actually publishes the spec, which is NOT
// always `git push`: a workflow_dispatch runs terraform.yml from the repo's
// DEFAULT branch, so an operator working on a feature branch can push all day
// and never move the tree the build reads. Name the branch they are on.
func publishHint(defaultBranch string) string {
	cur := GitOut("rev-parse", "--abbrev-ref", "HEAD")
	if cur == "" || cur == "HEAD" || cur == defaultBranch {
		return color.Cyan("git push")
	}
	return color.Cyan(fmt.Sprintf("git push -u origin %s", cur)) +
		fmt.Sprintf(" — then merge it into %s; the dispatch always runs from %s, not from %s",
			defaultBranch, defaultBranch, cur)
}

// repoMissing reports whether GitHub answered 404 for the repo itself.
//
// Distinct from tokens.go's repoExists, which returns false for ANY `gh api`
// error: that is fine for a readiness table, but here it would put "create the
// repo" in front of an operator whose repo exists and whose token merely cannot
// see it — advice that fails with "already exists" and sends them the wrong way.
// A 404 is the only answer that means absent, and even then the token may just
// lack visibility, which the message now says.
func repoMissing(repo string) bool {
	_, err := execOutput("gh", "api", "repos/"+repo, "--silent")
	return err != nil && isNotFoundErr(err)
}

// ghDefaultBranch returns the repo's default branch ("" when unresolvable).
func ghDefaultBranch(repo string) string {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := GHAPIJSON("repos/"+repo, &r); err != nil {
		return ""
	}
	return r.DefaultBranch
}

// ghFileSHA returns the git blob SHA of path on ref.
//
// The two failure modes must not be conflated. found=false means GitHub answered
// and the file is not there — the thing this gate exists to catch. ok=false means
// the question could not be asked at all (offline, unauthenticated gh, a 5xx,
// secondary rate limiting), and reading THAT as absence would block a build that
// is perfectly fine, on evidence we never had.
func ghFileSHA(repo, path, ref string) (sha string, found, ok bool) {
	var r struct {
		SHA string `json:"sha"`
	}
	// The ref is query data, and a branch name may legally contain characters a
	// query string reserves: `feat/#123` would silently truncate at the `#` and
	// ask about the DEFAULT ref instead — answering a different question than the
	// one the caller asked. (The path needs no escaping: rel is landingzone.yaml
	// or environments/<env>.yaml, and env is bound by validate.EnvName.)
	err := GHAPIJSON("repos/"+repo+"/contents/"+path+"?ref="+url.QueryEscape(ref), &r)
	if err != nil {
		return "", false, isNotFoundErr(err)
	}
	return r.SHA, r.SHA != "", true
}

// isNotFoundErr reports whether a `gh api` failure was an actual 404. gh writes
// "gh: Not Found (HTTP 404)" to stderr, which execOutput attaches to the error;
// anything else (network, 401/403, 5xx) is an unanswered question, not an answer.
//
// It matches the STATUS token, not a bare "404" or "Not Found". Those turn up in
// text that has nothing to do with the status — a port, a request id, a proxy's
// own message — and reading one as "the file is absent" would block a build on a
// transient, which is the failure this whole split exists to avoid.
func isNotFoundErr(err error) bool {
	return strings.Contains(err.Error(), "HTTP 404")
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

// GitOut runs git and returns trimmed stdout, "" on failure. A local copy: the
// sustain extraction took the original with stamp.go, and build_preflight is the
// only other caller. Three lines around execOutput, nothing to drift.
func GitOut(args ...string) string {
	out, err := execOutput("git", args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
