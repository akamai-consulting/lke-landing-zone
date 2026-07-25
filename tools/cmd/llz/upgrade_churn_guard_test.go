package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// churnFixture lays out a minimal template-repo checkout: an instance-template/
// tree (which is what makes the guard run at all) plus a delivered runbook.
func churnFixture(t *testing.T, scaffold, runbook string) {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{
		filepath.Join(dir, "instance-template", ".github", "workflows"),
		filepath.Join(dir, "docs", "runbooks"),
	} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(dir, "instance-template", ".github", "workflows", "terraform.yml"), scaffold)
	mustWrite(t, filepath.Join(dir, "docs", "runbooks", "recover.md"), runbook)
	t.Chdir(dir)
}

func TestUpgradeChurnGuardPasses(t *testing.T) {
	churnFixture(t,
		"jobs:\n  call:\n    uses: ./.github/workflows/llz-terraform.yml\n    with:\n      instance_repo: <@ instance_repo @>\n",
		"See [the guide](../adopter-guide.md) and [secrets](https://github.com/akamai-consulting/lke-landing-zone/blob/main/docs/secrets.md).\n")
	if err := stepUpgradeChurnGuard(globalOpts{}); err != nil {
		t.Fatalf("clean delivered surface must pass: %v", err)
	}
}

// Each banned shape is a real regression that already cost a live instance diff,
// so each gets its own case rather than one combined assertion.
func TestUpgradeChurnGuardCatchesEachShape(t *testing.T) {
	cases := []struct {
		name, scaffold, runbook, want string
	}{
		{
			name:     "copier version token in the scaffold",
			scaffold: "jobs:\n  call:\n    with:\n      some_pin: <@ llz_version @>\n",
			runbook:  "clean\n",
			want:     "copier `llz_version` token",
		},
		{
			name:     "retired template-ref input",
			scaffold: "on:\n  workflow_call:\n    inputs:\n      template-ref:\n        type: string\n",
			runbook:  "clean\n",
			want:     "`template-ref:` workflow input",
		},
		{
			name:     "version-pinned permalink in a delivered doc",
			scaffold: "jobs: {}\n",
			runbook:  "[secrets](https://github.com/akamai-consulting/lke-landing-zone/blob/v0.0.32/docs/secrets.md)\n",
			want:     "version-pinned template permalink",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			churnFixture(t, tc.scaffold, tc.runbook)
			err := stepUpgradeChurnGuard(globalOpts{})
			if err == nil {
				t.Fatal("expected the guard to fail")
			}
			if !strings.Contains(err.Error(), "must carry no version") {
				t.Errorf("error should name the invariant, got: %v", err)
			}
		})
	}
}

// A prose mention of a release ("fixed in v0.0.29") is not a permalink and must not
// trip the guard — otherwise runbooks can't reference history.
func TestUpgradeChurnGuardAllowsProseVersions(t *testing.T) {
	churnFixture(t,
		"jobs: {}\n",
		"Fixed in v0.0.29; see the v0.0.31 release notes. Upgrade with `llz upgrade --ref v0.0.32`.\n")
	if err := stepUpgradeChurnGuard(globalOpts{}); err != nil {
		t.Fatalf("prose version references must be allowed: %v", err)
	}
}

// Neither a template repo (no instance-template/) nor an instance (no copier
// answers) — the guard has no delivered surface to look at and must not fire.
func TestUpgradeChurnGuardSkipsOutsideBothCheckouts(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "README.md"), "an instance pinned at v0.0.32\n")
	t.Chdir(dir)
	if err := stepUpgradeChurnGuard(globalOpts{}); err != nil {
		t.Fatalf("must skip outside a template-repo or instance checkout: %v", err)
	}
}

// instanceChurnFixture lays out a minimal INSTANCE checkout: copier's answers file
// (what identifies it), the pinned docs pointer, and a delivered doc.
func instanceChurnFixture(t *testing.T, quickstart string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, ".copier-answers.yml"), "_commit: v0.0.33\nllz_version: v0.0.33\n")
	mustWrite(t, filepath.Join(dir, "docs", "README.md"),
		"> https://github.com/akamai-consulting/lke-landing-zone/tree/v0.0.33/docs\n")
	mustWrite(t, filepath.Join(dir, "docs", "quickstart.md"), quickstart)
	t.Chdir(dir)
}

// The failure this actually shipped: docs delivered by an older llz keep permalinks
// pinned to the PREVIOUS release, so they drift further behind the pointer on every
// upgrade. The instance's own `llz lint` is the only thing standing over that file.
func TestUpgradeChurnGuardCatchesStaleDeliveryInInstance(t *testing.T) {
	instanceChurnFixture(t,
		"[secrets](https://github.com/akamai-consulting/lke-landing-zone/blob/v0.0.32/docs/secrets.md)\n")
	err := stepUpgradeChurnGuard(globalOpts{})
	if err == nil {
		t.Fatal("expected the guard to fail on a stale delivered permalink")
	}
	if !strings.Contains(err.Error(), "older llz") {
		t.Errorf("the instance-side error should point at re-delivery, got: %v", err)
	}
}

// docs/README.md is the ONE file allowed to pin — the guard must not turn the
// deliberate exception into a lint failure.
func TestUpgradeChurnGuardAllowsThePinnedPointerInInstance(t *testing.T) {
	instanceChurnFixture(t,
		"[secrets](https://github.com/akamai-consulting/lke-landing-zone/blob/main/docs/secrets.md)\n")
	if err := stepUpgradeChurnGuard(globalOpts{}); err != nil {
		t.Fatalf("a correctly delivered instance must pass: %v", err)
	}
}
