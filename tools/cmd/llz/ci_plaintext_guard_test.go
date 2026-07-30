package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScanPlaintextShapes pins the four shapes the scanner claims to detect.
// Each is a real pattern from this tree, not a synthetic one.
func TestScanPlaintextShapes(t *testing.T) {
	cases := []struct {
		name, file, body string
		isGo             bool
		wantKey          string
	}{
		{
			name:    "scrape scheme http keys on the endpoint port",
			file:    "a/servicemonitor.yaml",
			body:    "  endpoints:\n    - port: metrics\n      scheme: http\n",
			wantKey: "a/servicemonitor.yaml:metrics",
		},
		{
			name:    "insecureSkipVerify keys on the endpoint port",
			file:    "b/servicemonitor.yaml",
			body:    "    - port: https\n      tlsConfig:\n        insecureSkipVerify: true\n",
			wantKey: "b/servicemonitor.yaml:https",
		},
		{
			name:    "in-cluster http URL keys on the URL",
			file:    "c/values.yaml",
			body:    "  lokiPushUrl: http://loki-gateway.llz-observability.svc.cluster.local/loki/api/v1/push\n",
			wantKey: "c/values.yaml:http://loki-gateway.llz-observability.svc.cluster.local",
		},
		{
			name:    "Go InsecureSkipVerify keys on the enclosing symbol",
			file:    "d/client.go",
			body:    "func HTTPClientInsecure(t time.Duration) *http.Client {\n\treturn &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}\n}\n",
			isGo:    true,
			wantKey: "d/client.go:HTTPClientInsecure",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scanPlaintext(c.file, c.body, c.isGo)
			if len(got) != 1 {
				t.Fatalf("want exactly 1 finding, got %d: %+v", len(got), got)
			}
			if got[0].key != c.wantKey {
				t.Errorf("key = %q, want %q", got[0].key, c.wantKey)
			}
		})
	}
}

// TestScanPlaintextIgnoresComments is the property that makes the registry
// usable: every accepted residual carries an explanatory comment naming the
// thing it accepts, and those comments must not themselves register as hops.
// Without this the guard would grow a new finding each time someone documented
// one.
func TestScanPlaintextIgnoresComments(t *testing.T) {
	yaml := "# was scheme: http before the mTLS work\n" +
		"# and talked to http://harbor-core.harbor.svc.cluster.local\n" +
		"      scheme: https\n"
	if got := scanPlaintext("x.yaml", yaml, false); len(got) != 0 {
		t.Errorf("comments must not be findings, got %+v", got)
	}
	golang := "// InsecureSkipVerify: true was the old posture\n\tMinVersion: tls.VersionTLS12,\n"
	if got := scanPlaintext("x.go", golang, true); len(got) != 0 {
		t.Errorf("Go comments must not be findings, got %+v", got)
	}
}

// TestScanPlaintextIgnoresExternalHTTP: only IN-CLUSTER Service URLs are hops
// this repo can secure. An http:// link to a docs site is not a residual, and
// flagging it would train reviewers to ignore the guard.
func TestScanPlaintextIgnoresExternalHTTP(t *testing.T) {
	body := "  runbook: http://example.com/runbook\n  docs: http://localhost:8080/x\n"
	if got := scanPlaintext("x.yaml", body, false); len(got) != 0 {
		t.Errorf("external/localhost URLs must not be findings, got %+v", got)
	}
}

// TestPlaintextGuardRealTree runs the guard over the actual repo. It is the test
// that fails when someone adds an unregistered plaintext hop — the whole point
// of the guard — and equally when a registry entry outlives its hop.
func TestPlaintextGuardRealTree(t *testing.T) {
	root := repoRootForGuardTest(t)
	if err := runCIPlaintextGuard(root); err != nil {
		t.Errorf("plaintext-guard failed on the real tree: %v", err)
	}
}

// TestPlaintextGuardFailsOnEmptyCorpus: a guard pointed at nothing must not
// report the same green as one that checked everything. Mirrors the
// requireCorpus contract the sibling guards share.
func TestPlaintextGuardFailsOnEmptyCorpus(t *testing.T) {
	err := runCIPlaintextGuard(t.TempDir())
	if err == nil {
		t.Fatal("expected a failure on an empty corpus")
	}
	if !strings.Contains(err.Error(), "empty corpus") {
		t.Errorf("error should name the empty corpus, got: %v", err)
	}
}

