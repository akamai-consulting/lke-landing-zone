package plaintext

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
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
	if err := Run(root); err != nil {
		t.Errorf("plaintext-guard failed on the real tree: %v", err)
	}
}

// TestPlaintextGuardFailsOnEmptyCorpus: a guard pointed at nothing must not
// report the same color.Green as one that checked everything. Mirrors the
// requireCorpus contract the sibling guards share.
func TestPlaintextGuardFailsOnEmptyCorpus(t *testing.T) {
	err := Run(t.TempDir())
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
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "platform-apl")); err != nil {
		t.Skipf("repo root not found at %s: %v", root, err)
	}
	return root
}

// guardRepoForTest is the reader a real run gets. Built from the EXTENSION rather
// than hand-rolled, so a test cannot hand itself a reader the declaration would
// not have produced.
func guardRepoForTest(t *testing.T, root string) capability.Repo {
	t.Helper()
	return capability.RepoForGate(Extension(), root)
}

// TestScanPlaintextGoServiceURL is the regression test for the guard's worst
// blind spot. The first revision `continue`d after the InsecureSkipVerify check,
// and its comment-stripper treated the "//" in "http://" as a comment start —
// between them, the two hops that MOTIVATED this guard were invisible to it:
// the Harbor REST base (Harbor admin password in a Basic-auth header) and
// Keycloak's JWKS URL (the signing keys OpenBao validates team logins with).
//
// A guard that misses the findings it was built for is worse than none: it
// reports color.Green and buys false confidence.
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
	repo := guardRepoForTest(t, repoRootForGuardTest(t))
	findings, _, err := collectPlaintextFindings(repo, plaintextScanDirs(repo))
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
	// The reader hands relForKey a repo-relative path, so the two layouts are
	// exactly the two shapes it can now receive.
	if got, want := relForKey("instance-template/platform-apl/x.yaml"), "platform-apl/x.yaml"; got != want {
		t.Errorf("nested layout: relForKey = %q, want %q", got, want)
	}
	if got, want := relForKey("platform-apl/x.yaml"), "platform-apl/x.yaml"; got != want {
		t.Errorf("flat layout: relForKey = %q, want %q", got, want)
	}
}

// A PeerAuthentication port exemption is an accepted plaintext hop, and it is
// spelled as mesh policy rather than as a URL or a scrape scheme. The guard was
// blind to the whole class, so the two exemptions the harbor namespace ships
// (CNPG on 8000, Prometheus on 8001) passed color.Green while the registry claimed to
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

// A resource EventSource watching `secrets` puts the whole Secret — `data`
// included — onto the event bus. This is a PAYLOAD hop rather than a transport
// one, so none of the URL/scheme/mesh shapes can see it; it survived the entire
// mTLS audit for exactly that reason. Pinned against the real cert-automation
// spelling.
func TestScanPlaintextEventSourceSecrets(t *testing.T) {
	body := "spec:\n  resource:\n    haproxy-tls:\n      namespace: cert-manager\n" +
		"      group: \"\"\n      version: v1\n      resource: secrets\n      eventTypes:\n        - UPDATE\n"
	fs := scanPlaintext("k/eventsource.yaml", body, false)
	if len(fs) != 1 {
		t.Fatalf("want exactly 1 finding, got %d: %+v", len(fs), fs)
	}
	if fs[0].key != "k/eventsource.yaml:eventsource-secrets" {
		t.Errorf("key = %q, want %q", fs[0].key, "k/eventsource.yaml:eventsource-secrets")
	}
	if !strings.Contains(fs[0].what, "event bus") {
		t.Errorf("what = %q, want it to name the bus", fs[0].what)
	}
}

