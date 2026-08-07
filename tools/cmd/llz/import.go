package main

// import.go — the brownfield-adoption verbs, reduced to flag sets.
//
// Everything they do is `import-brownfield` and lives in tools/internal/brownfield.
// What stays here is brownfieldDeps(): the scaffold pipeline the adoption drives
// (`llz new`, `llz env add`, the two spec editors, `llz render`), a kubectl seam,
// `--yes`, and two constants other verbs also read.
//
// THAT LIST IS THE CATALOG'S PREDICTION, CONFIRMED. It said import's "four
// non-trivial core calls become `llz render` / `llz new` argv" — the most specific
// claim it made about any candidate — and the closure census found exactly those,
// plus kubectl and the constants. See the extension declaration for the one thing
// the catalog got wrong.

import (
	envtopoext "github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/envtopology"
	"github.com/spf13/cobra"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/brownfield"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/envadd"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/envdef"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/newinstance"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/validate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/yamledit"
)

func brownfieldDeps() brownfield.Deps {
	return brownfield.Deps{
		New: func(org, ref, dir string) error {
			return newinstance.Run(cliopts.Global.DryRun, cliopts.Global.Yes, org, ref, dir, false)
		},
		ValidateEnvName: validate.EnvName,
		EnvAdd: func(env string, spec brownfield.EnvSpec) error {
			return envadd.Run(cliopts.Global.DryRun, env, envdef.Opts{
				Region:          spec.Region,
				ClusterDomain:   spec.ClusterDomain,
				ObjCluster:      spec.ObjCluster,
				AplChartVersion: spec.AplChartVersion,
				NodeType:        spec.NodeType,
				NodeCount:       spec.NodeCount,
				SubnetCIDR:      spec.SubnetCIDR,
			})
		},
		EnvSpecFile: envtopoext.SpecFile,
		EditSpec: func(path string, mutate func(*yamlv3.Node) error, parse func([]byte) error) error {
			return yamledit.EditSpecFile(path, mutate, parse)
		},
		SetSpecPath:            yamledit.SetSpecPath,
		Render:                 func(env string) error { return render.Run(cliopts.Global.DryRun, env, false, false, false) },
		KubectlOut:             kubectlprobe.Out,
		Confirm:                func() bool { return cliopts.Global.Yes },
		DefaultAplChartVersion: clusterspec.BaselineAplChartVersion,
		DefaultTemplateOrg:     templateid.DefaultOrg,
	}
}

func importCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "import",
		Short: "migrate an existing APL site onto LLZ (scan → init → sync)",
		Long: "Onboards a pre-LLZ APL site. `llz import scan` inventories the old cluster\n" +
			"(read-only, over the current kubectl context) into an import-report.yaml that\n" +
			"the later phases consume. init (author a LandingZone spec from the report) and\n" +
			"sync (emit GitOps artifacts for the discovered workloads) build on it.\n\n" +
			"All flags live on the subcommand — see `llz import scan --help`.",
		Example: "  # inventory a live cluster + the APL platform-values file + an IaC repo\n" +
			"  llz import scan --apl-values ./platform-values.yaml --repo ./clones/gsap",
	}
	c.AddCommand(importScanCmd(), importInitCmd(), importPlanCmd())
	return c
}

