package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/validate"
)

// validateEnvName returns an error if env is not a legal deployment name.
// The deployment-name contract (^[a-z][a-z0-9-]{1,30}$) lives in internal/validate
// so the LandingZone spec validator enforces the IDENTICAL rule. Deployments are
// created dynamically (there is no hardcoded env list — terraform.yml's region
// input is a free-form string, not a choice), so build/status/tokens validate the
// SHAPE of the name, not membership in a fixed set: otherwise `llz env add
// myteam-dev` would succeed but `llz build myteam-dev` refuse.
func validateEnvName(env string) error { return validate.EnvName(env) }

// ── argv builders (pure; covered by commands_test.go) ────────────────────────

// resolveScaffoldRef picks the template ref to scaffold/upgrade from: an explicit
// --ref verbatim (tag, branch, or SHA), else this llz binary's own version when it
// is a real release (the CLI is the version anchor), else "" — signalling the
// caller (scaffoldRef) to resolve the latest published release tag, since a dev
// build has no version to anchor to. The chosen value is rendered into the
// instance's pins as copier's llz_version, so the scaffold references exactly the
// release it was cut from.
func resolveScaffoldRef(ref string) string {
	if ref != "" {
		return ref
	}
	if _, _, _, ok := semver(version); ok {
		return normalizeLLZTag(version)
	}
	return ""
}

// latestReleaseFn resolves the newest published vX.Y.Z release of a template repo;
// seamed for tests. It reuses self-update's release picker, which drops drafts /
// pre-releases and ignores the llz/v* CLI tag track (latestLLZTag).
var latestReleaseFn = latestRelease

// scaffoldRef resolves the concrete ref to scaffold/pin to. It falls back from a
// dev build (no anchor version) to the latest published vX.Y.Z release of repo, so
// a scaffold never floats on `main` — which the template's own tflint gate
// (terraform_module_pinned_source) rejects, Renovate can't bump, and copier now
// refuses (the llz_version validator). repo is the template's <org>/<name>.
func scaffoldRef(ref, repo string) (string, error) {
	if r := resolveScaffoldRef(ref); r != "" {
		return r, nil
	}
	tag, err := latestReleaseFn(repo)
	if err != nil {
		return "", fmt.Errorf("this is a dev build of llz (no anchor version) and the latest %s release could not be resolved to pin to: %w\n"+
			"  pass --ref vX.Y.Z to pin to a release explicitly", repo, err)
	}
	return tag, nil
}

func copierCopyArgv(org, ref, dir string) []string {
	return []string{"copier", "copy", "--trust", "--vcs-ref", ref,
		"--data", "llz_version=" + ref,
		"gh:" + org + "/" + templateName, dir}
}

// copierUpdateArgv is the update invocation, and --defaults is load-bearing.
//
// Without it `copier update` RE-ASKS every question — upstream_org,
// instance_repo, openbao_team — using the stored answers as prompt defaults. Two
// costs, and the second is the one that bit:
//
//   - With no terminal that is not a prompt, it is an unhandled OSError out of
//     prompt_toolkit. `llz upgrade` inherits the operator's stdin, so it worked
//     by hand and died in CI, in a wrapper script, and over `ssh host 'llz
//     upgrade'` — with a Python traceback, not a message.
//   - Interactively it is three unexplained prompts mid-upgrade, on answers that
//     are rendered INTO managed files. instance_repo becomes the ArgoCD repoURL
//     and every `gh` target, so one stray keystroke re-renders the instance
//     against a repo that does not exist.
//
// --defaults keeps the stored answers (verified: a non-default instance_repo and
// openbao_team both survive v0.0.39 → v0.0.40 untouched). It does NOT make the
// answers safe on its own — copier still falls back to the template DEFAULT for
// an answer it cannot keep, which is why runUpgrade verifies them afterwards
// rather than trusting this flag. `llz ci upgrade-test` runs this exact argv.
func copierUpdateArgv(ref string) []string {
	a := []string{"copier", "update", "--trust", "--defaults"}
	if ref != "" {
		a = append(a, "--vcs-ref", ref, "--data", "llz_version="+ref)
	}
	return a
}

