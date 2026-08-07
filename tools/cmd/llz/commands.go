package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/buildpreflight"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/converge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/copier"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/envadd"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/envdef"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghcli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/instanceresolve"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/onboard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/proc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/reachability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/templateid"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/validate"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/color"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/sustain"
)

// ── argv builders (pure; covered by commands_test.go) ────────────────────────

func buildArgv(env string) []string {
	return []string{"gh", "workflow", "run", "terraform.yml",
		"--field", "region=" + env, "--field", "action=apply", "--field", "module=all"}
}

// statusArgv is the read-only convergence check set (matches the verify steps in
// docs/runbooks/bootstrap-openbao.md).
//
// The OpenBao namespace comes from converge.OpenbaoNamespace, not a literal. It WAS the
// literal "openbao", which has not existed since the platform namespaces were
// llz- prefixed — so `llz status <env>` reported nothing for the OpenBao pods and
// looked like a cluster with none, on every invocation. Same class as the three
// stale entries in healthNamespaces (#242).
func statusArgv() [][]string {
	return [][]string{
		{"kubectl", "-n", converge.OpenbaoNamespace, "get", "pods"},
		{"kubectl", "-n", "argocd", "get", "applications"},
		{"kubectl", "-n", "external-secrets", "get", "clustersecretstore"},
	}
}

// ── execution helpers ────────────────────────────────────────────────────────

// runGated is run() for cloud-mutating commands: it refuses to execute without
// --yes, printing the command instead so the operator can see exactly what would
// reach Linode/GitHub.
func runGated(g globalOpts, argv ...string) error {
	if g.dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) "+ghcli.Quote(argv))
		return nil
	}
	if !g.yes {
		fmt.Fprintln(os.Stderr, "would run: "+ghcli.Quote(argv))
		fmt.Fprintln(os.Stderr, "  (re-run with --yes to execute)")
		return nil
	}
	return proc.RunEcho(g.dryRun, argv...)
}

// ── commands ─────────────────────────────────────────────────────────────────

// templateSourceStatusFn reports whether the --org template source is reachable
// on GitHub; seamed for tests. runNew preflights it because copier clones
// gh:<org>/<template> over HTTPS, and a 404 there (typo'd/un-forked --org)
// surfaces as an interactive `Username for 'https://github.com':` onboard.Prompt rather
// than a clear error — the failure mode adopters actually hit.
var templateSourceStatusFn = onboard.RepoStatus

// templateUnreachableTail states what an unanswerable lookup does NOT prove about
// the --org template source. The default upstream is public, so gh is the only
// candidate; a fork named by --org may be private, where "it's public" would be a
// false claim and the fix is a login that can see it.
func templateUnreachableTail(org, repo string) string {
	if org == templateid.DefaultOrg {
		return "This is NOT a missing template — " + repo + " is public. Re-run `llz new` once gh can answer"
	}
	return "This does NOT mean " + repo + " is missing — a private fork also needs a `gh` login that can see it.\n" +
		"  Re-run `llz new` once gh can answer"
}

// missingTemplateSourceErr explains an absent --org template source: --org names
// the template to scaffold FROM (default: the public upstream), not where the
// instance lands, so the fix is to use the upstream or fork the template first.
func missingTemplateSourceErr(org string) error {
	return fmt.Errorf("template source %s/%s not found on GitHub (or not visible to your `gh` login).\n"+
		"  --org names the template to scaffold FROM, not where your instance lands.\n"+
		"  • scaffold from the public upstream:  llz new <dir> --org %s --push --yes\n"+
		"  • or fork the template there first:   gh repo fork %s/%s --org %s",
		org, templateid.Name, templateid.DefaultOrg, templateid.DefaultOrg, templateid.Name, org)
}

