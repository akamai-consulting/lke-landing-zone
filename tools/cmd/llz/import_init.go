package main

// import_init.go is the second phase of the migration flow: `llz import init`
// consumes an import-report.yaml (from `llz import scan`) and runs the COMPLETE
// scaffold — `llz new` (copier), then authors the LandingZone spec from the
// report (the `llz env add` path), sets the component toggles the report found,
// and renders. It also writes MIGRATION-TODO.md + inline notes for everything it
// cannot map from a scan (secret values, data, IDP, workload redeploy).
//
// It deliberately renders the migration TARGET versions, not the source's: the
// source APL/k8s versions describe the platform being left behind. apl-core pins
// to the platform baseline (clusterspec.BaselineAplChartVersion); k8s is left at
// the template default and flagged (a +lke version must be valid in the account).

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/spf13/cobra"
	yamlv3 "gopkg.in/yaml.v3"
	sigyaml "sigs.k8s.io/yaml"
)

// importInitAplChartVersion is the apl-core version an imported site is
// scaffolded onto — the migration TARGET, not the source's (see file header).
// It tracks the platform baseline: a hardcoded pin here silently scaffolds new
// instances a major behind whenever the baseline moves.
const importInitAplChartVersion = clusterspec.BaselineAplChartVersion

type importInitOpts struct {
	report string
	dir    string
	env    string
	org    string
	ref    string
}

func importInitCmd() *cobra.Command {
	var o importInitOpts
	c := &cobra.Command{
		Use:   "init",
		Short: "scaffold a new LLZ instance from an import-report.yaml (new + spec + render + TODO)",
		Long: "Consumes the report from `llz import scan` and runs the full scaffold: it\n" +
			"`llz new`s the instance, authors landingzone.yaml + environments/<env>.yaml\n" +
			"from the report (region, node pool, domain, object storage, components),\n" +
			"renders, and writes MIGRATION-TODO.md listing what a scan can't carry over\n" +
			"(secret values, PV/database data, IDP, Gitea→Git, Tekton→Argo, workload\n" +
			"redeploy). Renders the migration TARGET versions: apl-core " + importInitAplChartVersion + ", and\n" +
			"leaves k8s_version at the template default (set a valid +lke version by hand).",
		Example: "  llz import init --report import-report.yaml --dir ./gsap-llz --env prod",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().NFlag() == 0 {
				return cmd.Help()
			}
			return runImportInit(gopts, o)
		},
	}
	c.Flags().StringVar(&o.report, "report", defaultImportReport, "the import-report.yaml to scaffold from")
	c.Flags().StringVar(&o.dir, "dir", "lke-instance", "directory to scaffold the new instance into")
	c.Flags().StringVar(&o.env, "env", "prod", "deployment/environment name to author")
	c.Flags().StringVar(&o.org, "org", defaultTemplateOrg, "template org to scaffold from")
	c.Flags().StringVar(&o.ref, "ref", "", "template release tag (default: this llz binary's version)")
	return c
}

func runImportInit(g globalOpts, o importInitOpts) error {
	if err := validateEnvName(o.env); err != nil {
		return err
	}
	rep, err := loadImportReport(o.report)
	if err != nil {
		return err
	}

	// 1. Scaffold the instance (copier prompts for identity the report can't supply).
	if err := runNew(g, o.org, o.ref, o.dir, false); err != nil {
		return err
	}

	// 2. Author the spec from the report, then render (the `llz env add` path).
	if err := withinDir(o.dir, func() error {
		if err := runEnvAdd(g, o.env, reportToEnvAddOpts(rep)); err != nil {
			return fmt.Errorf("author spec: %w", err)
		}
		// 3. Apply the component toggles the scan found (comment-preserving, re-render once).
		if assigns := enabledComponentAssignments(rep); len(assigns) > 0 {
			if err := applyComponentToggles(g, o.env, assigns); err != nil {
				return fmt.Errorf("set components: %w", err)
			}
		}
		// 4. Write the migration checklist.
		if err := os.WriteFile(migrationTodoFile, []byte(buildMigrationTodo(rep, o.env)), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", migrationTodoFile, err)
		}
		return nil
	}); err != nil {
		return err
	}

	fmt.Printf("\n%s\n", bold("Imported into "+o.dir))
	fmt.Printf("  spec authored for env %q from %s; review %s/%s for the manual steps.\n", o.env, o.report, o.dir, migrationTodoFile)
	fmt.Printf("  apl-core pinned to %s; k8s_version left at the template default — set a valid +lke version.\n", importInitAplChartVersion)
	return nil
}

const migrationTodoFile = "MIGRATION-TODO.md"

