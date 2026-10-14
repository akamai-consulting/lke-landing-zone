package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real kustomization line, so the pattern is exercised against the shape the
// render actually emits rather than a simplified one. The `&timeout=` suffix is
// the part a naive `?ref=(.*)` swallows.
const kustomizationYAML = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - github.com/akamai-consulting/lke-landing-zone//platform-apl/manifest?ref=%s&timeout=80
  - github.com/akamai-consulting/lke-landing-zone//platform-apl/components/openbao?ref=%s&timeout=80
`

func writeOverlay(t *testing.T, dir, rel, ref string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(kustomizationYAML, "%s", ref)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The ref must stop at `&`. Reading `v0.0.44&timeout=80` as the ref would make
// every comparison fail, and the gate would be permanently red for a reason that
// has nothing to do with the upgrade.
func TestScanKustomizeRefsStopsAtTheQueryDelimiter(t *testing.T) {
	dir := t.TempDir()
	writeOverlay(t, dir, "probe/manifest/kustomization.yaml", "v0.0.45")

	u, err := ScanKustomizeRefs(dir, "v0.0.45")
	if err != nil {
		t.Fatalf("ScanKustomizeRefs: %v", err)
	}
	if u.Wanted != 2 {
		t.Errorf("Wanted = %d, want 2 — the `&timeout=` suffix was read as part of the ref", u.Wanted)
	}
	if len(u.Stale) != 0 {
		t.Errorf("Stale = %v, want none", u.Stale)
	}
}

// THE REGRESSION THIS GATE EXISTS FOR: the pin moved and the overlay did not.
func TestScanKustomizeRefsFindsAnOverlayLeftAtTheOldRelease(t *testing.T) {
	dir := t.TempDir()
	writeOverlay(t, dir, "probe/manifest/kustomization.yaml", "v0.0.44")
	writeOverlay(t, dir, "probe/apps/harbor/kustomization.yaml", "v0.0.45")

	u, err := ScanKustomizeRefs(dir, "v0.0.45")
	if err != nil {
		t.Fatalf("ScanKustomizeRefs: %v", err)
	}
	if u.Wanted != 2 {
		t.Errorf("Wanted = %d, want 2 (the file that WAS re-rendered)", u.Wanted)
	}
	stale := u.StaleFiles()
	if len(stale) != 1 || stale[0] != "probe/manifest/kustomization.yaml" {
		t.Fatalf("StaleFiles = %v, want the one file left behind", stale)
	}
	if got := u.Stale[stale[0]]; len(got) != 1 || got[0] != "v0.0.44" {
		t.Errorf("stale refs = %v, want [v0.0.44] — the message must name what it found, "+
			"not only what it wanted", got)
	}
}

// THE ARM THAT MATTERS MOST. An overlay with no `?ref=` at all makes "no stale
// refs" trivially true. Reporting that as a pass is how the check goes quiet on
// the render never having run — which is exactly the state it exists to detect.
func TestCheckRepinnedFailsClosedOnAnEmptyScan(t *testing.T) {
	msg := CheckRepinned("v0.0.44", RefUsage{Stale: map[string][]string{}}, "v0.0.45")
	if msg == "" {
		t.Fatal("an overlay carrying no ref at all must FAIL — nothing was compared")
	}
	for _, want := range []string{"NO `?ref=`", "Nothing was compared"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure does not explain the vacuity (%q):\n%s", want, msg)
		}
	}
}

func TestCheckRepinnedPassesWhenEverythingMoved(t *testing.T) {
	u := RefUsage{Wanted: 8, Stale: map[string][]string{}}
	if msg := CheckRepinned("v0.0.44", u, "v0.0.45"); msg != "" {
		t.Errorf("a fully re-rendered overlay must pass, got:\n%s", msg)
	}
}

// The failure has to name the files AND say what a stale ref costs, because the
// cost is the non-obvious part: the repo looks entirely correct and the CLUSTER
// is running the previous release.
func TestCheckRepinnedNamesTheFilesAndTheConsequence(t *testing.T) {
	u := RefUsage{Wanted: 1, Stale: map[string][]string{
		"probe/manifest/kustomization.yaml": {"v0.0.44"},
	}}
	msg := CheckRepinned("v0.0.44", u, "v0.0.45")
	for _, want := range []string{"probe/manifest/kustomization.yaml", "v0.0.44", "ArgoCD", "three releases"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure never mentions %q:\n%s", want, msg)
		}
	}
}

// A missing apl-values directory is not this function's verdict to give: "no
// overlay" and "an overlay full of stale refs" have different remedies, so the
// scan reports an empty result and CheckRepinned turns it into the vacuity
// failure above.
func TestScanKustomizeRefsToleratesAMissingOverlay(t *testing.T) {
	u, err := ScanKustomizeRefs(filepath.Join(t.TempDir(), "does-not-exist"), "v0.0.45")
	if err != nil {
		t.Fatalf("a missing overlay must not be a scan error: %v", err)
	}
	if u.Wanted != 0 || len(u.Stale) != 0 {
		t.Errorf("scan of a missing tree returned %+v", u)
	}
}

// ── The argv the gate drives ─────────────────────────────────────────────────

// --no-render MUST NOT COME BACK. It was there to keep the gate offline, but
// render reads a spec and writes files — it was never online — and its absence
// meant `llz upgrade`'s Lever 2 (re-render what the new pin invalidates) was
// exercised by nothing. Verified by mutation: restoring the flag reds
// render-fresh, pin-repointed AND converges-with-fresh on all three hops.
func TestUpgradeUnderTestArgvRenders(t *testing.T) {
	argv := UpgradeUnderTestArgv("/bin/llz", "v0.0.45")
	for _, banned := range []string{"--no-render"} {
		for _, got := range argv {
			if got == banned {
				t.Errorf("argv carries %s, so the gate measures an upgrade that renders nothing: %v",
					banned, argv)
			}
		}
	}
	// --no-doctor stays: the readiness check wants gh and the Linode API, and it is
	// advisory rather than part of what an upgrade delivers.
	if !contains(argv, "--no-doctor") {
		t.Errorf("argv lost --no-doctor, which is what keeps the gate offline: %v", argv)
	}
}

// The seeded deployment is what makes the render reachable at all: with no spec,
// clusterspec.InstancePresent is false and renderAfter returns immediately — so
// dropping --no-render would change nothing and the gate would still measure an
// upgrade that renders nothing.
func TestSeedSpecArgvIsNonInteractive(t *testing.T) {
	argv := SeedSpecArgv("/bin/llz")
	if !contains(argv, "--yes") {
		t.Errorf("the gate closes stdin, so seeding must be non-interactive: %v", argv)
	}
	if !contains(argv, probeEnv) {
		t.Errorf("argv does not name the probe deployment: %v", argv)
	}
}
