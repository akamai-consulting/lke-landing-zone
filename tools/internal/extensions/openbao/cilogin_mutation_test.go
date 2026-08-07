package openbao

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baoLoginCapture stands up a fake OpenBao that records the login request's
// path and decoded body, and hands back a token.
type baoLoginCapture struct {
	path string
	role string
	jwt  string
}

func serveBaoLogin(t *testing.T) (*httptest.Server, *baoLoginCapture) {
	t.Helper()
	got := &baoLoginCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		var in struct{ Role, Jwt string }
		_ = json.Unmarshal(b, &in)
		got.role, got.jwt = in.Role, in.Jwt
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"auth":{"client_token":"s.k8s-token"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func writeSATokenFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte("sa-jwt-abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestOpenBaoLoginResolvesAddr pins the --addr fallback chain: explicit flag,
// then $OPENBAO_ADDR, then the in-cluster ClusterIP. Getting this backwards
// points an in-cluster login at the wrong endpoint, and the failure surfaces as
// a TLS/dial error nowhere near the flag that caused it. Asserted through the
// dry-run line, which exists to show the operator exactly what would be dialled.
func TestOpenBaoLoginResolvesAddr(t *testing.T) {
	var err error
	dryRunLine := func(addr string) string {
		t.Helper()
		out := captureStderr(t, func() {
			err = RunCILogin(true, "kubernetes", "", addr, "", "", "OPENBAO_TOKEN")
		})
		if err != nil {
			t.Fatalf("dry-run must be a no-op success: %v", err)
		}
		return out
	}

	t.Setenv("OPENBAO_ADDR", "https://from-env:8200")
	if got := dryRunLine(""); !strings.Contains(got, "addr=https://from-env:8200") {
		t.Errorf("$OPENBAO_ADDR must win when --addr is unset:\n%s", got)
	}

	t.Setenv("OPENBAO_ADDR", "")
	if got := dryRunLine(""); !strings.Contains(got, "addr="+InClusterAddr) {
		t.Errorf("with neither --addr nor $OPENBAO_ADDR the in-cluster ClusterIP must be used:\n%s", got)
	}

	// An explicit --addr beats both.
	t.Setenv("OPENBAO_ADDR", "https://from-env:8200")
	if got := dryRunLine("https://explicit:8200"); !strings.Contains(got, "addr=https://explicit:8200") {
		t.Errorf("--addr must win over the environment:\n%s", got)
	}
}

// TestOpenBaoLoginKubernetesRoleAndMountReachOpenBao pins what actually goes on
// the wire. The role selects the OpenBao POLICY the issued token carries and the
// mount selects the auth method — substituting either for the other's default
// yields a 403/404 from OpenBao that reads like a broken cluster rather than a
// wrong argument.
func TestOpenBaoLoginKubernetesRoleAndMountReachOpenBao(t *testing.T) {
	srv, got := serveBaoLogin(t)
	stubInClusterBaoClient(t, srv.Client())
	t.Setenv("GITHUB_ENV", "")
	t.Setenv("OPENBAO_KUBERNETES_MOUNT", "")
	sa := writeSATokenFile(t)

	// Explicit role + mount are used verbatim.
	if err := RunCILogin(false, "kubernetes", "cluster-admin", srv.URL, "k8s-prod", sa, "OPENBAO_TOKEN"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got.role != "cluster-admin" {
		t.Errorf("role on the wire = %q, want the explicit cluster-admin", got.role)
	}
	if want := "/v1/auth/k8s-prod/login"; got.path != want {
		t.Errorf("login path = %q, want %q — the explicit mount must be used", got.path, want)
	}
	if got.jwt != "sa-jwt-abc" {
		t.Errorf("ServiceAccount JWT on the wire = %q, want the file's contents", got.jwt)
	}

	// Omitted role and mount fall back to the documented defaults.
	if err := RunCILogin(false, "kubernetes", "", srv.URL, "", sa, "OPENBAO_TOKEN"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got.role != "reconciler" {
		t.Errorf("default role = %q, want reconciler", got.role)
	}
	if want := "/v1/auth/kubernetes/login"; got.path != want {
		t.Errorf("default login path = %q, want %q", got.path, want)
	}
}

// TestOpenBaoLoginSurfacesAnExportFailure: the whole point of this verb is to
// hand the token to the following steps via $GITHUB_ENV. If the export fails and
// the command still exits 0, every downstream step runs with an EMPTY
// OPENBAO_TOKEN and fails with a 403 far from the cause.
func TestOpenBaoLoginSurfacesAnExportFailure(t *testing.T) {
	srv, _ := serveBaoLogin(t)
	stubInClusterBaoClient(t, srv.Client())
	sa := writeSATokenFile(t)
	// A $GITHUB_ENV inside a directory that does not exist → the append fails.
	t.Setenv("GITHUB_ENV", filepath.Join(t.TempDir(), "no-such-dir", "gh_env"))

	err := RunCILogin(false, "kubernetes", "reconciler", srv.URL, "kubernetes", sa, "OPENBAO_TOKEN")
	if err == nil {
		t.Fatal("a failed $GITHUB_ENV export must surface — silently exiting 0 leaves every later step tokenless")
	}
	if !strings.Contains(err.Error(), "GITHUB_ENV") {
		t.Errorf("error should name $GITHUB_ENV, got: %v", err)
	}
}

// A successful export is confirmed on stderr, so a log reader can tell an
// exported token from a skipped one.
func TestOpenBaoLoginConfirmsTheExport(t *testing.T) {
	srv, _ := serveBaoLogin(t)
	stubInClusterBaoClient(t, srv.Client())
	sa := writeSATokenFile(t)
	t.Setenv("GITHUB_ENV", filepath.Join(t.TempDir(), "gh_env"))

	var err error
	got := captureStderr(t, func() {
		err = RunCILogin(false, "kubernetes", "reconciler", srv.URL, "kubernetes", sa, "OPENBAO_TOKEN")
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !strings.Contains(got, "OPENBAO_TOKEN exported to $GITHUB_ENV") {
		t.Errorf("a successful export must be reported:\n%s", got)
	}
}
