package main

// import.go is the first phase of the APL-site migration flow: `llz import scan`
// takes a read-only inventory of an existing (pre-LLZ, e.g. Otomi/APL v4.x)
// cluster and writes an import-report.yaml the later phases (`llz import init`,
// which authors a new LandingZone spec from it, and `llz import sync`, which
// emits GitOps artifacts for the discovered workloads) consume.
//
// Scan reaches the old site over the current kubectl context (or --context) —
// the same read pattern as `llz verify`/`llz status`. It reads only metadata:
// node labels, namespaces, workload images, ingress hosts, PVC sizes, and secret
// NAMES (never values). The heavy lifting lives in pure functions (buildReport +
// its parsers) so the mapping is unit-tested the way the rest of the CLI is; the
// RunE is a thin kubectl-and-write shell.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

const (
	importReportAPIVersion = "llz.akamai-consulting.io/v1alpha1"
	importReportKind       = "ImportReport"
	defaultImportReport    = "import-report.yaml"
)

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

type importScanOpts struct {
	kubeconfig      string
	context         string
	output          string
	aplValues       string
	gitRepos        []string
	skipCluster     bool
	linode          bool
	linodeToken     string
	linodeClusterID uint64
}

func importScanCmd() *cobra.Command {
	var o importScanOpts
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
			return runImportScan(gopts, o)
		},
	}
	c.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to the kubeconfig file for the source cluster (default: $KUBECONFIG / ~/.kube/config)")
	c.Flags().StringVar(&o.context, "context", "", "kubectl context within the kubeconfig (default: its current context)")
	c.Flags().BoolVar(&o.skipCluster, "skip-cluster", false, "skip the live-cluster scan; inventory the repos only")
	c.Flags().StringVarP(&o.output, "output", "o", defaultImportReport, "path to write the inventory report")
	c.Flags().StringVar(&o.aplValues, "apl-values", "", "path to the APL 'DOWNLOAD PLATFORM VALUES' file (merged config: version, domain, teams, enabled apps, buckets). Config-only — secret values are never read")
	c.Flags().StringArrayVar(&o.gitRepos, "repo", nil, "path to a local clone of an IaC/app repo to inventory (repeatable; Terraform + Kubernetes)")
	c.Flags().BoolVar(&o.linode, "linode", false, "also query the Linode API for provisioning detail (node-pool autoscaling, VPC CIDR, firewalls, NodeBalancers, object storage)")
	c.Flags().StringVar(&o.linodeToken, "linode-token", "", "Linode PAT for --linode (default: $LINODE_API_TOKEN / $LINODE_TOKEN)")
	c.Flags().Uint64Var(&o.linodeClusterID, "linode-cluster-id", 0, "LKE cluster id for --linode (default: derived from the kube context name, e.g. lke<ID>-ctx)")
	return c
}