// loadImportReport reads + decodes an import-report.yaml.
func loadImportReport(path string) (importReport, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return importReport{}, fmt.Errorf("read %s: %w (run `llz import scan` first)", path, err)
	}
	var rep importReport
	if err := sigyaml.Unmarshal(b, &rep); err != nil {
		return importReport{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return rep, nil
}

// withinDir runs fn with the process CWD changed to dir, restoring it after. The
// spec-authoring + render helpers are layout-aware off the CWD, so init enters
// the freshly scaffolded instance to drive them.
func withinDir(dir string, fn func() error) error {
	prev, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("enter %s: %w", dir, err)
	}
	defer os.Chdir(prev)
	return fn()
}

// applyComponentToggles sets the components.<name>.enabled=true assignments on the
// env spec file (preserving comments, the same building blocks `llz env set` uses)
// and re-renders.
func applyComponentToggles(g globalOpts, env string, assigns []string) error {
	if g.dryRun {
		fmt.Printf("  %s would set: %s\n", dim("(dry-run)"), strings.Join(assigns, " "))
		return nil
	}
	envFile, err := envSpecFile(env)
	if err != nil {
		return err
	}
	if err := editSpecFile(envFile, func(doc *yamlv3.Node) error {
		for _, a := range assigns {
			i := strings.IndexByte(a, '=')
			if i < 0 {
				continue
			}
			if err := setSpecPath(doc, a[:i], a[i+1:]); err != nil {
				return err
			}
		}
		return nil
	}, func(b []byte) error { _, e := clusterspec.DecodeClusterDefinition(b); return e }); err != nil {
		return err
	}
	for _, a := range assigns {
		fmt.Printf("  %s spec.%s\n", green("set"), a)
	}
	return runRender(g, env, false, false, false)
}

// ── pure mapping (unit-tested) ───────────────────────────────────────────────

// reportToEnvAddOpts maps the scan report onto the `llz env add` inputs. It uses
// the migration TARGET apl-core version and deliberately leaves k8sVersion unset
// (the source version isn't a valid LKE target).
func reportToEnvAddOpts(rep importReport) envAddOpts {
	o := envAddOpts{
		region:          firstNonEmpty(linodeRegion(rep), rep.Cluster.Region),
		clusterDomain:   rep.DNS.DomainSuffix,
		objCluster:      reportObjCluster(rep),
		aplChartVersion: importInitAplChartVersion,
	}
	if nt, nc := largestPool(rep); nt != "" {
		o.nodeType = nt
		if nc > 0 {
			o.nodeCount = strconv.Itoa(nc)
		}
	}
	if rep.Linode != nil && rep.Linode.VPC != nil && len(rep.Linode.VPC.Subnets) > 0 {
		o.subnetCIDR = rep.Linode.VPC.Subnets[0]
	}
	return o
}

func linodeRegion(rep importReport) string {
	if rep.Linode != nil {
		return rep.Linode.Region
	}
	return ""
}

// reportObjCluster picks the Object Storage cluster id (e.g. "us-ord-1"): the APL
// values file carries the exact id; the Linode bucket region is the fallback.
func reportObjCluster(rep importReport) string {
	if apl := firstAplSignals(rep.Repos); apl != nil && apl.ObjectRegion != "" {
		return apl.ObjectRegion
	}
	if rep.Linode != nil {
		for _, b := range rep.Linode.ObjectStorage {
			if b.Region != "" {
				return b.Region
			}
		}
	}
	return ""
}

// largestPool returns the node type + count of the biggest pool — Linode pools
// (authoritative) preferred, then the kube-label-derived pools, then the majority.
func largestPool(rep importReport) (nodeType string, count int) {
	if rep.Linode != nil && len(rep.Linode.NodePools) > 0 {
		best := rep.Linode.NodePools[0]
		for _, p := range rep.Linode.NodePools {
			if p.Count > best.Count {
				best = p
			}
		}
		return best.Type, best.Count
	}
	if len(rep.Cluster.NodePools) > 0 {
		best := rep.Cluster.NodePools[0]
		for _, p := range rep.Cluster.NodePools {
			if p.Count > best.Count {
				best = p
			}
		}
		return best.NodeType, best.Count
	}
	return rep.Cluster.NodeType, rep.Cluster.NodeCount
}

// enabledComponentAssignments turns the report's enabled components into
// `components.<name>.enabled=true` assignments for `llz env set`, skipping the
// MANDATORY components (always-on; the spec doesn't toggle them).
func enabledComponentAssignments(rep importReport) []string {
	mandatory := map[string]bool{}
	for _, c := range clusterspec.Components {
		if c.Mandatory {
			mandatory[c.Name] = true
		}
	}
	var names []string
	for name, on := range rep.Platform.Components {
		if on && !mandatory[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, "components."+n+".enabled=true")
	}
	return out
}

