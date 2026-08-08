// Command llz is the adopter-facing front-end for standing up and maintaining an
// LKE landing-zone instance. It does not reimplement the bootstrap — it
// orchestrates the existing tools the quickstart documents (`copier`, `gh`,
// `kubectl`, the repo's `scripts/*.sh`, the Linode API) and adds the token
// wizard that provisions every credential the instance needs.
//
// Built on spf13/cobra: persistent flags apply to every subcommand —
//
//	--dry-run   print the argv that would run; execute/write nothing
//	--open      open creation links in a browser (open / xdg-open)
//	--yes, -y   actually execute cloud-mutating commands
//
// Cloud-mutating commands (tokens, secrets push, build) execute only with --yes.
// See docs/quickstart.md for the end-to-end flow.
package main

import (
	"fmt"
	"os"

	envtopoext "github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/envtopology"
	openbaoext "github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/openbao"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/envadd"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lint"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/newinstance"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/objenc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/onboard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/reachability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/reconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/selfupgrade"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/sustain"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/teardown"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/upgrade"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/copier"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envdef"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

// The reconciler package needs the same stamp and cannot read this one, so main
// hands it over at init. One source, set once.
func init() {
	reconciler.Version = version
	// Same one-source rule: selfupgrade decides whether an update is AVAILABLE, so
	// a stale "dev" there would make every release look newer than the running
	// binary, forever.
	selfupgrade.Version = version
	// copier anchors a scaffold to this binary's release when it has one.
	copier.Version = version
	templatecommit.Version = version
	// envadd regenerates promote.yml after adding an environment, but the WRITE
	// stays here: internal/promote declares transition:promoted[read-repo] and its
	// own guard refuses a write path, and write-repo is not legal at `promoted`.
	envadd.SyncPromoteWorkflow = syncPromoteWorkflow
	// sustain.Deps needs lockableScaffoldFiles and the global --yes, both main's.
	upgrade.SustainDeps = sustainDeps
	lint.SustainDeps = sustainDeps
	newinstance.InstallHooks = func(dryRun, yes bool, dir string) error {
		return lint.RunHooksInstall(globalOpts{DryRun: dryRun, Yes: yes}, dir)
	}
}

// globalOpts holds the persistent flags shared by every subcommand. It's
// populated from the root command's flags before any RunE runs.
// onboardOpts narrows globalOpts to the three fields internal/onboard reads.
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

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, color.Red("llz:"), err)
		os.Exit(1)
	}
}

// driftCmd STAYS IN PACKAGE MAIN: it is handed sustainDeps(), one of the fifteen
// deps assemblers that make up main's dependency-injection layer. A command that
// needs main to assemble its capability's Deps cannot sit on the far side of that
// assembly — the same reason ci_managedlock.go stayed.
func driftCmd() *cobra.Command {
	var branch, repoURL string
	var strict bool
	c := &cobra.Command{
		Use:   "drift",
		Short: "report how far behind the template this instance is",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return sustain.RunDrift(sustainDeps(), branch, repoURL, strict)
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
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&cliopts.Global.DryRun, "dry-run", false, "print commands; change nothing")
	pf.BoolVar(&cliopts.Global.Open, "open", false, "open token-creation links in a browser")
	pf.BoolVarP(&cliopts.Global.Yes, "yes", "y", false, "execute cloud-mutating commands (tokens / secrets push / build)")

	root.AddCommand(
		newCmd(), onboard.DoctorCmd(), upgrade.UpgradeCmd(), driftCmd(), envCmd(), envtopoext.SpecCmd(), envtopoext.NetworkCmd(), clusterspec.ComponentsCmd(),
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
	env.AddCommand(add, clusterspec.EnvShowCmd(), envtopoext.SetCmd(), envtopoext.EditCmd(), envtopoext.ListCmd(), envtopoext.RoleCmd(), envtopoext.PeerCmd(), envtopoext.ResolveCmd(), envNextCmd(), envPipelineCmd(), render.EnvVPCCmd())
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
		Run: func(_ *cobra.Command, _ []string) { fmt.Println("llz " + version) },
	}
}
