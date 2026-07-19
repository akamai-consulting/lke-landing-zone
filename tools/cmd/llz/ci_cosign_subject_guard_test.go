package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractCosignSubjects(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantWfs  []string
		wantSubj string
	}{
		{
			name: "the live policy form — org glob, quoted, trailing @* ref",
			body: `                - keyless:
                    subject: "https://github.com/akamai-consulting/*/.github/workflows/build-images.yml@*"
                    issuer: "https://token.actions.githubusercontent.com"`,
			wantWfs:  []string{"build-images.yml"},
			wantSubj: "https://github.com/akamai-consulting/*/.github/workflows/build-images.yml",
		},
		{
			name:    "fully-narrowed form — exact repo and branch ref, unquoted",
			body:    `    subject: https://github.com/akamai-consulting/lke-landing-zone/.github/workflows/publish-charts.yml@refs/heads/main`,
			wantWfs: []string{"publish-charts.yml"},
		},
		{
			name:    ".yaml extension is matched too, so a rename across extensions is caught",
			body:    `    subject: "https://github.com/o/r/.github/workflows/build-images.yaml@*"`,
			wantWfs: []string{"build-images.yaml"},
		},
		{
			name: "multiple attestors in one file each yield a ref",
			body: `    subject: "https://github.com/o/r/.github/workflows/a.yml@*"
    subject: "https://github.com/o/r/.github/workflows/b.yml@*"`,
			wantWfs: []string{"a.yml", "b.yml"},
		},
		{
			name: "a non-workflow keyless subject (email identity) is not our business",
			body: `    subject: "https://github.com/akamai-consulting"
    subject: "release@example.com"`,
			wantWfs: nil,
		},
		{
			// The live policy's comments show an alternative, fully-narrowed subject
			// as guidance. If commented-out examples counted, the guard would demand
			// a workflow for a pin nothing enforces.
			name:    "a subject-shaped string in a COMMENT is ignored",
			body:    `    # subject: "https://github.com/o/r/.github/workflows/gone.yml@*"`,
			wantWfs: nil, // leading '#' means the line does not match ^\s*subject:
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractCosignSubjects(tc.body)
			if len(got) != len(tc.wantWfs) {
				t.Fatalf("got %d subject(s), want %d: %+v", len(got), len(tc.wantWfs), got)
			}
			for i, w := range tc.wantWfs {
				if got[i].Workflow != w {
					t.Errorf("subject %d: workflow = %q, want %q", i, got[i].Workflow, w)
				}
			}
			if tc.wantSubj != "" && got[0].Subject != tc.wantSubj {
				t.Errorf("subject text = %q, want %q (the @ref must be stripped so the\n"+
					"error names the identity, not the ref glob)", got[0].Subject, tc.wantSubj)
			}
		})
	}
}

// TestCosignSubjectGuardRefusesEmpty pins the half of this guard that is easy to
// lose: a guard that finds no pins must not report the same green as one that
// found them all and checked them. If the policy is renamed or moved out of the
// scanned tree, "0 pins, 0 missing" is exactly the silence this guard exists to
// prevent one level down.
func TestCosignSubjectGuardRefusesEmpty(t *testing.T) {
	root := t.TempDir()
	// A populated manifest tree with no cosign subject in it, so the walk has a
	// real corpus (requireCorpus passes) and only the no-pins check can fire.
	dir := filepath.Join(root, "platform-apl", "manifest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cm.yaml"), []byte("kind: ConfigMap\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runCosignSubjectGuard(root)
	if err == nil {
		t.Fatal("guard passed with zero subject pins found — an empty result must not read as 'all valid'")
	}
	if !strings.Contains(err.Error(), "no keyless workflow") {
		t.Fatalf("failed for the wrong reason: %v", err)
	}
}

// TestCosignSubjectGuardCatchesRename is the guard's whole purpose, at the unit
// level: the pin is present and well-formed, but the workflow it names is gone.
func TestCosignSubjectGuardCatchesRename(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "platform-apl", "manifest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := `verifyImages:
  - attestors:
      - entries:
          - keyless:
              subject: "https://github.com/o/r/.github/workflows/build-images.yml@*"
`
	if err := os.WriteFile(filepath.Join(dir, "policy.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}

	// No .github/workflows/build-images.yml — the rename case.
	err := runCosignSubjectGuard(root)
	if err == nil {
		t.Fatal("guard passed while its pinned workflow was absent")
	}
	if !strings.Contains(err.Error(), "build-images.yml") {
		t.Fatalf("error must name the missing workflow so the fix is obvious: %v", err)
	}

	// Now create it: the same tree must pass, or the guard is just noise.
	wf := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wf, "build-images.yml"), []byte("name: build\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runCosignSubjectGuard(root); err != nil {
		t.Fatalf("guard failed with the pinned workflow present: %v", err)
	}
}

// TestCosignSubjectGuardRefusesWildcardRepo pins the owner's trust-anchor
// decision: the repo position must name one repo. A glob there accepts a
// signature from any repo matching it, which is a weaker anchor than it looks —
// the identity check appears present while barely constraining who signed.
//
// The @ref glob must stay allowed in the same breath, or the guard would reject
// the live policy: build-images.yml is dispatched on feature branches by
// release-e2e, so real signatures carry refs/heads/<branch>.
func TestCosignSubjectGuardRefusesWildcardRepo(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		wantErr bool
	}{
		{"org wildcard in the repo position", "https://github.com/akamai-consulting/*/.github/workflows/build-images.yml@*", true},
		{"glob spanning owner and repo", "https://github.com/*/.github/workflows/build-images.yml@*", true},
		{"single-char glob still widens", "https://github.com/akamai-consulting/lke-landing-zon?/.github/workflows/build-images.yml@*", true},
		{"pinned repo, glob ref — the live policy", "https://github.com/akamai-consulting/lke-landing-zone/.github/workflows/build-images.yml@*", false},
		{"pinned repo and pinned ref", "https://github.com/akamai-consulting/lke-landing-zone/.github/workflows/build-images.yml@refs/heads/main", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wildcardRepoSubjects([]cosignSubjectRef{{Subject: tc.subject}})
			if (len(got) > 0) != tc.wantErr {
				t.Fatalf("wildcardRepoSubjects(%q) flagged=%v, want flagged=%v", tc.subject, len(got) > 0, tc.wantErr)
			}
		})
	}
}
