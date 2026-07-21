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

	"github.com/spf13/cobra"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

const (
	templateName       = "lke-landing-zone"
	defaultTemplateOrg = "akamai-consulting"
)

// globalOpts holds the persistent flags shared by every subcommand. It's
// populated from the root command's flags before any RunE runs.
type globalOpts struct {
	dryRun bool
	open   bool
	yes    bool
}

var gopts globalOpts

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, red("llz:"), err)
		os.Exit(1)
	}
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
	pf.BoolVar(&gopts.dryRun, "dry-run", false, "print commands; change nothing")
	pf.BoolVar(&gopts.open, "open", false, "open token-creation links in a browser")
	pf.BoolVarP(&gopts.yes, "yes", "y", false, "execute cloud-mutating commands (tokens / secrets push / build)")

	root.AddCommand(
		newCmd(), doctorCmd(), upgradeCmd(), driftCmd(), envCmd(), specCmd(), networkCmd(), componentsCmd(),
		importCmd(), secretsCmd(), tokensCmd(), renderCmd(), buildCmd(), upCmd(), statusCmd(),
		lintCmd(), fmtCmd(), validateCmd(), checkCmd(), hooksCmd(), precommitCmd(),
		reapCmd(), openbaoCmd(), ciCmd(), credentialsCmd(), verifyCmd(), reconcileCmd(), versionCmd(), selfUpdateCmd(),
	)

	// Group the adopter-facing commands in `llz --help` so the front door is
	// legible; CI/plumbing (ci, lint, fmt, hooks, …) falls under "Additional
	// Commands". Groups must be registered before a command references them.
	root.AddGroup(
		&cobra.Group{ID: "spec", Title: "Author & deploy (the LandingZone spec):"},
		&cobra.Group{ID: "build", Title: "Provision, build & operate:"},
		&cobra.Group{ID: "day2", Title: "Day-2 & maintenance:"},
	)
	groupOf := map[string]string{
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
		fmt.Fprintln(os.Stderr, red("llz:"), err)
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
			return runNew(gopts, org, ref, dir, push)
		},
	}
	c.Flags().StringVar(&org, "org", defaultTemplateOrg, "template org to scaffold from")
	c.Flags().StringVar(&ref, "ref", "", "template release tag to scaffold + pin to (default: this llz binary's version)")
	c.Flags().BoolVar(&push, "push", false, "create the instance_repo on GitHub and push the scaffold (gh repo create; needs --yes)")
	return c
}

func doctorCmd() *cobra.Command {
	var repo, env, sshHost, knownHosts string
	var admin bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "am I ready to build? tooling + gh auth + deployment readiness + repo config",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(repo, env, admin, cmd.Flags().Changed("env"), sshHost, knownHosts)
		},
	}
	c.Flags().StringVar(&repo, "repo", "", "instance repo for the readiness check (default: .copier-answers.yml, or example repo in --admin)")
	c.Flags().StringVar(&env, "env", "e2e", "deployment env to check readiness for (scans tfvars + overlay, then the repo config)")
	c.Flags().BoolVar(&admin, "admin", false, "also check the template repo's e2e harness")
	c.Flags().StringVar(&sshHost, "ssh-host", "", "also check port-22 reachability + host keys for this SSH host (e.g. a self-hosted Git host)")
	c.Flags().StringVar(&knownHosts, "known-hosts", "", "with --ssh-host: diff live keys against this committed known_hosts file")
	return c
}

func tokensCmd() *cobra.Command {
	var admin bool
	var env, cluster, bucket, repo string
	c := &cobra.Command{
		Use:   "tokens",
		Short: "provision wizard: create state bucket/key, gather PATs, push",
		Long: "Idempotently provisions an instance's credentials: creates the Terraform-\n" +
			"state OBJ bucket + a scoped key (Linode API), generates the ArgoCD deploy\n" +
			"key, gathers GitHub PATs, computes image vars, writes .llz/*.env, and pushes.\n" +
			"Skips anything already configured. --admin also wires the template repo's\n" +
			"e2e harness and defaults to the example repo. Mutating steps need --yes.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := runTokens(gopts, admin, env, cluster, bucket, repo); err != nil {
				return err
			}
			// Recommend the rest of the flow — but only after a real run (not a
			// dry-run / no-yes plan), and only standalone (`llz up` chains the
			// next gates itself, so it would be redundant there).
			if gopts.yes && !gopts.dryRun {
				eff := env
				if eff == "" {
					eff = "e2e" // matches runTokens' admin default
				}
				printTokensNextSteps(eff)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&admin, "admin", false, "maintainer mode: also wire the template repo e2e harness; default to the example repo")
	c.Flags().StringVar(&env, "env", "", "deployment env to push to (default: e2e)")
	c.Flags().StringVar(&cluster, "cluster", "", "Linode OBJ cluster id, e.g. us-ord-1 (skip the picker)")
	c.Flags().StringVar(&bucket, "bucket", "", "state bucket name (default: <repo>-tfstate)")
	c.Flags().StringVar(&repo, "repo", "", "instance repo <owner>/<name> (default: .copier-answers.yml, or example repo in --admin)")
	return c
}