// The detector must not fire on every YAML line containing the word `secrets`.
// A guard that cried wolf on `subresource:`, on a resource LIST, or on prose
// would be turned off, and turning it off is how the hop it exists to catch
// comes back.
func TestEventSourceSecretsDetectorIsNarrow(t *testing.T) {
	for _, ln := range []string{
		`      subresource: secrets-extra`,
		`      resource: secretstores`,
		`      resources: ["secrets"]`,
		`  # resource: secrets was the old shape`,
		`      resource: certificates`,
	} {
		if fs := scanPlaintext("k/x.yaml", ln+"\n", false); len(fs) != 0 {
			t.Errorf("%s must not be a finding, got %+v", ln, fs)
		}
	}
}

// The guard began as an HTTP gate, which quietly implied HTTP is where cleartext
// lives. This platform runs CNPG Postgres and Redis: a plaintext DSN carries
// credentials and row data with no `http://` anywhere in it.
func TestCleartextWireProtocolsAreFindings(t *testing.T) {
	for _, tc := range []struct{ line, wantKey string }{
		{`  url: "postgres://user:pw@harbor-otomi-db:5432/registry"`, "p/x.yaml:postgres"},
		{`  url: "redis://harbor-redis:6379/0"`, "p/x.yaml:redis"},
		{`  broker: "amqp://guest:guest@rabbit:5672"`, "p/x.yaml:amqp"},
		{`  dsn: "mysql://root@db:3306/app"`, "p/x.yaml:mysql"},
		{`  uri: "mongodb://mongo:27017"`, "p/x.yaml:mongodb"},
		{`  dir: "ldap://dc:389"`, "p/x.yaml:ldap"},
	} {
		fs := scanPlaintext("p/x.yaml", tc.line+"\n", false)
		if len(fs) != 1 || fs[0].key != tc.wantKey {
			t.Errorf("%s -> %v, want one finding keyed %q", tc.line, fs, tc.wantKey)
		}
	}
}

// Spelling freedom: quoted and trailing-whitespace forms mean the same thing to
// Kubernetes and must not be a way around the gate. Same rationale as
// TestAlternateInsecureGoSpellingsAreFindings.
func TestEventSourceSecretsSpellings(t *testing.T) {
	for _, ln := range []string{
		`      resource: secrets`,
		`      resource: "secrets"`,
		`      resource: 'secrets'`,
		`      resource: Secrets`,
		`      resource: secrets   `,
	} {
		if fs := scanPlaintext("k/x.yaml", ln+"\n", false); len(fs) != 1 {
			t.Errorf("%s must be a finding, got %+v", ln, fs)
		}
	}
}

// The TLS-bearing spellings must stay silent, or the gate cries wolf on the very
// thing it wants people to adopt. RE2 has no negative lookahead, so this is
// enforced by the character right after the scheme name.
func TestTLSBearingProtocolsAreNotFindings(t *testing.T) {
	for _, ln := range []string{
		`  url: "rediss://harbor-redis:6380/0"`,
		`  broker: "amqps://rabbit:5671"`,
		`  dir: "ldaps://dc:636"`,
		`  uri: "mongodb+srv://mongo/db"`,
		`  url: "https://harbor-core.harbor.svc.cluster.local"`,
	} {
		if fs := scanPlaintext("p/x.yaml", ln+"\n", false); len(fs) != 0 {
			t.Errorf("%s must not be a finding, got %v", ln, fs)
		}
	}
}

// A Postgres DSN that turns TLS off outright, in either spelling.
func TestSSLModeDisableIsAFinding(t *testing.T) {
	for _, ln := range []string{`  dsn: "host=db sslmode=disable"`, `  sslmode: disable`} {
		fs := scanPlaintext("p/x.yaml", ln+"\n", false)
		if len(fs) != 1 || fs[0].key != "p/x.yaml:sslmode-disable" {
			t.Errorf("%s -> %v, want a sslmode-disable finding", ln, fs)
		}
	}
	if fs := scanPlaintext("p/x.yaml", "  sslmode: require\n", false); len(fs) != 0 {
		t.Errorf("sslmode: require must not be a finding, got %v", fs)
	}
}

