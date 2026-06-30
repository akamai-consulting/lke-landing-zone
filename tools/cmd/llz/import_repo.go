package main

// import_repo.go is the repo-scanning half of `llz import scan`: given a local
// clone of an arbitrary repo, it walks the tree and builds an inventory of the
// Terraform and Kubernetes resources it finds — WITHOUT assuming any layout (the
// gsap case: one repo with TF + kube mixed in no fixed structure). It also runs
// an APL-aware pass that opportunistically extracts Otomi/APL signals (teams,
// enabled apps, domains) from any YAML, so the same scanner serves both the APL
// values repo and a general IaC repo.
//
// The walk is the only I/O; it runs over an fs.FS so scanRepoTree is unit-tested
// with fstest.MapFS, and every classifier/parser below is a pure function.

import (
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
	"gopkg.in/yaml.v3"
)

// repoInventory is one scanned repo's contribution to the report. Role labels how
// it was passed (apl vs git); the sub-sections are present only when that kind of
// resource was found.
type repoInventory struct {
	Role       string              `json:"role,omitempty"` // "apl" | "git"
	Path       string              `json:"path"`
	Terraform  *terraformInventory `json:"terraform,omitempty"`
	Kubernetes *kubeInventory      `json:"kubernetes,omitempty"`
	APL        *aplSignals         `json:"apl,omitempty"`
}

type terraformInventory struct {
	Files       int               `json:"files"`
	Resources   map[string]int    `json:"resources,omitempty"`   // resource type → count
	DataSources map[string]int    `json:"dataSources,omitempty"` // data source type → count
	Modules     []string          `json:"modules,omitempty"`
	Providers   []string          `json:"providers,omitempty"`
	Vars        map[string]string `json:"vars,omitempty"` // high-signal tfvars (region, node_type, node_count, vpc_subnet_cidr, cluster_label)
}

type kubeInventory struct {
	Files      int            `json:"files"`
	Kinds      map[string]int `json:"kinds,omitempty"` // kind → count
	Namespaces []string       `json:"namespaces,omitempty"`
	HelmCharts []string       `json:"helmCharts,omitempty"`
}

// aplSignals are Otomi/APL declarations — either found by content in repo YAML
// (extractAplSignals, resilient to schema drift) or read from the merged
// platform-values file (parseAplValues, which also fills the richer fields below).
type aplSignals struct {
	Teams        []string `json:"teams,omitempty"`
	EnabledApps  []string `json:"enabledApps,omitempty"`
	DisabledApps []string `json:"disabledApps,omitempty"`
	Domains      []string `json:"domains,omitempty"`
	// Richer fields, populated only from the merged platform-values file.
	DomainSuffix  string            `json:"domainSuffix,omitempty"`
	AplVersion    string            `json:"aplVersion,omitempty"`
	ExternalDNS   *bool             `json:"externalDNS,omitempty"`
	ExternalIDP   *bool             `json:"externalIDP,omitempty"`
	Multitenant   *bool             `json:"multitenant,omitempty"`
	ObjectRegion  string            `json:"objectRegion,omitempty"`
	ObjectBuckets map[string]string `json:"objectBuckets,omitempty"` // app → bucket name (never credentials)
}

func (a *aplSignals) empty() bool {
	return len(a.Teams) == 0 && len(a.EnabledApps) == 0 && len(a.DisabledApps) == 0 && len(a.Domains) == 0
}