func importScanCmd() *cobra.Command {
	var o brownfield.ScanOpts
	c := &cobra.Command{
		Use:   "scan",
		Short: "read-only inventory of an existing APL site (cluster + repos) → import-report.yaml",
		Long: "Inventories an existing APL site from up to three sources and writes one\n" +
			"report:\n" +
			"  • the live cluster (current kubectl context or --context): k8s version,\n" +
			"    region, node pool, platform apps (→ LLZ component toggles), and a per-team\n" +
			"    breakdown (workloads, ingress hosts, PVC storage, secret counts);\n" +
			"  • --apl-values: the APL 'DOWNLOAD PLATFORM VALUES' file — APL version, domain\n" +
			"    suffix, teams, enabled apps, object-store buckets (config only, no secrets);\n" +
			"  • --repo: any IaC/app repo (no layout assumed) — walks the tree and inventories\n" +
			"    the Terraform + Kubernetes resources it finds.\n" +
			"Repos are read as local clones (clone them first). Where the sources disagree\n" +
			"(declared vs running) it emits drift warnings. Reads only metadata — secret\n" +
			"VALUES are never read. Purely read-only; no --yes needed.",
		Example: "  # live cluster (current kubectl context) + APL platform-values file + an IaC repo\n" +
			"  llz import scan --apl-values ./platform-values.yaml --repo ./clones/gsap\n\n" +
			"  # point at a specific kubeconfig file (and optionally a context within it)\n" +
			"  llz import scan --kubeconfig ./old-cluster.kubeconfig --context old-apl --repo ./clones/gsap\n\n" +
			"  # repos only, no live cluster (scan several repos)\n" +
			"  llz import scan --skip-cluster --repo ./clones/gsap --repo ./clones/infra\n\n" +
			"  # also pull Linode-API provisioning detail (autoscaling, VPC CIDR, firewalls, buckets)\n" +
			"  LINODE_API_TOKEN=… llz import scan --linode --apl-values ./platform-values.yaml\n\n" +
			"  # NOTE: --apl-values is the downloaded file; --repo takes LOCAL CLONE PATHS, not URLs.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Bare `llz import scan` (no flags) has no source to point at and would
			// just spew kubectl-unreachable warnings — show help instead.
			if cmd.Flags().NFlag() == 0 {
				return cmd.Help()
			}
			return brownfield.RunScan(brownfieldDeps(), o)
		},
	}
	c.Flags().StringVar(&o.Kubeconfig, "kubeconfig", "", "path to the kubeconfig file for the source cluster (default: $KUBECONFIG / ~/.kube/config)")
	c.Flags().StringVar(&o.Context, "context", "", "kubectl context within the kubeconfig (default: its current context)")
	c.Flags().BoolVar(&o.SkipCluster, "skip-cluster", false, "skip the live-cluster scan; inventory the repos only")
	c.Flags().StringVarP(&o.Output, "output", "o", brownfield.DefaultImportReport, "path to write the inventory report")
	c.Flags().StringVar(&o.AplValues, "apl-values", "", "path to the APL 'DOWNLOAD PLATFORM VALUES' file (merged config: version, domain, teams, enabled apps, buckets). Config-only — secret values are never read")
	c.Flags().StringArrayVar(&o.GitRepos, "repo", nil, "path to a local clone of an IaC/app repo to inventory (repeatable; Terraform + Kubernetes)")
	c.Flags().BoolVar(&o.Linode, "linode", false, "also query the Linode API for provisioning detail (node-pool autoscaling, VPC CIDR, firewalls, NodeBalancers, object storage)")
	c.Flags().StringVar(&o.LinodeToken, "linode-token", "", "Linode PAT for --linode (default: $LINODE_API_TOKEN / $LINODE_TOKEN)")
	c.Flags().Uint64Var(&o.LinodeClusterID, "linode-cluster-id", 0, "LKE cluster id for --linode (default: derived from the kube context name, e.g. lke<ID>-ctx)")
	return c
}

func importInitCmd() *cobra.Command {
	var o brownfield.InitOpts
	c := &cobra.Command{
		Use:   "init",
		Short: "scaffold a new LLZ instance from an import-report.yaml (new + spec + render + TODO)",
		Long: "Consumes the report from `llz import scan` and runs the full scaffold: it\n" +
			"`llz new`s the instance, authors landingzone.yaml + environments/<env>.yaml\n" +
			"from the report (region, node pool, domain, object storage, components),\n" +
			"renders, and writes MIGRATION-TODO.md listing what a scan can't carry over\n" +
			"(secret values, PV/database data, IDP, Gitea→Git, Tekton→Argo, workload\n" +
			"redeploy). Renders the migration TARGET versions: apl-core " + clusterspec.BaselineAplChartVersion + ", and\n" +
			"leaves k8s_version at the template default (set a valid +lke version by hand).",
		Example: "  llz import init --report import-report.yaml --dir ./gsap-llz --env prod",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().NFlag() == 0 {
				return cmd.Help()
			}
			return brownfield.RunInit(brownfieldDeps(), o)
		},
	}
	c.Flags().StringVar(&o.Report, "report", brownfield.DefaultImportReport, "the import-report.yaml to scaffold from")
	c.Flags().StringVar(&o.Dir, "dir", "lke-instance", "directory to scaffold the new instance into")
	c.Flags().StringVar(&o.Env, "env", "prod", "deployment/environment name to author")
	c.Flags().StringVar(&o.Org, "org", templateid.DefaultOrg, "template org to scaffold from")
	c.Flags().StringVar(&o.Ref, "ref", "", "template release tag (default: this llz binary's version)")
	return c
}

func importPlanCmd() *cobra.Command {
	var o brownfield.PlanOpts
	c := &cobra.Command{
		Use:   "plan",
		Short: "emit a data-migration runbook (OBJ buckets + databases) from an import-report.yaml",
		Long: "Reads the report from `llz import scan` and writes MIGRATION-PLAN.md: concrete,\n" +
			"copy-pasteable commands to move the Object Storage buckets (rclone) and the\n" +
			"databases (CNPG, per owning app) from the source account/cluster to the target\n" +
			"LLZ cluster. Target endpoints/credentials/bucket names are ${PLACEHOLDER} env\n" +
			"vars you fill. Read-only; generates a plan, runs nothing.",
		Example: "  llz import plan --report import-report.yaml -o MIGRATION-PLAN.md\n" +
			"  llz import plan --report import-report.yaml -o -   # print to stdout",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().NFlag() == 0 {
				return cmd.Help()
			}
			return brownfield.RunPlan(o)
		},
	}
	c.Flags().StringVar(&o.Report, "report", brownfield.DefaultImportReport, "the import-report.yaml to plan from")
	c.Flags().StringVarP(&o.Output, "output", "o", brownfield.MigrationPlanFile, `path to write the plan ("-" for stdout)`)
	return c
}
