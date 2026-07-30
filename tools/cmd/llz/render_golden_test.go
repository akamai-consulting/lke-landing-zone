package main

// Golden-file test for `llz render`.
//
// Why, given render_test.go already exists: those tests assert a handful of
// fields with strings.Contains, and everything they do not name can change
// silently. TestRenderEnvTfvars checks five substrings across two files; one
// render of one environment emits 27 files. Substring matching is also weaker
// than it reads — `strings.Contains(out, "node_count = 5")` is satisfied by
// "node_count = 50", so the existing assertion passes on a ten-times-too-large
// cluster.
//
// Rendering is the product here: the spec is the single source of truth and these
// artifacts are what terraform and Argo consume, so an unintended change to the
// mapping is a wrong-infrastructure bug. A golden file turns any such change into
// a reviewable diff instead of a silent one.
//
// WHAT IS AND IS NOT GOLDENED, which is the whole design decision:
//
//	spec-derived  (*.tfvars, apl-values/**) -> FULL CONTENT.
//	              These are what the spec→artifact mapping produces. Every byte is
//	              a claim about the mapping and is worth diffing.
//	static        (the generated TF roots copied out of the embedded tfroots
//	              package) -> PATH + SHA256 + SIZE ONLY.
//	              Their content is owned by tfroots, not by this mapping. Pinning
//	              it in full would mean every tfroots edit rewrites ~50KB of
//	              golden noise here, which is how a golden file gets regenerated
//	              unread — and a golden nobody reads is worse than none, because
//	              it looks like coverage. The hash still fails on any change, so
//	              the fact of a change stays visible; only the diff moves to where
//	              tfroots owns it.
//
// Determinism: resolveTemplateRef() reads LLZ_TEMPLATE_REF then .copier-answers.yml
// from the cwd, so the copier token is pinned via the environment below. Paths are
// emitted relative to a fixed fake instance root.
//
// Regenerate with: go test ./cmd/llz -run TestRenderGolden -update
// Review the diff before committing it — that review IS the test.

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/render_golden.txt from the current render")

// goldenSpec is deliberately richer than renderSpec: two environments (so
// per-env variation shows), an explicit component toggle, a non-default node
// pool, databases and object storage. Every field here widens what the golden
// guards, so additions are cheap and worthwhile.
const goldenSpec = `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: goldeninst }
spec:
  instance: { upstreamOrg: akamai-consulting, repo: akamai-consulting/goldeninst, forge: github, templateVersion: main }
  environments:
    prod:
      cluster:
        clusterLabel: platform-prod
        region: us-ord
        k8sVersion: v1.33.6+lke7
        nodePool: { type: g8-dedicated-8-4, count: 5 }
        bootstrap: { name: platform-prod, domainSuffix: prod.example.com }
        objectStorage: { cluster: us-ord-7 }
      components:
        harbor: { enabled: false }
    staging:
      cluster:
        clusterLabel: platform-staging
        region: us-sea
        k8sVersion: v1.33.6+lke7
        nodePool: { type: g6-standard-4, count: 2 }
        bootstrap: { name: platform-staging, domainSuffix: staging.example.com }
        objectStorage: { cluster: us-sea-1 }
`

// specDerived reports whether a rendered path's CONTENT is produced by the
// spec→artifact mapping (golden it in full) rather than copied from the embedded
// tfroots package (fingerprint it).
func specDerived(path string) bool {
	return strings.HasSuffix(path, ".tfvars") || strings.Contains(path, "/apl-values/")
}