func runImportScan(g globalOpts, o importScanOpts) error {
	if g.dryRun {
		fmt.Println(dim("→ (dry-run) read-only inventory scan via kubectl (current context); would write " + o.output))
		return nil
	}

	var ctx string
	// get reads from the live cluster (best-effort: a cluster may lack a resource
	// kind or deny a list — warn and carry on with an empty result). With
	// --skip-cluster it is a no-op, so the report is built from the repos alone.
	get := func(...string) string { return "" }
	if !o.skipCluster {
		ctx = o.context
		if ctx == "" {
			if cur, err := kubectlCtx(o.kubeconfig, "", "config", "current-context"); err == nil {
				ctx = strings.TrimSpace(cur)
			}
		}
		get = func(args ...string) string {
			out, err := kubectlCtx(o.kubeconfig, o.context, args...)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  %s  kubectl %s: %v\n", dim("WARN"), strings.Join(args, " "), err)
			}
			return out
		}
	}

	// Scan local repo clones (the only filesystem I/O; scanRepoTree itself is pure).
	var repos []repoInventory
	if o.aplValues != "" {
		b, err := os.ReadFile(o.aplValues)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s  read %s: %v\n", dim("WARN"), o.aplValues, err)
		} else if sig, err := parseAplValues(string(b)); err != nil {
			fmt.Fprintf(os.Stderr, "  %s  parse %s: %v\n", dim("WARN"), o.aplValues, err)
		} else {
			repos = append(repos, repoInventory{Role: "apl", Path: o.aplValues, APL: &sig})
		}
	}
	for _, r := range o.gitRepos {
		inv := scanRepoTree(os.DirFS(r))
		inv.Role, inv.Path = "git", r
		repos = append(repos, inv)
	}

	report := buildReport(reportInputs{
		context:      ctx,
		repos:        repos,
		versionJSON:  get("version", "-o", "json"),
		nodesJSON:    get("get", "nodes", "-o", "json"),
		nsJSON:       get("get", "namespaces", "-o", "json"),
		workloadJSON: get("get", "deployments,statefulsets,daemonsets,cronjobs,jobs", "-A", "-o", "json"),
		ingressJSON:  get("get", "ingress", "-A", "-o", "json"),
		pvcJSON:      get("get", "pvc", "-A", "-o", "json"),
		secretJSON:   get("get", "secrets", "-A", "-o", "json"),
		// Enrichment reads — Istio/cert-manager/CRD kinds may be absent; get()
		// warns and returns "", which the parsers treat as no data.
		gatewayJSON:        get("get", "gateways.networking.istio.io", "-A", "-o", "json"),
		virtualServiceJSON: get("get", "virtualservices.networking.istio.io", "-A", "-o", "json"),
		certificateJSON:    get("get", "certificates.cert-manager.io", "-A", "-o", "json"),
		lbServiceJSON:      get("get", "services", "-A", "-o", "json"),
		storageClassJSON:   get("get", "storageclasses", "-o", "json"),
		clusterIssuerJSON:  get("get", "clusterissuers.cert-manager.io", "-o", "json"),
		crdJSON:            get("get", "crd", "-o", "json"),
		resourceQuotaJSON:  get("get", "resourcequota", "-A", "-o", "json"),
		// Migration-planning reads.
		configMapJSON:      get("get", "configmaps", "-A", "-o", "json"),
		serviceAccountJSON: get("get", "serviceaccounts", "-A", "-o", "json"),
		networkPolicyJSON:  get("get", "networkpolicies", "-A", "-o", "json"),
		roleJSON:           get("get", "roles", "-A", "-o", "json"),
		roleBindingJSON:    get("get", "rolebindings", "-A", "-o", "json"),
		pvJSON:             get("get", "pv", "-o", "json"),
		cnpgJSON:           get("get", "clusters.postgresql.cnpg.io", "-A", "-o", "json"),
		snapshotClassJSON:  get("get", "volumesnapshotclasses", "-o", "json"),
		peerAuthJSON:       get("get", "peerauthentications.security.istio.io", "-A", "-o", "json"),
		authzPolicyJSON:    get("get", "authorizationpolicies.security.istio.io", "-A", "-o", "json"),
		podJSON:            get("get", "pods", "-A", "-o", "json"),
	})

	// Linode-API enrichment (opt-in): the provisioning detail kubectl can't see.
	if o.linode || o.linodeToken != "" {
		token := firstNonEmpty(o.linodeToken, os.Getenv("LINODE_API_TOKEN"), os.Getenv("LINODE_TOKEN"))
		lk, reason := enrichFromLinode(token, o.linodeClusterID, ctx)
		if reason != "" {
			fmt.Fprintf(os.Stderr, "  %s  %s\n", dim("WARN"), reason)
		}
		report.Linode = lk
	}

	b, err := yaml.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	if err := os.WriteFile(o.output, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", o.output, err)
	}
	printImportSummary(report, o.output)
	return nil
}

// kubectlCtx is kubectlOut with an optional --kubeconfig and --context prepended,
// selecting the source cluster. Either may be empty (kubectl then falls back to
// $KUBECONFIG/~/.kube/config and the kubeconfig's current context).
func kubectlCtx(kubeconfig, context string, args ...string) (string, error) {
	var pre []string
	if kubeconfig != "" {
		pre = append(pre, "--kubeconfig", kubeconfig)
	}
	if context != "" {
		pre = append(pre, "--context", context)
	}
	return kubectlOut(append(pre, args...)...)
}

func printImportSummary(r importReport, output string) {
	fmt.Printf("\n%s\n", bold("Import inventory ("+output+")"))
	fmt.Printf("  cluster   k8s=%s region=%s nodes=%d type=%s%s\n",
		orDash(r.Cluster.KubernetesVersion), orDash(r.Cluster.Region), r.Cluster.NodeCount, orDash(r.Cluster.NodeType), poolsSuffix(r.Cluster.NodePools))
	fmt.Printf("  platform  %s\n", orDash(strings.Join(r.Platform.Detected, ", ")))
	if r.Platform.AplVersion != "" {
		fmt.Printf("  apl       %s\n", r.Platform.AplVersion)
	}
	if r.DNS.DomainSuffix != "" || len(r.DNS.Domains) > 0 {
		fmt.Printf("  dns       suffix=%s domains=%d acme=%s\n", orDash(r.DNS.DomainSuffix), len(r.DNS.Domains), orDash(r.DNS.AcmeEmail))
	}
	fmt.Printf("  teams     %d   workloads %d   hosts %d   LBs %d   PVCs %d (%s)   secrets %d\n",
		len(r.Teams), r.Summary.Workloads, r.Summary.Hosts, r.Summary.LoadBalancers, r.Summary.PVCs, orDash(r.Summary.TotalStorage), r.Summary.Secrets)
	if len(r.Storage.Volumes) > 0 || len(r.Storage.Databases) > 0 {
		fmt.Printf("  data      %d PV(s)   %d database(s)   %d snapshot class(es)\n",
			len(r.Storage.Volumes), len(r.Storage.Databases), len(r.Storage.SnapshotClasses))
		if bc := r.Storage.VolumesByClass; len(bc) > 0 {
			fmt.Printf("  PV class  %s\n", formatClassCounts(bc))
		}
	}
	if r.Security.NetworkPolicies > 0 || len(r.Security.MTLSModes) > 0 {
		fmt.Printf("  security  %d NetworkPolic(ies)   %d AuthorizationPolic(ies)   mTLS=%s\n",
			r.Security.NetworkPolicies, r.Security.AuthorizationPolicies, orDash(strings.Join(r.Security.MTLSModes, ",")))
	}
	if len(r.Platform.HelmReleases) > 0 {
		fmt.Printf("  helm      %d release(s)\n", len(r.Platform.HelmReleases))
	}
	if lk := r.Linode; lk != nil {
		fmt.Printf("  linode    cluster=%d region=%s HA=%t pools=%d VPC=%s firewalls=%d NodeBalancers=%d buckets=%d\n",
			lk.ClusterID, orDash(lk.Region), lk.ControlPlaneHA, len(lk.NodePools), orDash(vpcCIDRSummary(lk.VPC)), len(lk.Firewalls), lk.NodeBalancers, len(lk.ObjectStorage))
	}
	for _, repo := range r.Repos {
		fmt.Printf("  repo      %s [%s]  %s\n", repo.Path, orDash(repo.Role), repoInventoryLine(repo))
	}
	for _, w := range r.Warnings {
		fmt.Printf("  %s  %s\n", red("WARN"), w)
	}
	dir, env := suggestedInstanceDir(r), suggestedEnv(r)
	fmt.Printf("\n%s\n", bold("Next steps"))
	fmt.Printf("  1. review %s\n", output)
	fmt.Printf("  2. scaffold the LLZ instance from it (new + spec + render + checklist):\n")
	fmt.Printf("     %s\n", cyan(fmt.Sprintf("llz import init --report %s --dir %s --env %s", output, dir, env)))
}

