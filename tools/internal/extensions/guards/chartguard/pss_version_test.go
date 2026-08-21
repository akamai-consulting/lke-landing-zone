package chartguard

// pss_version_test.go — THE GATE FOR ISSUE #447.
//
// `llz-cluster-foundation` labelled its `restricted` namespaces with
// `pod-security.kubernetes.io/{enforce,warn,audit}-version: v1.33`, under a comment
// saying the value "must match the cluster's Kubernetes minor" — a promise nothing
// kept and nothing checked. It sat there while the scaffold default moved to a
// v1.34 build, and would have kept sitting there.
//
// THE STALENESS WAS THE SYMPTOM, NOT THE DEFECT. A chart is published once and
// installed on every instance's cluster; LKE-Enterprise version availability is
// per-ACCOUNT and rotates within hours, so those clusters run different minors.
// "Matches the cluster's minor" is not a property one baked literal can have —
// whatever it names is wrong for somebody, and wrong SILENTLY: Pod Security
// Standards revisions are cumulative, so a lagging pin enforces a weaker ruleset
// than the profile name advertises while looking identical in the labels.
//
// SO THE GATE IS A CORPUS SCAN, NOT A FILE CHECK. The first cut asserted one key in
// one values.yaml, which is a gate that can only ever catch the instance already
// fixed. It missed `llz-cert-automation` and its platform-apl manifest, both
// pinned to v1.31, both live — and it would have missed a per-component
// `valuesObject:` override reinstating a minor on every cluster with the test still
// green. It now walks every chart and every delivered manifest, follows each PSS
// label back to the value that fills it, and judges what it finds.
//
// WHERE IT RUNS: the go-tests CI job (`make coverage` / `make test-race`), NOT
// `make lint` — neither lint path invokes `go test`, so a local pre-push `make
// lint` on a chart-only change will not run this. That is a real gap and it is
// bounded: the job is a required check, so the pin cannot MERGE. Promoting this to
// a registered `llz ci` verb would close the local half at the cost of a registry
// entry, a lint-group membership and a census line.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// pssLabel matches an emitted PSS version label and captures its value, which may
// be a literal or a Helm expression.
var pssLabel = regexp.MustCompile(`pod-security\.kubernetes\.io/(?:enforce|warn|audit)-version:\s*(\S.*?)\s*$`)

// helmValuesRef pulls `.Values.a.b.c` out of a Helm expression.
var helmValuesRef = regexp.MustCompile(`\.Values\.([\w.]+)`)

// helmVarAssign matches `{{- $pssVersion := .Values.podSecurityStandardsVersion }}`,
// the indirection llz-cluster-foundation uses so one value fills six labels.
var helmVarAssign = regexp.MustCompile(`\$(\w+)\s*:=\s*\.Values\.([\w.]+)`)

// helmVarRef matches a bare `{{ $pssVersion }}`.
var helmVarRef = regexp.MustCompile(`^\{\{[-\s]*\$(\w+)[-\s]*\}\}$`)

// aKubernetesMinor is what must never fill one of these labels: v1.33, 1.33,
// v1.33.6+lke7 — any literal naming a specific Kubernetes release.
var aKubernetesMinor = regexp.MustCompile(`^"?v?\d+\.\d+`)

// scannedTrees are the two places a PSS label can reach a cluster from: a
// first-party chart, and a manifest delivered straight into the GitOps tree.
var scannedTrees = []string{"kubernetes-charts", "platform-apl"}

