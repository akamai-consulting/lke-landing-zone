package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lockChartYAML = `apiVersion: v2
name: demo
version: 0.1.0
dependencies:
  - name: foo
    version: 1.2.3
    repository: https://example.com/charts
  - name: bar
    version: 4.5.6
    repository: https://example.com/charts
`

func ptr(s string) *string { return &s }

func TestCheckChartLock_InSync(t *testing.T) {
	lock := `dependencies:
  - name: foo
    version: 1.2.3
    repository: https://example.com/charts
  - name: bar
    version: 4.5.6
    repository: https://example.com/charts
`
	res := checkChartLock("charts/demo", ptr(lockChartYAML), ptr(lock))
	if len(res.Errors) != 0 || res.Skipped {
		t.Errorf("expected clean result, got %+v", res)
	}
}

func TestCheckChartLock_VersionAndRepoDrift(t *testing.T) {
	lock := `dependencies:
  - name: foo
    version: 9.9.9
    repository: https://example.com/charts
  - name: bar
    version: 4.5.6
    repository: https://OLD.example.com/charts
`
	res := checkChartLock("charts/demo", ptr(lockChartYAML), ptr(lock))
	joined := strings.Join(res.Errors, "\n")
	if !strings.Contains(joined, "'foo' version mismatch") {
		t.Errorf("missing foo version drift: %v", res.Errors)
	}
	if !strings.Contains(joined, "'bar' repository mismatch") {
		t.Errorf("missing bar repo drift: %v", res.Errors)
	}
}

func TestCheckChartLock_MissingFromLock(t *testing.T) {
	lock := `dependencies:
  - name: foo
    version: 1.2.3
    repository: https://example.com/charts
`
	res := checkChartLock("charts/demo", ptr(lockChartYAML), ptr(lock))
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "'bar' is declared in Chart.yaml but missing") {
		t.Errorf("expected missing-bar error, got %v", res.Errors)
	}
}

func TestCheckChartLock_StaleLockWarning(t *testing.T) {
	chart := `name: demo
dependencies:
  - name: foo
    version: 1.2.3
    repository: r
`
	lock := `dependencies:
  - name: foo
    version: 1.2.3
    repository: r
  - name: ghost
    version: 0.0.1
    repository: r
`
	res := checkChartLock("charts/demo", ptr(chart), ptr(lock))
	if len(res.Errors) != 0 {
		t.Errorf("stale entry is a warning, not an error: %v", res.Errors)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "'ghost'") {
		t.Errorf("expected ghost stale warning, got %v", res.Warnings)
	}
}

func TestCheckChartLock_NoChartYAML(t *testing.T) {
	res := checkChartLock("charts/demo", nil, nil)
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "No Chart.yaml") {
		t.Errorf("expected missing-Chart.yaml error, got %v", res.Errors)
	}
}

func TestCheckChartLock_NoDependenciesSkips(t *testing.T) {
	res := checkChartLock("charts/demo", ptr("name: demo\nversion: 1.0.0\n"), nil)
	if !res.Skipped || len(res.Errors) != 0 {
		t.Errorf("expected skip, got %+v", res)
	}
}

func TestCheckChartLock_MissingLock(t *testing.T) {
	res := checkChartLock("charts/demo", ptr(lockChartYAML), nil)
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "Chart.lock is missing") {
		t.Errorf("expected missing-lock error, got %v", res.Errors)
	}
}

// TestRunChartLockDrift exercises the file-reading wrapper end to end.
func TestRunChartLockDrift(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "charts", "good")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(good, "Chart.yaml"), []byte(lockChartYAML), 0o644)
	os.WriteFile(filepath.Join(good, "Chart.lock"), []byte(`dependencies:
  - name: foo
    version: 1.2.3
    repository: https://example.com/charts
  - name: bar
    version: 4.5.6
    repository: https://example.com/charts
`), 0o644)

	var out strings.Builder
	if err := runChartLockDrift(root, []string{"charts/good"}, &out); err != nil {
		t.Errorf("expected pass, got %v", err)
	}

	// Now break it.
	os.WriteFile(filepath.Join(good, "Chart.lock"), []byte(`dependencies:
  - name: foo
    version: 0.0.0
    repository: https://example.com/charts
`), 0o644)
	out.Reset()
	if err := runChartLockDrift(root, []string{"charts/good"}, &out); err == nil {
		t.Error("expected drift failure")
	}
}
