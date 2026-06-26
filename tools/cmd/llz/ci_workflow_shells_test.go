package main

import (
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
