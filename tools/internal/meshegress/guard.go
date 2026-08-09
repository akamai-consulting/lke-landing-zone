package meshegress

// ci_mesh_egress_guard.go implements `llz ci mesh-egress-guard` — the static
// guard extracted from the harbor-reconciler mesh-isolation failure.
//
// apl-core runs the platform app namespaces (harbor, …) under an Istio sidecar
// with STRICT mTLS: a plaintext request from a pod OUTSIDE that mesh to a
// Service inside it is rejected at the sidecar, regardless of NetworkPolicy.
// The batch-5 harbor reconciler learned this the expensive way — a pod in
// llz-reconciler (no sidecar) egressing to harbor-core.harbor.svc could never
// provision Harbor, so harbor stayed a CronJob IN the harbor namespace (in the
// mesh). Two ~50-minute e2e cycles to discover; the netpol :8080 fix was
// necessary-but-not-sufficient noise on the way.
//
// The guard makes that class a PR-time failure: any NetworkPolicy in the
// platform-bootstrap tree (platform-apl/manifest/ + platform-apl/components/)
// whose egress targets a KNOWN STRICT-mesh namespace — from a DIFFERENT namespace
// (i.e. a source pod that is not itself in that mesh) — is flagged. A workload
// that must talk to a mesh-STRICT Service belongs IN that namespace (a
// CronJob/Job/Deployment there, so it gets a sidecar), not in an outside one.
//
// meshStrictNamespaces starts with the one namespace proven STRICT (harbor) and
// is the single place to extend as others are confirmed. Same-namespace egress
// (harbor's own robot-provisioner CronJob → harbor-core) is fine and never
// flagged.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/guardkit"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/guardwalk"
)

// meshStrictNamespaces maps a namespace known to enforce Istio STRICT mTLS to the
// reason a non-mesh client can't reach it. Extend ONLY with namespaces actually
// confirmed STRICT — a false entry would flag a reachable target.
var meshStrictNamespaces = map[string]string{
	"harbor": "harbor-core runs behind an Istio sidecar with STRICT mTLS (apl-core). A plaintext request from a pod outside the harbor namespace is rejected at the sidecar — provisioning must run IN the harbor namespace (the harbor-robot-provisioner CronJob), not from llz-reconciler. See the batch-5 harbor-reconciler revert.",
}

// meshEgressRule is why one cross-mesh egress rule is allowed to stay.
type meshEgressRule struct{ reason, owner string }

// meshEgressAllowed registers cross-mesh egress rules that are accepted for now,
// keyed "<sourceNS>/<policy>-><targetNS>".
//
// Modelled on plaintextAllowed, including the stale-entry failure: an entry whose
// rule no longer exists is deleted, not kept. Rendering the charts made these
// visible for the first time, and the honest disposition for the one below is
// "probably dead, do not delete blind" rather than either silence or a guess.
var meshEgressAllowed = map[string]meshEgressRule{
	"llz-cert-automation/llz-cert-automation-allow-runner-egress->harbor": {
		owner: "llz",
		reason: "the haproxy-rebuild Workflow's push lane, opening :80 and :443 into harbor from an " +
			"`istio-injection: disabled` namespace. WAS 'BELIEVED DEAD' on the grounds that harbor had " +
			"been namespace-wide STRICT since #360, so an unmeshed client's plaintext :80 would be " +
			"rejected at the proxy regardless of this allow. THAT GROUND IS GONE: harbor has never been " +
			"STRICT on any cluster — the pre-split PeerAuthentication carried portLevelMtls on a " +
			"selector-less document, which Istio's CRD rejects by CEL rule, so the apiserver refused " +
			"every apply — and the repaired policy deliberately ships PERMISSIVE for ADR 0010 step 3's " +
			"measurement phase. So this allow has been live and reachable the whole time, and it stays " +
			"reachable until that flip. The OTHER half of the old reasoning still stands and is now the " +
			"only one: the Workflow's push target is `harborUrl`, which defaults to the EXTERNAL " +
			"`https://harbor.<cluster-domain>:5000` and therefore egresses through the ingress gateway, " +
			"not into this namespace. NOT DELETED, deliberately: 'believed unused' is not 'observed " +
			"unused', and the cost of being wrong is that the HAProxy certificate rebuild fails — " +
			"surfacing ~80 days later as an expired edge certificate, which is far worse than an " +
			"over-broad allow. TO CLOSE: the harbor PeerAuthentication's measurement phase answers this " +
			"for free — if istio_requests_total{connection_security_policy=\"none\"} shows no traffic " +
			"into harbor from llz-cert-automation across a converge, the rebuild is going out through " +
			"the gateway as believed; delete both the rule and this entry then",
	},
}