// checkNewTarget refuses to scaffold over a directory that already has content.
// `copier copy` into a populated dir does not stop — it renders on top and
// prompts per conflicting file — so the natural retry (`llz new my-instance`
// again, after a half-finished first run, or to "update" an instance) merges a
// fresh scaffold into a live instance instead of failing. An existing instance is
// updated with `llz upgrade`; a fresh one goes in an empty directory.
func checkNewTarget(dir string) error {
	ents, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return nil // absent — the normal case
	case err != nil:
		// Exists but unreadable (permissions, a dead symlink, an I/O error). Not
		// "empty": copier would fail on it too, later and less legibly.
		return fmt.Errorf("cannot read %s: %w", dir, err)
	}
	if !instanceresolve.IsInstanceRoot(dir) {
		// Hidden entries don't count as content: scaffolding into a freshly cloned
		// empty repo (only .git) is a legitimate path, and copier git-inits anyway.
		visible := 0
		for _, e := range ents {
			if !strings.HasPrefix(e.Name(), ".") {
				visible++
			}
		}
		if visible == 0 {
			return nil
		}
	}
	if instanceresolve.IsInstanceRoot(dir) {
		return fmt.Errorf("%s is already a landing-zone instance — `llz new` would render a second scaffold over it.\n"+
			"  • add a deployment to it:     %s\n"+
			"  • move it to a new release:   %s\n"+
			"  • really want a separate instance? scaffold it into a new directory",
			dir, color.Cyan("cd "+dir+" && llz env add <env> --region <region> --obj-cluster <obj-cluster>"),
			color.Cyan("cd "+dir+" && llz self-update && llz upgrade"))
	}
	return fmt.Errorf("%s already exists and is not empty — copier would render the scaffold on top of it.\n"+
		"  Pick an empty directory (or remove that one) and re-run", dir)
}

func runNew(g globalOpts, org, ref, dir string, push bool) error {
	if err := checkNewTarget(dir); err != nil {
		return err
	}
	// Before the GitHub round-trips below: copier is what actually renders the
	// scaffold, the check is free and local, and finding out it is missing AFTER
	// resolving a release tag wastes two API calls to reach the same dead end.
	if err := copier.Require(g.dryRun, "`llz new`"); err != nil {
		return err
	}
	repo := org + "/" + templateid.Name
	switch found, err := templateSourceStatusFn(repo); {
	case err != nil:
		return ghcli.UnreachableErr(repo, err, templateUnreachableTail(org, repo))
	case !found:
		return missingTemplateSourceErr(org)
	}
	ref, err := copier.Ref(ref, repo)
	if err != nil {
		return err
	}
	fmt.Printf("Scaffolding a new LKE landing-zone instance into %q from %s/%s@%s\n\n",
		dir, org, templateid.Name, ref)

	if err := proc.RunEcho(g.dryRun, copier.CopyArgv(org, ref, dir)...); err != nil {
		return fmt.Errorf("copier copy: %w", err)
	}

	// Arm the pre-commit hook in the freshly scaffolded instance. Best-effort:
	// `copier copy` git-inits the dir, but don't fail `new` if hook install does.
	if err := runHooksInstall(g, dir); err != nil {
		fmt.Fprintln(os.Stderr, "llz: could not arm pre-commit hook (run `llz hooks` in the instance):", err)
	}

	// Normalise the branch for EVERY scaffold, not just the --push one. copier's
	// `git init` names the first branch `master` unless the operator set
	// init.defaultBranch, and the rendered platform-bootstrap Application — plus
	// every carved App under it — asks Argo CD for `main`. An adopter who scaffolds
	// without --push and creates the repo by hand therefore built a tree whose Argo
	// revision does not exist: `llz render --check` passes, the cluster applies, and
	// it surfaces ~20 minutes later as an unresolvable revision inside the cluster.
	// The rename only touches a branch with no upstream, so it cannot rewrite an
	// instance that has deliberately been pushed elsewhere.
	// Best-effort: at this point copier has git-init'd but not committed, so HEAD
	// is unborn, and renaming an unborn branch needs git >= 2.30. On an older git
	// the rename errors — and this step is cosmetic until something is pushed, so
	// failing `llz new` over it would be worse than the drift it prevents.
	if err := ensureScaffoldBranch(g, dir); err != nil {
		fmt.Fprintf(os.Stderr, "%s could not rename the scaffold branch to %s (%v).\n", color.Yellow("!"), bootstrapBranch, err)
		fmt.Fprintf(os.Stderr, "  Do it before pushing — Argo CD tracks %s: %s\n",
			bootstrapBranch, color.Cyan("git -C "+dir+" branch -M "+bootstrapBranch))
	}

	pushed := false
	if push {
		var err error
		if pushed, err = pushInstanceRepo(g, dir); err != nil {
			return err
		}
	}

	printNextSteps(dir, pushed)
	return nil
}

