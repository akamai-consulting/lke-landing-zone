// Package cli assembles the `llz` command tree — the SPINE. It is the one place
// that can see both what the extension registry declares and what the binary
// actually offers, so it is where the two are joined.
//
// Built on spf13/cobra: persistent flags apply to every subcommand —
//
//	--dry-run   print the argv that would run; execute/write nothing
//	--open      open creation links in a browser (open / xdg-open)
//	--yes, -y   actually execute cloud-mutating commands
//
// Cloud-mutating commands (tokens, secrets push, build) execute only with --yes.
// See docs/quickstart.md for the end-to-end flow.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY IT IS NOT package main, WHICH IT WAS UNTIL THIS MOVE.
//
// package main is the one package that cannot be imported, so nothing outside it
// could construct the tree. Three consequences, all of them load-bearing:
//
//   - THE WIRING GUARD COULD ONLY LIVE INSIDE IT. `command_wiring_test.go` — the
//     check that every verb an extension exposes is reachable — had to be written
//     in package main because that is the only place that could see both the tree
//     and the registry. So did every other test about the tree.
//   - THE REGISTRY COULD NOT DRIVE A COMMAND MAIN ASSEMBLED. `template-sustain`
//     was undriveable purely because `sustainDeps()` lived here; internal/shared
//     cannot import main. The assemblers are now cli/deps, which anything may
//     import.
//   - THE BUDGET MEASURED THE WRONG THING. ADR 0014 caps this code because it was
//     untestable-by-language-rule. It no longer is, so the cap survives with a
//     different argument (see .core-surface-budget.yaml): this is the WIRING
//     layer, and wiring is all it may hold. A second, tiny exact budget on
//     tools/cmd/llz keeps that package from re-accreting.
//
// WHAT BELONGS HERE is command construction and the group layout — nothing else.
// Logic that arrives in this package is logic in the one place with no boundary of
// its own, which is the condition ADR 0013 and the extension model both exist to
// end. The remaining ~920 lines that are commands rather than wiring (ci_teardown,
// import, promote, apl_app, ext, ci_guards, ci_gen_toc, ci_managedlock,
// build_watch, ci_build_failure_summary) are the extraction backlog, and the
// budget is what keeps pressure on them.
// ─────────────────────────────────────────────────────────────────────────────
package cli

import (
	"fmt"
	"os"

	openbaoext "github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/openbao"

	"github.com/spf13/cobra"

	clideps "github.com/akamai-consulting/lke-landing-zone/tools/internal/cli/deps"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/reachability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/sustain"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/environments"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/objenc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/reconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/teardown"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/copier"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envdef"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/lint"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/newinstance"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/onboard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/selfupgrade"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/upgrade"
)

// Version is stamped at build time via
// -ldflags "-X .../tools/internal/cli.Version=...".
//
// EXPORTED BECAUSE THE LINKER NEEDS A PATH TO IT. It was `main.version` and
// unexported, which worked only because the stamping target and the consumer were
// the same package. The ldflag moved with this package (llz-release.yml), and
// version_stamp_test.go pins that the two agree — a silently unstamped binary
// reports "dev" forever, which makes every release look newer than itself to
// selfupgrade.
var Version = "dev"

// The reconciler package needs the same stamp and cannot read this one, so main
// hands it over at init. One source, set once.
func init() {
	reconciler.Version = Version
	// Same one-source rule: selfupgrade decides whether an update is AVAILABLE, so
	// a stale "dev" there would make every release look newer than the running
	// binary, forever.
	selfupgrade.Version = Version
	// copier anchors a scaffold to this binary's release when it has one.
	copier.Version = Version
	templatecommit.Version = Version
	// envadd regenerates promote.yml after adding an environment, but the WRITE
	// stays here: internal/promote declares transition:promoted[read-repo] and its
	// own guard refuses a write path, and write-repo is not legal at `promoted`.
	environments.SyncPromoteWorkflow = syncPromoteWorkflow
	// sustain.Deps needs lockableScaffoldFiles and the global --yes. Both used to
	// be main's, which is what made `llz ci managed-fresh` undriveable from the
	// registry; they are cli/deps now and the gate driver assembles the same value.
	upgrade.SustainDeps = clideps.Sustain
	lint.SustainDeps = clideps.Sustain
	newinstance.InstallHooks = func(dryRun, yes bool, dir string) error {
		return lint.RunHooksInstall(globalOpts{DryRun: dryRun, Yes: yes}, dir)
	}
}