// TestPlaintextRegistryEntriesAreReviewable: a registry whose reasons say
// "accepted" is a rubber stamp. Each entry must name an owner from the known set
// and give a reason substantial enough to review.
func TestPlaintextRegistryEntriesAreReviewable(t *testing.T) {
	owners := map[string]bool{"llz": true, "apl-core": true, "inherent": true}
	for k, r := range plaintextAllowed {
		if !owners[r.owner] && !strings.HasPrefix(r.owner, "upstream-") {
			t.Errorf("%s: owner %q is not one of llz|apl-core|inherent|upstream-*", k, r.owner)
		}
		if len(r.reason) < 60 {
			t.Errorf("%s: reason is too thin to review (%d chars) — say WHAT crosses the wire", k, len(r.reason))
		}
		if !strings.Contains(k, ":") {
			t.Errorf("%s: key must be <path>:<locator>", k)
		}
	}
}

func repoRootForGuardTest(t *testing.T) string {
	t.Helper()
	// Tests run from tools/cmd/llz; the repo root is three levels up.
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "platform-apl")); err != nil {
		t.Skipf("repo root not found at %s: %v", root, err)
	}
	return root
}

// TestScanPlaintextGoServiceURL is the regression test for the guard's worst
// blind spot. The first revision `continue`d after the InsecureSkipVerify check,
// and its comment-stripper treated the "//" in "http://" as a comment start —
// between them, the two hops that MOTIVATED this guard were invisible to it:
// the Harbor REST base (Harbor admin password in a Basic-auth header) and
// Keycloak's JWKS URL (the signing keys OpenBao validates team logins with).
//
// A guard that misses the findings it was built for is worse than none: it
// reports green and buys false confidence.
func TestScanPlaintextGoServiceURL(t *testing.T) {
	cases := []struct{ name, body, wantKey string }{
		{
			name:    "URL in a function body",
			body:    "func run() {\n\tapiURL := envOr(\"HARBOR_API_URL\", \"http://harbor-core.harbor.svc.cluster.local\")\n}\n",
			wantKey: "x.go:http://harbor-core.harbor.svc.cluster.local",
		},
		{
			name:    "URL in a const with a port and path",
			body:    "const jwks = \"http://keycloak-keycloakx-http.keycloak.svc.cluster.local:8080/realms/otomi/certs\"\n",
			wantKey: "x.go:http://keycloak-keycloakx-http.keycloak.svc.cluster.local",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scanPlaintext("x.go", c.body, true)
			if len(got) != 1 {
				t.Fatalf("want 1 finding, got %d: %+v", len(got), got)
			}
			if got[0].key != c.wantKey {
				t.Errorf("key = %q, want %q", got[0].key, c.wantKey)
			}
		})
	}
}

// TestStripCommentKeepsSchemeSeparator pins the specific mechanic above: "://"
// must not be mistaken for the start of a Go comment.
func TestStripCommentKeepsSchemeSeparator(t *testing.T) {
	line := "\tu := \"http://svc.ns.svc.cluster.local\" // trailing note"
	got := stripComment(line, true)
	if !strings.Contains(got, "http://svc.ns.svc.cluster.local") {
		t.Errorf("scheme separator was eaten as a comment: %q", got)
	}
	if strings.Contains(got, "trailing note") {
		t.Errorf("the real trailing comment survived: %q", got)
	}
}

// TestScanPlaintextEvasionShapes pins the tolerance the patterns need. YAML lets
// the same meaning be spelled several ways, and the first revision of this guard
// matched only one of them — anchored, lowercase, unquoted. `scheme: "http"`,
// `scheme: HTTP` and a flow-style `{port: m, scheme: http}` all mean the same
// thing to Kubernetes, so a guard that misses them is one a reviewer bypasses by
// adding quotes without ever knowing there was a gate.
func TestScanPlaintextEvasionShapes(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"double-quoted scheme", "    - port: m\n      scheme: \"http\"\n"},
		{"single-quoted scheme", "    - port: m\n      scheme: 'http'\n"},
		{"uppercase scheme", "    - port: m\n      scheme: HTTP\n"},
		{"trailing comment", "    - port: m\n      scheme: http # legacy\n"},
		{"flow style", "    - {port: m, scheme: http}\n"},
		{"quoted insecureSkipVerify", "    - port: m\n      insecureSkipVerify: \"true\"\n"},
		{"svc without cluster.local", "  u: http://harbor-core.harbor.svc\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := scanPlaintext("p.yaml", c.body, false); len(got) != 1 {
				t.Errorf("want 1 finding, got %d: %+v", len(got), got)
			}
		})
	}
}

// TestScanPlaintextDoesNotFlagHTTPS is the other half of that tolerance: `https`
// must not match, and RE2 has no negative lookahead, so the exclusion rides on
// the trailing character class. Easy to break while widening the pattern.
func TestScanPlaintextDoesNotFlagHTTPS(t *testing.T) {
	for _, body := range []string{
		"    - port: m\n      scheme: https\n",
		"    - port: m\n      scheme: \"https\"\n",
		"    - {port: m, scheme: https}\n",
		"  u: https://harbor-core.harbor.svc.cluster.local\n",
	} {
		if got := scanPlaintext("p.yaml", body, false); len(got) != 0 {
			t.Errorf("https must not be a finding (%q), got %+v", body, got)
		}
	}
}