// printNextSteps renders the post-scaffold guide: a color.Bold header, color.Dim context
// notes, and the ordered command sequence with color.Cyan commands + color.Dim, column-
// aligned `#` comments. Everything degrades to plain text off a TTY (color.go),
// and the lines stay copy-paste-safe (commands run; notes are shell comments).
//
// THIS LIST IS THE QUICKSTART, and has to be kept as one. It is the first thing
// an adopter reads after `llz new`, so it out-ranks docs/quickstart.md in
// practice — nobody opens a doc while a terminal is already telling them what to
// type. It had drifted from the quickstart in exactly the two ways that matter:
//
//   - no `git push`. `llz env add` commits and does not push, the build renders
//     from the pushed tree, and "committed, not pushed" was therefore the state
//     this list walked every adopter into. #405 named that one of the two
//     default-path slips a literal top-to-bottom read hits, and fixed it in
//     docs/quickstart.md; the copy of the same sequence living in Go string
//     literals was outside that audit's corpus (Markdown) and kept its version.
//   - `llz validate --env`, which prints a deprecation notice and points at
//     `llz doctor --env`. `llz ci docs-guard` catches a deprecated flag in a
//     doc — there is a test asserting exactly this string — but the guard reads
//     Markdown, and this is not.
//
// Keeping the two in sync is a review obligation, not a mechanical one: there is
// no gate that reads both.
func printNextSteps(dir string, pushed bool) {
	cdNote := "commit + push to your GitHub repo (or re-run `llz new --push --yes`)"
	if pushed {
		cdNote = "instance repo created + pushed ✓"
	}

	// Trailing-comment alignment column, capped so a long command (or dir name)
	// doesn't shove every comment off to the right — overflowing lines just trail
	// with two spaces.
	const col = 32
	cmd := func(c, note string) {
		pad := col - len(c)
		if pad < 2 {
			pad = 2
		}
		fmt.Printf("  %s%s%s\n", color.Cyan(c), strings.Repeat(" ", pad), color.Dim("# "+note))
	}
	note := func(s string) { fmt.Println(color.Dim("  # " + s)) }

	fmt.Println("\n" + color.Bold("Next steps"))
	note("The declarative LandingZone spec is the source of truth — landingzone.yaml +")
	note("environments/<env>.yaml. `llz env add` authors them; see the committed")
	note("landingzone.yaml.example + docs/landing-zone-spec.md for the full model.")
	fmt.Println()
	cmd("cd "+dir, cdNote)
	cmd("llz env add <env> --region <linode-region> --obj-cluster <obj-cluster>", "authors the spec + renders")
	note("tune it: llz env set <env> cluster.nodePool.count=8  (or `llz env edit <env>`); llz env show <env>")
	// doctor BEFORE the push, which is the order the quickstart now teaches too.
	// `env add` commits its own output and nothing else commits at all, so an edit
	// made after the push — filling a placeholder, `llz env set`, `llz spec set` —
	// is simply absent from the tree the build renders. Pushing first made that the
	// DEFAULT outcome for anyone doctor sent back to fix something.
	cmd("llz doctor --env <env>", "the readiness gate — fix what it lists BEFORE publishing")
	cmd(`git add -A && git commit -m "llz: fill values" && git push`, "`env add` commits its own output; later edits are yours")
	cmd("llz up <env> --yes", "tokens → doctor → build, stopping at the first failure")
	note("`llz up` is interactive: it opens pre-filled links and reads a Linode PAT + GitHub PATs.")
	note("run the gates individually to inspect each one — llz tokens --env <env> --yes /")
	note("llz doctor --env <env> (the single readiness gate) / llz build <env> --yes")
	note("local checks: llz lint / llz validate; add your own commands in .llz/commands.yaml")
}

// The owner-kind seam moved to ghcli.OwnerKindFn with the function it wraps: two
// swappable vars over one call is the second-seam bug this campaign has now paid
// for twice. instanceRepoExistsFn reports whether the instance repo itself is
// already there (an adopter who created it by hand after a failed --push).
var (
	instanceRepoExistsFn = onboard.RepoExists
	ghLoginFn            = ghLogin
)