// onboardOptsOf narrows globalOpts to the three fields internal/onboard reads.
//
// (Two lines describing globalOpts itself sat here, orphaned above this function
// and stale besides — they said the fields are "populated from the root command's
// flags before any RunE runs", which is the opposite of the reason the alias
// exists. The type's real doc is on its declaration below.)
//
// A CONVERTER, NOT AN EXPORT. Handing the whole struct over would put package
// main's flag model on the other side of a package boundary and make every future
// field visible there whether or not it is read. Three fields, named once.
// A FUNCTION, NOT A METHOD, since globalOpts became an alias for cliopts.Opts
// and Go will not let a package define methods on a type it does not own.
func onboardOptsOf(g globalOpts) onboard.Opts {
	return onboard.Opts{DryRun: g.DryRun, Open: g.Open, Yes: g.Yes}
}

// globalOpts is an ALIAS, not a definition: the fields moved to internal/cliopts
// so an extension's command can read them at RunE time. See that package for why
// the read has to be late rather than threaded in at construction.
type globalOpts = cliopts.Opts

// Main builds the tree, runs it, and returns the process exit code.
//
// IT RETURNS A CODE RATHER THAN CALLING os.Exit, which is the whole reason the
// entry point is worth having as a function at all: os.Exit skips deferred calls
// and cannot be observed by a test. cmd/llz does the exiting, and it is the only
// thing left there.
func Main() int {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, color.Red("llz:"), err)
		return 1
	}
	return 0
}

// driftCmd is `llz drift`. It is handed clideps.Sustain(), one of the deps
// assemblers that make up the CLI's dependency-injection layer.
//
// IT USED TO SAY "STAYS IN PACKAGE MAIN" — the assembler was main's, so any
// command needing it had to be too. That constraint is gone: cli/deps is an
// ordinary package, so this reads as an ordinary command again. What it is NOT is
// an extension: `llz drift` has no declaration, which the unclaimed-command
// ratchet in commands_claimed_test.go counts.
func driftCmd() *cobra.Command {
	var branch, repoURL string
	var strict bool
	c := &cobra.Command{
		Use:   "drift",
		Short: "report how far behind the template this instance is",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return sustain.RunDrift(clideps.Sustain(), branch, repoURL, strict)
		},
	}
	c.Flags().StringVar(&branch, "branch", "main", "template branch to compare against")
	c.Flags().StringVar(&repoURL, "repo-url", "", "override the fetch URL (default: derived from .copier-answers.yml)")
	c.Flags().BoolVar(&strict, "strict", false, "exit non-zero when the instance is behind")
	return c
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "llz",
		Short: "LKE landing-zone instance front-end",
		Long: "llz scaffolds, provisions, and maintains an LKE landing-zone instance.\n" +
			"It orchestrates copier/gh/kubectl/scripts + the Linode API; cloud-mutating\n" +
			"commands run only with --yes.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&cliopts.Global.DryRun, "dry-run", false, "print commands; change nothing")
	pf.BoolVar(&cliopts.Global.Open, "open", false, "open token-creation links in a browser")
	pf.BoolVarP(&cliopts.Global.Yes, "yes", "y", false, "execute cloud-mutating commands (tokens / secrets push / build)")

	root.AddCommand(
		newCmd(), onboard.DoctorCmd(), upgrade.UpgradeCmd(), driftCmd(), envCmd(), environments.SpecCmd(), environments.NetworkCmd(), clusterspec.ComponentsCmd(),
		importCmd(), onboard.SecretsCmd(), onboard.TokensCmd(), render.RenderCmd(), buildCmd(), upCmd(), statusCmd(),
		lint.LintCmd(), lint.FmtCmd(), lint.ValidateCmd(), lint.CheckCmd(), lint.HooksCmd(), lint.PrecommitCmd(),
		teardown.ReapCmd(), openbaoext.OpenbaoCmd(), ciCmd(), credrotate.CredentialsCmd(), reachability.VerifyCmd(), reconciler.Cmd(), objenc.ObjProxyCmd(), versionCmd(), selfupgrade.SelfUpdateCmd(),
		aplCmd(), extensionCmd(),
	)

	// Group the adopter-facing commands in `llz --help` so the front door is
	// legible; CI/plumbing (ci, lint, fmt, hooks, …) falls under "Additional
	// Commands". Groups must be registered before a command references them.
	root.AddGroup(
		&cobra.Group{ID: "apl", Title: "App Platform (front door — ADR 0013, Phase 0):"},
		&cobra.Group{ID: "spec", Title: "Author & deploy (the LandingZone spec):"},
		&cobra.Group{ID: "build", Title: "Provision, build & operate:"},
		&cobra.Group{ID: "day2", Title: "Day-2 & maintenance:"},
	)
	groupOf := map[string]string{
		"apl": "apl",
		"new": "spec", "env": "spec", "spec": "spec", "network": "spec", "components": "spec", "render": "spec", "import": "spec",
		"tokens": "build", "secrets": "build", "doctor": "build", "validate": "build",
		"build": "build", "up": "build", "status": "build",
		"upgrade": "day2", "drift": "day2", "credentials": "day2", "openbao": "day2",
		"verify": "day2", "reap": "day2", "reconcile": "day2", "self-update": "day2",
	}
	for _, c := range root.Commands() {
		if g, ok := groupOf[c.Name()]; ok {
			c.GroupID = g
		}
	}

	// Operator-defined commands from .llz/commands.yaml (added last so the
	// built-in set wins any name collision). See docs/extending-llz.md.
	if cmds, err := loadExtCommands("."); err != nil {
		fmt.Fprintln(os.Stderr, color.Red("llz:"), err)
	} else {
		addExtCommands(root, cmds)
	}

	// Make unknown subcommands fail loud on every command group, not just the
	// root. Cobra only auto-rejects an unknown subcommand at the ROOT (its
	// legacyArgs validator guards on !HasParent); a non-runnable group like
	// `llz ci` instead falls through to its own help text and exits 0. That trap
	// turned a stale-image skew into a SILENT no-op in CI: a baked llz lacking a
	// freshly-added `ci wait-apl-pipeline` ran `llz ci wait-apl-pipeline`, printed
	// help, exited 0 — so the cluster-bootstrap apl_pipeline_ready readiness gate
	// "succeeded" in 0s and the AppProject apply raced the Argo CD CRDs into a
	// hard failure. Reject stray args on every group so the next such skew errors
	// at the gate instead.
	hardenUnknownSubcommands(root)
	return root
}