func secretsCmd() *cobra.Command {
	s := &cobra.Command{Use: "secrets", Short: "gather + push instance credentials"}
	s.AddCommand(
		&cobra.Command{
			Use: "gather", Short: "paste-everything token wizard (links + .llz/*.env)",
			Args: cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error { return gather(gopts, ".") },
		},
		&cobra.Command{
			Use: "push <env>", Short: "write gathered tokens into infra-<env> (--yes)",
			Args: cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error { return pushSecrets(gopts, args[0]) },
		},
	)
	return s
}

// ── run ──────────────────────────────────────────────────────────────────────

func buildCmd() *cobra.Command {
	return &cobra.Command{
		Use: "build <env>", Short: "dispatch the terraform.yml apply (module=all) (--yes)",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error { return cmdBuild(args, gopts) },
	}
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
		RunE: func(_ *cobra.Command, args []string) error { return cmdUp(args[0], gopts, admin, skipTokens) },
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
		RunE: func(_ *cobra.Command, args []string) error { return cmdStatus(args, gopts, wait, timeout) },
	}
	c.Flags().BoolVar(&wait, "wait", false, "poll until the required support-plane Applications are Synced+Healthy")
	c.Flags().IntVar(&timeout, "timeout", 300, "seconds to wait when --wait is set")
	return c
}

// ── maintain ─────────────────────────────────────────────────────────────────

func upgradeCmd() *cobra.Command {
	var ref string
	var commit bool
	c := &cobra.Command{
		Use: "upgrade", Short: "copier update + re-stamp the template version (conflict-gated, summarized, optionally committed)",
		Long: "Updates the instance from its pinned template: `copier update` + apply the\n" +
			"template's declared file removals + re-stamp .template-version. Then gates on\n" +
			"leftover merge-conflict markers (fails loudly instead of shipping them), prints\n" +
			"a one-view summary of the churn, and — with --commit — records it as a single\n" +
			"labeled `chore(template): upgrade vX -> vY` commit so you review one diff.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runUpgrade(gopts, ref, commit) },
	}
	c.Flags().StringVar(&ref, "ref", "", "template release tag to update + re-pin to (default: this llz binary's version)")
	c.Flags().BoolVar(&commit, "commit", false, "stage + record the upgrade as one labeled git commit")
	return c
}

func driftCmd() *cobra.Command {
	var branch, repoURL string
	var strict bool
	c := &cobra.Command{
		Use:   "drift",
		Short: "report how far behind the template this instance is",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return runDrift(branch, repoURL, strict) },
	}
	c.Flags().StringVar(&branch, "branch", "main", "template branch to compare against")
	c.Flags().StringVar(&repoURL, "repo-url", "", "override the fetch URL (default: derived from .template-version)")
	c.Flags().BoolVar(&strict, "strict", false, "exit non-zero when the instance is behind")
	return c
}

