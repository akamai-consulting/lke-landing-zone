package releasepublish

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseHelmPushDigest(t *testing.T) {
	cases := map[string]string{
		"Pushed: ghcr.io/x/y:1\nDigest: sha256:abc123\n": "sha256:abc123",
		"digest: sha256:deadbeef":                        "sha256:deadbeef",
		"no digest here":                                 "",
	}
	for in, want := range cases {
		if got := parseHelmPushDigest(in); got != want {
			t.Errorf("parseHelmPushDigest(%q) = %q, want %q", in, got, want)
		}
	}
}

// stubPublishSeams swaps the helm/cosign seams for fakes and restores them.
type fakeRegistry struct {
	published map[string]bool // "name:version" already pushed
	signed    map[string]bool // "regRef:version" (or "@digest") signed
	pushes    []string
	signs     []string
}

func stubPublishSeams(t *testing.T, reg *fakeRegistry, inspect map[string][2]string) {
	t.Helper()
	oi, op, os_, opk, opu, osi, osl := pcInspect, pcPublished, pcSigned, pcPackage, pcPush, pcSign, pcSleep
	t.Cleanup(func() {
		pcInspect, pcPublished, pcSigned, pcPackage, pcPush, pcSign, pcSleep = oi, op, os_, opk, opu, osi, osl
	})

	pcInspect = func(dir string) (string, string, error) {
		nv := inspect[filepath.Base(dir)]
		return nv[0], nv[1], nil
	}
	pcPublished = func(ociRef, version string) bool { return reg.published[base(ociRef)+":"+version] }
	pcSigned = func(regRef string) bool { return reg.signed[regRef] }
	pcPackage = func(dir, dest string) error { return nil }
	pcPush = func(tgz, ociDest string) (string, error) {
		reg.pushes = append(reg.pushes, tgz)
		return "Digest: sha256:deadbeef", nil
	}
	pcSign = func(ref string) error { reg.signs = append(reg.signs, ref); reg.signed[ref] = true; return nil }
	pcSleep = func(time.Duration) {}
}

func base(ref string) string { // last path element (chart name)
	i := len(ref) - 1
	for i >= 0 && ref[i] != '/' {
		i--
	}
	return ref[i+1:]
}

func mkChartDirs(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		d := filepath.Join(root, n)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "Chart.yaml"), []byte("name: "+n+"\nversion: 1.0.0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestRunPublishCharts(t *testing.T) {
	root := mkChartDirs(t, "cf", "ob", "ca")
	inspect := map[string][2]string{
		"cf": {"llz-cluster-foundation", "0.1.6"},
		"ob": {"llz-openbao-platform", "0.1.13"},
		"ca": {"llz-cert-automation", "0.1.5"},
	}
	// cf: published + signed → skip. ob: published + UNSIGNED → re-sign. ca: new → push+sign.
	reg := &fakeRegistry{
		published: map[string]bool{
			"llz-cluster-foundation:0.1.6": true,
			"llz-openbao-platform:0.1.13":  true,
		},
		signed: map[string]bool{
			"ghcr.io/acme/charts/llz-cluster-foundation:0.1.6": true,
		},
	}
	stubPublishSeams(t, reg, inspect)

	o := PublishChartsOpts{ChartsDir: root, Selected: "all", Registry: "ghcr.io", Owner: "acme", RepoPath: "charts", Retries: 3}
	if err := RunPublishCharts(o); err != nil {
		t.Fatalf("RunPublishCharts: %v", err)
	}
	// cf skipped (no push, no new sign); ob re-signed by tag; ca pushed + signed by digest.
	if len(reg.pushes) != 1 {
		t.Errorf("expected exactly 1 push (ca), got %v", reg.pushes)
	}
	wantSigned := []string{
		"ghcr.io/acme/charts/llz-openbao-platform:0.1.13",         // ob re-sign in place (by tag)
		"ghcr.io/acme/charts/llz-cert-automation@sha256:deadbeef", // ca sign by digest
	}
	for _, w := range wantSigned {
		if !contains(reg.signs, w) {
			t.Errorf("missing sign %q; signs=%v", w, reg.signs)
		}
	}
	// cf must NOT be re-signed (already signed).
	if contains(reg.signs, "ghcr.io/acme/charts/llz-cluster-foundation:0.1.6") {
		t.Errorf("cf was re-signed but was already signed; signs=%v", reg.signs)
	}
}