// requireCopier fails with an install route when the copier CLI is absent.
//
// `llz new` and `llz upgrade` are thin wrappers around `copier copy` / `copier
// update`, and copier is a Python tool that is on no machine by default. Without
// this, the scaffold died on exec's own words — `copier copy: exec: "copier":
// executable file not found in $PATH` — as the SECOND command of the quickstart,
// naming a tool the operator has never heard of and no way to get it. The
// installer only checks `gh`, and `llz doctor` (which does list copier) reports
// rather than gates AND is two steps further down the quickstart, so nothing
// between install and scaffold said this out loud.
//
// action names the command, because the two have different recovery paths: a
// failed `llz new` leaves nothing behind, a failed `llz upgrade` is mid-flight.
// g so a --dry-run, which never execs copier (run() returns before exec), reports
// the gap without failing on it — the flag's contract is "print what would run,
// change nothing", and a dry-run that hard-errors on a tool it was not going to
// invoke breaks it. Warn rather than stay silent: "this would work" is the wrong
// answer too.
func requireCopier(g globalOpts, action string) error {
	if lookable("copier") {
		return nil
	}
	if g.dryRun {
		fmt.Fprintf(os.Stderr, "%s `copier` is not on PATH — %s would fail here (dry-run: not executing it).\n",
			yellow("!"), action)
		fmt.Fprintf(os.Stderr, "  install it first: %s\n", cyan("pipx install copier"))
		return nil
	}
	//lint:ignore ST1005 multi-line operator diagnostic: the trailing period closes an embedded install-route block, not a sentence fragment
	return fmt.Errorf("the `copier` CLI is not on PATH — %s renders the scaffold with it.\n"+
		"  copier is a Python tool and is not installed by default (the llz installer does not add it):\n"+
		"  • pipx install copier      (what this repo's own CI uses)\n"+
		"  • uv tool install copier\n"+
		"  • brew install copier\n"+
		"  Then re-run %s. `llz doctor` lists the rest of the toolchain it expects.", action, action)
}

func buildArgv(env string) []string {
	return []string{"gh", "workflow", "run", "terraform.yml",
		"--field", "region=" + env, "--field", "action=apply", "--field", "module=all"}
}

// secretSetArgv routes a secret to its scope. Reads the requirement table rather
// than hardcoding --env, matching pushToRepo: TF_STATE_ENCRYPTION_PASSPHRASE is
// repo-level (one per instance), and pushing it env-scoped through `llz secrets
// push` would give a second deployment a different passphrase.
func secretSetArgv(env, name string) []string {
	argv := []string{"gh", "secret", "set", name}
	if secretIsEnvScoped(name) {
		argv = append(argv, "--env", "infra-"+env)
	}
	return argv
}

// ghSecretSetStdin pipes value into `gh secret set <name>` (with --env <ghEnv>
// when ghEnv is non-empty), keeping the value off argv. gh resolves auth + repo
// from the ambient GH_TOKEN/GH_REPO. Shared body for the ghSetSecretFn (--env,
// ci_openbao_init.go) and ghSetRepoSecretFn (repo-level, ci_harbor.go) seams.
func ghSecretSetStdin(name, ghEnv, value string) error {
	args := []string{"secret", "set", name}
	label := name
	if ghEnv != "" {
		args = append(args, "--env", ghEnv)
		label = fmt.Sprintf("%s --env %s", name, ghEnv)
	}
	cmd := exec.Command("gh", args...)
	cmd.Stdin = strings.NewReader(value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh secret set %s: %s", label, strings.TrimSpace(string(out)))
	}
	return nil
}

func variableSetArgv(name string) []string {
	return []string{"gh", "variable", "set", name}
}

// statusArgv is the read-only convergence check set (matches the verify steps in
// docs/runbooks/bootstrap-openbao.md).
//
// The OpenBao namespace comes from openbaoNamespace, not a literal. It WAS the
// literal "openbao", which has not existed since the platform namespaces were
// llz- prefixed — so `llz status <env>` reported nothing for the OpenBao pods and
// looked like a cluster with none, on every invocation. Same class as the three
// stale entries in healthNamespaces (#242).
func statusArgv() [][]string {
	return [][]string{
		{"kubectl", "-n", openbaoNamespace, "get", "pods"},
		{"kubectl", "-n", "argocd", "get", "applications"},
		{"kubectl", "-n", "external-secrets", "get", "clustersecretstore"},
	}
}

// ── execution helpers ────────────────────────────────────────────────────────

// run executes argv, streaming stdio. In dry-run it prints and returns.
func run(g globalOpts, argv ...string) error {
	fmt.Fprintln(os.Stderr, "→ "+shellQuote(argv))
	if g.dryRun {
		return nil
	}
	return execArgv(argv, "")
}