// filterMeshEgressAllowed splits findings into unregistered (returned) and
// registered (reported as ok), and records which allowlist keys were matched so
// stale entries can be failed.
func filterMeshEgressAllowed(all []meFinding) (unregistered []meFinding, seen map[string]bool) {
	seen = map[string]bool{}
	for _, f := range all {
		k := f.sourceNS + "/" + f.policy + "->" + f.targetNS
		rule, ok := meshEgressAllowed[k]
		if !ok {
			unregistered = append(unregistered, f)
			continue
		}
		seen[k] = true
		fmt.Printf("  ok: %s %s — [%s] %s\n", f.file, k, rule.owner, rule.reason)
	}
	return unregistered, seen
}

// meDoc is the minimal NetworkPolicy shape the guard inspects.
type meDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Egress []struct {
			To []struct {
				NamespaceSelector *struct {
					MatchLabels map[string]string `yaml:"matchLabels"`
				} `yaml:"namespaceSelector"`
			} `yaml:"to"`
		} `yaml:"egress"`
	} `yaml:"spec"`
}

// meFinding is one NetworkPolicy → mesh-strict-namespace egress violation.
type meFinding struct {
	file, policy, sourceNS, targetNS, reason string
}

func Cmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "mesh-egress-guard",
		Short: "fail when a NetworkPolicy egresses to a STRICT-mesh namespace from outside that mesh",
		Long: "Static guard for the harbor-reconciler mesh-isolation class: a pod outside an\n" +
			"Istio STRICT-mTLS namespace (e.g. harbor) cannot reach a Service inside it, so a\n" +
			"NetworkPolicy that egresses there from a different namespace describes traffic that\n" +
			"will be silently dropped at the sidecar. Run the client IN that namespace instead.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return Run(root) },
	}
	cmd.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return cmd
}

func Run(root string) error {
	if err := requireRenderedCharts(root); err != nil {
		return err
	}
	dirs := meshEgressScanDirs(root)
	all, examined, err := collectMeshEgressFindings(dirs)
	if err != nil {
		return err
	}
	if err := guardkit.RequireCorpus("mesh-egress-guard", examined, dirs); err != nil {
		return err
	}
	findings, seen := filterMeshEgressAllowed(all)
	for k, rule := range meshEgressAllowed {
		if !seen[k] {
			return fmt.Errorf("mesh-egress-guard: allowlist entry %q matches nothing. The rule was "+
				"removed or re-scoped — delete the entry. Same rationale as plaintextAllowed: a "+
				"registry that outlives its rules stops being reviewable (owner: %s)", k, rule.owner)
		}
	}
	if len(findings) == 0 {
		fmt.Printf("mesh-egress-guard: no unregistered NetworkPolicy egress to a STRICT-mesh namespace (%d allowed).\n", len(meshEgressAllowed))
		return nil
	}
	for _, f := range findings {
		fmt.Printf("::error file=%s::NetworkPolicy %q in namespace %q egresses to the %q namespace, which enforces Istio STRICT mTLS — a pod outside that mesh cannot reach it (traffic is dropped at the sidecar, not by NetworkPolicy). %s\n",
			f.file, f.policy, f.sourceNS, f.targetNS, f.reason)
	}
	return fmt.Errorf("mesh-egress-guard: %d cross-mesh egress rule(s) to a STRICT-mesh namespace", len(findings))
}

