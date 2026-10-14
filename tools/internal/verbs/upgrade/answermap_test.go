package upgrade

// answermap_test.go — the three answer-map tests, moved with their subject.
//
// They lived in ci_upgrade_test_gate_test.go, a file about a CI gate, because the
// helpers did. Only ReadAnswerMap and AnswerRegressions are exported, and only
// because that gate still calls them; currentAnswerMap has no caller outside this
// package and stayed unexported, which is why its test had to move rather than
// being repointed.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentAnswerMap(t *testing.T) {
	// nil outside an instance — `llz upgrade` runs this before copier, and a hard
	// error there would break upgrading a tree that simply has no answers file yet.
	chdir(t, t.TempDir())
	if got := currentAnswerMap(); got != nil {
		t.Errorf("currentAnswerMap = %v outside an instance; want nil", got)
	}
	if err := os.WriteFile(".copier-answers.yml", []byte("instance_repo: o/r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := currentAnswerMap(); got["instance_repo"] != "o/r" {
		t.Errorf("currentAnswerMap = %v", got)
	}
}

func TestAnswerRegressions(t *testing.T) {
	base := map[string]string{
		"instance_repo": "probe-org/probe-instance",
		"openbao_team":  "probe-team",
		"upstream_org":  "akamai-consulting",
		"_commit":       "v0.0.39",
		"llz_version":   "v0.0.39",
		"_src_path":     "gh:acme/tmpl",
	}
	clone := func(over map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		return m
	}

	// The pin and copier's provenance are exactly what an upgrade is FOR.
	t.Run("moving the pin and provenance is not a regression", func(t *testing.T) {
		after := clone(map[string]string{"_commit": "v0.0.40", "llz_version": "v0.0.40", "_src_path": "/local"})
		if got := AnswerRegressions(base, after); len(got) != 0 {
			t.Errorf("answerRegressions = %v; want none", got)
		}
	})

	// The live bug: copier substitutes the template DEFAULT for an answer it
	// cannot keep — an answer the CURRENT template's validator rejects — and exits
	// 0. instance_repo is the ArgoCD repoURL and every `gh` target.
	t.Run("a silently reset answer is reported with both values", func(t *testing.T) {
		after := clone(map[string]string{"instance_repo": "your-org/your-instance-repo"})
		got := AnswerRegressions(base, after)
		if len(got) != 1 {
			t.Fatalf("answerRegressions = %v; want exactly the instance_repo reset", got)
		}
		for _, want := range []string{"instance_repo", "probe-org/probe-instance", "your-org/your-instance-repo"} {
			if !strings.Contains(got[0], want) {
				t.Errorf("regression %q is missing %q — the operator needs to see what it WAS to restore it", got[0], want)
			}
		}
	})

	// A dropped key renders the template default next, which is the same loss.
	t.Run("a dropped answer counts", func(t *testing.T) {
		after := clone(nil)
		delete(after, "openbao_team")
		got := AnswerRegressions(base, after)
		if len(got) != 1 || !strings.Contains(got[0], "dropped") {
			t.Errorf("answerRegressions = %v; want the dropped openbao_team reported", got)
		}
	})

	// nil `before` is the not-an-instance / pre-copier case. An upgrade cannot have
	// lost an answer that was never recorded, and reporting one would make
	// `llz upgrade` fail on a tree it should simply update.
	t.Run("no recorded answers means nothing to regress", func(t *testing.T) {
		if got := AnswerRegressions(nil, base); len(got) != 0 {
			t.Errorf("answerRegressions = %v; want none", got)
		}
	})
}

func TestReadAnswerMap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.yml")
	if err := os.WriteFile(p, []byte("_commit: v0.0.39\ninstance_repo: o/r\npromotion_rank: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := ReadAnswerMap(p)
	if err != nil {
		t.Fatal(err)
	}
	if m["instance_repo"] != "o/r" || m["_commit"] != "v0.0.39" {
		t.Errorf("readAnswerMap = %v", m)
	}
	// Non-string scalars must survive as comparable text, not blow up the compare.
	if m["promotion_rank"] != "3" {
		t.Errorf("numeric answer = %q; want \"3\"", m["promotion_rank"])
	}
	if _, err := ReadAnswerMap(filepath.Join(dir, "missing.yml")); err == nil {
		t.Error("readAnswerMap on a missing file returned no error")
	}
}