// runGated is run() for cloud-mutating commands: it refuses to execute without
// --yes, printing the command instead so the operator can see exactly what would
// reach Linode/GitHub.
func runGated(g globalOpts, argv ...string) error {
	if g.dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) "+shellQuote(argv))
		return nil
	}
	if !g.yes {
		fmt.Fprintln(os.Stderr, "would run: "+shellQuote(argv))
		fmt.Fprintln(os.Stderr, "  (re-run with --yes to execute)")
		return nil
	}
	return run(g, argv...)
}

// execArgv runs argv with an optional stdin string (used to pipe secret values
// into `gh secret set` without putting them in the process arguments).
func execArgv(argv []string, stdin string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	} else {
		cmd.Stdin = os.Stdin
	}
	return cmd.Run()
}

// shellQuote renders argv for display, quoting tokens that need it.
func shellQuote(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		if a == "" || strings.ContainsAny(a, " \t\"'$&|;<>()") {
			b.WriteString("'" + strings.ReplaceAll(a, "'", `'\''`) + "'")
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}

// ── commands ─────────────────────────────────────────────────────────────────

// templateSourceStatusFn reports whether the --org template source is reachable
// on GitHub; seamed for tests. runNew preflights it because copier clones
// gh:<org>/<template> over HTTPS, and a 404 there (typo'd/un-forked --org)
// surfaces as an interactive `Username for 'https://github.com':` prompt rather
// than a clear error — the failure mode adopters actually hit.
var templateSourceStatusFn = repoStatus

// ghUnreachableErr covers the case a preflight must NOT blame on the repo: we
// could not ask GitHub at all. `gh` missing or unauthenticated makes every lookup
// fail, and reporting that as "not found" sends the operator off to fix something
// that is fine. tail is the caller's one-line "so do not conclude X" — the checks
// are identical everywhere, the wrong conclusion is not.
// The host is ghHost(), never the github.com literal: `llz doctor` already scopes
// its own auth check that way (GH_HOST), so a remediation naming the default host
// would tell a GHE operator to authenticate somewhere doctor is not even looking
// — and doctor would go on failing after they followed it exactly.
func ghUnreachableErr(subject string, err error, tail string) error {
	return fmt.Errorf("could not check %s on GitHub: %w\n"+
		"  llz drives GitHub through the `gh` CLI, so this is almost always gh, not your instance:\n"+
		"  • installed?      gh version                             (https://cli.github.com)\n"+
		"  • authenticated?  gh auth status --hostname %s   (then: gh auth login --hostname %s)\n"+
		"  %s", subject, err, ghHost(), ghHost(), tail)
}

// templateUnreachableTail states what an unanswerable lookup does NOT prove about
// the --org template source. The default upstream is public, so gh is the only
// candidate; a fork named by --org may be private, where "it's public" would be a
// false claim and the fix is a login that can see it.
func templateUnreachableTail(org, repo string) string {
	if org == defaultTemplateOrg {
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
		org, templateName, defaultTemplateOrg, defaultTemplateOrg, templateName, org)
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
	if !isInstanceRoot(dir) {
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
	if isInstanceRoot(dir) {
		return fmt.Errorf("%s is already a landing-zone instance — `llz new` would render a second scaffold over it.\n"+
			"  • add a deployment to it:     %s\n"+
			"  • move it to a new release:   %s\n"+
			"  • really want a separate instance? scaffold it into a new directory",
			dir, cyan("cd "+dir+" && llz env add <env> --region <region> --obj-cluster <obj-cluster>"),
			cyan("cd "+dir+" && llz self-update && llz upgrade"))
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
	if err := requireCopier(g, "`llz new`"); err != nil {
		return err
	}
	repo := org + "/" + templateName
	switch found, err := templateSourceStatusFn(repo); {
	case err != nil:
		return ghUnreachableErr(repo, err, templateUnreachableTail(org, repo))
	case !found:
		return missingTemplateSourceErr(org)
	}
	ref, err := scaffoldRef(ref, repo)
	if err != nil {
		return err
	}
	fmt.Printf("Scaffolding a new LKE landing-zone instance into %q from %s/%s@%s\n\n",
		dir, org, templateName, ref)

	if err := run(g, copierCopyArgv(org, ref, dir)...); err != nil {
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
		fmt.Fprintf(os.Stderr, "%s could not rename the scaffold branch to %s (%v).\n", yellow("!"), bootstrapBranch, err)
		fmt.Fprintf(os.Stderr, "  Do it before pushing — Argo CD tracks %s: %s\n",
			bootstrapBranch, cyan("git -C "+dir+" branch -M "+bootstrapBranch))
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

// printNextSteps renders the post-scaffold guide: a bold header, dim context
// notes, and the ordered command sequence with cyan commands + dim, column-
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
		fmt.Printf("  %s%s%s\n", cyan(c), strings.Repeat(" ", pad), dim("# "+note))
	}
	note := func(s string) { fmt.Println(dim("  # " + s)) }

	fmt.Println("\n" + bold("Next steps"))
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

// ghOwnerKindFn classifies the <owner> half of instance_repo on GitHub; seamed
// for tests. instanceRepoExistsFn reports whether the instance repo itself is
// already there (an adopter who created it by hand after a failed --push).
var (
	ghOwnerKindFn        = ghOwnerKind
	instanceRepoExistsFn = repoExists
	ghLoginFn            = ghLogin
)

// ghOwnerKind classifies a GitHub account name: "User", "Organization", or ""
// when GitHub definitively 404s it. A non-nil error means indeterminate (no `gh`
// auth, offline, rate-limited) — callers must not treat that as "absent".
// /users/<name> resolves orgs too, so one call answers both questions.
func ghOwnerKind(owner string) (string, error) {
	out, err := execOutput("gh", "api", "users/"+owner, "--jq", ".type")
	if err != nil {
		if ghNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ghNotFound reports whether a `gh api` failure was a 404 rather than an auth,
// network, or rate-limit problem — gh writes "gh: Not Found (HTTP 404)" to
// stderr, which (*exec.Cmd).Output captures on the ExitError.
func ghNotFound(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		s := string(ee.Stderr)
		return strings.Contains(s, "HTTP 404") || strings.Contains(s, "Not Found")
	}
	return false
}

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
		owner, repo, dir, owner, owner, ghHost(), repo, dir, mine, shortRepoName(repo))
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
		owner, login, repo, login, shortRepoName(repo), ghHost(), owner, dir, repo)
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
	host := ghHost()
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
	a, err := readAnswers(dir)
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
		if err := run(g, "git", "-C", dir, "add", "-A"); err != nil {
			return false, err
		}
		if err := run(g, "git", "-C", dir, "commit", "-q", "-m", "Initial instance scaffold (llz new)"); err != nil {
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
		kind, err := ghOwnerKindFn(owner)
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
			yellow("!"), branch, bootstrapBranch)
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
		yellow("!"), branch, verb, bootstrapBranch, bootstrapBranch)
	return run(g, "git", "-C", dir, "branch", "-M", bootstrapBranch)
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
	fmt.Fprintf(os.Stderr, "%s %s already exists on GitHub — %s.\n", yellow("!"), repo, did)
	sub := "add"
	if _, err := execOutput("git", "-C", dir, "remote", "get-url", "origin"); err == nil {
		sub = "set-url"
	}
	// ghHost(), not the github.com literal: `gh repo create` resolves the host
	// itself from GH_HOST, but this remote URL is hand-built, so on GHE it would
	// point origin at a host the repo does not live on and the push below fails.
	if err := runGated(g, "git", "-C", dir, "remote", sub, "origin", "https://"+ghHost()+"/"+repo+".git"); err != nil {
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

func runUpgrade(g globalOpts, ref string, commit, noRender bool) error {
	// Same prerequisite as `llz new`, and worse to discover late: this one runs
	// snapshot/restore around copier, so failing at the exec is failing mid-flight.
	if err := requireCopier(g, "`llz upgrade`"); err != nil {
		return err
	}
	oldRef := currentTemplateRef() // "" if this is not an instance checkout yet
	// Snapshot before copier runs — it rewrites this file in place, so afterwards
	// there is nothing left to compare against (see Lever 0).
	answersBefore := currentAnswerMap()

	// Always resolve to a concrete ref so the instance's llz_version pins update in
	// lockstep with the template code (a bare `copier update` would float the code
	// to the latest tag but leave the recorded llz_version stale). updateRepo()
	// names the template this instance tracks (its .copier-answers upstream_org).
	ref, err := scaffoldRef(ref, updateRepo())
	if err != nil {
		return err
	}
	// Copier has exactly one update strategy — re-render + 3-way merge — but the
	// manifest declares three (managed / merge / owned). Capture the `owned` files
	// BEFORE copier runs so we can put them back after; `managed` files are then
	// overwritten from a clean render of the target ref. Without this the classes
	// are documentation only and copier merges everything alike.
	//
	// Degrade gracefully: a pre-manifest instance (or one whose manifest failed to
	// parse) still upgrades exactly as it did before, just without class enforcement.
	policy, policyErr := loadTemplateManifest("")
	var owned upgradeSnapshot
	if policyErr != nil {
		fmt.Fprintf(os.Stderr, "%s no usable .template-manifest (%v) — upgrading without manifest-class enforcement\n", yellow("!"), policyErr)
	} else {
		if owned, err = snapshotUpgradeOwned(policy); err != nil {
			return fmt.Errorf("snapshot owned files: %w", err)
		}
		defer owned.cleanup()
	}
	if err := run(g, copierUpdateArgv(ref)...); err != nil {
		return fmt.Errorf("copier update: %w", err)
	}
	if policyErr == nil {
		if err := applyUpgradeManifestPolicy(g, ref, owned); err != nil {
			return fmt.Errorf("apply manifest policy: %w", err)
		}
	}
	// copier update never deletes a file the template dropped between versions, so
	// apply the template's declared removals (.template-removals) ourselves — now
	// up to date from the copier update above. Honors --dry-run internally.
	// Runs AFTER the manifest policy: a removed file is absent from the clean render,
	// so the overwrite pass can't resurrect it, but the ordering makes that explicit.
	if err := applyTemplateRemovals(g); err != nil {
		return fmt.Errorf("apply template removals: %w", err)
	}
	if g.dryRun {
		return nil
	}
	// No provenance re-stamp: copier's `update` already rewrote .copier-answers.yml
	// (_commit + llz_version), which IS the record — see stamp.go. That also retires
	// the stamp/rollback dance this used to need around the conflict gate below.
	newRef := currentTemplateRef()

	// ── Lever 0: the answers must be the ones we came in with ────────────────
	// copier does not error on an answer it cannot keep — it substitutes the
	// template DEFAULT and exits 0. The reachable case is an answer the CURRENT
	// template's validator rejects (instance_repo grew one in #405), which is
	// exactly the malformed value an instance scaffolded before that validator can
	// be holding. instance_repo is the ArgoCD repoURL and every `gh` target, so the
	// silent outcome is an instance re-rendered against `your-org/your-instance-repo`
	// — a repository that does not exist — with a clean exit and a normal-looking
	// diffstat.
	//
	// Checked rather than trusted: --defaults on the update argv preserves answers
	// in the normal case, but it does not close this one, and a flag that is right
	// 99% of the time is not a substitute for verifying the 1%.
	if regressions := answerRegressions(answersBefore, currentAnswerMap()); len(regressions) > 0 {
		fmt.Fprintf(os.Stderr, "\n%s copier update rewrote %d answer(s) it does not own:\n", red("✗"), len(regressions))
		for _, r := range regressions {
			fmt.Fprintf(os.Stderr, "    %s\n", r)
		}
		return fmt.Errorf("the tree is at %s but its identity changed — restore the answer(s) above in "+
			".copier-answers.yml and re-run (a value the template's validator now rejects is replaced by "+
			"the DEFAULT, silently; fix the value, don't accept the default)", newRef)
	}

	// ── Lever 1: make the upgrade reviewable + safe ──────────────────────────
	// A botched copier 3-way merge can leave <<<<<<< markers that otherwise ship
	// invalid YAML far downstream (the gsap-apl incident). Fail loudly here BEFORE
	// the operator commits, instead of relying on `llz lint` to catch it later.
	if bad := upgradeConflictFiles(); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "\n%s copier update left merge-conflict markers in %d file(s):\n", red("✗"), len(bad))
		for _, f := range bad {
			fmt.Fprintf(os.Stderr, "    %s\n", f)
		}
		return fmt.Errorf("resolve the conflict marker(s) above, then commit — the working tree is at %s but is NOT yet valid", newRef)
	}

	// ── Lever 2: re-render what the new pin invalidates ──────────────────────
	// The pin copier just rewrote (.copier-answers.yml llz_version) is the value
	// resolveTemplateRef feeds into every apl-values remote ref, so EVERY committed
	// kustomization goes stale the moment copier finishes — deterministically, on
	// every upgrade, with no operator judgment involved. Leaving it manual is not a
	// nuisance but a correctness gap: `--commit` recorded a knowingly-stale render,
	// and a live instance ran three releases behind on what ArgoCD DEPLOYS (see
	// stepRenderFresh). Since that guard landed, the omission also makes `--commit`
	// fail outright — the pre-commit hook runs `llz lint`, which now refuses the
	// very commit this command is trying to make.
	//
	// AFTER the conflict gate on purpose: rendering over merge markers would bury
	// the real failure under a parse error from a file the operator has to fix anyway.
	if !noRender {
		if err := renderAfterUpgrade(g); err != nil {
			return err
		}
	}

	// ── Lever 3: name what the new pin invalidates that this command cannot fix ──
	// Same class as lever 2, different owner. TF_IMAGE/KUBE_IMAGE are derived FROM
	// the pin copier just rewrote, so they go stale on every upgrade with no
	// operator judgment involved — but the value CI reads is a GitHub repo
	// VARIABLE, and `llz upgrade` is a local command that pushes nothing.
	// Re-rendering cannot reach it. Left unsaid, the skew surfaces as a failed
	// `llz ci assert-image-fresh` on the first pipeline run after every upgrade.
	reportCIImageSkew(newRef)

	// One place to see what the upgrade touched, so a big managed-file churn is a
	// single reviewable summary rather than a scattered surprise at commit time.
	// Runs after the re-render so the diffstat is the WHOLE upgrade — scaffold plus
	// the apl-values churn the new pin implies — not the half an operator would
	// then have to re-review.
	printUpgradeSummary(oldRef, newRef)

	// A single labeled commit so the operator reviews ONE diff and history reads
	// "template vX → vY", not N unrelated file changes. Opt-in (--commit) — we
	// never silently commit someone's working tree.
	if commit {
		return commitUpgrade(g, oldRef, newRef)
	}
	fmt.Fprintln(os.Stderr, dim("  (re-run with --commit to stage + commit this upgrade as one labeled commit)"))
	return nil
}

// renderAfterUpgrade reconciles the spec into the artifacts the new pin
// invalidated — the gitignored <env>.tfvars and the COMMITTED apl-values
// kustomizations whose `?ref=` is the pin itself.
//
// No-op for a pre-spec instance, using the same InstancePresent guard
// stepRenderFresh uses: there is no spec to render from, and an upgrade must
// still work for an instance that has not adopted one.
//
// A render failure here leaves a tree that is genuinely half-upgraded — the
// scaffold is at the new ref while apl-values still points at the old one — so it
// says exactly that rather than surfacing the bare render error.
func renderAfterUpgrade(g globalOpts) error {
	tfDir, _, _ := instanceLayout()
	if !clusterspec.InstancePresent(filepath.Dir(tfDir)) {
		return nil
	}
	if err := runRender(g, "", false, false, false); err != nil {
		return fmt.Errorf("the copier update applied cleanly, but re-rendering the spec failed — the scaffold is "+
			"at the new ref while the committed apl-values still reference the OLD one, which is what ArgoCD would "+
			"sync. Fix the problem below and run `llz render` before committing:\n%w", err)
	}
	return nil
}

// currentTemplateRef reads the ref this checkout is pinned to (copier's answers,
// see stamp.go), falling back to the short SHA. "" outside an instance.
func currentTemplateRef() string {
	if ref := pinnedTemplateRef(); ref != "" {
		return ref
	}
	tv := resolveTemplateVersion()
	if tv.TemplateRef != "" {
		return tv.TemplateRef
	}
	if len(tv.TemplateSHA) >= 8 {
		return tv.TemplateSHA[:8]
	}
	return tv.TemplateSHA
}

// upgradeConflictFiles returns the working-tree files copier update just changed
// (or added) that carry a merge-conflict marker — reusing the Phase 0
// conflictMarkerLines predicate so this gate and `llz lint` agree.
func upgradeConflictFiles() []string {
	changed := map[string]bool{}
	for _, f := range strings.Split(gitOut("diff", "--name-only"), "\n") {
		if f = strings.TrimSpace(f); f != "" {
			changed[f] = true
		}
	}
	for _, f := range strings.Split(gitOut("ls-files", "--others", "--exclude-standard"), "\n") {
		if f = strings.TrimSpace(f); f != "" {
			changed[f] = true
		}
	}
	var bad []string
	for f := range changed {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if len(conflictMarkerLines(string(data))) > 0 {
			bad = append(bad, f)
		}
	}
	sort.Strings(bad)
	return bad
}

// printUpgradeSummary shows the version delta + a diffstat of the churn.
func printUpgradeSummary(oldRef, newRef string) {
	from := oldRef
	if from == "" {
		from = "(unknown)"
	}
	fmt.Printf("\n%s template %s → %s\n", bold("upgrade"), from, newRef)
	if stat := gitOut("diff", "--stat"); stat != "" {
		fmt.Println(stat)
	} else if untracked := gitOut("ls-files", "--others", "--exclude-standard"); untracked != "" {
		fmt.Printf("new files:\n%s\n", untracked)
	} else {
		fmt.Println(dim("  no file changes — already up to date."))
	}
}

// reportCIImageSkew warns when the tree's ci image variables still name the
// commit the PREVIOUS pin resolved to, and prints both routes back.
//
// Deliberately a warning, not a fix and not an error. The authoritative copy of
// these two values is a GitHub repo variable; `llz upgrade` neither reads nor
// writes repo config, and quietly growing a network mutation here would make an
// otherwise-local command need credentials. So it says the thing at the moment
// the skew is created — the #405 lens, one lever up: fail (or here, warn) at the
// mistake rather than 20 minutes into the run that inherits it.
//
// The `gh variable set` pair is worded to match `llz ci assert-image-fresh`'s
// remediation verbatim, because an operator who ignores this will meet that one
// next and the two must read as the same instruction.
var reportCIImageSkew = func(ref string) {
	local := readEnvFile(".llz/vars.env")
	skew := staleCIImageVars(ref, func(k string) string { return local[k] })
	if len(skew) == 0 {
		return
	}
	repo := "<owner>/<instance>"
	if a, _ := readAnswers("."); a != nil && strings.Contains(a.InstanceRepo, "/") {
		repo = a.InstanceRepo
	}
	fmt.Fprintf(os.Stderr, "\n%s the new pin moved the ci images this instance should run — %d variable(s) still name the old commit:\n",
		yellow("!"), len(skew))
	for _, s := range skew {
		fmt.Fprintf(os.Stderr, "    %s\n      have %s\n      want %s\n", bold(s.Name), dim(s.Have), cyan(s.Want))
	}
	fmt.Fprintf(os.Stderr, "  CI reads these as %s repo variables, which this command does not push. Re-pin with either:\n", repo)
	fmt.Fprintf(os.Stderr, "    %s\n", cyan("llz tokens --env <env> --yes"))
	for _, s := range skew {
		fmt.Fprintf(os.Stderr, "    %s\n", cyan(fmt.Sprintf("gh variable set %s --repo %s --body %s", s.Name, repo, s.Want)))
	}
	fmt.Fprintln(os.Stderr, dim("  Until then the first pipeline run fails `llz ci assert-image-fresh`."))
}

// commitUpgrade stages the whole tree and records the upgrade as one labeled
// commit, so history reads "template vX → vY". No-op (not an error) when the
// update produced no changes.
func commitUpgrade(g globalOpts, oldRef, newRef string) error {
	if strings.TrimSpace(gitOut("status", "--porcelain")) == "" {
		fmt.Fprintln(os.Stderr, green("✓")+" already up to date — nothing to commit.")
		return nil
	}
	from := oldRef
	if from == "" {
		from = "(initial)"
	}
	msg := fmt.Sprintf("chore(template): upgrade %s → %s\n\nApplied via `llz upgrade`.", from, newRef)
	if err := run(g, "git", "add", "-A"); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	if err := run(g, "git", "commit", "-m", msg); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	fmt.Fprintf(os.Stderr, "%s committed template upgrade %s → %s\n", green("✓"), from, newRef)
	return nil
}

func cmdEnvAdd(g globalOpts, name string, o envAddOpts) error {
	return runEnvAdd(g, name, o)
}

func cmdBuild(args []string, g globalOpts, skipPreflight bool) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: llz build <env>")
	}
	env := args[0]
	if err := validateEnvName(env); err != nil {
		return err
	}
	// The dispatch is fire-and-forget — GitHub accepts any `region` string and
	// fails later, in CI, on the tree it checked out. Ask the remote first
	// (build_preflight.go); --skip-preflight is the escape hatch for a build whose
	// spec deliberately lives elsewhere (another branch, another checkout).
	// --dry-run prints the argv and dispatches nothing, so it has nothing to gate.
	if !skipPreflight && !g.dryRun {
		if err := buildPreflight(env); err != nil {
			return err
		}
	}
	return runGated(g, buildArgv(env)...)
}

