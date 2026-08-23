package defaultdeny

// guard.go implements `llz ci default-deny-egress` — a pod whose egress is
// policed by some NetworkPolicy must be granted some egress by one.
//
// WHY: ZERO EGRESS IS INDISTINGUISHABLE FROM A HEALTHY POD. NetworkPolicies are
// additive and there is no "deny" rule: a namespace-wide policy with
// `podSelector: {}` and `policyTypes: [Egress]` starts policing every pod in the
// namespace, and any pod that no companion policy selects is left able to reach
// nothing at all — not the apiserver, not even DNS. Nothing reports it. The pod
// stays 1/1 Running, its endpoints stay healthy, and whatever it was supposed to
// do simply never happens.
//
// IT HAPPENED TO openbao-cert-watcher, and the shape is worth keeping. That
// Deployment exists to notice the OpenBao serving certificate rotating and
// restart the StatefulSet so the new one is loaded. llz-openbao-platform ships
// `openbao-default-deny` over Ingress AND Egress with `podSelector: {}`, and its
// one companion, `openbao-allow`, selects `app.kubernetes.io/name: openbao`. The
// watcher carries `app.kubernetes.io/name: openbao-cert-watcher`. Its own policy
// declared `policyTypes: [Ingress]` and a comment saying there was "no egress
// restriction" — true of the file, false of the namespace. Every
// `kubectl get certificate` was dropped, the loop logged "not readable yet
// (rbac? not issued?)" once a minute forever, and at renewal nothing restarted
// OpenBao. The secret-store cascade the Deployment exists to prevent happened
// exactly as if it were not deployed.
//
// THE JOIN IS THE POINT. The default-deny is in a CHART
// (kubernetes-charts/llz-openbao-platform) and the pod it stranded is in
// platform-apl/. Reading either tree alone shows nothing wrong, and neither
// kube-linter nor the kind dry-run reads platform-apl/ at all. This guard reads
// both and matches them by namespace, which is why it needs the RENDERED charts:
// a chart's policy lives under templates/ (skipped — Go-templated YAML parses as
// garbage) and its namespace is `{{ .Release.Namespace }}`.
//
// WHAT IT DOES NOT CLAIM. It does not judge whether the egress a pod is granted
// is SUFFICIENT — that is not decidable here, and a pod allowed only DNS passes.
// The line it draws is the one that is decidable and was crossed: policed, and
// granted nothing.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/guardkit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/guardwalk"
)

// renderedChartsDir mirrors the Makefile's RENDER_DIR default. The two must
// agree; TestRenderedDirMatchesMakefile keeps them in step.
const renderedChartsDir = "rendered"

// workloadKinds are the kinds that carry a pod template this guard can read a
// label set off. A bare Pod is included because platform-apl/ is free to ship one.
var workloadKinds = map[string]bool{
	"Deployment": true, "StatefulSet": true, "DaemonSet": true,
	"Job": true, "CronJob": true, "Pod": true, "ReplicaSet": true,
}

// selector is a NetworkPolicy podSelector, or a workload's own selector.
type selector struct {
	MatchLabels      map[string]string `yaml:"matchLabels"`
	MatchExpressions []any             `yaml:"matchExpressions"`
}

type podTemplate struct {
	Metadata struct {
		Labels map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
}

// ddDoc is the sliver of a manifest this guard reads. One struct for both kinds,
// because guardwalk decodes a file once and the fields are disjoint.
type ddDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace"`
		Labels    map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Spec struct {
		// NetworkPolicy
		PodSelector *selector        `yaml:"podSelector"`
		PolicyTypes []string         `yaml:"policyTypes"`
		Egress      []map[string]any `yaml:"egress"`
		// Workloads
		Template    *podTemplate `yaml:"template"`
		JobTemplate *struct {
			Spec struct {
				Template podTemplate `yaml:"template"`
			} `yaml:"spec"`
		} `yaml:"jobTemplate"`
	} `yaml:"spec"`
}

// policy is one NetworkPolicy reduced to the question this guard asks of it.
type policy struct {
	file, name, namespace string
	sel                   *selector
	policesEgress         bool
	grantsEgress          bool
}

// workload is one pod template reduced likewise.
type workload struct {
	file, kind, name, namespace string
	labels                      map[string]string
}

