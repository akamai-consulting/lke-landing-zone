package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

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

func copierUpdateArgv(ref string) []string {
	a := []string{"copier", "update", "--trust"}
	if ref != "" {
		a = append(a, "--vcs-ref", ref, "--data", "llz_version="+ref)
	}
	return a
}

func buildArgv(env string) []string {
	return []string{"gh", "workflow", "run", "terraform.yml",
		"--field", "region=" + env, "--field", "action=apply", "--field", "module=all"}
}

func bootstrapArgv(kind, env string) ([]string, error) {
	wf := map[string]string{
		"dns": "bootstrap-dns.yml",
	}[kind]
	if wf == "" {
		return nil, fmt.Errorf("unknown bootstrap kind %q (want dns)", kind)
	}
	return []string{"gh", "workflow", "run", wf, "--field", "region=" + env}, nil
}

func secretSetArgv(env, name string) []string {
	return []string{"gh", "secret", "set", name, "--env", "infra-" + env}
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
func statusArgv() [][]string {
	return [][]string{
		{"kubectl", "-n", "openbao", "get", "pods"},
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

// templateSourceExistsFn reports whether the --org template source is reachable
// on GitHub; seamed for tests. runNew preflights it because copier clones
// gh:<org>/<template> over HTTPS, and a 404 there (typo'd/un-forked --org)
// surfaces as an interactive `Username for 'https://github.com':` prompt rather
// than a clear error — the failure mode adopters actually hit.
var templateSourceExistsFn = repoExists

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

func runNew(g globalOpts, org, ref, dir string, push bool) error {
	repo := org + "/" + templateName
	if !templateSourceExistsFn(repo) {
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
	cmd("llz validate --env <env>", "catch unfilled placeholders before a build")
	cmd("llz tokens --env <env> --yes", "create state bucket+key, gather PATs, push")
	cmd("llz doctor --env <env>", "confirm every required value is set")
	cmd("llz build <env> --yes", "kick off the apply")
	note("local checks: llz lint / llz validate; add your own commands in .llz/commands.yaml")
}

// pushInstanceRepo creates the instance's GitHub repo and pushes the freshly
// scaffolded tree, closing the §3 loop (the repo learned from .copier-answers.yml).
// Returns whether the push actually happened. Gated by --yes; respects --dry-run.
func pushInstanceRepo(g globalOpts, dir string) (bool, error) {
	a, err := readAnswers(dir)
	if err != nil || a == nil || a.InstanceRepo == "" || a.InstanceRepo == "your-org/your-instance-repo" {
		fmt.Fprintln(os.Stderr, "llz: --push: instance_repo not set in .copier-answers.yml — skipping (create + push by hand)")
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
	// gh repo create makes a new GitHub repo (outward-facing) — gate on --yes.
	return g.yes && !g.dryRun, runGated(g, "gh", "repo", "create", repo,
		"--private", "--source", dir, "--remote", "origin", "--push")
}

func runUpgrade(g globalOpts, ref string) error {
	// Always resolve to a concrete ref so the instance's llz_version pins update in
	// lockstep with the template code (a bare `copier update` would float the code
	// to the latest tag but leave the recorded llz_version stale). updateRepo()
	// names the template this instance tracks (its .copier-answers upstream_org).
	ref, err := scaffoldRef(ref, updateRepo())
	if err != nil {
		return err
	}
	if err := run(g, copierUpdateArgv(ref)...); err != nil {
		return fmt.Errorf("copier update: %w", err)
	}
	// copier update never deletes a file the template dropped between versions, so
	// apply the template's declared removals (.template-removals) ourselves — now
	// up to date from the copier update above. Honors --dry-run internally.
	if err := applyTemplateRemovals(g); err != nil {
		return fmt.Errorf("apply template removals: %w", err)
	}
	// Re-stamp natively: an instance carries no template-scripts/ to shell out to.
	if g.dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) stamp .template-version")
		return nil
	}
	if err := stampTemplateVersion(""); err != nil {
		return fmt.Errorf("stamp template version: %w", err)
	}
	return nil
}

func cmdEnvAdd(g globalOpts, name string, o envAddOpts) error {
	return runEnvAdd(g, name, o)
}

func cmdBuild(args []string, g globalOpts) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: llz build <env>")
	}
	env := args[0]
	if err := validateEnvName(env); err != nil {
		return err
	}
	return runGated(g, buildArgv(env)...)
}

// up{Tokens,Doctor,Build} are the seams cmdUp drives — package-level vars so a
// unit test can record the call order and inject a failure without the
// cloud-mutating side effects of the real commands. Defaults call the real ones.
var (
	upTokens = func(g globalOpts, admin bool, env string) error { return runTokens(g, admin, env, "", "", "") }
	upDoctor = func(g globalOpts, admin bool, env string) error { return runDoctor("", env, admin, true, "", "") }
	upBuild  = func(g globalOpts, env string) error { return cmdBuild([]string{env}, g) }
)

// cmdUp sequences the first-build flow into one command: provision credentials
// (tokens) → confirm the readiness gate (doctor) → dispatch the apply (build),
// then print the steps the tooling can't do for you. It stops at the first
// failure. Cloud-mutating steps honour --yes/--dry-run via the delegated commands.
func cmdUp(env string, g globalOpts, admin, skipTokens bool) error {
	if err := validateEnvName(env); err != nil {
		return err
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
	fmt.Println(dim("      – delete OPENBAO_ROOT_TOKEN from infra-"+env) + dim("   (`llz status` flags it if left)"))
	fmt.Println(b("Once LINODE_DNS_TOKEN exists, finish cert DNS-01:"))
	fmt.Println("      " + cyan("llz bootstrap dns "+env+" --yes"))
}

func cmdStatus(args []string, g globalOpts, wait bool, timeout int) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: llz status <env>")
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

func cmdBootstrap(args []string, g globalOpts) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: llz bootstrap <dns> <env>")
	}
	argv, err := bootstrapArgv(args[0], args[1])
	if err != nil {
		return err
	}
	return runGated(g, argv...)
}
