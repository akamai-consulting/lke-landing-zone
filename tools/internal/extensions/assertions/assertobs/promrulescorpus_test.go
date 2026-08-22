package assertobs

// promrulescorpus_test.go — every PrometheusRule must be inside the corpus the
// gate is pointed at.
//
// check-prom-rules walks a root and filters by `kind: PrometheusRule`, so what it
// validates is decided entirely by the --rules-dir the Makefile passes. That
// pointed at platform-apl/components/observability/prometheus-rules — the folder
// named after the concern — and llz-reconciler's PrometheusRule has never lived
// there. Seventeen rules, including every alert on the reconciler's own gauges,
// sat outside the corpus of the gate that validates PromQL, and the gate reported
// success over the files it had been handed.
//
// That is the vacuous-pass shape one level up. The gate is not wrong about what
// it checked; it is wrong about what "all of them" means, and nothing could see
// the difference because the count it prints is a count of what it was pointed
// at.
//
// So this reads the ROOT out of the Makefile and the FILES out of the tree, and
// fails when one is not covered by the other. Neither side is restated here:
// adding a rule file anywhere, or narrowing the flag, breaks it.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// repoRoot locates the repository from this test's own source path, so it does
// not depend on where `go test` was invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot locate this test's source path")
	}
	// tools/internal/extensions/assertions/assertobs/<file> → six levels up.
	root, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Makefile")); err != nil {
		t.Skipf("repository root not found at %s: %v", root, err)
	}
	return root
}

// rulesDirsFromMakefile reads the roots the gate is actually pointed at.
// Stops at a comma or a closing paren: the flag is inside a $(call …) so the
// value runs up against the macro's own punctuation, and \S+ swallowed it.
var rulesDirRe = regexp.MustCompile(`--rules-dir\s+([^\s,)]+)`)

func rulesDirsFromMakefile(t *testing.T, root string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	// Only the prom-rules-check recipe, so an unrelated --rules-dir elsewhere
	// cannot widen what this test believes is covered.
	body := string(b)
	i := strings.Index(body, "\nprom-rules-check:")
	if i < 0 {
		t.Fatal("no prom-rules-check target in the Makefile — the gate this pins is gone")
	}
	end := strings.Index(body[i+1:], "\n\n")
	if end < 0 {
		end = len(body) - i - 1
	}
	var dirs []string
	for _, m := range rulesDirRe.FindAllStringSubmatch(body[i:i+1+end], -1) {
		// The recipe runs from tools/, so `../platform-apl` is repo-relative.
		dirs = append(dirs, filepath.Clean(strings.TrimPrefix(m[1], "../")))
	}
	if len(dirs) == 0 {
		t.Fatal("prom-rules-check passes no --rules-dir — it would check nothing")
	}
	return dirs
}

func TestEveryPrometheusRuleIsInTheCheckedCorpus(t *testing.T) {
	root := repoRoot(t)
	dirs := rulesDirsFromMakefile(t, root)

	var found []string
	for _, tree := range []string{"platform-apl", "kubernetes-charts", "instance-template"} {
		base := filepath.Join(root, tree)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || (!strings.HasSuffix(p, ".yaml") && !strings.HasSuffix(p, ".yml")) {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			// The same signal check-prom-rules itself filters on.
			if regexp.MustCompile(`(?m)^kind:\s*PrometheusRule\s*$`).Match(b) {
				rel, _ := filepath.Rel(root, p)
				found = append(found, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}

	// FAIL CLOSED ON AN EMPTY SWEEP. Finding no PrometheusRule at all means the
	// walk broke or the signal changed, not that the repo has no alerts — and
	// "covered everything" over an empty set is the failure this test is about.
	if len(found) == 0 {
		t.Fatal("found no PrometheusRule files at all — the walk or the kind match is broken, " +
			"and an empty sweep reports full coverage")
	}

	for _, f := range found {
		covered := false
		for _, d := range dirs {
			if f == d || strings.HasPrefix(f, d+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("%s is a PrometheusRule outside every --rules-dir the Makefile passes (%v) — "+
				"check-prom-rules never validates its PromQL, and reports success without it", f, dirs)
		}
	}
}
