package cli

import (
	"fmt"
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/buildpreflight"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/reachability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/converge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/environments"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envdef"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/proc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/validate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/newinstance"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/onboard"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/dispatchwatch"
)

// ── argv builders (pure; covered by commands_test.go) ────────────────────────

// buildArgv is the dispatch. assertInvariants forwards terraform.yml's
// `assert_invariants` input, which turns on the post-converge assertion battery
// inside the bootstrap job that already holds cluster access — the assert-suite
// lanes, Loki S3-backed, volume encryption/tags/labels, and the Managed Postgres
// admin credential.
//
// A FIELD RATHER THAN ALWAYS-ON, because the two callers want different answers.
// An operator running `llz build` by hand during a bring-up wants the apply and
// the converge; the assertions are minutes of extra probing against a cluster
// they are watching anyway. An unattended run — the scheduled apply, a promotion
// stage — has nobody watching, and "converged" alone does not say the volumes are
// still encrypted or that Postgres still accepts the credential it was seeded
// with. Those are exactly the invariants that rot silently between applies.
func buildArgv(env string, assertInvariants bool) []string {
	argv := []string{"gh", "workflow", "run", "terraform.yml",
		"--field", "region=" + env, "--field", "action=apply", "--field", "module=all"}
	if assertInvariants {
		argv = append(argv, "--field", "assert_invariants=true")
	}
	return argv
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
	return environments.Run(g.DryRun, name, o)
}

func cmdBuild(args []string, g globalOpts, skipPreflight, watch, assertInvariants bool) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: llz build <env>")
	}
	env := args[0]
	if err := validate.EnvName(env); err != nil {
		return err
	}
	// Rejected UP FRONT rather than after the dispatch, because the disarmed
	// watcher is indistinguishable from a clean run at the point it matters. The
	// caller that wants --watch is an unattended CI step whose exit code is the
	// only signal anyone sees; handing it a dispatch it cannot follow is how a
	// scheduled apply reports success for an apply that failed.
	if watch && !g.Yes {
		return fmt.Errorf("--watch requires --yes: without it nothing is dispatched, so there is no run to follow")
	}
	// Checked HERE rather than left to `gh`, because the downstream symptom is a
	// misdiagnosis. With no token the dispatch is refused, no run is created, and
	// the watcher reports "GitHub never registered the run" — which reads as a
	// GitHub problem and sends the reader to the Actions tab instead of to the
	// repository secret that is actually missing. Only under --watch: an operator
	// at a terminal has an authenticated gh, and this is the unattended path.
	if watch && os.Getenv("GH_TOKEN") == "" && os.Getenv("GITHUB_TOKEN") == "" {
		return fmt.Errorf("--watch needs GH_TOKEN (or GITHUB_TOKEN) in the environment and neither is set — " +
			"in CI that is the LLZ_AUTOMATION_TOKEN repository secret, which an armed scheduled-apply requires; see docs/secrets.md")
	}
	// The dispatch is fire-and-forget — GitHub accepts any `region` string and
	// fails later, in CI, on the tree it checked out. Ask the remote first
	// (build_preflight.go); --skip-preflight is the escape hatch for a build whose
	// spec deliberately lives elsewhere (another branch, another checkout).
	// --dry-run prints the argv and dispatches nothing, so it has nothing to gate.
	if !skipPreflight && !g.DryRun {
		if err := buildpreflight.Run(env); err != nil {
			return err
		}
	}
	// The dispatch is the point of no return AND the point the operator loses
	// sight of the flow — `gh workflow run` prints no run id. Baseline BEFORE the
	// dispatch: "newest run" afterwards is only ours if it is newer than what was
	// there before. See build_watch.go.
	w := dispatchwatch.Begin(g.DryRun, g.Yes, "terraform.yml")
	if err := newinstance.Gated(g.DryRun, g.Yes, buildArgv(env, assertInvariants)...); err != nil {
		return err
	}
	if watch {
		// wait() prints the run it settled on, so report()'s "here is where it went"
		// would be the same fact twice. The two are alternatives, not a sequence.
		return w.Wait()
	}
	w.Report()
	return nil
}