// reInsecureGo matched only the composite-literal form. These are the same
// decision written differently — a gate a reviewer clears by assigning instead of
// initialising is not a gate. client-go's rest.Config spells it `Insecure`, so
// every kubeconfig-building path was invisible to the original pattern.
func TestAlternateInsecureGoSpellingsAreFindings(t *testing.T) {
	for _, ln := range []string{
		"\tcfg.TLSClientConfig.InsecureSkipVerify = true\n",
		"\tc := rest.Config{TLSClientConfig: rest.TLSClientConfig{Insecure: true}}\n",
	} {
		if fs := scanPlaintext("t/x.go", ln, true); len(fs) == 0 {
			t.Errorf("%q must be a finding", strings.TrimSpace(ln))
		}
	}
	// The safe spellings stay quiet.
	for _, ln := range []string{
		"\tcfg.InsecureSkipVerify = false\n",
		"\tc := rest.Config{Insecure: false}\n",
	} {
		if fs := scanPlaintext("t/x.go", ln, true); len(fs) != 0 {
			t.Errorf("%q must not be a finding, got %v", strings.TrimSpace(ln), fs)
		}
	}
}

// Command-line opt-outs, including inside a YAML container command/args block —
// which the walker already reads as text.
func TestInsecureCommandLineFlagsAreFindings(t *testing.T) {
	for _, ln := range []string{
		`            - "curl -k https://harbor-core/api"`,
		`            - "wget --no-check-certificate https://x"`,
		`            - "kubectl --insecure-skip-tls-verify get po"`,
		`            - "helm push chart oci://reg --plain-http"`,
	} {
		if fs := scanPlaintext("p/x.yaml", ln+"\n", false); len(fs) != 1 {
			t.Errorf("%s -> %v, want one finding", ln, fs)
		}
	}
	// `-k` only counts as a curl flag; an unrelated -k must not trip it.
	if fs := scanPlaintext("p/x.yaml", `            - "sort -k 2 file"`+"\n", false); len(fs) != 0 {
		t.Errorf("unrelated -k must not be a finding, got %v", fs)
	}
}

// Config keys that mean "do not verify TLS": unsafe when true (insecure,
// skipVerify) and unsafe when false (sslVerify, tlsVerify, verify_ssl).
func TestInsecureConfigKeysAreFindings(t *testing.T) {
	for _, ln := range []string{
		`  insecure: true`, `  skipVerify: true`, `  insecureSkipTLSVerify: true`,
		`  sslVerify: false`, `  tlsVerify: false`, `  verify_ssl: false`,
	} {
		if fs := scanPlaintext("p/x.yaml", ln+"\n", false); len(fs) != 1 {
			t.Errorf("%s -> %v, want one finding", ln, fs)
		}
	}
	// Their safe polarities must stay silent.
	for _, ln := range []string{`  insecure: false`, `  sslVerify: true`, `  tlsVerify: true`} {
		if fs := scanPlaintext("p/x.yaml", ln+"\n", false); len(fs) != 0 {
			t.Errorf("%s must not be a finding, got %v", ln, fs)
		}
	}
}

// The gap this closes. `scheme: http` and no `scheme:` at all are the SAME hop
// on the wire — prometheus-operator defaults the field to http — and every
// pattern in this guard except this one reads a decision somebody typed. Omitting
// the line is both plaintext and the more likely spelling: `scheme: https` is
// something you add on purpose, and nobody types `scheme: http` when leaving it
// out does the same thing.
func TestScanPlaintextFlagsScrapeEndpointWithNoScheme(t *testing.T) {
	doc := `apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: llz-thing
spec:
  endpoints:
    - port: metrics
      path: /metrics
`
	got := scanPlaintext("platform-apl/components/x/sm.yaml", doc, false)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}
	if got[0].key != "platform-apl/components/x/sm.yaml:metrics" {
		t.Errorf("key = %q, want it keyed on the endpoint port", got[0].key)
	}
	if !strings.Contains(got[0].what, "no `scheme:`") {
		t.Errorf("the finding must say the scheme is absent, not merely http: %q", got[0].what)
	}
}

