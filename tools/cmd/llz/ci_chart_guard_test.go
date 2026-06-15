package main

import (
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