// ghLogin returns the authenticated `gh` login, or "" if it can't be resolved.
// Only used to make remediation text concrete ("chandraS/<name>"), never to gate.
func ghLogin() string {
	out, err := execOutput("gh", "api", "user", "--jq", ".login")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// missingRepoOwnerErr explains an instance_repo whose OWNER does not exist. This
// is the failure a first-time adopter actually hits: `gh repo create` creates a
// repository, never its org, so an absent owner surfaces as a bare "GraphQL:
// <login> does not have the correct permissions to execute `CreateRepository`"
// — which reads like a token-scope problem and names no next step.
//
// An absent owner splits two ways, and the fixes do NOT overlap: the name is the
// one they meant (create the org — a user owner they can log in as always exists)
// or the name is wrong (a typo, or they meant their own account). Only the first
// resumes from the scaffold on disk; correcting the name means re-scaffolding,
// because instance_repo is rendered INTO the workflows, not just recorded in
// .copier-answers.yml. Telling everyone to "create the org" would send a typo
// straight into a second wrong repo.
func missingRepoOwnerErr(repo, owner, dir, login string) error {
	mine := "<your-login>"
	if login != "" {
		mine = login
	}
	return fmt.Errorf("--push: GitHub owner %q does not exist (or your `gh` login cannot see it), so %s cannot be created.\n"+
		"  `gh repo create` creates a REPOSITORY, never its owner — the <owner> half of instance_repo must exist first.\n"+
		"  The scaffold in %s is complete and committed; only the push is left. Is %q the owner you meant?\n"+
		"  • yes — it's an org you        create it (named exactly %s): https://%s/organizations/new\n"+
		"    have not created yet:        gh repo create %s --private --source %s --remote origin --push\n"+
		"  • no — misspelled, or you      re-scaffold with the right answer: llz new <new-dir> --push --yes\n"+
		"    meant your own account:      (e.g. instance_repo %s/%s — a user owner exists already, nothing to create)\n"+
		"                                 instance_repo is rendered INTO the workflows, so correcting it means\n"+
		"                                 re-scaffolding — editing .copier-answers.yml alone is not enough",
		owner, repo, dir, owner, owner, ghcli.Host(), repo, dir, mine, envdef.ShortRepoName(repo))
}

// foreignUserOwnerErr covers an instance_repo owned by a DIFFERENT GitHub user.
// GitHub has no way to satisfy it — one user cannot create a repository inside
// another user's account, whatever the token's scopes — so `gh repo create` fails
// with the same bare CreateRepository error as an absent owner, and every
// "check your permissions" hint is a dead end. Reachable by a typo that happens
// to land on a real login, or by naming a colleague's account.
func foreignUserOwnerErr(repo, owner, dir, login string) error {
	return fmt.Errorf("--push: instance_repo owner %q is another GitHub USER's account (you are authenticated as %q).\n"+
		"  GitHub lets you create repositories in your own account or in an org you belong to — never in another\n"+
		"  user's account, at any token scope, so `gh repo create %s` cannot succeed. Pick one:\n"+
		"  • wrong owner?                 re-scaffold with the right answer: llz new <new-dir> --push --yes\n"+
		"                                 (e.g. instance_repo %s/%s — instance_repo is rendered INTO the workflows,\n"+
		"                                 so editing .copier-answers.yml alone is not enough)\n"+
		"  • sharing with that person?    use an org you both belong to as the owner, and re-scaffold\n"+
		"  • logged in as the wrong you?  gh auth switch --hostname %s --user %s, then from %s:\n"+
		"                                 gh repo create %s --private --source . --remote origin --push",
		owner, login, repo, login, envdef.ShortRepoName(repo), ghcli.Host(), owner, dir, repo)
}

// createRepoErr wraps a failed `gh repo create` with the checks that explain it.
// gh's own message is preserved above this text (it streams to stderr). ownerKind
// is the GitHub account type of the owner ("Organization", "User", or "" when it
// could not be classified). The org-membership probe is meaningless for a user
// owner — `user/memberships/orgs/<user>` just 404s — and read:org is only ever
// needed for an org, so a "User" owner is offered neither. An UNCLASSIFIED owner
// gets the org guidance: instance_repo owners are usually orgs, the classifier
// only comes up empty when gh could not answer at all, and the probe is harmless
// (a 404) if the guess is wrong — where withholding a working check is not.
func createRepoErr(repo, dir, ownerKind string, err error) error {
	owner, _, ok := strings.Cut(repo, "/")
	if !ok {
		owner = repo
	}
	host := ghcli.Host()
	first := fmt.Sprintf("  • is `gh` authed as the right account?    gh auth status --hostname %s\n"+
		"  • does the token carry the repo scope?    gh auth refresh -h %s -s repo\n"+
		"  • is the owner one you can create in?     %q must be your own account, or an org you belong to\n",
		host, host, owner)
	if ownerKind == "Organization" || ownerKind == "" {
		first = fmt.Sprintf("  • can you create repos in that org?       gh api user/memberships/orgs/%s --jq .role\n"+
			"  • is `gh` authed as the right account?    gh auth status --hostname %s\n"+
			"  • does the token carry the scopes?        gh auth refresh -h %s -s repo,read:org\n", owner, host, host)
	}
	return fmt.Errorf("--push: `gh repo create %s` failed: %w\n"+
		"  The scaffold in %s is complete and committed; only the push is left. Check, in order:\n"+
		"%s"+
		"  Then finish from the scaffold:\n"+
		"    gh repo create %s --private --source %s --remote origin --push",
		repo, err, dir, first, repo, dir)
}

// pushInstanceRepo creates the instance's GitHub repo and pushes the freshly
// scaffolded tree, closing the §3 loop (the repo learned from .copier-answers.yml).
// Returns whether the push actually happened. Gated by --yes; respects --dry-run.
func pushInstanceRepo(g globalOpts, dir string) (bool, error) {
	a, err := answers.Read(dir)
	if err != nil || a == nil || a.InstanceRepo == "" || a.InstanceRepo == "your-org/your-instance-repo" {
		fmt.Fprintf(os.Stderr, "llz: --push: instance_repo is still the placeholder in %s/.copier-answers.yml — skipping the repo create.\n", dir)
		// runNew has already normalised the branch to `main` (ensureScaffoldBranch),
		// which is what the platform-bootstrap Application tracks — so the create
		// below just needs the owner/name.
		fmt.Fprintf(os.Stderr, "  Create it yourself once you know the <owner>/<name>:\n"+
			"    gh repo create <owner>/<name> --private --source %s --remote origin --push\n"+
			"  The <owner> (an org, or your own user) must already exist — that command creates the repo, not the org.\n", dir)
		return false, nil
	}
	repo := a.InstanceRepo

	// gh repo create --push needs at least one commit; copier git-inits but does
	// not commit, so seed an initial commit if the tree has none.
	if _, err := execOutput("git", "-C", dir, "rev-parse", "HEAD"); err != nil {
		if err := proc.RunEcho(g.dryRun, "git", "-C", dir, "add", "-A"); err != nil {
			return false, err
		}
		if err := proc.RunEcho(g.dryRun, "git", "-C", dir, "commit", "-q", "-m", "Initial instance scaffold (llz new)"); err != nil {
			return false, err
		}
	}
	// (runNew already ran ensureScaffoldBranch, for the --push and no-push paths
	// alike, so the tree is on `main` by here.)

	// Preflight the destination before the create: an absent owner and an
	// already-created repo are both dead ends for `gh repo create`, and both are
	// what a first-time adopter runs into. Skipped in --dry-run (nothing reaches
	// GitHub there, so the printed plan stays the plain create).
	owner, _, hasOwner := strings.Cut(repo, "/")
	var ownerKind string
	if hasOwner && !g.dryRun {
		kind, err := ghcli.OwnerKindFn(owner)
		switch login := ghLoginFn(); {
		case err != nil:
			// Indeterminate (no gh auth, offline, rate limit) — don't block on a
			// classification we couldn't make; createRepoErr explains the failure.
		case kind == "":
			return false, missingRepoOwnerErr(repo, owner, dir, login)
		case kind == "User" && login != "" && !strings.EqualFold(owner, login):
			// A real account, just not one we can ever create in. Only claimed when
			// we know who we are — an unresolvable login proves nothing.
			return false, foreignUserOwnerErr(repo, owner, dir, login)
		}
		ownerKind = kind
		if instanceRepoExistsFn(repo) {
			if err := adoptExistingRepo(g, dir, repo); err != nil {
				return false, err
			}
			return g.yes, nil
		}
	}

	// gh repo create makes a new GitHub repo (outward-facing) — gate on --yes.
	if err := runGated(g, "gh", "repo", "create", repo,
		"--private", "--source", dir, "--remote", "origin", "--push"); err != nil {
		return false, createRepoErr(repo, dir, ownerKind, err)
	}
	return g.yes && !g.dryRun, nil
}

// bootstrapBranch is the branch a fresh instance must be pushed to: the
// platform-bootstrap Argo Application tracks apps_repo_revision, which defaults
// to "main" (ci_bootstrap_cluster.go).
const bootstrapBranch = "main"

// ensureScaffoldBranch renames a not-yet-pushed scaffold onto bootstrapBranch.
// `git init` still names the first branch `master` unless the operator has set
// init.defaultBranch, and nothing downstream forgives that: `gh repo create
// --source --push` pushes whatever branch is checked out, so the scaffold lands
// on `master` while the platform-bootstrap Application — and every carved App
// under it — asks Argo CD for `main`. That failure surfaces an hour later as an
// unresolvable revision inside the cluster, not here, where it costs one rename.
// (The workflows already tolerate either name: terraform.yml triggers on
// [main, master]. Argo CD is the half that does not.)
//
// Only ever renames a branch with no upstream — nothing has been pushed yet, so
// there is no history to rewrite and an instance deliberately living on another
// branch (already pushed) is left alone.
func ensureScaffoldBranch(g globalOpts, dir string) error {
	cur, err := execOutput("git", "-C", dir, "symbolic-ref", "--short", "HEAD")
	branch := strings.TrimSpace(string(cur))
	if err != nil || branch == "" || branch == bootstrapBranch {
		return nil
	}
	if _, err := execOutput("git", "-C", dir, "rev-parse", "--symbolic-full-name", "@{upstream}"); err == nil {
		return nil
	}
	// `git branch -M` is --move --force: renaming onto an EXISTING main deletes
	// that branch and everything only it pointed at, silently. A one-commit
	// copier scaffold has no second branch, but pushInstanceRepo also runs over
	// directories llz did not just create — so never clobber, just say so.
	if _, err := execOutput("git", "-C", dir, "show-ref", "--verify", "--quiet", "refs/heads/"+bootstrapBranch); err == nil {
		fmt.Fprintf(os.Stderr, "%s scaffold is on %q but a %q branch already exists — leaving both alone.\n",
			color.Yellow("!"), branch, bootstrapBranch)
		fmt.Fprintf(os.Stderr, "  The platform-bootstrap Application tracks %s (apps_repo_revision), so push the scaffold there\n"+
			"  yourself once you have reconciled the two branches.\n", bootstrapBranch)
		return nil
	}
	// run() is a no-op under --dry-run, and unlike runGated it does not mark its
	// echo as one — so an announcement in the past tense would have --dry-run
	// reporting a local branch mutation that did not happen, from the one flag
	// whose entire contract is "changes nothing".
	verb := "renaming"
	if g.dryRun {
		verb = "would rename"
	}
	fmt.Fprintf(os.Stderr, "%s scaffold is on %q; %s to %q — the platform-bootstrap Application tracks %s (apps_repo_revision)\n",
		color.Yellow("!"), branch, verb, bootstrapBranch, bootstrapBranch)
	return proc.RunEcho(g.dryRun, "git", "-C", dir, "branch", "-M", bootstrapBranch)
}

// adoptExistingRepo wires an instance_repo that ALREADY exists as `origin` and
// pushes into it, instead of `gh repo create`ing it again (which fails with
// "Name already exists on this account"). This is the second half of the absent-
// owner story: the adopter creates the org/repo by hand, re-runs, and would
// otherwise hit a fresh dead end.
func adoptExistingRepo(g globalOpts, dir, repo string) error {
	// runGated only PRINTS without --yes (and in --dry-run), so the announcement
	// has to match the mode — otherwise it claims a push that never happened and
	// the operator walks away believing the repo is populated.
	did := "wiring it as `origin` and pushing into it"
	if g.dryRun || !g.yes {
		did = "would wire it as `origin` and push into it"
	}
	fmt.Fprintf(os.Stderr, "%s %s already exists on GitHub — %s.\n", color.Yellow("!"), repo, did)
	sub := "add"
	if _, err := execOutput("git", "-C", dir, "remote", "get-url", "origin"); err == nil {
		sub = "set-url"
	}
	// ghcli.Host(), not the github.com literal: `gh repo create` resolves the host
	// itself from GH_HOST, but this remote URL is hand-built, so on GHE it would
	// point origin at a host the repo does not live on and the push below fails.
	if err := runGated(g, "git", "-C", dir, "remote", sub, "origin", "https://"+ghcli.Host()+"/"+repo+".git"); err != nil {
		return err
	}
	if err := runGated(g, "git", "-C", dir, "push", "-u", "origin", "HEAD"); err != nil {
		return fmt.Errorf("--push: %s exists but the push was rejected: %w\n"+
			"  The remote most likely already has commits (a README/.gitignore from repo creation).\n"+
			"  Reconcile from the scaffold, then push:\n"+
			"    git -C %s pull --rebase origin HEAD && git -C %s push -u origin HEAD\n"+
			"  (or recreate the repo empty: `gh repo create %s --private` with no initial files)",
			repo, err, dir, dir, repo)
	}
	return nil
}

func cmdEnvAdd(g globalOpts, name string, o envdef.Opts) error {
	return envadd.Run(g.dryRun, name, o)
}

func cmdBuild(args []string, g globalOpts, skipPreflight bool) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: llz build <env>")
	}
	env := args[0]
	if err := validate.EnvName(env); err != nil {
		return err
	}
	// The dispatch is fire-and-forget — GitHub accepts any `region` string and
	// fails later, in CI, on the tree it checked out. Ask the remote first
	// (build_preflight.go); --skip-preflight is the escape hatch for a build whose
	// spec deliberately lives elsewhere (another branch, another checkout).
	// --dry-run prints the argv and dispatches nothing, so it has nothing to gate.
	if !skipPreflight && !g.dryRun {
		if err := buildpreflight.Run(env); err != nil {
			return err
		}
	}
	return runGated(g, buildArgv(env)...)
}

