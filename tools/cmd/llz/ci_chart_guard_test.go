package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestChangedChartDirs(t *testing.T) {
	in := []string{
		"kubernetes-charts/foo/templates/rbac.yaml",
		"kubernetes-charts/foo/Chart.yaml",
		"kubernetes-charts/bar/values.yaml",
		"kubernetes-charts/README.md", // directly under root — no chart
		"tools/cmd/llz/x.go",          // unrelated
		"kubernetes-charts/",          // trailing slash, no chart component
	}
	got := changedChartDirs(in)
	want := []string{"kubernetes-charts/bar", "kubernetes-charts/foo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changedChartDirs = %v, want %v", got, want)
	}
}

func TestClassifyChartBump(t *testing.T) {
	tests := []struct {
		name           string
		old, new       string
		wantOK         bool
		wantMsgPattern string
	}{
		{"unchanged version fails", "0.4.0", "0.4.0", false, "still 0.4.0"},
		{"bumped version passes", "0.4.0", "0.4.1", true, "0.4.0 → 0.4.1"},
		{"removed chart exempt", "0.4.0", "", true, "removed"},
		{"new chart exempt", "", "0.1.0", true, "new chart"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, msg := classifyChartBump("kubernetes-charts/foo", tt.old, tt.new)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v (msg: %q)", ok, tt.wantOK, msg)
			}
			if !strings.Contains(msg, tt.wantMsgPattern) {
				t.Errorf("msg = %q, want it to contain %q", msg, tt.wantMsgPattern)
			}
		})
	}
}

func TestChartVersion(t *testing.T) {
	yaml := "apiVersion: v2\nname: foo\nversion: 0.4.1\nappVersion: \"latest\"\n"
	if got := chartVersion(yaml); got != "0.4.1" {
		t.Errorf("chartVersion = %q, want 0.4.1", got)
	}
	// appVersion must not be mistaken for version.
	if got := chartVersion("appVersion: 9.9.9\n"); got != "" {
		t.Errorf("chartVersion(appVersion only) = %q, want empty", got)
	}
	if got := chartVersion("name: x\n"); got != "" {
		t.Errorf("chartVersion(no version) = %q, want empty", got)
	}
}

// runChartVersionGuard reads the new version from the working tree and the old
// version from `git show base:...`. Drive it with a temp root for the new files
// and a stubbed execOutput for git, exercising both the pass and fail exits.
func TestRunChartVersionGuard(t *testing.T) {
	root := t.TempDir()
	writeChart := func(name, version string) {
		dir := filepath.Join(root, "kubernetes-charts", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"),
			[]byte("name: "+name+"\nversion: "+version+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeChart("bumped", "0.5.0")   // base 0.4.0 → bumped: ok
	writeChart("unbumped", "0.4.0") // base 0.4.0 → unchanged: fail

	stubGit := func(_ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "diff"):
			return []byte("kubernetes-charts/bumped/templates/x.yaml\n" +
				"kubernetes-charts/unbumped/values.yaml\n"), nil
		case strings.Contains(joined, "show") && strings.Contains(joined, "bumped/Chart.yaml"):
			return []byte("name: bumped\nversion: 0.4.0\n"), nil
		case strings.Contains(joined, "show") && strings.Contains(joined, "unbumped/Chart.yaml"):
			return []byte("name: unbumped\nversion: 0.4.0\n"), nil
		}
		return nil, nil
	}

	withExecOutput(t, stubGit)
	// "unbumped" changed without a version bump → the guard must fail.
	err := runChartVersionGuard("BASE", root)
	if err == nil || !strings.Contains(err.Error(), "unbumped") {
		t.Fatalf("runChartVersionGuard = %v, want failure naming the unbumped chart", err)
	}

	// Bump it; now both charts are clean → guard passes.
	writeChart("unbumped", "0.4.1")
	if err := runChartVersionGuard("BASE", root); err != nil {
		t.Fatalf("runChartVersionGuard after bump = %v, want nil", err)
	}

	// Missing --base is an explicit error.
	if err := runChartVersionGuard("", root); err == nil {
		t.Error("runChartVersionGuard(no base) = nil, want error")
	}
}

// TestRunChartVersionGuardFailsOnUnresolvableBase pins the fail-closed contract.
// Per-chart `git show` errors are (correctly) discarded, because "path absent at
// base" is exactly how a genuinely new chart looks. But an unresolvable base ref
// — a bad --base, or the far more likely shallow clone without the base commit —
// produces the same empty result for EVERY chart, and classifyChartBump exempts
// new charts from the bump requirement. The guard would then pass a changeset
// having compared nothing.
func TestRunChartVersionGuardFailsOnUnresolvableBase(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "kubernetes-charts", "unbumped")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Same version as the base would have: without the base check this chart is
	// silently reclassified as "new" and waved through.
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"),
		[]byte("name: unbumped\nversion: 0.4.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "rev-parse"):
			return nil, errors.New("fatal: Needed a single revision")
		case strings.Contains(joined, "diff"):
			return []byte("kubernetes-charts/unbumped/values.yaml\n"), nil
		}
		// Every `git show` fails, as it would against a commit we don't have.
		return nil, errors.New("fatal: invalid object name")
	})

	err := runChartVersionGuard("BASE", root)
	if err == nil {
		t.Fatal("an unresolvable base must FAIL — every chart looks new, so the bump check silently applies to nothing")
	}
	if !strings.Contains(err.Error(), "does not resolve") {
		t.Errorf("error should name the unresolvable base: %v", err)
	}
}