func TestRunPublishCharts_Selected(t *testing.T) {
	root := mkChartDirs(t, "cf", "ob")
	inspect := map[string][2]string{"cf": {"llz-cluster-foundation", "0.1.6"}, "ob": {"llz-openbao-platform", "0.1.13"}}
	reg := &fakeRegistry{published: map[string]bool{}, signed: map[string]bool{}}
	stubPublishSeams(t, reg, inspect)

	o := PublishChartsOpts{ChartsDir: root, Selected: "llz-openbao-platform", Registry: "ghcr.io", Owner: "acme", RepoPath: "charts", Retries: 2}
	if err := RunPublishCharts(o); err != nil {
		t.Fatalf("RunPublishCharts: %v", err)
	}
	if len(reg.pushes) != 1 {
		t.Errorf("--selected should push only the one chart, got %v", reg.pushes)
	}
}

func TestRunPublishCharts_PushRetries(t *testing.T) {
	root := mkChartDirs(t, "ca")
	stubPublishSeams(t, &fakeRegistry{published: map[string]bool{}, signed: map[string]bool{}}, map[string][2]string{"ca": {"llz-cert-automation", "0.1.5"}})
	// Fail push twice, then succeed — retryPC must recover.
	n := 0
	pcPush = func(tgz, ociDest string) (string, error) {
		n++
		if n < 3 {
			return "", os.ErrDeadlineExceeded
		}
		return "Digest: sha256:deadbeef", nil
	}
	pcSign = func(string) error { return nil }
	if err := RunPublishCharts(PublishChartsOpts{ChartsDir: root, Selected: "all", Registry: "ghcr.io", Owner: "acme", RepoPath: "charts", Retries: 5}); err != nil {
		t.Fatalf("push should recover after Retries: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 push attempts, got %d", n)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// TestPublishChartsRefusesASelectionThatMatchesNothing.
//
// `--selected llz-cluster-foundaton` published nothing and exited 0, so the
// caller — a workflow_dispatch, or chart-publish-check's self-heal, which builds
// the name from a pin it just read — reported success and then waited for a chart
// that was never going to appear. The name comes from a human or from another
// command's output; either way, failing to match it is the one thing worth saying
// out loud.
func TestPublishChartsRefusesASelectionThatMatchesNothing(t *testing.T) {
	reg := &fakeRegistry{published: map[string]bool{}, signed: map[string]bool{}}
	stubPublishSeams(t, reg, map[string][2]string{
		"llz-cluster-foundation": {"llz-cluster-foundation", "0.1.14"},
		"llz-openbao-platform":   {"llz-openbao-platform", "0.1.22"},
	})
	root := mkChartDirs(t, "llz-cluster-foundation", "llz-openbao-platform")

	err := RunPublishCharts(PublishChartsOpts{
		ChartsDir: root, Selected: "llz-cluster-foundaton", // typo, one letter
		Registry: "ghcr.io", Owner: "acme", RepoPath: "acme/charts", DestDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("a --selected that matched no chart published nothing and reported success — the caller " +
			"then waits for a chart that will never appear")
	}
	if !strings.Contains(err.Error(), "llz-cluster-foundaton") {
		t.Errorf("the error must quote the selection that missed, got: %v", err)
	}
	// And it must name what WAS there, because the fix is almost always a typo and
	// the reader cannot spot it against a list they do not have.
	if !strings.Contains(err.Error(), "llz-cluster-foundation") {
		t.Errorf("the error must list the charts that exist, got: %v", err)
	}
	if len(reg.pushes) != 0 {
		t.Errorf("nothing may be published on a missed selection, got %v", reg.pushes)
	}
}

// TestPublishChartsStillHonoursARealSelection pins the exclusion.
func TestPublishChartsStillHonoursARealSelection(t *testing.T) {
	reg := &fakeRegistry{published: map[string]bool{}, signed: map[string]bool{}}
	stubPublishSeams(t, reg, map[string][2]string{
		"llz-cluster-foundation": {"llz-cluster-foundation", "0.1.14"},
		"llz-openbao-platform":   {"llz-openbao-platform", "0.1.22"},
	})
	root := mkChartDirs(t, "llz-cluster-foundation", "llz-openbao-platform")
	if err := RunPublishCharts(PublishChartsOpts{
		ChartsDir: root, Selected: "llz-cluster-foundation",
		Registry: "ghcr.io", Owner: "acme", RepoPath: "acme/charts", DestDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("a real selection must publish: %v", err)
	}
	if len(reg.pushes) != 1 {
		t.Errorf("expected exactly the selected chart to be pushed, got %v", reg.pushes)
	}
}
