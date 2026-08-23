package defaultdeny

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func decode(t *testing.T, body string) ddDoc {
	t.Helper()
	var d ddDoc
	if err := yaml.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// ── policyTypes defaulting ───────────────────────────────────────────────────

// TestEgressPolicedFollowsKubernetesDefaulting. Kubernetes defaults an absent
// policyTypes to Ingress always, and Egress only when the policy carries egress
// rules. Both directions of getting that wrong are fatal to this guard: read as
// "polices" it flags every correct ingress-only policy, read as "does not" it
// misses the namespace-wide deny it exists for.
func TestEgressPolicedFollowsKubernetesDefaulting(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml string
		want bool
	}{
		"explicit Egress":           {"spec:\n  policyTypes: [Ingress, Egress]\n", true},
		"explicit Ingress only":     {"spec:\n  policyTypes: [Ingress]\n", false},
		"explicit Egress only":      {"spec:\n  policyTypes: [Egress]\n", true},
		"absent, with egress rules": {"spec:\n  egress:\n    - ports: [{port: 53}]\n", true},
		"absent, no egress rules":   {"spec:\n  ingress: []\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := egressPoliced(decode(t, tc.yaml)); got != tc.want {
				t.Errorf("egressPoliced = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── selector semantics ───────────────────────────────────────────────────────

// TestEmptySelectorMatchesEveryPod. This is how a default-deny is written, so a
// guard that treated `podSelector: {}` as matching nothing would find no
// default-deny anywhere and pass over the whole class.
func TestEmptySelectorMatchesEveryPod(t *testing.T) {
	for name, sel := range map[string]*selector{
		"absent": nil,
		"empty":  {},
	} {
		t.Run(name, func(t *testing.T) {
			if !matches(sel, map[string]string{"anything": "at-all"}) {
				t.Error("an empty podSelector selects every pod in the namespace")
			}
			if !matches(sel, nil) {
				t.Error("including a pod with no labels")
			}
		})
	}
}

func TestMatchLabelsIsASubsetMatch(t *testing.T) {
	sel := &selector{MatchLabels: map[string]string{"app.kubernetes.io/name": "openbao"}}
	if !matches(sel, map[string]string{"app.kubernetes.io/name": "openbao", "extra": "ok"}) {
		t.Error("extra pod labels do not prevent a match")
	}
	// The real bug, in one line: same key, different value.
	if matches(sel, map[string]string{"app.kubernetes.io/name": "openbao-cert-watcher"}) {
		t.Error("`openbao` must not select `openbao-cert-watcher` — that near-miss is the whole finding")
	}
	if matches(sel, nil) {
		t.Error("a pod with no labels is not selected by a matchLabels policy")
	}
}

// ── pod labels come from the POD TEMPLATE ────────────────────────────────────

// TestPodLabelsComeFromTheTemplateNotTheWorkload. A NetworkPolicy selects PODS.
// A workload's own metadata.labels and its pod template's labels are allowed to
// differ, and reading the wrong one makes this guard answer confidently about an
// object no policy ever sees.
func TestPodLabelsComeFromTheTemplateNotTheWorkload(t *testing.T) {
	d := decode(t, `
kind: Deployment
metadata:
  labels: {app.kubernetes.io/name: the-workload-label}
spec:
  template:
    metadata:
      labels: {app.kubernetes.io/name: the-pod-label}
`)
	got, ok := podLabelsOf(d)
	if !ok || got["app.kubernetes.io/name"] != "the-pod-label" {
		t.Errorf("podLabelsOf = %v (ok=%v), want the POD template's labels", got, ok)
	}
}

func TestPodLabelsForCronJobAndPod(t *testing.T) {
	cj := decode(t, `
kind: CronJob
spec:
  jobTemplate:
    spec:
      template:
        metadata:
          labels: {app.kubernetes.io/name: rotator}
`)
	if got, ok := podLabelsOf(cj); !ok || got["app.kubernetes.io/name"] != "rotator" {
		t.Errorf("CronJob pod labels = %v (ok=%v)", got, ok)
	}
	pod := decode(t, "kind: Pod\nmetadata:\n  labels: {app.kubernetes.io/name: oneoff}\n")
	if got, ok := podLabelsOf(pod); !ok || got["app.kubernetes.io/name"] != "oneoff" {
		t.Errorf("Pod labels = %v (ok=%v)", got, ok)
	}
	if _, ok := podLabelsOf(decode(t, "kind: Deployment\nspec: {}\n")); ok {
		t.Error("a workload with no pod template is not one this guard judges")
	}
}

// ── Scan ─────────────────────────────────────────────────────────────────────

func pol(ns, name string, sel map[string]string, polices, grants bool) policy {
	var s *selector
	if sel != nil {
		s = &selector{MatchLabels: sel}
	}
	return policy{file: "f/" + name + ".yaml", name: name, namespace: ns, sel: s, policesEgress: polices, grantsEgress: grants}
}

func wl(ns, name string, labels map[string]string) workload {
	return workload{file: "w/" + name + ".yaml", kind: "Deployment", name: name, namespace: ns, labels: labels}
}

// TestScanFindsThePodTheAllowDoesNotSelect is the bug, reconstructed: a
// namespace-wide deny plus one allow keyed on a DIFFERENT value of the same
// label.
func TestScanFindsThePodTheAllowDoesNotSelect(t *testing.T) {
	f, err := Scan(
		[]policy{
			pol("llz-openbao", "openbao-default-deny", nil, true, false),
			pol("llz-openbao", "openbao-allow", map[string]string{"app.kubernetes.io/name": "openbao"}, true, true),
		},
		[]workload{
			wl("llz-openbao", "openbao-cert-watcher", map[string]string{"app.kubernetes.io/name": "openbao-cert-watcher"}),
			wl("llz-openbao", "platform-openbao", map[string]string{"app.kubernetes.io/name": "openbao"}),
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 1 || f[0].Workload.name != "openbao-cert-watcher" {
		t.Fatalf("expected only the watcher, got %+v", f)
	}
	// Name what IS there. The policy that turned enforcement on is usually in
	// another tree, and "no egress" without it sends the reader to the wrong file.
	if len(f[0].Policing) == 0 || !strings.Contains(f[0].Policing[0], "openbao-default-deny") {
		t.Errorf("the finding must name the policy that polices it, got %v", f[0].Policing)
	}
}

// TestScanIgnoresAnUnpolicedPod pins the exclusion. Most namespaces carry no
// default-deny at all, and a guard that flagged their pods would be deleted.
func TestScanIgnoresAnUnpolicedPod(t *testing.T) {
	f, err := Scan(
		[]policy{pol("other", "ingress-only", nil, false, false)},
		[]workload{wl("other", "app", map[string]string{"a": "b"})})
	if err != nil || len(f) != 0 {
		t.Errorf("a pod nothing polices is not a finding: %+v (err=%v)", f, err)
	}
}

// TestScanDoesNotCrossNamespaces. A deny in one namespace says nothing about a
// pod in another, and joining them would flag most of the tree.
func TestScanDoesNotCrossNamespaces(t *testing.T) {
	f, err := Scan(
		[]policy{pol("llz-openbao", "openbao-default-deny", nil, true, false)},
		[]workload{wl("harbor", "some-pod", map[string]string{"a": "b"})})
	if err != nil || len(f) != 0 {
		t.Errorf("a policy in another namespace does not police this pod: %+v (err=%v)", f, err)
	}
}

// TestANamespaceWideAllowCoversEveryPod — several namespaces grant DNS with a
// `podSelector: {}` policy that carries egress rules. Reading only matchLabels
// policies as grants would flag every pod in all of them.
func TestANamespaceWideAllowCoversEveryPod(t *testing.T) {
	f, err := Scan(
		[]policy{
			pol("cert-manager", "cert-manager-default-deny", nil, true, false),
			pol("cert-manager", "cert-manager-allow-dns", nil, true, true),
		},
		[]workload{wl("cert-manager", "webhook", map[string]string{"app": "webhook"})})
	if err != nil || len(f) != 0 {
		t.Errorf("a namespace-wide allow covers every pod in it: %+v (err=%v)", f, err)
	}
}

// TestScanRefusesAMatchExpressionsSelector. Guessing is worse than stopping:
// answering "matches" hides a stranded pod, answering "does not" invents a
// finding. Neither is a verdict this guard is entitled to.
func TestScanRefusesAMatchExpressionsSelector(t *testing.T) {
	p := pol("ns", "expr-policy", nil, true, true)
	p.sel = &selector{MatchExpressions: []any{map[string]any{"key": "app", "operator": "Exists"}}}
	_, err := Scan([]policy{p}, []workload{wl("ns", "app", map[string]string{"app": "x"})})
	if err == nil || !strings.Contains(err.Error(), "matchExpressions") {
		t.Fatalf("expected a refusal naming matchExpressions, got %v", err)
	}
}

// ── Run: fail-closed arms ────────────────────────────────────────────────────

func runIn(t *testing.T, files map[string]string) (string, error) {
	t.Helper()
	root := t.TempDir()
	for p, body := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := captureStdout(t)
	err := Run(root)
	return out(), err
}

func captureStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	var done bool
	var buf strings.Builder
	return func() string {
		if done {
			return buf.String()
		}
		done = true
		_ = w.Close()
		os.Stdout = prev
		b := make([]byte, 1<<16)
		n, _ := r.Read(b)
		buf.Write(b[:n])
		_ = r.Close()
		return buf.String()
	}
}

// TestRunRefusesWithoutARenderedTree. The default-deny lives in a CHART, so
// without `make render-charts` every pod reads as unpoliced and this guard would
// pass over exactly the case it exists for — quietly, on a machine where someone
// forgot a make target.
func TestRunRefusesWithoutARenderedTree(t *testing.T) {
	_, err := runIn(t, map[string]string{"platform-apl/components/x/a.yaml": "kind: Deployment\n"})
	if err == nil || !strings.Contains(err.Error(), "no rendered charts") {
		t.Fatalf("expected a refusal naming the rendered tree, got %v", err)
	}
}

func TestRunRefusesAnEmptyCorpus(t *testing.T) {
	_, err := runIn(t, map[string]string{"rendered/.keep": ""})
	if err == nil || !strings.Contains(err.Error(), "examined 0 manifest files") {
		t.Fatalf("expected an empty-corpus refusal, got %v", err)
	}
}

// TestRunRefusesACorpusWithNoPolicies — manifests but no NetworkPolicy means
// every pod reads as unpoliced. That is what a corpus whose policies moved or
// stopped parsing looks like, and it is not a pass.
func TestRunRefusesACorpusWithNoPolicies(t *testing.T) {
	_, err := runIn(t, map[string]string{
		"rendered/c.yaml": "kind: Deployment\nmetadata: {name: a, namespace: n}\nspec:\n  template:\n    metadata:\n      labels: {app: a}\n",
	})
	if err == nil || !strings.Contains(err.Error(), "not one NetworkPolicy") {
		t.Fatalf("expected a no-policies refusal, got %v", err)
	}
}

func TestRunRefusesACorpusWithNoPods(t *testing.T) {
	_, err := runIn(t, map[string]string{
		"rendered/c.yaml": "kind: NetworkPolicy\nmetadata: {name: deny, namespace: n}\nspec:\n  podSelector: {}\n  policyTypes: [Egress]\n",
	})
	if err == nil || !strings.Contains(err.Error(), "not one pod template") {
		t.Fatalf("expected a no-pods refusal, got %v", err)
	}
}

// TestRunReportsTheStrandedPodAcrossTrees is the end-to-end shape of the real
// bug: the deny in the rendered chart tree, the pod it strands in platform-apl/.
func TestRunReportsTheStrandedPodAcrossTrees(t *testing.T) {
	out, err := runIn(t, map[string]string{
		"rendered/chart.yaml":                    "kind: NetworkPolicy\nmetadata: {name: ns-default-deny, namespace: llz-openbao}\nspec:\n  podSelector: {}\n  policyTypes: [Ingress, Egress]\n",
		"platform-apl/components/openbao/w.yaml": "kind: Deployment\nmetadata: {name: watcher, namespace: llz-openbao}\nspec:\n  template:\n    metadata:\n      labels: {app.kubernetes.io/name: watcher}\n",
	})
	if err == nil {
		t.Fatal("expected a failure")
	}
	for _, want := range []string{"::error file=platform-apl/components/openbao/w.yaml", "watcher", "ns-default-deny", "rendered/chart.yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("report should contain %q:\n%s", want, out)
		}
	}
}

func TestRunPassesWhenTheStrandedPodIsGrantedEgress(t *testing.T) {
	out, err := runIn(t, map[string]string{
		"rendered/chart.yaml": "kind: NetworkPolicy\nmetadata: {name: ns-default-deny, namespace: llz-openbao}\nspec:\n  podSelector: {}\n  policyTypes: [Ingress, Egress]\n",
		"platform-apl/components/openbao/w.yaml": "kind: Deployment\nmetadata: {name: watcher, namespace: llz-openbao}\nspec:\n  template:\n    metadata:\n      labels: {app.kubernetes.io/name: watcher}\n" +
			"---\nkind: NetworkPolicy\nmetadata: {name: watcher-np, namespace: llz-openbao}\nspec:\n  podSelector:\n    matchLabels: {app.kubernetes.io/name: watcher}\n  policyTypes: [Ingress, Egress]\n  egress:\n    - ports: [{port: 443}]\n",
	})
	if err != nil {
		t.Fatalf("a granted pod is not a finding: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("expected an OK line, got %q", out)
	}
}

// ── the RENDER_DIR the Makefile and this guard each name ─────────────────────

// TestRenderedDirMatchesMakefile. Two files hold the same directory name and the
// guard hard-fails when it is absent — so a Makefile rename would turn this gate
// into a permanent red with a confusing message, or (worse, if the check were
// softened) a permanent green over an unrendered tree.
func TestRenderedDirMatchesMakefile(t *testing.T) {
	b, err := os.ReadFile("../../../../../Makefile")
	if err != nil {
		t.Skipf("Makefile not readable from here: %v", err)
	}
	m := regexp.MustCompile(`(?m)^RENDER_DIR\s*\?=\s*(\S+)`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("no RENDER_DIR assignment in the Makefile — this guard's constant now agrees with nothing")
	}
	if m[1] != renderedChartsDir {
		t.Errorf("Makefile RENDER_DIR is %q, this guard's constant is %q", m[1], renderedChartsDir)
	}
}

// ── from the code review of the PR that introduced this guard ───────────────

// TestRunRefusesWhenTheRenderedTreeContributedNothing.
//
// Checking only that `rendered/` EXISTS let an empty one pass. platform-apl/
// alone supplies 23 policies, so neither RequireCorpus nor the len(policies)==0
// backstop fires — and the guard printed OK over the very pod it exists to find.
// Reachable two ways: `llz ci gates --only default-deny-egress`, which the
// Makefile advertises and which carries no render-charts prerequisite, and a
// render-charts.sh run that dies after its own `rm -rf; mkdir -p`.
//
// The namespace-wide default-denies live in the CHARTS. Without them every pod
// reads as unpoliced, which is a pass over the whole class.
func TestRunRefusesWhenTheRenderedTreeContributedNothing(t *testing.T) {
	_, err := runIn(t, map[string]string{
		// rendered/ exists and holds YAML, but no NetworkPolicy.
		"rendered/chart.yaml": "kind: ConfigMap\nmetadata: {name: c, namespace: llz-openbao}\n",
		// The platform tree alone supplies both a policy and a pod, so every other
		// vacuity arm is satisfied.
		"platform-apl/components/openbao/np.yaml": "kind: NetworkPolicy\nmetadata: {name: watcher-np, namespace: llz-openbao}\n" +
			"spec:\n  podSelector:\n    matchLabels: {app.kubernetes.io/name: watcher}\n  policyTypes: [Egress]\n  egress:\n    - ports: [{port: 443}]\n",
		"platform-apl/components/openbao/w.yaml": "kind: Deployment\nmetadata: {name: watcher, namespace: llz-openbao}\n" +
			"spec:\n  template:\n    metadata:\n      labels: {app.kubernetes.io/name: watcher}\n",
	})
	if err == nil {
		t.Fatal("an empty rendered tree must not pass — the chart-side default-denies are the whole " +
			"reason this guard reads two trees")
	}
	if !strings.Contains(err.Error(), "contributed no NetworkPolicy") {
		t.Errorf("the error must name the rendered tree as the thing that was missing, got: %v", err)
	}
}

// TestAnObjectWithNoNamespaceIsRefused. The namespace is the JOIN KEY, so an
// object without one cannot be placed on either side — and bucketing it into
// pseudo-namespace "" is the one arm here that would fail OPEN: an unnamespaced
// default-deny polices nothing this guard can see, and an unnamespaced pod
// template reads as unpoliced. Namespace-agnostic manifests are ordinary in a
// kustomize component, so this is a matter of time.
func TestAnObjectWithNoNamespaceIsRefused(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"policy": {
			"rendered/c.yaml":                  "kind: NetworkPolicy\nmetadata: {name: deny}\nspec:\n  podSelector: {}\n  policyTypes: [Egress]\n",
			"platform-apl/components/a/w.yaml": "kind: Deployment\nmetadata: {name: d, namespace: n}\nspec:\n  template:\n    metadata:\n      labels: {a: b}\n",
		},
		"workload": {
			"rendered/c.yaml":                  "kind: NetworkPolicy\nmetadata: {name: deny, namespace: n}\nspec:\n  podSelector: {}\n  policyTypes: [Egress]\n",
			"platform-apl/components/a/w.yaml": "kind: Deployment\nmetadata: {name: d}\nspec:\n  template:\n    metadata:\n      labels: {a: b}\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runIn(t, files)
			if err == nil || !strings.Contains(err.Error(), "declares no metadata.namespace") {
				t.Fatalf("an object with no namespace cannot be joined and must be refused, got %v", err)
			}
		})
	}
}

// TestAnEmptyMatchLabelValueDoesNotMatchAMissingKey. `labels[k] != v` reads a
// MISSING key as the empty string, so a selector entry with an empty value —
// `{foo: ""}`, which is legal and which people write — matched every pod LACKING
// that key entirely, and would mark an unselected pod as granted egress it does
// not have.
func TestAnEmptyMatchLabelValueDoesNotMatchAMissingKey(t *testing.T) {
	sel := &selector{MatchLabels: map[string]string{"sidecar.istio.io/inject": ""}}
	if matches(sel, map[string]string{"app.kubernetes.io/name": "watcher"}) {
		t.Error("a pod that does not carry the key at all is not selected by it")
	}
	if !matches(sel, map[string]string{"sidecar.istio.io/inject": ""}) {
		t.Error("a pod carrying the key with an empty value IS selected")
	}
}
