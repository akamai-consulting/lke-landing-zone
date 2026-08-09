package workflowshells

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanWorkflowShells(t *testing.T) {
	cases := []struct {
		name        string
		yaml        string
		wantViolate bool
	}{
		{
			name: "container job, no bash default -> violation (the discover bug)",
			yaml: `
jobs:
  discover:
    container:
      image: ghcr.io/x/ci
    steps:
      - run: |
          set -euo pipefail
          echo hi
`,
			wantViolate: true,
		},
		{
			name: "container job, workflow-level bash default -> ok",
			yaml: `
defaults:
  run:
    shell: bash
jobs:
  discover:
    container:
      image: ghcr.io/x/ci
    steps:
      - run: set -o pipefail
`,
		},
		{
			name: "container job, job-level bash default -> ok",
			yaml: `
jobs:
  j:
    container: { image: x }
    defaults:
      run:
        shell: bash
    steps:
      - run: echo hi
`,
		},
		{
			name: "container job, per-step shell -> ok",
			yaml: `
jobs:
  j:
    container: { image: x }
    steps:
      - run: echo hi
        shell: bash
`,
		},
		{
			name: "no container -> ok (host default is bash)",
			yaml: `
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: set -o pipefail
`,
		},
		{
			name: "container job, only uses-steps -> ok",
			yaml: `
jobs:
  j:
    container: { image: x }
    steps:
      - uses: actions/checkout@v4
`,
		},
		{
			name:        "unparseable -> no findings",
			yaml:        "this: : : not yaml",
			wantViolate: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanWorkflowShells("wf.yml", []byte(tc.yaml))
			if tc.wantViolate && len(got) == 0 {
				t.Fatalf("expected a violation, got none")
			}
			if !tc.wantViolate && len(got) > 0 {
				t.Fatalf("expected no violation, got %v", got)
			}
			if tc.wantViolate && !strings.Contains(got[0], "/bin/sh") {
				t.Errorf("violation message should explain the sh fallback: %q", got[0])
			}
		})
	}
}

// AN EMPTY DIRECTORY IS NOT A PASS. This guard's subject is a path handed in, so
// a moved .github/, a wrong --dir, or a driver arg carried over from another tree
// all present as "nothing to check" — and it used to print "every container job
// declares a bash shell default" over zero jobs.
func TestRunRefusesADirectoryWithNoWorkflows(t *testing.T) {
	err := Run(t.TempDir())
	if err == nil {
		t.Fatal("an empty directory reported clean — a guard that examined nothing cannot " +
			"say everything is fine")
	}
	if !strings.Contains(err.Error(), "no workflow YAML") {
		t.Errorf("error %q does not say the corpus was empty, so a reader would hunt for a "+
			"violation that does not exist", err)
	}
}

// And a directory holding only non-workflow files is the same case: entries exist,
// none is YAML, so `examined` is still zero.
func TestRunRefusesADirectoryWithNoYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run(dir); err == nil {
		t.Error("a directory with no YAML reported clean")
	}
}