func envCmd() *cobra.Command {
	env := &cobra.Command{Use: "env", Short: "manage deployments (environments)"}
	var o envAddOpts
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
		RunE: func(_ *cobra.Command, args []string) error { return cmdEnvAdd(gopts, args[0], o) },
	}
	f := add.Flags()
	f.StringVar(&o.templateEnv, "template-env", "example", "template env to clone")
	f.StringVar(&o.region, "region", "", "Linode region for cluster/<env>.tfvars (e.g. us-sea)")
	f.StringVar(&o.regionShort, "region-short", "", "3-letter REGION_SHORT for volume labels (default: first 3 chars of <env>)")
	f.StringVar(&o.clusterDomain, "cluster-domain", "", "base domain → cluster.domainSuffix (default: <env>.internal)")
	f.StringVar(&o.objCluster, "obj-cluster", "", "Linode Object Storage cluster (e.g. us-sea-1)")
	f.StringVar(&o.k8sVersion, "k8s-version", "", "LKE-E k8s version (a +lke version in your account)")
	f.StringVar(&o.nodeType, "node-type", "", "Linode node type for the pool (e.g. g8-dedicated-8-4; default: example value)")
	f.StringVar(&o.nodeCount, "node-count", "", "node pool size, integer (default: example value)")
	f.StringVar(&o.runnerIPv4CIDRs, "runner-ipv4-cidrs", "", "comma-separated operator/CI egress IPv4 CIDRs (never 0.0.0.0/0)")
	f.StringVar(&o.runnerIPv6CIDRs, "runner-ipv6-cidrs", "", "comma-separated operator/CI egress IPv6 CIDRs")
	f.StringVar(&o.aplChartVersion, "apl-chart-version", "", "apl-core chart version (apl_chart_version)")
	f.StringVar(&o.aplValuesRepoURL, "apl-values-repo-url", "", "HTTPS GitOps repo URL (default: derived from instance_repo)")
	f.StringVar(&o.haRole, "ha-role", "", "OpenBao HA role: active | standby | standalone (default: standalone)")
	f.StringVar(&o.haGroup, "ha-group", "", "OpenBao HA group id (required for --ha-role active|standby; pairs the two peers)")
	f.StringVar(&o.network, "network", "", "shared VPC name (spec.networks, see `llz network add`) to co-locate in; default: dedicated VPC")
	f.StringVar(&o.subnetCIDR, "subnet-cidr", "", "cluster.network.subnetCIDR (/13 or /14); HA peers need DISTINCT CIDRs")
	f.IntVar(&o.promotionRank, "promotion-rank", 0, "position in the code-promotion pipeline (ascending: dev=1, staging=2, prod=3; 0 = not in a pipeline)")
	f.BoolVar(&o.dryRun, "dry-run", false, "print what would be created; write nothing")
	env.AddCommand(add, envShowCmd(), envSetCmd(), envEditCmd(), envListCmd(), envRoleCmd(), envPeerCmd(), envResolveCmd(), envNextCmd(), envPipelineCmd(), envVPCCmd())
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
			tfDir, _, relPrefix := instanceLayout()
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

func openbaoCmd() *cobra.Command {
	s := &cobra.Command{
		Use:   "openbao",
		Short: "read/write secrets in the OpenBao cluster(s) by HA role (KV v2)",
		Long: "Reads from / writes to the OpenBao cluster(s) over the KV v2 HTTP API,\n" +
			"addressed by HA role. `set` dual-writes to an active+standby pair, or\n" +
			"single-writes a standalone deployment (when no standby is configured).\n" +
			"Auth + addresses come from OPENBAO_ADDR_{ACTIVE,STANDBY} and\n" +
			"OPENBAO_TOKEN_{ACTIVE,STANDBY} (or OPENBAO_TOKEN); OPENBAO_NAMESPACE is\n" +
			"optional.\n" +
			"\n" +
			"When OPENBAO_ADDR_ACTIVE is unset on a standalone deployment, get/set open\n" +
			"an ephemeral `kubectl port-forward` to the leader pod in the cluster your\n" +
			"kubectl context points at (TLS verify skipped on the loopback tunnel) — so\n" +
			"a plain `llz openbao get/set` with just a token Just Works, no address to\n" +
			"wire. Set OPENBAO_ADDR_ACTIVE to override. Distinct from `llz secrets`\n" +
			"(which manages GitHub secrets).",
	}
	// `exec` is a thin pass-through to `bao` inside the cluster. SetInterspersed
	// (false) makes cobra STOP flag-parsing at the first positional (the bao
	// subcommand: write / kv / read), so bao's own flags (-f, -format=json, -)
	// reach bao instead of cobra rejecting them ("unknown shorthand flag") — no
	// `--` separator required. llz's global --dry-run still parses because it
	// precedes `openbao`; an explicit `llz openbao exec -- …` also still works.
	execCmd := &cobra.Command{
		Use:   "exec [--] <bao args...>",
		Short: "run a bao command in the cluster via kubectl exec (day-2 auth/policy admin; needs OPENBAO_ROOT_TOKEN)",
		Args:  cobra.MinimumNArgs(1),
		RunE:  func(_ *cobra.Command, a []string) error { return runOpenbaoExec(gopts, a) },
	}
	execCmd.Flags().SetInterspersed(false)

	s.AddCommand(
		&cobra.Command{
			Use:   "get <active|standby> <secret/path> <key>",
			Short: "read one field from a cluster by HA role (value to stdout)",
			Args:  cobra.ExactArgs(3),
			RunE:  func(_ *cobra.Command, a []string) error { return runOpenbaoGet(a[0], a[1], a[2]) },
		},
		&cobra.Command{
			Use:   "set <secret/path> <key=value>...",
			Short: "dual-write to active+standby, or single-write a standalone (--yes); rollback + hash-verify",
			Args:  cobra.MinimumNArgs(2),
			RunE:  func(_ *cobra.Command, a []string) error { return runOpenbaoSet(gopts, a[0], a[1:]) },
		},
		execCmd,
		regenRootCmd(),
	)
	return s
}