// TestChartScalarStripsQuotes pins the symmetry fix. The PIN side of every
// comparison strips quotes (extractChartPins, siblingValue); Chart.yaml's reader
// did not. A legal `version: "0.1.11"` therefore made chart-pin-guard compare
// `"0.1.11"` against `0.1.11` and report drift that does not exist.
func TestChartScalarStripsQuotes(t *testing.T) {
	for _, tt := range []struct {
		name, yaml, want string
	}{
		{"unquoted", "name: llz-foo\nversion: 0.1.11\n", "0.1.11"},
		{"double-quoted", "name: llz-foo\nversion: \"0.1.11\"\n", "0.1.11"},
		{"single-quoted", "name: llz-foo\nversion: '0.1.11'\n", "0.1.11"},
		{"absent", "name: llz-foo\n", ""},
		// appVersion and nested keys must never be mistaken for the chart version.
		{"appVersion not matched", "appVersion: 9.9.9\nversion: 0.1.11\n", "0.1.11"},
		{"nested not matched", "dependencies:\n  version: 3.3.3\n", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := chartVersion(tt.yaml); got != tt.want {
				t.Errorf("chartVersion = %q, want %q", got, tt.want)
			}
		})
	}
	if got := chartName("name: \"llz-foo\"\nversion: 0.1.0\n"); got != "llz-foo" {
		t.Errorf("chartName = %q, want llz-foo (quotes stripped, as the pin side does)", got)
	}
}

// TestRunChartVersionGuardSeesWorkingTree locks the false-green this guard used
// to give before a commit. Detection used to be `git diff base...HEAD` alone —
// committed changes only — while the NEW version is read from the working tree.
// So an edited-but-uncommitted chart produced an empty changed set and the guard
// printed "No chart directories changed", passing a chart that the very next
// commit fails on in CI. Here the committed diff is deliberately EMPTY and the
// change exists only in the working tree; the guard must still fail.
func TestRunChartVersionGuardSeesWorkingTree(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "kubernetes-charts", "wt-only")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"),
		[]byte("name: wt-only\nversion: 0.4.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stubGit := func(_ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		// Committed diff vs the base: nothing committed yet.
		case strings.Contains(joined, "BASE...HEAD"):
			return nil, nil
		// Staged + unstaged edits against HEAD: this is where the change lives.
		case strings.Contains(joined, "diff --name-only HEAD"):
			return []byte("kubernetes-charts/wt-only/values.yaml\n"), nil
		case strings.Contains(joined, "ls-files"):
			return nil, nil
		case strings.Contains(joined, "show") && strings.Contains(joined, "wt-only/Chart.yaml"):
			return []byte("name: wt-only\nversion: 0.4.0\n"), nil
		}
		return nil, nil
	}
	withExecOutput(t, stubGit)

	err := runChartVersionGuard("BASE", root)
	if err == nil || !strings.Contains(err.Error(), "wt-only") {
		t.Fatalf("runChartVersionGuard = %v, want failure naming the uncommitted chart", err)
	}
}

// TestRunChartVersionGuardUntrackedChart covers the other working-tree source: a
// brand-new chart directory that git does not track yet still counts as changed.
// It is exempt from the bump rule (no prior published artifact), so the guard
// passes — but it must be SEEN, not silently absent from the report.
func TestRunChartVersionGuardUntrackedChart(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "kubernetes-charts", "brand-new")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"),
		[]byte("name: brand-new\nversion: 0.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stubGit := func(_ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "ls-files") {
			return []byte("kubernetes-charts/brand-new/Chart.yaml\n"), nil
		}
		return nil, nil // no committed diff, no tracked-file diff, absent at base
	}
	withExecOutput(t, stubGit)

	if err := runChartVersionGuard("BASE", root); err != nil {
		t.Fatalf("runChartVersionGuard = %v, want nil (new chart is exempt from the bump rule)", err)
	}
}