// buildMigrationTodo renders the markdown checklist of everything a scan can't
// carry into the new instance — so nothing is silently dropped.
func buildMigrationTodo(rep importReport, env string) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w("# Migration TODO — %s\n\n", env)
	w("Generated by `llz import init` from the scan report. The instance scaffold +\n")
	w("spec are rendered; the items below are the migration work a scan cannot do.\n\n")

	w("## Source\n")
	w("- APL/Otomi version: **%s** (migrating onto apl-core %s)\n", orNA(rep.Platform.AplVersion), importInitAplChartVersion)
	w("- Cluster: k8s %s, region %s, %d node(s)\n", orNA(rep.Cluster.KubernetesVersion), orNA(rep.Cluster.Region), rep.Cluster.NodeCount)
	w("- Domain: %s\n\n", orNA(rep.DNS.DomainSuffix))

	w("## Decide / set by hand\n")
	w("- [ ] **k8s_version**: left at the template default — set a valid `+lke` version for your account in `cluster/%s.tfvars`.\n", env)
	w("- [ ] **API-server allow CIDRs**: not derivable from the scan — set `apiServerAllowCIDRs` (operator/CI egress) in `environments/%s.yaml`.\n", env)
	w("- [ ] **DNS cutover**: the new cluster gets new IPs; repoint records once validated (source domain suffix was %s).\n\n", orNA(rep.DNS.DomainSuffix))

	// Platform apps the source DISABLED. LLZ components are coarser than APL's
	// per-app flags (e.g. observability bundles alertmanager; policyEngine bundles
	// policy-reporter), so enabling a component turns these back on — they can't be
	// expressed as component toggles and must be re-disabled via apl-values
	// _rawValues if the source's intent is to be preserved.
	if apl := firstAplSignals(rep.Repos); apl != nil && len(apl.DisabledApps) > 0 {
		w("## Platform apps disabled in the source (LLZ components are coarser)\n")
		w("The source had these apps disabled. Enabling a component re-enables its\n")
		w("bundled sub-apps, so re-disable any you still want off via apl-values `_rawValues`:\n")
		w("- [ ] %s\n\n", strings.Join(apl.DisabledApps, ", "))
	}

	// Carried-over warnings (Gitea / Tekton / Keycloak / drift).
	if len(rep.Warnings) > 0 {
		w("## Platform differences (from the scan)\n")
		for _, warn := range rep.Warnings {
			w("- [ ] %s\n", warn)
		}
		w("\n")
	}

	w("## Secrets — re-seed in OpenBao/ESO (values are NOT migrated)\n")
	for _, t := range rep.Teams {
		if len(t.SecretRefs) == 0 {
			continue
		}
		w("- team `%s` (%d): ", t.Name, len(t.SecretRefs))
		names := make([]string, 0, len(t.SecretRefs))
		for _, s := range t.SecretRefs {
			names = append(names, s.Name)
		}
		w("%s\n", strings.Join(names, ", "))
	}
	w("\n")

	w("## Data — migrate (Velero / dump-restore)\n")
	w("- [ ] %d PersistentVolume(s), %s total — see `storage.volumes` (Linode volume handles) in the report.\n", rep.Summary.PVCs, orNA(rep.Summary.TotalStorage))
	if len(rep.Storage.Databases) > 0 {
		w("- [ ] %d database(s) — migrate via the owning app, not raw volume/CDC (version jump):\n", len(rep.Storage.Databases))
		for _, d := range rep.Storage.Databases {
			client := "no client found"
			if len(d.Clients) > 0 {
				client = "client: " + strings.Join(d.Clients, ", ")
			}
			w("    - %s/%s (%s, %s) — %s\n", d.Namespace, d.Name, d.Engine, d.Kind, client)
		}
	}
	if rep.Linode != nil && len(rep.Linode.ObjectStorage) > 0 {
		w("- [ ] %d object-storage bucket(s) to recreate + copy contents (region %s).\n", len(rep.Linode.ObjectStorage), reportObjCluster(rep))
	}
	w("\n")

	w("## Workloads — redeploy (use `llz import sync`)\n")
	for _, t := range rep.Teams {
		w("- [ ] team `%s`: %d workload(s), %d image(s) to re-push, %d ingress host(s).\n", t.Name, t.Workloads, len(t.Images), len(t.Hosts))
	}
	w("\n")

	if len(rep.Platform.HelmReleases) > 0 {
		w("## Installed Helm releases (reference)\n")
		for _, h := range rep.Platform.HelmReleases {
			w("- %s/%s — %s %s\n", h.Namespace, h.Name, orNA(h.Chart), h.ChartVersion)
		}
		w("\n")
	}
	return b.String()
}

func orNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}