// suggestedInstanceDir proposes a scaffold directory for `llz import init`,
// derived from the source cluster's name so the command is copy-pasteable.
func suggestedInstanceDir(r importReport) string {
	if r.Linode != nil && r.Linode.Label != "" {
		return r.Linode.Label
	}
	return "lke-instance"
}

// suggestedEnv proposes a deployment/env name. The source is one cluster, so a
// single conventional default keeps the suggested command runnable.
func suggestedEnv(importReport) string { return "prod" }

// formatClassCounts renders a PV-classification count map as "database 6, cache 5, …"
// in a stable (descending count, then name) order.
func formatClassCounts(byClass map[string]int) string {
	type kv struct {
		k string
		n int
	}
	pairs := make([]kv, 0, len(byClass))
	for k, n := range byClass {
		pairs = append(pairs, kv{k, n})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].n != pairs[j].n {
			return pairs[i].n > pairs[j].n
		}
		return pairs[i].k < pairs[j].k
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, fmt.Sprintf("%s %d", p.k, p.n))
	}
	return strings.Join(parts, ", ")
}

// vpcCIDRSummary renders a VPC's subnet CIDRs (or "" when none / no VPC).
func vpcCIDRSummary(v *lkeVPC) string {
	if v == nil {
		return ""
	}
	return strings.Join(v.Subnets, ",")
}

// poolsSuffix renders " (N pools)" when the node layout has more than one pool.
func poolsSuffix(pools []nodePool) string {
	if len(pools) <= 1 {
		return ""
	}
	return fmt.Sprintf(" (%d pools)", len(pools))
}

// repoInventoryLine summarizes one scanned repo for the terminal (TF resource
// count, kube object kinds, APL teams/apps).
func repoInventoryLine(r repoInventory) string {
	var parts []string
	if r.Terraform != nil {
		n := 0
		for _, c := range r.Terraform.Resources {
			n += c
		}
		parts = append(parts, fmt.Sprintf("tf: %d resource(s) in %d file(s)", n, r.Terraform.Files))
	}
	if r.Kubernetes != nil {
		n := 0
		for _, c := range r.Kubernetes.Kinds {
			n += c
		}
		parts = append(parts, fmt.Sprintf("k8s: %d object(s) in %d file(s)", n, r.Kubernetes.Files))
	}
	if r.APL != nil {
		parts = append(parts, fmt.Sprintf("apl: %d team(s), %d enabled app(s)", len(r.APL.Teams), len(r.APL.EnabledApps)))
	}
	if len(parts) == 0 {
		return "no Terraform or Kubernetes resources found"
	}
	return strings.Join(parts, "; ")
}

// ── report model ─────────────────────────────────────────────────────────────

type importReport struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Source     importSource    `json:"source"`
	Cluster    importCluster   `json:"cluster"`
	Platform   importPlatform  `json:"platform"`
	DNS        importDNS       `json:"dns,omitempty"`
	Network    importNetwork   `json:"network,omitempty"`
	Storage    importStorage   `json:"storage,omitempty"`
	Security   importSecurity  `json:"security,omitempty"`
	Linode     *importLinode   `json:"linode,omitempty"`
	Teams      []importTeam    `json:"teams,omitempty"`
	Repos      []repoInventory `json:"repos,omitempty"`
	Warnings   []string        `json:"warnings,omitempty"`
	Summary    importSummary   `json:"summary"`
}

type importSource struct {
	Context string   `json:"context,omitempty"`
	Repos   []string `json:"repos,omitempty"`
}