// up{Tokens,Doctor,Build} are the seams cmdUp drives — package-level vars so a
// unit test can record the call order and inject a failure without the
// cloud-mutating side effects of the real commands. Defaults call the real ones.
var (
	upTokens = func(g globalOpts, admin bool, env string) error { return runTokens(g, admin, env, "", "", "") }
	upDoctor = func(g globalOpts, admin bool, env string) error { return runDoctor("", env, admin, true, "", "") }
	// skipPreflight=true: cmdUp already ran buildPreflight itself (upPreflight,
	// before the token wizard), and running it again here printed the whole
	// unpublished-edits warning block twice in one `llz up`.
	upBuild = func(g globalOpts, env string) error { return cmdBuild([]string{env}, g, true) }
	// upPreflight is the same dispatch check, run before the chain starts; seamed
	// alongside the three stages so the order test can drive it.
	upPreflight = buildPreflight
)

// cmdUp sequences the first-build flow into one command: provision credentials
// (tokens) → confirm the readiness gate (doctor) → dispatch the apply (build),
// then print the steps the tooling can't do for you. It stops at the first
// failure. Cloud-mutating steps honour --yes/--dry-run via the delegated commands.
func cmdUp(env string, g globalOpts, admin, skipTokens bool) error {
	if err := validateEnvName(env); err != nil {
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
		fmt.Println(bold("══ 1/3  llz tokens — provision credentials ══"))
		if err := upTokens(g, admin, env); err != nil {
			return fmt.Errorf("tokens: %w", err)
		}
	}
	fmt.Println("\n" + bold("══ 2/3  llz doctor — readiness gate ══"))
	if err := upDoctor(g, admin, env); err != nil {
		return fmt.Errorf("doctor: %w (fix the above, then re-run `llz up %s`)", err, env)
	}
	fmt.Println("\n" + bold("══ 3/3  llz build — dispatch the apply ══"))
	if err := upBuild(g, env); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	printManualActions(env)
	return nil
}