// hardenUnknownSubcommands walks the command tree and makes every non-runnable
// command group reject positional args, so `llz <group> <unknown>` errors
// ("unknown command") instead of silently printing help and exiting 0. Leaf
// commands (Runnable) and groups that already declare an Args validator are left
// untouched. A real subcommand is dispatched before arg validation runs, so this
// only fires on an arg that resolves AT the group — i.e. an unknown subcommand.
//
// NoArgs alone is not enough: cobra short-circuits a non-runnable command to its
// help text BEFORE validating args (command.go — `if !c.Runnable() { return
// flag.ErrHelp }` precedes ValidateArgs), so a pure group's Args validator is
// never consulted. Pairing NoArgs with a help-printing RunE makes the group
// runnable, so ValidateArgs runs: a stray token is rejected, while a bare
// `llz <group>` falls through to RunE and still prints help + exits 0.
func hardenUnknownSubcommands(cmd *cobra.Command) {
	for _, sub := range cmd.Commands() {
		hardenUnknownSubcommands(sub)
	}
	if cmd.HasSubCommands() && !cmd.Runnable() && cmd.Args == nil {
		cmd.Args = cobra.NoArgs
		cmd.RunE = func(c *cobra.Command, _ []string) error { return c.Help() }
	}
}

// ── setup ────────────────────────────────────────────────────────────────────

func newCmd() *cobra.Command {
	var org, ref string
	var push bool
	c := &cobra.Command{
		Use:   "new [dir]",
		Short: "scaffold a new instance (copier copy; --push to create + push the repo)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			dir := "lke-instance"
			if len(args) > 0 {
				dir = args[0]
			}
			return newinstance.Run(cliopts.Global.DryRun, cliopts.Global.Yes, org, ref, dir, push)
		},
	}
	c.Flags().StringVar(&org, "org", templateid.DefaultOrg, "template org to scaffold from")
	c.Flags().StringVar(&ref, "ref", "", "template release tag to scaffold + pin to (default: this llz binary's version)")
	c.Flags().BoolVar(&push, "push", false, "create the instance_repo on GitHub and push the scaffold (gh repo create; needs --yes)")
	return c
}