// up{Tokens,Doctor,Build} are the seams cmdUp drives — package-level vars so a
// unit test can record the call order and inject a failure without the
// cloud-mutating side effects of the real commands. Defaults call the real ones.
var (
	upTokens = func(g globalOpts, admin bool, env string) error {
		return onboard.RunTokens(g.onboardOpts(), admin, env, "", "", "")
	}
	upDoctor = func(g globalOpts, admin bool, env string) error {
		return onboard.RunDoctor("", env, admin, true, "", "")
	}
	// skipPreflight=true: cmdUp already ran buildpreflight.Run itself (upPreflight,
	// before the token wizard), and running it again here printed the whole
	// unpublished-edits warning block twice in one `llz up`.
	upBuild = func(g globalOpts, env string) error { return cmdBuild([]string{env}, g, true) }
	// upPreflight is the same dispatch check, run before the chain starts; seamed
	// alongside the three stages so the order test can drive it.
	upPreflight = buildpreflight.Run
)

// cmdUp sequences the first-build flow into one command: provision credentials
// (tokens) → confirm the readiness gate (doctor) → dispatch the apply (build),
// then print the steps the tooling can't do for you. It stops at the first
// failure. Cloud-mutating steps honour --yes/--dry-run via the delegated commands.
func cmdUp(env string, g globalOpts, admin, skipTokens bool) error {
	if err := validate.EnvName(env); err != nil {
		return err
	}
	// Stage 3 preflights the dispatch anyway, but stage 1 is an interactive token
	// wizard: discovering "this deployment was never pushed" AFTER minting PATs
	// and creating a state bucket is a bad trade for a read-only check. Ask now.
	if !g.dryRun {
		if err := upPreflight(env); err != nil {
			return err
		}
	}
	if !skipTokens {
		fmt.Println(color.Bold("══ 1/3  llz tokens — provision credentials ══"))
		if err := upTokens(g, admin, env); err != nil {
			return fmt.Errorf("tokens: %w", err)
		}
	}
	fmt.Println("\n" + color.Bold("══ 2/3  llz doctor — readiness gate ══"))
	if err := upDoctor(g, admin, env); err != nil {
		return fmt.Errorf("doctor: %w (fix the above, then re-run `llz up %s`)", err, env)
	}
	fmt.Println("\n" + color.Bold("══ 3/3  llz build — dispatch the apply ══"))
	if err := upBuild(g, env); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	printManualActions(env)
	return nil
}

