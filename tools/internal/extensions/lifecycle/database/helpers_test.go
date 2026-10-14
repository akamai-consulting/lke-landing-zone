package database

import (
	"crypto/tls"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

// helpers_test.go — fixtures the moved tests need.

// clearOpenbaoEnv unsets every OPENBAO_* var so a test starts from a known
// address state. A COPY — package main has one for its forward tests.
func clearOpenbaoEnv(t *testing.T) {
	t.Helper()
	for _, k := range os.Environ() {
		if i := strings.IndexByte(k, '='); i > 0 && strings.HasPrefix(k[:i], "OPENBAO_") {
			t.Setenv(k[:i], "")
		}
	}
}

// seamForward stubs the package's OpenBaoForward capability with a client pointed
// at addr, and reports whether it was reached.
//
// Package main's helper of the same name swaps portForwardOpenbaoFn — the
// mechanism. This one swaps what THIS package reaches through, which is the seam
// the extraction created. Same name, different subject, and the pair is the whole
// argument for the seam existing.
func seamForward(t *testing.T, addr string, err error) *bool {
	t.Helper()
	called := false
	prev := OpenBaoForward
	OpenBaoForward = func(string) (*openbao.Client, func(), error) {
		called = true
		if err != nil {
			return nil, func() {}, err
		}
		return openbao.NewWithClient(addr, os.Getenv("OPENBAO_ROOT_TOKEN"), "", insecureClient()), func() {}, nil
	}
	t.Cleanup(func() { OpenBaoForward = prev })
	return &called
}
func withGHASummaryFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "summary")
	t.Setenv("GITHUB_STEP_SUMMARY", p)
	return p
}

// insecureClient trusts the httptest server's self-signed certificate. The stub
// speaks TLS because the real OpenBao does, and a client that refused it would
// make every forward test fail for a reason unrelated to what it checks.
func insecureClient() *http.Client {
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // test stub
	}
}