type importCluster struct {
	KubernetesVersion string         `json:"kubernetesVersion,omitempty"`
	Region            string         `json:"region,omitempty"`
	NodeCount         int            `json:"nodeCount"`
	NodeType          string         `json:"nodeType,omitempty"` // majority instance type (see nodePools for the full layout)
	NodePools         []nodePool     `json:"nodePools,omitempty"`
	StorageClasses    []storageClass `json:"storageClasses,omitempty"`
}

// importPlatform records the platform apps found running and the LLZ component
// toggles they map to (forward-compat with `llz import init`, which feeds
// Components into the rendered spec). Operators come from installed CRDs (more
// reliable than namespaces); Versions are best-effort from running image tags.
type importPlatform struct {
	Detected     []string          `json:"detected,omitempty"`
	Components   map[string]bool   `json:"components,omitempty"`
	Operators    []string          `json:"operators,omitempty"`
	AplVersion   string            `json:"aplVersion,omitempty"`
	Versions     map[string]string `json:"versions,omitempty"` // app → image tag
	HelmReleases []helmRelease     `json:"helmReleases,omitempty"`
}

// importDNS is the cluster's DNS/cert posture, for spec.dns + cluster.domainSuffix.
type importDNS struct {
	DomainSuffix string   `json:"domainSuffix,omitempty"` // longest common suffix of the discovered hosts
	Domains      []string `json:"domains,omitempty"`
	AcmeEmail    string   `json:"acmeEmail,omitempty"`
	Solvers      []string `json:"solvers,omitempty"` // cert-manager ClusterIssuer solver types (dns01/http01)
}

// importNetwork captures externally-reachable plumbing (NodeBalancers).
type importNetwork struct {
	LoadBalancers []lbService `json:"loadBalancers,omitempty"`
}

type lbService struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Addresses []string `json:"addresses,omitempty"`
}

type importTeam struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	Workloads       int               `json:"workloads"`
	Images          []string          `json:"images,omitempty"` // re-push list for the new registry
	Hosts           []string          `json:"hosts,omitempty"`  // Ingress + Istio VirtualService/Gateway + cert dnsNames
	PVCs            int               `json:"pvcs"`
	Storage         string            `json:"storage,omitempty"`
	Secrets         int               `json:"secrets"`
	SecretRefs      []secretRef       `json:"secretRefs,omitempty"` // re-seed checklist (names+types, never values)
	ConfigMaps      int               `json:"configMaps,omitempty"`
	ServiceAccounts int               `json:"serviceAccounts,omitempty"`
	NetworkPolicies int               `json:"networkPolicies,omitempty"`
	Roles           int               `json:"roles,omitempty"`
	RoleBindings    int               `json:"roleBindings,omitempty"`
	ResourceQuota   map[string]string `json:"resourceQuota,omitempty"` // cpu/memory hard limits
}

type importSummary struct {
	Namespaces    int    `json:"namespaces"`
	Workloads     int    `json:"workloads"`
	Hosts         int    `json:"hosts"`
	LoadBalancers int    `json:"loadBalancers"`
	PVCs          int    `json:"pvcs"`
	TotalStorage  string `json:"totalStorage,omitempty"`
	Secrets       int    `json:"secrets"`
}

// ── assembly (pure) ──────────────────────────────────────────────────────────

type reportInputs struct {
	context                                                                        string
	repos                                                                          []repoInventory
	versionJSON, nodesJSON, nsJSON, workloadJSON, ingressJSON, pvcJSON, secretJSON string
	// Enrichment sources (all best-effort; empty when the CRD/resource is absent).
	gatewayJSON, virtualServiceJSON, certificateJSON, lbServiceJSON string
	storageClassJSON, clusterIssuerJSON, crdJSON                    string
	resourceQuotaJSON                                               string
	// Migration-planning sources (batch 3).
	configMapJSON, serviceAccountJSON, networkPolicyJSON, roleJSON, roleBindingJSON string
	pvJSON, cnpgJSON, snapshotClassJSON, peerAuthJSON, authzPolicyJSON              string
	podJSON                                                                         string // PVC→workload usage classification
}