// ── run ──────────────────────────────────────────────────────────────────────

func buildCmd() *cobra.Command {
	var skipPreflight bool
	c := &cobra.Command{
		Use: "build <env>", Short: "dispatch the terraform.yml apply (module=all) (--yes)",
		Long: "Dispatches terraform.yml (action=apply module=all) for a deployment. GitHub\n" +
			"runs the workflow from the repo's default branch and renders the tfvars from\n" +
			"the spec IN THAT CHECKOUT, so the build preflights that the deployment exists\n" +
			"locally AND on that branch — a spec that was committed but never pushed would\n" +
			"otherwise fail minutes later, in CI. --skip-preflight bypasses the check.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error { return cmdBuild(args, cliopts.Global, skipPreflight) },
	}
	c.Flags().BoolVar(&skipPreflight, "skip-preflight", false, "dispatch without checking the deployment is on the branch CI builds from")
	return c
}

func upCmd() *cobra.Command {
	var admin, skipTokens bool
	c := &cobra.Command{
		Use:   "up <env>",
		Short: "guided bootstrap: tokens → doctor → build, then the manual-action checklist (--yes)",
		Long: "Sequences the first-build flow into one command: provision credentials\n" +
			"(`llz tokens`), confirm the readiness gate (`llz doctor`), then dispatch the\n" +
			"apply (`llz build`). Stops at the first failure, and ends by printing the\n" +
			"steps the tooling cannot do for you (escrow the OpenBao unseal keys + root\n" +
			"token, delete OPENBAO_ROOT_TOKEN). Cloud-mutating steps need --yes; --dry-run\n" +
			"previews the whole chain.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error { return cmdUp(args[0], cliopts.Global, admin, skipTokens) },
	}
	c.Flags().BoolVar(&admin, "admin", false, "maintainer mode: pass through to `llz tokens --admin`")
	c.Flags().BoolVar(&skipTokens, "skip-tokens", false, "skip the `llz tokens` step (credentials already provisioned)")
	return c
}

func statusCmd() *cobra.Command {
	var wait bool
	var timeout int
	c := &cobra.Command{
		Use: "status <env>", Short: "convergence checks (openbao / argocd / ESO) + Application health",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error { return cmdStatus(args, cliopts.Global, wait, timeout) },
	}
	c.Flags().BoolVar(&wait, "wait", false, "poll until the required support-plane Applications are Synced+Healthy")
	c.Flags().IntVar(&timeout, "timeout", 300, "seconds to wait when --wait is set")
	return c
}

// ── maintain ─────────────────────────────────────────────────────────────────