// printManualActions lists the post-build steps the bootstrap genuinely cannot do
// on the operator's behalf — surfaced once here so they don't get lost.
func printManualActions(env string) {
	b := func(s string) string { return "  " + color.Dim("•") + " " + s }
	fmt.Println("\n" + color.Bold("══ remaining manual actions (the tooling can't do these for you) ══"))
	fmt.Println(b("Watch convergence:   " + color.Cyan("llz status "+env+" --wait")))
	fmt.Println(b("After OpenBao bootstrap, from the job summary (shown once):"))
	fmt.Println(color.Dim("      – escrow unseal keys 4 & 5 + the root token to secure offline storage"))
	// Repeated here because stage 1's banner has scrolled past a full build by now,
	// and this secret has the same "lose it and the data is gone" blast radius.
	fmt.Println(color.Dim("      – and TF_STATE_ENCRYPTION_PASSPHRASE, if `llz tokens` generated one above"))
	fmt.Println(color.Dim("      – delete OPENBAO_ROOT_TOKEN from infra-"+env) + color.Dim("   (`llz status` flags it if left)"))
	fmt.Println(b("DNS-01 certs wire automatically once TF_VAR_linode_dns_token is set at apply"))
	fmt.Println(color.Dim("      (the letsencrypt ClusterIssuers sync via Argo; re-apply TF if the token came later)"))
}