// buildReport assembles the inventory from raw kubectl JSON. It is pure (no exec,
// no filesystem) so the whole mapping is unit-tested from fixture JSON. The
// section builders below own one report field each; buildReport only sequences
// them and threads the few values two sections share.
func buildReport(in reportInputs) importReport {
	namespaces := parseObjectNames(in.nsJSON)
	workloads := parseWorkloads(in.workloadJSON)
	pvcs := parsePVCs(in.pvcJSON)

	detected, components, warnings := detectComponents(namespaces, workloads)
	// Operators from installed CRDs corroborate (and extend) the namespace-based
	// platform detection.
	operators, crdComponents := parseCRDOperators(in.crdJSON)
	for c := range crdComponents {
		components[c] = true
	}

	// Hosts come from several routing sources (APL routes via Istio, not Ingress).
	hostsByNS := mergeHostSources(
		parseIngressHosts(in.ingressJSON),
		parseIstioHosts(in.gatewayJSON, in.virtualServiceJSON),
		parseCertDNSNames(in.certificateJSON),
	)

	roll := buildTeams(in, namespaces, workloads, pvcs, hostsByNS)
	cluster := buildCluster(in)
	platform := buildPlatform(in, workloads, detected, components, operators)
	storage := buildStorage(in, workloads)
	security := buildSecurity(in, roll.networkPolicies)
	dns := buildDNS(in, hostsByNS)

	// Snapshot the LIVE-only components (namespace + CRD detection) before folding
	// in the APL-declared ones, so the drift check below isn't circular.
	liveComponents := map[string]bool{}
	for k, v := range components {
		liveComponents[k] = v
	}
	foldAplSignals(firstAplSignals(in.repos), &platform, &dns)
	// An empty component set serializes as absent, not as an empty map. Decided
	// once, here, so every producer above can append to a live map.
	if len(platform.Components) == 0 {
		platform.Components = nil
	}

	lbs := parseLoadBalancers(in.lbServiceJSON)
	var repoPaths []string
	for _, r := range in.repos {
		repoPaths = append(repoPaths, r.Path)
	}
	warnings = append(warnings, repoDriftWarnings(cluster, detected, liveComponents, in.repos)...)

	return importReport{
		APIVersion: importReportAPIVersion,
		Kind:       importReportKind,
		Source:     importSource{Context: in.context, Repos: repoPaths},
		Cluster:    cluster,
		Platform:   platform,
		DNS:        dns,
		Network:    importNetwork{LoadBalancers: lbs},
		Storage:    storage,
		Security:   security,
		Teams:      roll.teams,
		Repos:      in.repos,
		Warnings:   warnings,
		Summary: importSummary{
			Namespaces:    len(namespaces),
			Workloads:     len(workloads),
			Hosts:         roll.totalHosts,
			LoadBalancers: len(lbs),
			PVCs:          len(pvcs),
			TotalStorage:  formatStorage(roll.totalBytes),
			Secrets:       roll.totalSecrets,
		},
	}
}

// teamRollup is buildTeams' output: the per-team breakdown plus the cluster-wide
// totals the summary and security sections reuse (so the per-namespace maps are
// walked once).
type teamRollup struct {
	teams           []importTeam
	totalHosts      int
	totalBytes      int64
	totalSecrets    int
	networkPolicies int
}

// buildTeams rolls the per-namespace inventory up into the team breakdown. Only
// APL/Otomi team namespaces become teams; the totals cover the whole cluster.
func buildTeams(in reportInputs, namespaces []string, workloads []workload, pvcs []pvc, hostsByNS map[string][]string) teamRollup {
	wlByNS := map[string]int{}
	for _, w := range workloads {
		wlByNS[w.Namespace]++
	}
	pvcCountByNS := map[string]int{}
	pvcBytesByNS := map[string]int64{}
	var roll teamRollup
	for _, p := range pvcs {
		pvcCountByNS[p.Namespace]++
		b := parseQuantityBytes(p.Size)
		pvcBytesByNS[p.Namespace] += b
		roll.totalBytes += b
	}
	secretsByNS := parseSecretCounts(in.secretJSON)
	for _, n := range secretsByNS {
		roll.totalSecrets += n
	}
	quotaByNS := parseResourceQuotas(in.resourceQuotaJSON)
	imagesByNS := imagesByNamespace(workloads)
	secretsRefByNS := parseSecretInventory(in.secretJSON)
	// The plain per-namespace object counters, in report-field order.
	cmByNS := countByNamespace(in.configMapJSON, skipNoiseConfigMap)
	saByNS := countByNamespace(in.serviceAccountJSON, nil)
	npByNS := countByNamespace(in.networkPolicyJSON, nil)
	roleByNS := countByNamespace(in.roleJSON, nil)
	rbByNS := countByNamespace(in.roleBindingJSON, nil)
	roll.networkPolicies = totalCount(npByNS)

	for _, ns := range namespaces {
		name, ok := teamFromNamespace(ns)
		if !ok {
			continue
		}
		hosts := dedupeSorted(hostsByNS[ns])
		roll.totalHosts += len(hosts)
		roll.teams = append(roll.teams, importTeam{
			Name:            name,
			Namespace:       ns,
			Workloads:       wlByNS[ns],
			Images:          imagesByNS[ns],
			Hosts:           hosts,
			PVCs:            pvcCountByNS[ns],
			Storage:         formatStorage(pvcBytesByNS[ns]),
			Secrets:         secretsByNS[ns],
			SecretRefs:      secretsRefByNS[ns],
			ConfigMaps:      cmByNS[ns],
			ServiceAccounts: saByNS[ns],
			NetworkPolicies: npByNS[ns],
			Roles:           roleByNS[ns],
			RoleBindings:    rbByNS[ns],
			ResourceQuota:   quotaByNS[ns],
		})
	}
	sort.Slice(roll.teams, func(i, j int) bool { return roll.teams[i].Namespace < roll.teams[j].Namespace })
	return roll
}