// TestScanPlaintextResetsPortPerDocument: a registry key must identify ONE hop.
// Port context is tracked line-by-line, and before this it leaked across YAML
// document separators — a finding in the second document was keyed on the first
// document's port. Two hops in one file could then collide on the same key, so
// registering one would silently vouch for the other.
func TestScanPlaintextResetsPortPerDocument(t *testing.T) {
	body := "kind: ServiceMonitor\nspec:\n  endpoints:\n    - port: alpha\n      scheme: https\n" +
		"---\nkind: ServiceMonitor\nspec:\n  endpoints:\n    - scheme: http\n"
	got := scanPlaintext("multi.yaml", body, false)
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(got), got)
	}
	if got[0].key == "multi.yaml:alpha" {
		t.Error("port leaked across the document separator — the finding is keyed on another document's port")
	}
	if got[0].key != "multi.yaml:scheme-http" {
		t.Errorf("key = %q, want the no-port fallback multi.yaml:scheme-http", got[0].key)
	}
}

// TestScanPlaintextLocatorNeverEmpty: an empty locator makes unrelated findings
// in one file share a key.
func TestScanPlaintextLocatorNeverEmpty(t *testing.T) {
	for _, body := range []string{
		"  tlsConfig:\n    insecureSkipVerify: true\n",
		"  endpoints:\n    - scheme: http\n",
	} {
		for _, f := range scanPlaintext("x.yaml", body, false) {
			if strings.HasSuffix(f.key, ":") {
				t.Errorf("empty locator in key %q", f.key)
			}
		}
	}
}

// TestScanPlaintextKeysAreUniquePerFile guards the same property on the real
// tree: no two findings in one file may share a key.
func TestScanPlaintextKeysAreUniquePerFile(t *testing.T) {
	root := repoRootForGuardTest(t)
	findings, _, err := collectPlaintextFindings(root, plaintextScanDirs(root))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, f := range findings {
		seen[f.key]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("key %q covers %d distinct findings — registering it would vouch for all of them", k, n)
		}
	}
}

// TestRelForKeyIsLayoutStable: registry keys must name the HOP, not the
// checkout it was read from. The sibling guards accept --root as either a
// template checkout or an instance, where the same trees sit one level down
// under instance-template/. Before this, every key carried that prefix in one
// layout and not the other — so running the guard against an instance reported
// the entire tree as unregistered AND every registry entry as stale.
func TestRelForKeyIsLayoutStable(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "instance-template", "platform-apl", "x.yaml")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(dir, "platform-apl", "x.yaml")
	if err := os.MkdirAll(filepath.Dir(flat), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := relForKey(dir, nested), "platform-apl/x.yaml"; got != want {
		t.Errorf("nested layout: relForKey = %q, want %q", got, want)
	}
	if got, want := relForKey(dir, flat), "platform-apl/x.yaml"; got != want {
		t.Errorf("flat layout: relForKey = %q, want %q", got, want)
	}
}

// A PeerAuthentication port exemption is an accepted plaintext hop, and it is
// spelled as mesh policy rather than as a URL or a scrape scheme. The guard was
// blind to the whole class, so the two exemptions the harbor namespace ships
// (CNPG on 8000, Prometheus on 8001) passed green while the registry claimed to
// enumerate every accepted residual.
//
// Each exemption must key on ITS OWN port, which comes from a quoted MAP KEY
// (`"8000":`) that rePortName cannot see — keying both on the same locator would
// let one registry entry silently vouch for the other.
func TestPermissiveMTLSIsPerPortFinding(t *testing.T) {
	const doc = `
apiVersion: security.istio.io/v1
kind: PeerAuthentication
metadata:
  name: harbor-strict-mtls
spec:
  mtls:
    mode: STRICT
  portLevelMtls:
    "8000":
      mode: PERMISSIVE
    "8001":
      mode: PERMISSIVE
`
	got := map[string]bool{}
	for _, f := range scanPlaintext("p/pa.yaml", doc, false) {
		got[f.key] = true
	}
	for _, want := range []string{"p/pa.yaml:mtls-8000", "p/pa.yaml:mtls-8001"} {
		if !got[want] {
			t.Errorf("missing per-port finding %q; got %v", want, got)
		}
	}
	// STRICT is the enforced case and must not be reported.
	if len(got) != 2 {
		t.Errorf("expected exactly the two PERMISSIVE ports, got %v", got)
	}
}