// Finding is one pod policed into silence.
type Finding struct {
	Workload workload
	// Policing names the policies that turned egress enforcement on for this pod,
	// so the reader is not left hunting for which document did it — it is usually
	// in another tree.
	Policing []string
}

// allowed records a pod that is deliberately given zero egress. Keyed
// "<namespace>/<kind>/<name>". Empty today, and kept for the same reason
// meshEgressAllowed is: the answer "yes, on purpose" has to be written down
// somewhere reviewable rather than argued again each time.
//
// An entry that matches nothing is a FAILURE, not a tidy-up: a registry that
// outlives its rules stops being reviewable.
var allowed = map[string]string{}

// egressPoliced reports whether a NetworkPolicy polices egress. Kubernetes
// defaults policyTypes when it is absent: Ingress always, and Egress only if the
// policy carries egress rules. Getting that default wrong in either direction is
// the whole guard — read as "polices" it flags correct policies, read as "does
// not" it misses the case it exists for.
func egressPoliced(d ddDoc) bool {
	if len(d.Spec.PolicyTypes) == 0 {
		return len(d.Spec.Egress) > 0
	}
	for _, t := range d.Spec.PolicyTypes {
		if t == "Egress" {
			return true
		}
	}
	return false
}

// matches reports whether a policy's podSelector selects a pod's labels. An
// absent or empty selector selects EVERY pod in the namespace, which is exactly
// how a default-deny is written.
//
// A matchExpressions selector is not decided here and must not be guessed at:
// answering "matches" hides a real finding and answering "does not" invents one.
// Callers surface it as an error instead — see Scan.
func matches(sel *selector, labels map[string]string) bool {
	if sel == nil || (len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0) {
		return true
	}
	for k, v := range sel.MatchLabels {
		// TWO-VALUE LOOKUP. `labels[k] != v` reads a MISSING key as the empty
		// string, so a selector entry with an empty value — `{foo: ""}`, which is
		// legal and which people write — would match every pod LACKING that key
		// entirely, and mark an unselected pod as granted egress it does not have.
		got, ok := labels[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

func hasExpressions(sel *selector) bool { return sel != nil && len(sel.MatchExpressions) > 0 }

// podLabelsOf pulls the pod-template labels a NetworkPolicy would select on.
// NOT the workload's own metadata.labels: a NetworkPolicy selects PODS, and the
// two label sets are allowed to differ. Reading the wrong one is a silent
// mismatch that would make this guard answer confidently about the wrong object.
func podLabelsOf(d ddDoc) (map[string]string, bool) {
	switch {
	case d.Kind == "Pod":
		return d.Metadata.Labels, true
	case d.Spec.JobTemplate != nil:
		return d.Spec.JobTemplate.Spec.Template.Metadata.Labels, true
	case d.Spec.Template != nil:
		return d.Spec.Template.Metadata.Labels, true
	}
	return nil, false
}

// Scan is the whole decision over an already-collected corpus. Pure, so every arm
// is testable without a tree.
func Scan(policies []policy, workloads []workload) ([]Finding, error) {
	byNS := map[string][]policy{}
	for _, p := range policies {
		byNS[p.namespace] = append(byNS[p.namespace], p)
	}
	var findings []Finding
	for _, w := range workloads {
		var policing []string
		granted := false
		for _, p := range byNS[w.namespace] {
			if !p.policesEgress {
				continue
			}
			if hasExpressions(p.sel) {
				return nil, fmt.Errorf("default-deny-egress: NetworkPolicy %q (%s) uses a matchExpressions "+
					"podSelector, which this guard does not evaluate. It has never met one, so rather than "+
					"guess — answering \"matches\" hides a stranded pod and answering \"does not\" invents a "+
					"finding — it stops here. Teach matches() the operator semantics, with a test",
					p.name, p.file)
			}
			if !matches(p.sel, w.labels) {
				continue
			}
			policing = append(policing, p.name+" ("+p.file+")")
			if p.grantsEgress {
				granted = true
			}
		}
		if len(policing) == 0 || granted {
			continue
		}
		sort.Strings(policing)
		findings = append(findings, Finding{Workload: w, Policing: policing})
	}
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i].Workload, findings[j].Workload
		if a.namespace != b.namespace {
			return a.namespace < b.namespace
		}
		return a.name < b.name
	})
	return findings, nil
}