// serializeRender renders the targets into a stable, reviewable text form:
// sorted by path, full content for spec-derived files, hash+size for the rest.
func serializeRender(targets map[string]string, instRoot string) string {
	paths := make([]string, 0, len(targets))
	for p := range targets {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var b strings.Builder
	for _, p := range paths {
		rel := strings.TrimPrefix(strings.TrimPrefix(p, instRoot), "/")
		if specDerived(p) {
			fmt.Fprintf(&b, "=== %s\n%s", rel, targets[p])
			if !strings.HasSuffix(targets[p], "\n") {
				b.WriteString("\n")
			}
			continue
		}
		fmt.Fprintf(&b, "--- %s  sha256=%x  bytes=%d\n",
			rel, sha256.Sum256([]byte(targets[p])), len(targets[p]))
	}
	return b.String()
}

func TestRenderGolden(t *testing.T) {
	// Pin the copier token that tfrootTokens() resolves; otherwise the render
	// depends on a .copier-answers.yml in whatever cwd the test runs from.
	t.Setenv("LLZ_TEMPLATE_REF", "v0.0.0-golden")

	const instRoot = "/inst"
	tfDir := filepath.Join(instRoot, "terraform-iac-bootstrap")
	aplDir := filepath.Join(instRoot, "apl-values")

	lz, err := clusterspec.Decode([]byte(goldenSpec))
	if err != nil {
		t.Fatalf("decode goldenSpec: %v", err)
	}
	// tfvarsOnly=false so the apl-values artifacts render too — those are the
	// committed, Argo-synced half of the output and the half `llz render --check`
	// drift-guards in an instance.
	targets, err := renderTargets(lz, []string{"prod", "staging"}, tfDir, aplDir, false)
	if err != nil {
		t.Fatalf("renderTargets: %v", err)
	}
	got := serializeRender(targets, instRoot)

	path := filepath.Join("testdata", "render_golden.txt")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes) — REVIEW THE DIFF before committing", path, len(got))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (regenerate with: go test ./cmd/llz -run TestRenderGolden -update)", err)
	}
	if string(want) == got {
		return
	}

	// Report the first differing line rather than dumping both files: a 50KB
	// diff in test output is not read, and an unread failure message is the same
	// problem as an unread golden.
	gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(gl) || i < len(wl); i++ {
		var g, w string
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			t.Fatalf("render output changed at line %d\n  golden: %q\n  actual: %q\n\n"+
				"If this change is intended, regenerate and REVIEW the diff:\n"+
				"  go test ./cmd/llz -run TestRenderGolden -update\n"+
				"(%d golden lines, %d actual)", i+1, w, g, len(wl), len(gl))
		}
	}
	t.Fatalf("render output differs in length only: golden %d lines, actual %d", len(wl), len(gl))
}

// The golden is only meaningful if the serialization it compares actually
// distinguishes a changed render. This asserts the two halves of that: a changed
// spec-derived VALUE shows up (full content), and a changed static file shows up
// too (via its hash), so neither half can drift silently.
func TestRenderGoldenSerializationDiscriminates(t *testing.T) {
	base := map[string]string{
		"/inst/terraform-iac-bootstrap/cluster/prod.tfvars": "node_count = 5\n",
		"/inst/terraform-iac-bootstrap/cluster/main.tf":     "resource \"x\" {}\n",
	}
	got := serializeRender(base, "/inst")

	// A one-character change in a spec-derived value must change the output.
	changed := map[string]string{
		"/inst/terraform-iac-bootstrap/cluster/prod.tfvars": "node_count = 50\n",
		"/inst/terraform-iac-bootstrap/cluster/main.tf":     "resource \"x\" {}\n",
	}
	if serializeRender(changed, "/inst") == got {
		t.Error("a changed .tfvars value did not change the serialization — the golden would not catch it " +
			"(this is exactly the node_count = 5 vs 50 case that strings.Contains misses)")
	}

	// A change to a STATIC file must also show, via its hash, even though its
	// content is not spelled out.
	staticChanged := map[string]string{
		"/inst/terraform-iac-bootstrap/cluster/prod.tfvars": "node_count = 5\n",
		"/inst/terraform-iac-bootstrap/cluster/main.tf":     "resource \"y\" {}\n",
	}
	if serializeRender(staticChanged, "/inst") == got {
		t.Error("a changed static file did not change the serialization — the fingerprint is not doing its job")
	}

	// Full content for spec-derived, hash for static: the whole point of the split.
	if !strings.Contains(got, "node_count = 5") {
		t.Error("spec-derived content must appear verbatim")
	}
	if strings.Contains(got, `resource "x"`) {
		t.Error("static content must NOT be spelled out — it is fingerprinted")
	}
	if !strings.Contains(got, "sha256=") {
		t.Error("static files must carry a hash")
	}
}