func regenRootCmd() *cobra.Command {
	var o regenRootOpts
	c := &cobra.Command{
		Use:   "regen-root <region>",
		Short: "quorum-regenerate the OpenBao root token (3-of-5 unseal keys; kubectl context = target)",
		Long: "Runs the `bao operator generate-root` quorum flow against the active raft\n" +
			"leader in the cluster your kubectl context points at. Unseal keys are read in\n" +
			"terminal raw mode (never echoed/stored). <region> names the infra-<region>\n" +
			"GitHub environment for --update-gha-secret. Run after a bootstrap revokes root.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error { return runRegenRoot(gopts, a[0], o) },
	}
	c.Flags().BoolVar(&o.updateGHA, "update-gha-secret", false, "write the new root to infra-<region>.OPENBAO_ROOT_TOKEN")
	c.Flags().StringVar(&o.repo, "repo", "", "owner/repo for gh (avoids multi-remote auto-detect failures)")
	return c
}

func verifyCmd() *cobra.Command {
	var o verifyOpts
	c := &cobra.Command{
		Use:   "verify",
		Short: "post-bootstrap acceptance snapshot (SSH wiring, platform apps, ESO) — read-only",
		Long: "Read-only validation of a freshly-bootstrapped apl-core cluster against the\n" +
			"current kubectl context: the ArgoCD SSH repository Secret + known_hosts, the\n" +
			"repo-server handshake, platform Applications Synced+Healthy, apl-git-config\n" +
			"pointed at the external HTTPS repo, OpenBao seal status, and the ESO store.\n" +
			"It does not wait — re-run if a check is just mid-reconcile.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runVerify(gopts, o) },
	}
	c.Flags().StringVar(&o.sshSourceHost, "ssh-source-host", "", "SSH source-of-truth host to check for (e.g. a self-hosted Git host); empty skips the SSH-source checks")
	return c
}

func reapCmd() *cobra.Command {
	var o reapOpts
	c := &cobra.Command{
		Use:   "reap",
		Short: "sweep orphaned Linode resources from failed cluster cycles (--yes to delete)",
		Long: "Account-wide manual sweep of Linode resources whose backing LKE cluster is\n" +
			"gone — NodeBalancers, VPCs, Volumes (and, with --cluster-label, the orphan\n" +
			"cluster + its node firewall + BYO VPC), in dependency order. Reads the Linode\n" +
			"PAT from LINODE_API_TOKEN (or LINODE_TOKEN). Dry-run by default; deletes only\n" +
			"with --yes. Volumes need a scope (--region or --volume-ids).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runReap(gopts, o) },
	}
	f := c.Flags()
	f.StringVar(&o.region, "region", "", "scope NodeBalancers/VPCs/Volumes to one Linode region (e.g. us-ord)")
	f.StringVar(&o.clusterLabel, "cluster-label", "", "also reap the orphan cluster + its node firewall + <label>-vpc")
	f.StringVar(&o.env, "env", "", "also reap the deployment's minted Linode creds (obj-storage keys platform-loki-<env>/platform-harbor-registry-<env> + in-cluster PAT llz-incluster-<env>)")
	f.StringVar(&o.fwLabel, "fw-label", "", "exact firewall label to search (default: platform-nodes-fw + <label>-nodes)")
	f.StringVar(&o.volumeIDs, "volume-ids", "", "space-separated Volume id allowlist (scopes the Volume sweep)")
	f.StringVar(&o.tagMustInclude, "tag-must-include", "", "only delete Volumes whose tags include this (e.g. block-storage)")
	f.BoolVar(&o.force, "force", false, "delete the node firewall even if a live cluster still carries --cluster-label")
	return c
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "print the llz version", Args: cobra.NoArgs,
		Run: func(_ *cobra.Command, _ []string) { fmt.Println("llz " + version) },
	}
}

func selfUpdateCmd() *cobra.Command {
	var ref, repo string
	c := &cobra.Command{
		Use:   "self-update",
		Short: "replace this llz binary with a release build (gh-authenticated, checksum-verified)",
		Long: "Downloads an llz release for this OS/arch from the template repo (via `gh`,\n" +
			"so it works against a private repo), verifies it against the\n" +
			"release SHA256SUMS, and atomically overwrites the running binary. Defaults to\n" +
			"the latest vX.Y.Z release; --dry-run reports the target without installing.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runSelfUpdate(gopts, repo, ref) },
	}
	c.Flags().StringVar(&ref, "ref", "", "release to install, e.g. v0.2.0 (default: latest)")
	c.Flags().StringVar(&repo, "repo", "", "template repo to pull from (default: upstream_org/lke-landing-zone)")
	return c
}