// scanRepoTree walks fsys, classifies each file, and aggregates a Terraform +
// Kubernetes inventory plus any APL signals. It is pure (fs.FS in, struct out) so
// the whole discovery is tested with fstest.MapFS. Unreadable files and parse
// failures are skipped — a partial inventory beats aborting on one bad file.
func scanRepoTree(fsys fs.FS) repoInventory {
	tf := &terraformInventory{Resources: map[string]int{}, DataSources: map[string]int{}}
	kube := &kubeInventory{Kinds: map[string]int{}}
	apl := &aplSignals{}
	providers := map[string]bool{}
	modules := map[string]bool{}
	namespaces := map[string]bool{}
	var charts []string
	var tfvars terraform.TFVars
	var sawTF, sawKube bool

	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != "." && skipRepoDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		switch classifyRepoFile(p) {
		case fileTerraform:
			b, e := fs.ReadFile(fsys, p)
			if e != nil {
				return nil
			}
			sawTF = true
			tf.Files++
			res, ds, mods, provs := parseTerraformHCL(string(b))
			for k, n := range res {
				tf.Resources[k] += n
			}
			for k, n := range ds {
				tf.DataSources[k] += n
			}
			for _, m := range mods {
				modules[m] = true
			}
			for _, pr := range provs {
				providers[pr] = true
			}
		case fileTFVars:
			b, e := fs.ReadFile(fsys, p)
			if e != nil {
				return nil
			}
			sawTF = true
			tf.Files++
			mergeTFVars(&tfvars, terraform.ParseTFVars(string(b)))
		case fileHelmChart:
			b, e := fs.ReadFile(fsys, p)
			if e != nil {
				return nil
			}
			if name := parseChartName(string(b)); name != "" {
				sawKube = true
				kube.Files++
				charts = append(charts, name)
			}
		case fileYAML:
			b, e := fs.ReadFile(fsys, p)
			if e != nil {
				return nil
			}
			docs := decodeYAMLDocs(string(b))
			var isKube bool
			for _, m := range docs {
				if r, ok := kubeResourceFromDoc(m); ok {
					isKube = true
					kube.Kinds[r.Kind]++
					if r.Namespace != "" {
						namespaces[r.Namespace] = true
					}
				}
				extractAplSignals(m, apl)
			}
			if isKube {
				sawKube = true
				kube.Files++
			}
		}
		return nil
	})

	inv := repoInventory{}
	if sawTF {
		tf.Vars = selectedTFVars(tfvars)
		tf.Modules = sortedSetKeys(modules)
		tf.Providers = sortedSetKeys(providers)
		if len(tf.Resources) == 0 {
			tf.Resources = nil
		}
		if len(tf.DataSources) == 0 {
			tf.DataSources = nil
		}
		inv.Terraform = tf
	}
	if sawKube {
		kube.Namespaces = sortedSetKeys(namespaces)
		kube.HelmCharts = dedupeSorted(charts)
		if len(kube.Kinds) == 0 {
			kube.Kinds = nil
		}
		inv.Kubernetes = kube
	}
	apl.Teams = dedupeSorted(apl.Teams)
	apl.EnabledApps = dedupeSorted(apl.EnabledApps)
	apl.DisabledApps = dedupeSorted(apl.DisabledApps)
	apl.Domains = dedupeSorted(apl.Domains)
	if !apl.empty() {
		inv.APL = apl
	}
	return inv
}

// ── file classification (pure) ───────────────────────────────────────────────

type repoFileKind int

const (
	fileOther repoFileKind = iota
	fileTerraform
	fileTFVars
	fileYAML
	fileHelmChart
)

func classifyRepoFile(p string) repoFileKind {
	base := path.Base(p)
	switch {
	case base == "Chart.yaml" || base == "Chart.yml":
		return fileHelmChart
	case strings.HasSuffix(p, ".tf"):
		return fileTerraform
	case strings.HasSuffix(p, ".tfvars") || strings.HasSuffix(p, ".tfvars.json"):
		return fileTFVars
	case strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml"):
		return fileYAML
	}
	return fileOther
}

// skipRepoDir prunes VCS/dependency/build dirs that would only add noise (and
// huge walk cost) to the inventory.
func skipRepoDir(name string) bool {
	switch name {
	case ".git", ".terraform", "node_modules", "vendor", ".idea", ".vscode":
		return true
	}
	return false
}

// ── Terraform inventory (pure) ───────────────────────────────────────────────

var (
	reTFResource = regexp.MustCompile(`(?m)^\s*resource\s+"([^"]+)"\s+"([^"]+)"`)
	reTFData     = regexp.MustCompile(`(?m)^\s*data\s+"([^"]+)"\s+"([^"]+)"`)
	reTFModule   = regexp.MustCompile(`(?m)^\s*module\s+"([^"]+)"`)
	reTFProvider = regexp.MustCompile(`(?m)^\s*provider\s+"([^"]+)"`)
)

// parseTerraformHCL extracts block headers from .tf content: resource/data types
// (→ count by type) and module/provider names. It is a header scan, not a
// semantic HCL parse — exactly what an inventory needs, and robust without an HCL
// dependency. Comment lines start with # or //, so the ^\s*resource anchor skips
// them.
func parseTerraformHCL(content string) (resources, dataSources map[string]int, modules, providers []string) {
	resources = map[string]int{}
	dataSources = map[string]int{}
	for _, m := range reTFResource.FindAllStringSubmatch(content, -1) {
		resources[m[1]]++
	}
	for _, m := range reTFData.FindAllStringSubmatch(content, -1) {
		dataSources[m[1]]++
	}
	for _, m := range reTFModule.FindAllStringSubmatch(content, -1) {
		modules = append(modules, m[1])
	}
	for _, m := range reTFProvider.FindAllStringSubmatch(content, -1) {
		providers = append(providers, m[1])
	}
	return resources, dataSources, modules, providers
}

// mergeTFVars folds src into dst, first-non-empty wins (matches ParseTFVars'
// within-file rule, extended across files).
func mergeTFVars(dst *terraform.TFVars, src terraform.TFVars) {
	if dst.Region == "" {
		dst.Region = src.Region
	}
	if dst.NodeType == "" {
		dst.NodeType = src.NodeType
	}
	if dst.NodeCount == 0 {
		dst.NodeCount = src.NodeCount
	}
	if dst.ClusterLabel == "" {
		dst.ClusterLabel = src.ClusterLabel
	}
	// ParseTFVars defaults VPCSubnetCIDR; only carry a non-default override.
	if dst.VPCSubnetCIDR == "" || dst.VPCSubnetCIDR == terraform.DefaultVPCSubnetCIDR {
		if src.VPCSubnetCIDR != "" && src.VPCSubnetCIDR != terraform.DefaultVPCSubnetCIDR {
			dst.VPCSubnetCIDR = src.VPCSubnetCIDR
		}
	}
}