// A namespace-wide PERMISSIVE has no port context. It must still be a finding —
// it is strictly WORSE than a port exemption (every port accepts cleartext) — and
// it must not key on an empty locator, which would make it collide with an
// unrelated hop in the same file.
func TestNamespaceWidePermissiveIsReported(t *testing.T) {
	fs := scanPlaintext("p/pa.yaml", "spec:\n  mtls:\n    mode: PERMISSIVE\n", false)
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %d (%v)", len(fs), fs)
	}
	if fs[0].key != "p/pa.yaml:mtls-mode" {
		t.Errorf("key = %q, want p/pa.yaml:mtls-mode", fs[0].key)
	}
}

// DestinationRule tls.mode: DISABLE turns encryption off outright, so it belongs
// to the same class as PERMISSIVE.
func TestDestinationRuleDisableIsReported(t *testing.T) {
	if fs := scanPlaintext("p/dr.yaml", "  tls:\n    mode: DISABLE\n", false); len(fs) != 1 {
		t.Fatalf("tls.mode: DISABLE must be a finding, got %v", fs)
	}
}

// `mode:` alone is everywhere in Kubernetes YAML. Only the two mesh values are
// findings — matching the key would flood the gate and train reviewers around it.
// UNSET means "inherit", so the hop belongs to whatever sets the mesh default.
func TestBenignModeValuesAreNotFindings(t *testing.T) {
	for _, ln := range []string{
		"        mode: 0644\n", "  defaultMode: 420\n", "  volumeMode: Block\n",
		"    mode: STRICT\n", "    mode: UNSET\n",
	} {
		if fs := scanPlaintext("p/x.yaml", ln, false); len(fs) != 0 {
			t.Errorf("%q must not be a finding, got %v", ln, fs)
		}
	}
}

// reSvcHTTP only ever saw the fully-qualified name, so writing the SHORT form
// Kubernetes DNS also resolves bypassed the gate entirely — and the short form is
// the spelling a reviewer reaches for first.
func TestShortFormInClusterURLsAreFindings(t *testing.T) {
	for _, tc := range []struct{ line, wantKey string }{
		{`  value: "http://harbor-core.harbor"`, "p/x.yaml:http://harbor-core.harbor"},
		{`  value: "http://harbor-core"`, "p/x.yaml:http://harbor-core"},
		// The port must not enter the key: adding one later would strand the entry.
		{`  value: "http://harbor-core.harbor:8080/api"`, "p/x.yaml:http://harbor-core.harbor"},
		// Credentials must not enter the key either — it would rotate with them.
		{`  value: "http://user:pw@git-server.git-server"`, "p/x.yaml:http://git-server.git-server"},
	} {
		fs := scanPlaintext("p/x.yaml", tc.line+"\n", false)
		if len(fs) != 1 || fs[0].key != tc.wantKey {
			t.Errorf("%s -> %v, want single finding keyed %q", tc.line, fs, tc.wantKey)
		}
	}
}

// The same hole existed in Go, where the most consequential URLs in this tree
// live as constants.
func TestShortFormInClusterURLsAreFindingsInGo(t *testing.T) {
	fs := scanPlaintext("t/x.go", "const u = \"http://harbor-core.harbor:8080\"\n", true)
	if len(fs) != 1 || fs[0].key != "t/x.go:http://harbor-core.harbor" {
		t.Fatalf("got %v, want a single short-form finding", fs)
	}
}

// The cost of a false positive is a reviewer who learns to bypass the gate, so
// external hosts must stay silent. These are the shapes this repo actually
// contains.
func TestExternalAndLoopbackHostsAreNotFindings(t *testing.T) {
	for _, ln := range []string{
		`  value: "http://nl-ams-1.linodeobjects.com"`,
		`  value: "http://git.lke1.akamai-apl.net/otomi/values.git"`,
		`  value: "http://www.w3.org/2001/XMLSchema"`,
		`  value: "http://localhost:8200"`,
		`  value: "http://127.0.0.1:8210"`,
		`  value: "http://10.0.0.1:9090"`,
		// Templated hosts do not match by design (documented on reAnyHTTPHost).
		`  value: "http://{{ .Values.host }}"`,
	} {
		if fs := scanPlaintext("p/x.yaml", ln+"\n", false); len(fs) != 0 {
			t.Errorf("%s must not be a finding, got %v", ln, fs)
		}
	}
}

// The fully-qualified form keeps its EXACT historical key. Re-keying it would
// strand every existing registry entry as stale in one commit.
func TestFullyQualifiedURLKeyIsUnchanged(t *testing.T) {
	fs := scanPlaintext("t/x.go", "const u = \"http://harbor-core.harbor.svc.cluster.local\"\n", true)
	if len(fs) != 1 || fs[0].key != "t/x.go:http://harbor-core.harbor.svc.cluster.local" {
		t.Fatalf("got %v, want the unchanged fully-qualified key", fs)
	}
}