// up{Tokens,Doctor,Build} are the seams cmdUp drives — package-level vars so a
// unit test can record the call order and inject a failure without the
// cloud-mutating side effects of the real commands. Defaults call the real ones.
var (
	upTokens = func(g globalOpts, admin bool, env string) error {
		return onboard.RunTokens(onboardOptsOf(g), admin, env, "", "", "")
	}
	upDoctor = func(g globalOpts, admin bool, env string) error {
		return onboard.RunDoctor("", env, admin, true, "", "")
	}
	// skipPreflight=true: cmdUp already ran buildpreflight.Run itself (upPreflight,
	// before the token wizard), and running it again here printed the whole
	// unpublished-edits warning block twice in one `llz up`.
	// watch=false: `llz up` is the interactive first-build flow, which ends by
	// printing the manual-action checklist. Blocking it for ~40 minutes on the
	// apply would bury that checklist behind a wait the operator did not ask for.
	// assertInvariants=false: `llz up` is an interactive first build the operator
	// is watching, and its converge already proves the platform came up.
	upBuild = func(g globalOpts, env string) error { return cmdBuild([]string{env}, g, true, false, false) }
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
	if !g.DryRun {
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
	// ORDER IS THE POINT. This used to open with `llz status <env> --wait`, which
	// cannot succeed for the first ~20 minutes (the cluster does not exist yet) and
	// then still needs a kubeconfig this machine has none of and a control-plane ACL
	// that has never contained it — none of which it mentioned. So the first
	// instruction after a successful dispatch was one guaranteed to fail, with a
	// fifteen-line remediation block as the reward for following it. Wait for the
	// build, then get access, then check health.
	fmt.Println(b("1. Wait for the build (~40 min) — the run URL + `gh run watch` are printed above."))
	fmt.Println(color.Dim("      A failure there is recoverable: docs/runbooks/first-build-failed.md"))
	fmt.Println(b("2. Get cluster access — the build ran in CI, so this machine has no kubeconfig:"))
	fmt.Println(color.Dim("      export LINODE_API_TOKEN=$(grep ^LINODE_API_TOKEN .llz/secrets.env | cut -d= -f2-)"))
	fmt.Println(color.Dim("      llz ci fetch-kubeconfig --region " + env + " --output ~/.kube/" + env + ".yaml"))
	fmt.Println(color.Dim("      export KUBECONFIG=~/.kube/" + env + ".yaml"))
	fmt.Println(color.Dim("      (refused? `llz ci runner-acl open --region " + env + "` — the ACL never held this host)"))
	fmt.Println(b("3. Watch convergence:   " + color.Cyan("llz status "+env+" --wait")))
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
		if err := proc.RunEcho(g.DryRun, argv...); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// Argo CD Application health (report-only by default; --wait polls + gates).
	fmt.Println()
	if err := converge.ReportArgoHealth(g.DryRun, wait, timeout); err != nil && firstErr == nil {
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
	for _, n := range envreq.GHSecretNames("repos/" + repo + "/environments/infra-" + env + "/secrets") {
		if n == "OPENBAO_ROOT_TOKEN" {
			fmt.Printf("\n%s OPENBAO_ROOT_TOKEN is still set in infra-%s — escrow it offline and delete it.\n", color.Yellow("⚠"), env)
			fmt.Println(color.Dim("  It is only needed to seed secrets at bootstrap; leaving it set is a standing liability."))
			fmt.Printf("  Remove it: %s\n", color.Cyan(fmt.Sprintf("gh secret delete OPENBAO_ROOT_TOKEN --env infra-%s --repo %s", env, repo)))
			return
		}
	}
}