// printManualActions lists the post-build steps the bootstrap genuinely cannot do
// on the operator's behalf — surfaced once here so they don't get lost.
func printManualActions(env string) {
	b := func(s string) string { return "  " + dim("•") + " " + s }
	fmt.Println("\n" + bold("══ remaining manual actions (the tooling can't do these for you) ══"))
	fmt.Println(b("Watch convergence:   " + cyan("llz status "+env+" --wait")))
	fmt.Println(b("After OpenBao bootstrap, from the job summary (shown once):"))
	fmt.Println(dim("      – escrow unseal keys 4 & 5 + the root token to secure offline storage"))
	// Repeated here because stage 1's banner has scrolled past a full build by now,
	// and this secret has the same "lose it and the data is gone" blast radius.
	fmt.Println(dim("      – and TF_STATE_ENCRYPTION_PASSPHRASE, if `llz tokens` generated one above"))
	fmt.Println(dim("      – delete OPENBAO_ROOT_TOKEN from infra-"+env) + dim("   (`llz status` flags it if left)"))
	fmt.Println(b("DNS-01 certs wire automatically once TF_VAR_linode_dns_token is set at apply"))
	fmt.Println(dim("      (the letsencrypt ClusterIssuers sync via Argo; re-apply TF if the token came later)"))
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
	if err := statusPreflight(args[0]); err != nil {
		warnIfRootTokenPresent(args[0])
		return err
	}
	// Read-only kubectl checks against the cluster kubectl currently points at.
	var firstErr error
	for _, argv := range statusArgv() {
		if err := run(g, argv...); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// Argo CD Application health (report-only by default; --wait polls + gates).
	fmt.Println()
	if err := reportArgoHealth(g, wait, timeout); err != nil && firstErr == nil {
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
	if !lookable("gh") {
		return
	}
	repo, err := resolveInstanceRepo("", false)
	if err != nil {
		return
	}
	for _, n := range ghSecretNames("repos/" + repo + "/environments/infra-" + env + "/secrets") {
		if n == "OPENBAO_ROOT_TOKEN" {
			fmt.Printf("\n%s OPENBAO_ROOT_TOKEN is still set in infra-%s — escrow it offline and delete it.\n", yellow("⚠"), env)
			fmt.Println(dim("  It is only needed to seed secrets at bootstrap; leaving it set is a standing liability."))
			fmt.Printf("  Remove it: %s\n", cyan(fmt.Sprintf("gh secret delete OPENBAO_ROOT_TOKEN --env infra-%s --repo %s", env, repo)))
			return
		}
	}
}