// key is the allowlist key for a workload.
func key(w workload) string { return w.namespace + "/" + w.kind + "/" + w.name }

// Collect walks the corpus once and reduces it to the two lists Scan needs.
func Collect(repo capability.Repo, dirs []string) (policies []policy, workloads []workload, examined int, err error) {
	examined, err = guardwalk.Walk(repo, dirs, func(path string, raw []byte) error {
		for _, d := range guardwalk.DecodeDocs(string(raw), func(d ddDoc) bool {
			return d.Kind == "NetworkPolicy" || workloadKinds[d.Kind]
		}) {
			// A NAMESPACE IS THE JOIN KEY, so an object without one cannot be judged
			// and must not be bucketed into pseudo-namespace "". That is the one
			// vacuity arm in this guard that would fail OPEN: an unnamespaced
			// default-deny would police nothing this guard can see, and an
			// unnamespaced pod template would read as unpoliced. Namespace-agnostic
			// manifests are normal in a kustomize component, so this is a matter of
			// time rather than a hypothetical.
			if d.Metadata.Namespace == "" {
				return fmt.Errorf("%s: %s/%s declares no metadata.namespace, and this guard joins policies "+
					"to pods BY namespace — it cannot place either side. Add the namespace, or exclude the "+
					"file from the scan roots; passing over it would read as \"nothing polices this pod\"",
					path, d.Kind, d.Metadata.Name)
			}
			if d.Kind == "NetworkPolicy" {
				policies = append(policies, policy{
					file: path, name: d.Metadata.Name, namespace: d.Metadata.Namespace,
					sel: d.Spec.PodSelector, policesEgress: egressPoliced(d),
					grantsEgress: len(d.Spec.Egress) > 0,
				})
				continue
			}
			labels, ok := podLabelsOf(d)
			if !ok {
				continue // a workload kind with no pod template is not one of ours
			}
			workloads = append(workloads, workload{
				file: path, kind: d.Kind, name: d.Metadata.Name,
				namespace: d.Metadata.Namespace, labels: labels,
			})
		}
		return nil
	})
	return policies, workloads, examined, err
}

// ScanDirs resolves the scan roots: the platform tree plus the RENDERED charts.
// Both halves are load-bearing and for opposite reasons — the default-deny that
// strands a pod is usually in a chart, and the pod it strands is usually in
// platform-apl/.
func ScanDirs(repo capability.Repo) []string {
	return append(guardwalk.PlatformTreeDirs(repo), renderedChartsDir)
}