func TestNoPodSecurityLabelPinsAKubernetesMinor(t *testing.T) {
	root := repoRootForChartTest(t)

	checked := 0
	for _, tree := range scannedTrees {
		for _, path := range yamlFilesUnder(t, filepath.Join(root, tree)) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			text := string(raw)
			vars := map[string]string{}
			for _, m := range helmVarAssign.FindAllStringSubmatch(text, -1) {
				vars[m[1]] = m[2]
			}
			for _, line := range strings.Split(text, "\n") {
				m := pssLabel.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				checked++
				value, origin := resolvePSSValue(t, root, path, strings.TrimSpace(m[1]), vars)
				if value == "" {
					// COULD NOT TELL IS NOT NOTHING THERE. An expression this cannot follow
					// back to a default is a label whose content is unknown, which is exactly
					// the state the pin was in. Fail and make someone look.
					t.Errorf("%s: could not resolve %q back to the value that fills it — a PSS version "+
						"this gate cannot read is one nobody is checking. Teach it the new shape, or "+
						"inline a literal `latest`.", rel(root, path), strings.TrimSpace(m[1]))
					continue
				}
				if aKubernetesMinor.MatchString(value) {
					t.Errorf("%s: PSS version is %q (from %s) — a published chart or a delivered manifest "+
						"cannot name the Kubernetes minor of every cluster it lands on, and a pin that lags "+
						"enforces a WEAKER cumulative ruleset than the profile name advertises, silently. "+
						"Use `latest`: it is the upstream default and is resolved by the apiserver that "+
						"does the enforcing. A pin that is genuinely needed has to come from the instance's "+
						"own spec, not from a file shipped to everyone.", rel(root, path), value, origin)
				}
			}
		}
	}

	// FAIL CLOSED ON VACUITY. Zero labels found means the walk broke, the trees
	// moved, or the regex stopped matching — none of which is "no minor is pinned",
	// and all of which look identical to a clean run from the exit status.
	if checked == 0 {
		t.Fatalf("found no pod-security.kubernetes.io/*-version labels under %v — this gate examined "+
			"NOTHING, which is not the same as finding nothing wrong", scannedTrees)
	}
}

// resolvePSSValue turns whatever fills a PSS label into the concrete default, and
// says where that default came from. Three shapes, all of them live in this repo:
// a literal, `{{ .Values.a.b }}`, and `{{ $var }}` bound to a .Values path earlier
// in the same template.
func resolvePSSValue(t *testing.T, root, path, raw string, vars map[string]string) (value, origin string) {
	t.Helper()
	if !strings.Contains(raw, "{{") {
		return raw, "the manifest itself"
	}
	key := ""
	if m := helmVarRef.FindStringSubmatch(raw); m != nil {
		key = vars[m[1]]
	} else if m := helmValuesRef.FindStringSubmatch(raw); m != nil {
		key = m[1]
	}
	if key == "" {
		return "", ""
	}
	// templates/foo.yaml -> the chart's values.yaml, two levels up.
	valuesPath := filepath.Join(filepath.Dir(filepath.Dir(path)), "values.yaml")
	v, ok := valueAt(t, valuesPath, key)
	if !ok {
		return "", ""
	}
	return v, rel(root, valuesPath) + " (" + key + ")"
}

// valueAt reads a dotted path out of a values.yaml.
func valueAt(t *testing.T, path, dotted string) (string, bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var cur any = doc
	for _, seg := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[seg]
		if !ok {
			return "", false
		}
	}
	s, ok := cur.(string)
	return s, ok
}

func yamlFilesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")) {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}

// TestTheRestrictedNamespacesStillCarryAVersionLabel — the other direction. The fix
// for a stale pin must not become "drop the labels": an absent -version label also
// resolves to latest, but by ACCIDENT, and it takes the enforced ruleset out of the
// one place a reader looks for it.
func TestTheRestrictedNamespacesStillCarryAVersionLabel(t *testing.T) {
	root := repoRootForChartTest(t)
	path := filepath.Join(root, "kubernetes-charts", "llz-cluster-foundation", "templates", "namespaces.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	tpl := string(raw)
	for _, mode := range []string{"enforce", "warn", "audit"} {
		label := "pod-security.kubernetes.io/" + mode + "-version"
		if !strings.Contains(tpl, label) {
			t.Errorf("the template no longer emits %s — the enforced ruleset must stay visible in "+
				"the namespace's own labels, not be inherited by omission", label)
		}
	}
}

// repoRootForChartTest walks up to the directory holding go.mod's parent, so the
// gate survives the tools module moving.
//
// IT FAILS RATHER THAN SKIPS. A t.Skip here would turn every assertion above into
// a silent pass the day a path changes — a gate reporting success having examined
// nothing, which is the failure this whole file is written around.
func repoRootForChartTest(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "kubernetes-charts")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no repo root (a directory containing kubernetes-charts/) above %q — this gate "+
				"cannot run, and must not report a pass for it", dir)
		}
		dir = parent
	}
}