func cmdStatus(args []string, g globalOpts, wait bool, timeout int) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: llz status <env>")
	}
	// Every check below is a kubectl call, so with no cluster access they all fail
	// the same way and bury the one thing worth saying under three copies of
	// kubectl's connection dump. Probe once and say it instead (status_preflight.go).
	//
	// The OPENBAO_ROOT_TOKEN check still runs: it reads GitHub, not the cluster,
	// and it is the standing "you have not escrowed + deleted this yet" nag that
	// `llz status` promises on EVERY run. Gating it behind cluster reachability
	// would silence it exactly for the operator who has no kubeconfig — which is
	// most of them, right after the build that printed the token.
	if err := reachability.StatusPreflight(args[0]); err != nil {
		warnIfRootTokenPresent(args[0])
		return err
	}
	// Read-only kubectl checks against the cluster kubectl currently points at.
	var firstErr error
	for _, argv := range statusArgv() {
		if err := proc.RunEcho(g.dryRun, argv...); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// Argo CD Application health (report-only by default; --wait polls + gates).
	fmt.Println()
	if err := converge.ReportArgoHealth(g.dryRun, wait, timeout); err != nil && firstErr == nil {
		firstErr = err
	}
	// Standing security-hygiene check: the OpenBao root token must not linger in
	// infra-<env> after first-time bootstrap (report-only — it does not gate health).
	warnIfRootTokenPresent(args[0])
	return firstErr
}