func buildCluster(in reportInputs) importCluster {
	nodeCount, nodeType, region := parseNodes(in.nodesJSON)
	return importCluster{
		KubernetesVersion: parseServerVersion(in.versionJSON),
		Region:            region,
		NodeCount:         nodeCount,
		NodeType:          nodeType,
		NodePools:         parseNodePools(in.nodesJSON),
		StorageClasses:    parseStorageClasses(in.storageClassJSON),
	}
}

// buildPlatform pairs the caller's already-merged detection results (namespaces +
// CRDs) with the image-tag version guesses and the Helm release inventory.
func buildPlatform(in reportInputs, workloads []workload, detected []string, components map[string]bool, operators []string) importPlatform {
	aplVersion, versions := parseImageVersions(workloads)
	return importPlatform{
		Detected:     detected,
		Components:   components,
		Operators:    operators,
		AplVersion:   aplVersion,
		Versions:     versions,
		HelmReleases: parseHelmReleases(in.secretJSON),
	}
}

// buildStorage assembles the data-migration plan: classified PVs plus the
// databases (CNPG clusters and self-managed DB workloads) and their clients.
func buildStorage(in reportInputs, workloads []workload) importStorage {
	classifiedPVs, pvByClass := classifyVolumes(parsePVs(in.pvJSON), parsePVCConsumers(in.podJSON))
	databases := append(parseCNPGClusters(in.cnpgJSON), detectDBWorkloads(workloads)...)
	databases = attachDBClients(databases, parsePodSecretRefs(in.podJSON))
	return importStorage{
		Volumes:         classifiedPVs,
		VolumesByClass:  pvByClass,
		SnapshotClasses: parseObjectNames(in.snapshotClassJSON),
		Databases:       databases,
	}
}

// buildSecurity takes the NetworkPolicy total from the team rollup (already
// counted there) and adds the Istio authorization/mTLS posture.
func buildSecurity(in reportInputs, networkPolicies int) importSecurity {
	return importSecurity{
		NetworkPolicies:       networkPolicies,
		AuthorizationPolicies: totalCount(countByNamespace(in.authzPolicyJSON, nil)),
		MTLSModes:             parsePeerAuthModes(in.peerAuthJSON),
	}
}

func buildDNS(in reportInputs, hostsByNS map[string][]string) importDNS {
	allHosts := allHostValues(hostsByNS)
	acmeEmail, solvers := parseClusterIssuers(in.clusterIssuerJSON)
	return importDNS{
		DomainSuffix: commonDomainSuffix(allHosts),
		Domains:      allHosts,
		AcmeEmail:    acmeEmail,
		Solvers:      solvers,
	}
}

// foldAplSignals overlays the APL platform-values file, which is authoritative
// for facts the cluster scan can only guess: the APL version, the domain suffix,
// and which components are on. No-op when no apl-role repo was scanned.
//
// Precondition: platform.Components is non-nil (detectComponents guarantees it);
// buildReport nils an empty set only after this fold.
func foldAplSignals(apl *aplSignals, platform *importPlatform, dns *importDNS) {
	if apl == nil {
		return
	}
	if apl.AplVersion != "" {
		platform.AplVersion = apl.AplVersion // beats the image-tag guess
	}
	if apl.DomainSuffix != "" {
		dns.DomainSuffix = apl.DomainSuffix // fills the gap when hosts share no common suffix
		dns.Domains = dedupeSorted(append(dns.Domains, apl.DomainSuffix))
	}
	for c := range aplComponentsFromApps(apl.EnabledApps) {
		platform.Components[c] = true
	}
}

// repoDriftWarnings cross-checks the declared intent in the repos against the
// live cluster and flags divergence — the high-value signal of a migration
// inventory. Compares Terraform-declared region/node count against live, APL
// enabled apps against what's actually running, and APL disabled apps against
// what was detected running (the inverse divergence). liveComponents is
// the LIVE-only component set (namespace + CRD detection, BEFORE the APL fold) so
// the check isn't circular. An APL app counts as running if its mapped LLZ
// component is live, or its name appears in liveDetected — several APL apps share
// one component (loki/prometheus/otel → observability), so a per-name match alone
// produces false positives.
func repoDriftWarnings(cluster importCluster, liveDetected []string, liveComponents map[string]bool, repos []repoInventory) []string {
	var warnings []string
	detectedSet := map[string]bool{}
	for _, a := range liveDetected {
		detectedSet[strings.ToLower(a)] = true
	}
	appRunning := func(app string) bool {
		if c := aplAppComponent[app]; c != "" && liveComponents[c] {
			return true
		}
		return detectedSet[strings.ToLower(app)]
	}
	haveLive := len(detectedSet) > 0 || len(liveComponents) > 0
	for _, r := range repos {
		if r.Terraform != nil {
			if reg := r.Terraform.Vars["region"]; reg != "" && cluster.Region != "" && reg != cluster.Region {
				warnings = append(warnings, fmt.Sprintf("drift: %s declares region %q but the live cluster is in %q", r.Path, reg, cluster.Region))
			}
			if nc := r.Terraform.Vars["node_count"]; nc != "" && cluster.NodeCount != 0 && nc != strconv.Itoa(cluster.NodeCount) {
				warnings = append(warnings, fmt.Sprintf("drift: %s declares node_count %s but the live cluster has %d node(s)", r.Path, nc, cluster.NodeCount))
			}
		}
		if r.APL != nil && haveLive {
			for _, app := range r.APL.EnabledApps {
				if !appRunning(app) {
					warnings = append(warnings, fmt.Sprintf("drift: %s declares app %q enabled but it was not detected running", r.Path, app))
				}
			}
			// Inverse drift: declared disabled but detected running. Match the app's
			// OWN name in liveDetected only (not the component map) — a component
			// being live doesn't mean a disabled sub-app of it is (e.g. alertmanager
			// off inside a running observability), but a directly-detected app like
			// trivy genuinely contradicts a "disabled" declaration.
			for _, app := range r.APL.DisabledApps {
				if detectedSet[strings.ToLower(app)] {
					warnings = append(warnings, fmt.Sprintf("drift: %s declares app %q disabled but it was detected running", r.Path, app))
				}
			}
		}
	}
	return warnings
}