// An explicit scheme — either one — is the main loop's business. https must be
// silent, and http must produce exactly ONE finding rather than being reported
// twice by two overlapping rules.
func TestScanPlaintextDoesNotDoubleReportAnExplicitScheme(t *testing.T) {
	for _, tc := range []struct {
		scheme string
		want   int
	}{
		{"https", 0},
		{"http", 1},
	} {
		doc := "kind: ServiceMonitor\nspec:\n  endpoints:\n    - port: metrics\n      scheme: " + tc.scheme + "\n"
		if got := scanPlaintext("a/sm.yaml", doc, false); len(got) != tc.want {
			t.Errorf("scheme %s: got %d findings, want %d: %+v", tc.scheme, len(got), tc.want, got)
		}
	}
}

// Per ENDPOINT, not per document. A monitor that secures one endpoint and forgets
// the next is the realistic mistake, and a document-level check would call that
// file clean.
func TestScanPlaintextFlagsOnlyTheEndpointMissingAScheme(t *testing.T) {
	doc := `kind: ServiceMonitor
spec:
  endpoints:
    - port: secure
      scheme: https
    - port: forgotten
      path: /metrics
`
	got := scanPlaintext("a/sm.yaml", doc, false)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}
	if !strings.HasSuffix(got[0].key, ":forgotten") {
		t.Errorf("the wrong endpoint was reported: %s", got[0].key)
	}
}

// PodMonitor spells the list `podMetricsEndpoints` and defaults identically.
func TestScanPlaintextCoversPodMonitors(t *testing.T) {
	doc := "kind: PodMonitor\nspec:\n  podMetricsEndpoints:\n    - port: metrics\n"
	if got := scanPlaintext("a/pm.yaml", doc, false); len(got) != 1 {
		t.Errorf("a PodMonitor endpoint with no scheme must be reported, got %+v", got)
	}
}

// Only these two kinds. `endpoints:` is an ordinary key elsewhere in Kubernetes
// (a core/v1 Endpoints object, a chart's values), and flagging those would put
// permanent noise in front of every reviewer — which is how a gate gets bypassed.
func TestScanPlaintextIgnoresEndpointsOutsideAMonitor(t *testing.T) {
	doc := "kind: Endpoints\nsubsets:\n  endpoints:\n    - port: 8080\n"
	if got := scanPlaintext("a/ep.yaml", doc, false); len(got) != 0 {
		t.Errorf("a non-monitor `endpoints:` must not be reported, got %+v", got)
	}
}

// Multi-document files must report a line in the FILE, not in the fragment: a
// finding a reviewer cannot navigate to is half a finding.
func TestScanDefaultedSchemeReportsFileRelativeLines(t *testing.T) {
	doc := "kind: ConfigMap\nmetadata:\n  name: pad\ndata: {}\n---\nkind: ServiceMonitor\nspec:\n  endpoints:\n    - port: metrics\n"
	got := scanDefaultedScrapeScheme("a/multi.yaml", doc)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}
	if got[0].line != 9 {
		t.Errorf("line = %d, want 9 (the `- port: metrics` line in the FILE)", got[0].line)
	}
}

// The corpus is clean today — three of the four monitors declare `https` because
// #360 moved them — so this closes a LATENT gap. That is the cheapest moment to
// add a drift gate, and this test states the fact so a future reader does not
// mistake "no findings" for "not wired up".
func TestDefaultedSchemeIsLatentOnThisTree(t *testing.T) {
	repo := guardRepoForTest(t, "../../../../..")
	dirs := plaintextScanDirs(repo)
	findings, examined, err := collectPlaintextFindings(repo, dirs)
	if err != nil {
		t.Fatal(err)
	}
	if examined == 0 {
		t.Fatal("examined nothing")
	}
	for _, f := range findings {
		if strings.Contains(f.what, "no `scheme:`") {
			t.Errorf("%s:%d has a defaulted scrape scheme — secure it or register it", f.file, f.line)
		}
	}
}