func envCmd() *cobra.Command {
	env := &cobra.Command{Use: "env", Short: "manage deployments (environments)"}
	var o envdef.Opts
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "scaffold a deployment — authors the LandingZone spec, then renders it",
		Long: "Spec-first: authors landingzone.yaml (on the first env, from\n" +
			".copier-answers.yml + seeded spec.defaults) and one environments/<name>.yaml\n" +
			"ClusterDefinition from the flags, then runs `llz render` to reconcile the spec\n" +
			"into the tfvars + a THIN apl-values/<name>/ overlay (the manifests live ONCE\n" +
			"in platform-apl/manifest + components/, never cloned per env). --region and\n" +
			"--obj-cluster are required (the spec validates them). Layout-aware (instance\n" +
			"root or template checkout). Edit environments/<name>.yaml + re-run `llz render`\n" +
			"(or `llz env set`) to change a deployment.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error { return cmdEnvAdd(cliopts.Global, args[0], o) },
	}
	f := add.Flags()
	f.StringVar(&o.TemplateEnv, "template-env", "example", "template env to clone")
	f.StringVar(&o.Region, "region", "", "GEOGRAPHIC Linode region, e.g. us-sea — not the deployment name (that is the positional <env>)")
	f.StringVar(&o.RegionShort, "region-short", "", "3-letter REGION_SHORT for volume labels (default: first 3 chars of <env>)")
	// DEPRECATED and WRITE-NOTHING. Linode owns the cluster domain on Managed App
	// Platform (lke<id>.akamai-apl.net) and the spec validator REJECTS
	// cluster.bootstrap.domainSuffix outright, so there is nothing for this to set.
	// It survived as a flag that echoed the value back in the summary banner —
	// which read exactly like it had been applied. Marked deprecated so cobra says
	// so on every use rather than leaving the reader to discover it at apply time.
	f.StringVar(&o.ClusterDomain, "cluster-domain", "", "DEPRECATED, ignored: Linode owns the cluster domain and the validator rejects cluster.bootstrap.domainSuffix")
	_ = f.MarkDeprecated("cluster-domain", "ignored — Linode owns the cluster domain (lke<id>.akamai-apl.net) and LLZ discovers it in-cluster, so this writes nothing")
	f.StringVar(&o.ObjCluster, "obj-cluster", "", "Linode Object Storage cluster (e.g. us-sea-1)")
	f.StringVar(&o.K8sVersion, "k8s-version", "", "LKE-E k8s version (a +lke version in your account)")
	f.StringVar(&o.NodeType, "node-type", "", "Linode node type for the pool (e.g. g8-dedicated-8-4; default: example value)")
	f.StringVar(&o.NodeCount, "node-count", "", "node pool size, integer (default: example value)")
	f.StringVar(&o.RunnerIPv4CIDRs, "runner-ipv4-cidrs", "", "comma-separated operator/CI egress IPv4 CIDRs (never 0.0.0.0/0)")
	f.StringVar(&o.RunnerIPv6CIDRs, "runner-ipv6-cidrs", "", "comma-separated operator/CI egress IPv6 CIDRs")
	f.StringVar(&o.AplChartVersion, "apl-chart-version", "", "apl-core chart version (apl_chart_version)")
	f.StringVar(&o.AplValuesRepoURL, "apl-values-repo-url", "", "HTTPS GitOps repo URL (default: derived from instance_repo)")
	f.StringVar(&o.HARole, "ha-role", "", "OpenBao HA role: active | standby | standalone (default: standalone)")
	f.StringVar(&o.HAGroup, "ha-group", "", "OpenBao HA group id (required for --ha-role active|standby; pairs the two peers)")
	f.StringVar(&o.Network, "network", "", "shared VPC name (spec.networks, see `llz network add`) to co-locate in; default: dedicated VPC")
	f.StringVar(&o.SubnetCIDR, "subnet-cidr", "", "cluster.network.subnetCIDR (/13 or /14); HA peers need DISTINCT CIDRs")
	f.IntVar(&o.PromotionRank, "promotion-rank", 0, "position in the code-promotion pipeline (ascending: dev=1, staging=2, prod=3; 0 = not in a pipeline)")
	f.BoolVar(&o.DryRun, "dry-run", false, "print what would be created; write nothing")
	env.AddCommand(add, clusterspec.EnvShowCmd(), environments.SetCmd(), environments.EditCmd(), environments.ListCmd(), environments.RoleCmd(), environments.PeerCmd(), environments.ResolveCmd(), envNextCmd(), envPipelineCmd(), render.EnvVPCCmd())
	return env
}

func envPipelineCmd() *cobra.Command {
	var check bool
	c := &cobra.Command{
		Use:   "pipeline",
		Short: "regenerate .github/workflows/promote.yml from the promotion_rank ordering",
		Long: "Renders the native code-promotion workflow (a static needs:-chain over the\n" +
			"ranked deployments) from each deployment's promotion_rank — the same\n" +
			"generation `llz env add` runs, exposed standalone for the hand-edit path\n" +
			"(you changed a promotion_rank directly in a cluster/<env>.tfvars).\n" +
			"--check writes nothing and exits non-zero when promote.yml has drifted from\n" +
			"the ranks (wire it into CI as the \"did you regenerate?\" gate). Needs ≥2\n" +
			"ranked deployments to form a pipeline; runs only in a rendered instance.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			tfDir, _, relPrefix := instancelayout.Detect()
			changed, err := syncPromoteWorkflow(tfDir, relPrefix, check)
			if err != nil {
				return err
			}
			if check && changed {
				return fmt.Errorf("promote.yml is out of date with the promotion_rank ordering — run `llz env pipeline` and commit the result")
			}
			if check {
				fmt.Println("promote.yml is in sync with the promotion_rank ordering.")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&check, "check", false, "verify promote.yml matches the ranks; exit non-zero on drift (writes nothing)")
	return c
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "print the llz version", Args: cobra.NoArgs,
		Run: func(_ *cobra.Command, _ []string) { fmt.Println("llz " + Version) },
	}
}