// meshEgressScanDirs resolves the scan roots: the platform tree plus the
// RENDERED first-party charts.
//
// The rendered tree is the load-bearing addition. The first-party charts ship
// NetworkPolicies of their own, and scanning platform-apl/ alone was a real blind
// spot rather than a theoretical one — llz-cert-automation's runner egress opens
// :80 and :443 into the STRICT harbor namespace from a namespace labelled
// `istio-injection: disabled`, precisely the shape this guard exists to fail.
//
// Pointing at kubernetes-charts/ does NOT fix that, which is worth stating because
// it is the obvious wrong fix: every chart NetworkPolicy lives under templates/,
// and walkManifests skips templates/ by design (Go-templated YAML parses as
// garbage). Even reaching the file would not help — the target is written
// `kubernetes.io/metadata.name: {{ .Values.networkPolicy.harborNamespace }}`, which
// no amount of YAML parsing resolves to "harbor".
//
// Rendering resolves both problems at once: `make render-charts` materializes the
// charts with values applied, so the namespace is a literal and the file is real
// YAML. That is the same tree k8s-lint and k8s-validate already consume.
func meshEgressScanDirs(root string) []string {
	return append(guardwalk.PlatformTreeDirs(root), filepath.Join(root, renderedChartsDir))
}

// renderedChartsDir mirrors the Makefile's RENDER_DIR default. The two must agree;
// they are kept in sync by TestMeshEgressRenderedDirMatchesMakefile.
const renderedChartsDir = "rendered"

// requireRenderedCharts fails when the rendered tree is absent.
//
// This is requireCorpus's argument applied one level up: a guard whose corpus is
// half-missing prints the same color.Green as one that scanned everything. Since the
// chart-shipped policies are ONLY visible after rendering, running without a
// rendered tree would silently return to the exact blind spot this change closes —
// and it would do so quietly, on a machine where someone forgot a make target.
func requireRenderedCharts(root string) error {
	dir := filepath.Join(root, renderedChartsDir)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("mesh-egress-guard: no rendered charts at %s — the first-party charts' "+
			"NetworkPolicies are only visible once rendered (their templates/ dirs are skipped, and "+
			"their target namespaces are Helm values). Run `make render-charts` first, or `make "+
			"mesh-egress-guard`, which does it for you", dir)
	}
	return nil
}

// collectMeshEgressFindings walks the dirs and flags every NetworkPolicy egress
// whose namespaceSelector targets a meshStrictNamespaces entry from a different
// source namespace.
func collectMeshEgressFindings(dirs []string) (findings []meFinding, examined int, err error) {
	examined, err = guardwalk.Walk(dirs, func(path string, raw []byte) error {
		for _, doc := range guardwalk.DecodeDocs(string(raw), func(d meDoc) bool { return d.Kind == "NetworkPolicy" }) {
			for _, e := range doc.Spec.Egress {
				for _, to := range e.To {
					if to.NamespaceSelector == nil {
						continue
					}
					target := to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
					reason, strict := meshStrictNamespaces[target]
					// Same-namespace egress (the mesh workload's own netpol) is
					// in-mesh and fine; only a DIFFERENT source namespace is a
					// cross-mesh reach.
					if strict && target != doc.Metadata.Namespace {
						findings = append(findings, meFinding{
							file: path, policy: doc.Metadata.Name,
							sourceNS: doc.Metadata.Namespace, targetNS: target, reason: reason,
						})
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, examined, err
	}
	guardwalk.SortFindings(findings, func(f meFinding) (string, string) { return f.file, f.targetNS })
	return findings, examined, nil
}