// warnIfRootTokenPresent flags an OPENBAO_ROOT_TOKEN left behind in the infra-<env>
// environment. First-time bootstrap requires the operator to escrow the unseal keys
// + root token offline and DELETE the root token from infra-<env> — it is only
// needed to seed secrets at bootstrap, and is a standing liability once that is
// done. The one-time job-summary warning is easy to miss, so status re-checks it on
// every run. Best-effort: skips silently without gh or a resolvable repo.
func warnIfRootTokenPresent(env string) {
	if !kubectlprobe.Lookable("gh") {
		return
	}
	repo, err := answers.ResolveInstanceRepo("", false)
	if err != nil {
		return
	}
	for _, n := range configreadiness.GHSecretNames("repos/" + repo + "/environments/infra-" + env + "/secrets") {
		if n == "OPENBAO_ROOT_TOKEN" {
			fmt.Printf("\n%s OPENBAO_ROOT_TOKEN is still set in infra-%s — escrow it offline and delete it.\n", color.Yellow("⚠"), env)
			fmt.Println(color.Dim("  It is only needed to seed secrets at bootstrap; leaving it set is a standing liability."))
			fmt.Printf("  Remove it: %s\n", color.Cyan(fmt.Sprintf("gh secret delete OPENBAO_ROOT_TOKEN --env infra-%s --repo %s", env, repo)))
			return
		}
	}
}

// sustainDeps is what internal/sustain is handed: provenance, two shell-outs, and
// the --yes bit. No cluster, no cloud — sustain answers repo questions.
func sustainDeps() sustain.Deps {
	return sustain.Deps{
		LockableScaffoldFiles: lockableScaffoldFiles,
		ReadAnswers: func(dir string) (*sustain.Answers, error) {
			a, err := answers.Read(dir)
			if err != nil || a == nil {
				return nil, err
			}
			return &sustain.Answers{Commit: a.Commit, SrcPath: a.SrcPath, Version: a.Version}, nil
		},
		Exec:    execOutput,
		Run:     proc.Run,
		Confirm: func() bool { return gopts.yes },
	}
}
