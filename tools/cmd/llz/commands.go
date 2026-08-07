package main

import (
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/buildpreflight"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/converge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/envadd"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/envdef"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/newinstance"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/onboard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/proc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/reachability"
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
	return newinstance.Gated(g.dryRun, g.yes, buildArgv(env)...)
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
