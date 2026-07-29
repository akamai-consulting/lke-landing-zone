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