// ── kubectl JSON parsers (pure) ──────────────────────────────────────────────

// k8sObjectMeta is the metadata subset the scan parsers read from a
// `kubectl get … -o json` list item. Cluster-scoped kinds simply leave
// Namespace empty.
type k8sObjectMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// k8sObjectList is a kubectl list read for object metadata alone. Parsers that
// also need spec/type fields declare their own item shape (embedding
// k8sObjectMeta) rather than reusing this.
type k8sObjectList struct {
	Items []struct {
		Metadata k8sObjectMeta `json:"metadata"`
	} `json:"items"`
}

// parseObjectNames returns the sorted object names from a kubectl list — the
// shared body of the namespace and VolumeSnapshotClass scans.
func parseObjectNames(js string) []string {
	var d k8sObjectList
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	var out []string
	for _, it := range d.Items {
		if it.Metadata.Name != "" {
			out = append(out, it.Metadata.Name)
		}
	}
	sort.Strings(out)
	return out
}

func parseServerVersion(js string) string {
	var d struct {
		ServerVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"serverVersion"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return ""
	}
	return d.ServerVersion.GitVersion
}

// parseNodes returns the node count and the MOST COMMON instance-type and region
// labels (a mixed pool reports its majority; ties break lexicographically for
// stable output). Both the GA and legacy beta label keys are consulted.
func parseNodes(js string) (count int, nodeType, region string) {
	var d struct {
		Items []struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return 0, "", ""
	}
	types := map[string]int{}
	regions := map[string]int{}
	for _, it := range d.Items {
		l := it.Metadata.Labels
		if t := firstLabel(l, "node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type"); t != "" {
			types[t]++
		}
		if r := firstLabel(l, "topology.kubernetes.io/region", "failure-domain.beta.kubernetes.io/region"); r != "" {
			regions[r]++
		}
	}
	return len(d.Items), mostCommon(types), mostCommon(regions)
}

type workload struct {
	Namespace string
	Name      string
	Kind      string
	Images    []string
}