// Run reads the corpus and reports.
func Run(root string) error {
	repo := capability.RepoForGate(Extension(), root)
	if _, err := repo.Stat(renderedChartsDir); err != nil {
		return fmt.Errorf("default-deny-egress: no rendered charts at %s — the namespace-wide "+
			"default-deny policies live in the first-party charts, whose templates/ dirs are skipped "+
			"and whose namespaces are Helm values. Without them every pod looks unpoliced and this "+
			"guard passes over exactly the case it exists for. Run `make render-charts` first, or "+
			"`make default-deny-egress`, which does it for you", renderedChartsDir)
	}
	dirs := ScanDirs(repo)
	policies, workloads, examined, err := Collect(repo, dirs)
	if err != nil {
		return err
	}
	if err := guardkit.RequireCorpus("default-deny-egress", examined, dirs); err != nil {
		return err
	}
	// A corpus with manifests but no POLICIES is its own vacuity: every pod would
	// read as unpoliced and the guard would pass having judged nothing. The
	// platform ships a default-deny in several namespaces, so zero means the
	// policies moved or stopped parsing.
	if len(policies) == 0 {
		return fmt.Errorf("default-deny-egress: %d manifest file(s) examined and not one NetworkPolicy "+
			"among them — every pod then reads as unpoliced and this passes vacuously. The policies "+
			"moved, or the rendered tree is stale", examined)
	}
	if len(workloads) == 0 {
		return fmt.Errorf("default-deny-egress: %d manifest file(s) examined and not one pod template "+
			"among them — there is nothing left to judge and this would pass vacuously", examined)
	}

	// THE RENDERED TREE MUST CONTRIBUTE, NOT MERELY EXIST. Checking only that the
	// directory is there let an EMPTY one pass: platform-apl/ alone supplies 23
	// policies, so neither the corpus check nor the len(policies)==0 backstop
	// fires, and the guard printed OK over the very pod it exists to find. That is
	// reachable two ways — `llz ci gates --only default-deny-egress`, which the
	// Makefile advertises and which carries no render-charts prerequisite, and a
	// render-charts.sh run that dies after its own `rm -rf; mkdir -p`.
	//
	// The namespace-wide default-denies live in the CHARTS. If none came from the
	// rendered tree, every pod reads as unpoliced and this guard has nothing to say.
	fromRendered := 0
	for _, p := range policies {
		if strings.HasPrefix(p.file, renderedChartsDir+"/") {
			fromRendered++
		}
	}
	if fromRendered == 0 {
		return fmt.Errorf("default-deny-egress: %s contributed no NetworkPolicy to the scan (%d found, all "+
			"from the platform tree). The namespace-wide default-denies live in the CHARTS, so without them "+
			"every pod reads as unpoliced and this would pass over exactly the class it exists for. Run "+
			"`make render-charts`, or `make default-deny-egress`, which does it for you",
			renderedChartsDir, len(policies))
	}

	findings, err := Scan(policies, workloads)
	if err != nil {
		return err
	}

	kept := findings[:0]
	seen := map[string]bool{}
	for _, f := range findings {
		if _, ok := allowed[key(f.Workload)]; ok {
			seen[key(f.Workload)] = true
			continue
		}
		kept = append(kept, f)
	}
	findings = kept
	for k, why := range allowed {
		if !seen[k] {
			return fmt.Errorf("default-deny-egress: allowlist entry %q matches nothing. The pod was "+
				"removed, renamed, or has been granted egress — delete the entry, because a registry "+
				"that outlives its rules stops being reviewable (reason on file: %s)", k, why)
		}
	}

	if len(findings) == 0 {
		fmt.Printf("default-deny-egress: OK — %d pod template(s) against %d NetworkPolicy(s); every pod whose egress is policed is granted some (%d acknowledged).\n",
			len(workloads), len(policies), len(allowed))
		return nil
	}
	for _, f := range findings {
		fmt.Printf("::error file=%s::%s %q in namespace %q has ZERO egress: %s polices its egress and no policy grants it any. It will reach nothing — not DNS, not the apiserver — while staying 1/1 Running.\n",
			f.Workload.file, f.Workload.kind, f.Workload.name, f.Workload.namespace, strings.Join(f.Policing, ", "))
	}
	fmt.Printf("\n%s %d pod(s) are policed into total silence:\n", color.Red("✗"), len(findings))
	for _, f := range findings {
		fmt.Printf("    %s  %s/%s in %s\n        policed by: %s\n        pod labels: %s\n",
			f.Workload.file, f.Workload.kind, f.Workload.name, f.Workload.namespace,
			strings.Join(f.Policing, ", "), labelString(f.Workload.labels))
	}
	fmt.Print("\nNetworkPolicies are additive and there is no deny rule: once ANY policy polices a\n" +
		"pod's egress, everything not explicitly allowed is dropped. A pod no allow selects\n" +
		"reaches nothing, and nothing reports it — the pod stays Running with healthy\n" +
		"endpoints and simply never does its job.\n\n" +
		"Fix: give the pod a NetworkPolicy with `policyTypes: [Egress]` and the egress it\n" +
		"needs. On LKE-E that is DNS matched on the kube-system NAMESPACE (managed CoreDNS\n" +
		"is not labelled k8s-app: kube-dns) and, for the apiserver, a BARE ports rule on 443\n" +
		"AND 6443 — there is no in-cluster kube-apiserver pod to select, and Cilium evaluates\n" +
		"egress post-DNAT. Not `to: ipBlock: 0.0.0.0/0`, which misses cluster identities.\n" +
		"platform-apl/components/llzReconciler/llz-reconciler/network-policy.yaml is the model.\n\n" +
		"If the pod is meant to have no egress, say so in this guard's `allowed` map.\n")
	return fmt.Errorf("default-deny-egress: %d pod(s) whose egress is policed are granted none", len(findings))
}

func labelString(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ",")
}
