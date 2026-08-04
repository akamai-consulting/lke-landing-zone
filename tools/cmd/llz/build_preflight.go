package main

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
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// ghAPIJSON runs `gh api <path>` and decodes the response into out. Package var
// so tests substitute the whole GitHub round-trip.
var ghAPIJSON = func(path string, out any) error {
	b, err := execOutput("gh", "api", path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// buildPreflight verifies the deployment the dispatch names is present, locally
// and on the branch CI builds from. A returned error blocks the dispatch; every
// unknown degrades to nil (or an advisory line) so this can only ever fail a
// build that was already going to fail.
func buildPreflight(env string) error {
	tfDir, _, _ := instanceLayout()
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
	return remoteDeploymentPresent(specRoot, env)
}

// knownLocally reports whether the local spec declares env. The error case is a
// spec that fails to PARSE: it yields an empty deployment list, and reporting
// that as "no deployment <env>" would blame the argument for a broken file — the
// same wrong-diagnosis-from-a-failed-lookup mistake ghFileSHA made.
func knownLocally(tfDir, env string) (bool, error) {
	if _, _, err := loadSpec(); err != nil {
		return false, fmt.Errorf("the LandingZone spec does not load, so the deployment set is unknown: %w\n"+
			"  fix it (`llz doctor --env %s` reports the details), then re-run", err, env)
	}
	names, err := listDeployments(tfDir)
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
// Absent there (or unanswerable) → the typo `validateEnvName` cannot catch,
// because deployment names are free-form and the dispatch input is a plain
// string that GitHub accepts unconditionally.
func unknownDeploymentErr(tfDir, env string) error {
	if repo, branch, ok := buildBranch(); ok {
		if _, found, answered := ghFileSHA(repo, clusterspec.EnvironmentsDir+"/"+env+".yaml", branch); answered && found {
			fmt.Fprintf(os.Stderr, "%s %q is not in your checkout but IS on %s's %s branch — the build uses the pushed tree, so this dispatch is fine (%s to see it locally).\n",
				yellow("!"), env, repo, branch, cyan("git pull"))
			return nil
		}
	}
	names, _ := listDeployments(tfDir)
	have := "none are scaffolded yet"
	if len(names) > 0 {
		have = "this instance has: " + strings.Join(names, ", ")
	}
	return fmt.Errorf("no deployment %q in this instance (%s), and none on the branch the build reads.\n"+
		"  • building an existing one?  check the name — `llz env list`\n"+
		"  • someone else added it?     %s\n"+
		"  • adding a new one?          %s",
		env, have, cyan("git pull"),
		cyan("llz env add "+env+" --region <region> --obj-cluster <obj-cluster>"))
}

// buildBranch resolves the repo + default branch a dispatch would run from.
// ok=false whenever that cannot be established — no gh, no resolvable repo, an
// unreachable or not-yet-created repo — in which case callers must not draw any
// conclusion from the absence of an answer.
func buildBranch() (repo, branch string, ok bool) {
	if !lookable("gh") {
		return "", "", false
	}
	repo, err := resolveInstanceRepo("", false)
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
				yellow("!"), repo)
			fmt.Fprintf(os.Stderr, "  create + push it (%s), or fix the auth `llz doctor` reports.\n",
				cyan("gh repo create "+repo+" --private --source . --remote origin --push"))
			return nil
		}
		fmt.Fprintf(os.Stderr, "%s could not resolve %s's default branch — dispatching without the spec check.\n",
			yellow("!"), repo)
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
				yellow("!"), rel, repo, branch)
			return nil
		}
		if !found {
			return notOnBuildBranchErr(rel, repo, branch, env)
		}
		// Present but stale: the build will run the pushed spec, not the edits in
		// the working tree. Advisory — building an older revision on purpose is
		// legitimate, and this is exactly what a promotion flow looks like.
		local := gitOut("hash-object", filepath.Join(specRoot, filepath.FromSlash(rel)))
		if local != "" && remoteSHA != "" && local != remoteSHA {
			fmt.Fprintf(os.Stderr, "%s %s on %s differs from your working copy — the build uses the PUSHED one (%s).\n",
				yellow("!"), rel, branch, publishHint(branch))
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
	cur := gitOut("rev-parse", "--abbrev-ref", "HEAD")
	if cur == "" || cur == "HEAD" || cur == defaultBranch {
		return cyan("git push")
	}
	return cyan(fmt.Sprintf("git push -u origin %s", cur)) +
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
	if err := ghAPIJSON("repos/"+repo, &r); err != nil {
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
	// or environments/<env>.yaml, and env is bound by validateEnvName.)
	err := ghAPIJSON("repos/"+repo+"/contents/"+path+"?ref="+url.QueryEscape(ref), &r)
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