func parseWorkloads(js string) []workload {
	// containers holds a pod-template container list; podSpec is the shared shape.
	type podSpec struct {
		Spec struct {
			Containers []struct {
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"spec"`
	}
	var d struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				// Deployment/StatefulSet/DaemonSet/Job: spec.template.spec.containers.
				Template podSpec `json:"template"`
				// CronJob nests the pod template under spec.jobTemplate.spec.template.
				JobTemplate struct {
					Spec struct {
						Template podSpec `json:"template"`
					} `json:"spec"`
				} `json:"jobTemplate"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	var out []workload
	for _, it := range d.Items {
		containers := it.Spec.Template.Spec.Containers
		if len(containers) == 0 {
			containers = it.Spec.JobTemplate.Spec.Template.Spec.Containers // CronJob
		}
		var images []string
		for _, c := range containers {
			if c.Image != "" {
				images = append(images, c.Image)
			}
		}
		out = append(out, workload{
			Namespace: it.Metadata.Namespace,
			Name:      it.Metadata.Name,
			Kind:      it.Kind,
			Images:    images,
		})
	}
	return out
}

// parseIngressHosts maps namespace → ingress rule hosts (networking.k8s.io/v1).
func parseIngressHosts(js string) map[string][]string {
	var d struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				Rules []struct {
					Host string `json:"host"`
				} `json:"rules"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	out := map[string][]string{}
	for _, it := range d.Items {
		for _, r := range it.Spec.Rules {
			if h := normalizeHost(r.Host); h != "" {
				out[it.Metadata.Namespace] = append(out[it.Metadata.Namespace], h)
			}
		}
	}
	return out
}

type pvc struct {
	Namespace    string
	StorageClass string
	Size         string
}

func parsePVCs(js string) []pvc {
	var d struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				StorageClassName string `json:"storageClassName"`
				Resources        struct {
					Requests struct {
						Storage string `json:"storage"`
					} `json:"requests"`
				} `json:"resources"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	var out []pvc
	for _, it := range d.Items {
		out = append(out, pvc{
			Namespace:    it.Metadata.Namespace,
			StorageClass: it.Spec.StorageClassName,
			Size:         it.Spec.Resources.Requests.Storage,
		})
	}
	return out
}

// parseSecretCounts counts user secrets per namespace, skipping the
// service-account tokens and Helm release bookkeeping that would drown out the
// real credential footprint. Secret VALUES are never read.
func parseSecretCounts(js string) map[string]int {
	var d secretList
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	out := map[string]int{}
	for _, it := range d.Items {
		switch it.Type {
		case "kubernetes.io/service-account-token", "helm.sh/release.v1":
			continue
		}
		out[it.Metadata.Namespace]++
	}
	return out
}

// ── component detection (pure) ───────────────────────────────────────────────

// platformSignal maps a substring found in a namespace name to the human-readable
// platform app and the LLZ component toggle it implies. Order is the report's
// "detected" order.
var platformSignals = []struct {
	match     string // namespace substring
	app       string // human label for report.platform.detected
	component string // LLZ component toggle (clusterspec.Components name); "" = no toggle
}{
	{"harbor", "harbor", "harbor"},
	{"loki", "loki", "observability"},
	{"prometheus", "prometheus", "observability"},
	{"grafana", "grafana", "observability"},
	{"monitoring", "monitoring", "observability"},
	{"kyverno", "kyverno", "policyEngine"},
	{"trivy", "trivy", "imageScanning"},
	{"cert-manager", "cert-manager", "certManagerBootstrapCA"},
	{"external-secrets", "external-secrets", "externalSecrets"},
	{"argo-workflows", "argo-workflows", "argoWorkflows"},
	{"argo-events", "argo-events", "argoEvents"},
	{"argocd", "argocd", "argocd"},
	{"gitea", "gitea", "gitea"},
	{"tekton", "tekton", ""},
	{"keycloak", "keycloak", ""},
	{"istio", "istio", ""},
}

// detectComponents inspects the namespace names for known platform apps, returning
// the detected app labels, the LLZ component toggles they imply, and migration
// warnings for pieces LLZ models differently (Gitea, Tekton, Keycloak).
func detectComponents(namespaces []string, _ []workload) (detected []string, components map[string]bool, warnings []string) {
	components = map[string]bool{}
	seen := map[string]bool{}
	for _, sig := range platformSignals {
		if !anyContains(namespaces, sig.match) {
			continue
		}
		if !seen[sig.app] {
			detected = append(detected, sig.app)
			seen[sig.app] = true
		}
		if sig.component != "" {
			components[sig.component] = true
		}
	}
	if seen["gitea"] {
		warnings = append(warnings, "in-cluster Gitea detected — LLZ uses external HTTPS Git (BYO Git on v6); migrate repos off Gitea before cutover")
	}
	if seen["tekton"] {
		warnings = append(warnings, "Tekton pipelines detected — LLZ ships Argo Workflows instead; CI pipelines must be rewritten")
	}
	if seen["keycloak"] {
		warnings = append(warnings, "Keycloak/IDP detected — re-establish the realm + users (or wire an external IDP) on the new cluster")
	}
	// Always a non-nil map: callers fold further components into it. buildReport
	// nils an empty set once, at the end, so it serializes as absent.
	return detected, components, warnings
}

// ── small pure helpers ───────────────────────────────────────────────────────

// teamFromNamespace reports the team name for an APL/Otomi team namespace
// ("team-<name>"), excluding the platform's own "team-admin".
func teamFromNamespace(ns string) (string, bool) {
	const prefix = "team-"
	if !strings.HasPrefix(ns, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(ns, prefix)
	if name == "" || name == "admin" {
		return "", false
	}
	return name, true
}

func firstLabel(labels map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := labels[k]; v != "" {
			return v
		}
	}
	return ""
}

// mostCommon returns the highest-count key, breaking ties lexicographically so
// output is deterministic across runs.
func mostCommon(counts map[string]int) string {
	best := ""
	bestN := 0
	for k, n := range counts {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}

func anyContains(items []string, sub string) bool {
	for _, s := range items {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// parseQuantityBytes converts a Kubernetes storage quantity (e.g. "8Gi", "500Mi",
// "1.5Ti", "1000000") into bytes. Binary (Ki/Mi/Gi/Ti/Pi) and decimal (k/M/G/T/P)
// suffixes are supported; an unparseable value contributes 0.
func parseQuantityBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	units := []struct {
		suf string
		mul float64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50},
		{"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suf) {
			f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, u.suf)), 64)
			if err != nil {
				return 0
			}
			return int64(f * u.mul)
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

// formatStorage renders a byte count back to the largest whole binary unit (e.g.
// 236223201280 → "220Gi"), dropping a trailing ".0". Zero renders empty.
func formatStorage(b int64) string {
	if b <= 0 {
		return ""
	}
	units := []struct {
		suf  string
		size int64
	}{
		{"Pi", 1 << 50}, {"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10},
	}
	for _, u := range units {
		if b >= u.size {
			v := float64(b) / float64(u.size)
			if v == float64(int64(v)) {
				return fmt.Sprintf("%d%s", int64(v), u.suf)
			}
			return fmt.Sprintf("%.1f%s", v, u.suf)
		}
	}
	return fmt.Sprintf("%dB", b)
}