// selectedTFVars emits only the high-signal, non-empty provisioning vars (the
// ones `llz import init` maps onto cluster.*).
func selectedTFVars(v terraform.TFVars) map[string]string {
	out := map[string]string{}
	if v.Region != "" {
		out["region"] = v.Region
	}
	if v.NodeType != "" {
		out["node_type"] = v.NodeType
	}
	if v.NodeCount != 0 {
		out["node_count"] = strconv.Itoa(v.NodeCount)
	}
	if v.ClusterLabel != "" {
		out["cluster_label"] = v.ClusterLabel
	}
	if v.VPCSubnetCIDR != "" && v.VPCSubnetCIDR != terraform.DefaultVPCSubnetCIDR {
		out["vpc_subnet_cidr"] = v.VPCSubnetCIDR
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ── Kubernetes + Helm (pure) ─────────────────────────────────────────────────

type kubeResource struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
}

// decodeYAMLDocs decodes every document in a (possibly multi-doc) YAML stream
// into a generic map. A doc that fails to parse — e.g. a Helm template full of
// {{ }} — ends the stream for that file; earlier docs are kept.
func decodeYAMLDocs(content string) []map[string]any {
	dec := yaml.NewDecoder(strings.NewReader(content))
	var out []map[string]any
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break
		}
		if m != nil {
			out = append(out, m)
		}
	}
	return out
}

// kubeResourceFromDoc reads a k8s object's identity from a decoded YAML doc. ok is
// false unless both apiVersion and kind are present (so plain config/values YAML
// is not mistaken for a manifest).
func kubeResourceFromDoc(m map[string]any) (kubeResource, bool) {
	kind, _ := m["kind"].(string)
	av, _ := m["apiVersion"].(string)
	if kind == "" || av == "" {
		return kubeResource{}, false
	}
	r := kubeResource{APIVersion: av, Kind: kind}
	if md, ok := m["metadata"].(map[string]any); ok {
		r.Name, _ = md["name"].(string)
		r.Namespace, _ = md["namespace"].(string)
	}
	return r, true
}

// parseChartName returns a Helm Chart.yaml's name field.
func parseChartName(content string) string {
	var d struct {
		Name string `yaml:"name"`
	}
	if yaml.Unmarshal([]byte(content), &d) != nil {
		return ""
	}
	return d.Name
}

// ── APL/Otomi signal extraction (pure) ───────────────────────────────────────

// extractAplSignals probes a decoded YAML doc for Otomi/APL declarations by key
// presence (teamConfig/teams, apps.<x>.enabled, cluster.domainSuffix / dns), so
// it tolerates the schema differences between APL versions and arbitrary file
// layouts. Found values are appended to sig (deduped by the caller).
func extractAplSignals(m map[string]any, sig *aplSignals) {
	// Teams: `teamConfig:` or `teams:` as a mapping of team-name → config, or a
	// list of {name: ...}.
	for _, key := range []string{"teamConfig", "teams"} {
		switch t := m[key].(type) {
		case map[string]any:
			for name := range t {
				if name != "" && name != "admin" {
					sig.Teams = append(sig.Teams, name)
				}
			}
		case []any:
			for _, e := range t {
				if em, ok := e.(map[string]any); ok {
					if name, _ := em["name"].(string); name != "" && name != "admin" {
						sig.Teams = append(sig.Teams, name)
					}
				}
			}
		}
	}

	// Apps: `apps:` mapping of app-name → {enabled: bool}.
	if apps, ok := m["apps"].(map[string]any); ok {
		for name, v := range apps {
			cfg, ok := v.(map[string]any)
			if !ok {
				continue
			}
			en, present := cfg["enabled"]
			if !present {
				continue
			}
			if b, _ := en.(bool); b {
				sig.EnabledApps = append(sig.EnabledApps, name)
			} else {
				sig.DisabledApps = append(sig.DisabledApps, name)
			}
		}
	}

	// Domain: cluster.domainSuffix, top-level domain, or dns.domainFilters[].
	if cl, ok := m["cluster"].(map[string]any); ok {
		if d, _ := cl["domainSuffix"].(string); d != "" {
			sig.Domains = append(sig.Domains, d)
		}
	}
	if d, _ := m["domain"].(string); d != "" {
		sig.Domains = append(sig.Domains, d)
	}
	if dns, ok := m["dns"].(map[string]any); ok {
		if filters, ok := dns["domainFilters"].([]any); ok {
			for _, f := range filters {
				if s, _ := f.(string); s != "" {
					sig.Domains = append(sig.Domains, s)
				}
			}
		}
	}
}

// ── small helpers ────────────────────────────────────────────────────────────

func sortedSetKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
