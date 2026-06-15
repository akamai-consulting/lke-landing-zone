package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withHarborPFFiles redirects the pid/log paths into a temp dir.
func withHarborPFFiles(t *testing.T) {
	t.Helper()
	prevLog, prevPid := harborPFLog, harborPFPid
	dir := t.TempDir()
	harborPFLog, harborPFPid = filepath.Join(dir, "pf.log"), filepath.Join(dir, "pf.pid")
	t.Cleanup(func() { harborPFLog, harborPFPid = prevLog, prevPid })
}

// withGHAEnvFile captures $GITHUB_ENV writes; returns the path.
func withGHAEnvFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gha-env")
	t.Setenv("GITHUB_ENV", p)
	return p
}

func ghaEnvContains(t *testing.T, path, want string) bool {
	t.Helper()
	b, _ := os.ReadFile(path)
	return strings.Contains(string(b), want)
}

func TestHarborEnsureProject(t *testing.T) {
	adminSecret := func(a string) ([]byte, error) {
		if strings.Contains(a, "get secret harbor-admin-password") {
			return []byte("cGFzcw=="), nil // "pass"
		}
		return nil, errors.New("unexpected: " + a)
	}

	// 201 and 409 both succeed without flagging errors.
	for _, status := range []int{201, 409} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v2.0/projects" {
				t.Errorf("posted to %s", r.URL.Path)
			}
			w.WriteHeader(status)
		}))
		t.Setenv("HARBOR_API_URL", srv.URL)
		withKubectl(t, adminSecret)
		env := withGHAEnvFile(t)
		if err := runCIHarborEnsureProject(); err != nil {
			t.Errorf("status %d: %v", status, err)
		}
		if ghaEnvContains(t, env, "BOOTSTRAP_ERRORS") {
			t.Errorf("status %d must not flag BOOTSTRAP_ERRORS", status)
		}
		srv.Close()
	}

	// Unexpected status defers the failure: BOOTSTRAP_ERRORS=true, exit 0.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	t.Setenv("HARBOR_API_URL", srv.URL)
	withKubectl(t, adminSecret)
	env := withGHAEnvFile(t)
	if err := runCIHarborEnsureProject(); err != nil {
		t.Errorf("unexpected status must defer, not fail: %v", err)
	}
	if !ghaEnvContains(t, env, "BOOTSTRAP_ERRORS=true") {
		t.Error("HTTP 500 must set BOOTSTRAP_ERRORS=true")
	}

	// Harbor unreachable → warning skip, no error flag.
	t.Setenv("HARBOR_API_URL", "http://127.0.0.1:1")
	withKubectl(t, adminSecret)
	env = withGHAEnvFile(t)
	if err := runCIHarborEnsureProject(); err != nil {
		t.Errorf("unreachable Harbor must skip: %v", err)
	}
	if ghaEnvContains(t, env, "BOOTSTRAP_ERRORS") {
		t.Error("unreachable Harbor must not flag BOOTSTRAP_ERRORS")
	}

	// No port-forward URL / no admin Secret → summary-note skips.
	t.Setenv("HARBOR_API_URL", "")
	if err := runCIHarborEnsureProject(); err != nil {
		t.Errorf("missing HARBOR_API_URL must skip: %v", err)
	}
	t.Setenv("HARBOR_API_URL", "http://localhost:1")
	withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("NotFound") })
	if err := runCIHarborEnsureProject(); err != nil {
		t.Errorf("missing admin Secret must skip: %v", err)
	}
}

// stubHarborRobotKV serves secret/harbor/robot reads through the bao seam.
func stubHarborRobotKV(t *testing.T, username, password string) {
	t.Helper()
	prev := baoExecFn
	baoExecFn = func(_, _, _ string, args ...string) (string, string, error) {
		field := ""
		for _, a := range args {
			if strings.HasPrefix(a, "-field=") {
				field = strings.TrimPrefix(a, "-field=")
			}
		}
		v := map[string]string{"username": username, "password": password}[field]
		if v == "" {
			return "", "no value", errors.New("exit 2")
		}
		return v + "\n", "", nil
	}
	t.Cleanup(func() { baoExecFn = prev })
}

func TestHarborSmoke(t *testing.T) {
	// 200 with the robot's basic auth → pass.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if user != "robot$ci" || pass != "s3cret" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	stubHarborRobotKV(t, "robot$ci", "s3cret")
	t.Setenv("HARBOR_API_URL", srv.URL)
	env := withGHAEnvFile(t)
	if err := runCIHarborSmoke(); err != nil {
		t.Errorf("authenticated smoke: %v", err)
	}
	if ghaEnvContains(t, env, "BOOTSTRAP_ERRORS") {
		t.Error("passing smoke must not flag BOOTSTRAP_ERRORS")
	}

	// 401 → deferred failure (stale credentials would break image pulls).
	stubHarborRobotKV(t, "robot$ci", "wrong")
	env = withGHAEnvFile(t)
	if err := runCIHarborSmoke(); err != nil {
		t.Errorf("401 must defer, not fail: %v", err)
	}
	if !ghaEnvContains(t, env, "BOOTSTRAP_ERRORS=true") {
		t.Error("401 must set BOOTSTRAP_ERRORS=true")
	}

	// Unseeded secret / missing URL / unreachable Harbor all skip cleanly.
	stubHarborRobotKV(t, "", "")
	if err := runCIHarborSmoke(); err != nil {
		t.Errorf("unseeded robot must skip: %v", err)
	}
	stubHarborRobotKV(t, "robot$ci", "s3cret")
	t.Setenv("HARBOR_API_URL", "")
	if err := runCIHarborSmoke(); err != nil {
		t.Errorf("missing HARBOR_API_URL must skip: %v", err)
	}
	t.Setenv("HARBOR_API_URL", "http://127.0.0.1:1")
	env = withGHAEnvFile(t)
	if err := runCIHarborSmoke(); err != nil {
		t.Errorf("unreachable Harbor must skip: %v", err)
	}
	if ghaEnvContains(t, env, "BOOTSTRAP_ERRORS") {
		t.Error("unreachable Harbor must not flag BOOTSTRAP_ERRORS")
	}
}

func TestHarborPortForwardSkipsWithoutService(t *testing.T) {
	withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("NotFound") })
	sum := filepath.Join(t.TempDir(), "sum")
	t.Setenv("GITHUB_STEP_SUMMARY", sum)
	if err := runCIHarborPortForward(18080, 1); err != nil {
		t.Fatalf("absent harbor-core must skip: %v", err)
	}
	b, _ := os.ReadFile(sum)
	if !strings.Contains(string(b), "harbor-core absent") {
		t.Errorf("summary missing the skip note:\n%s", b)
	}
}

func TestHarborPortForwardReadyAndTimeout(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.Contains(a, "get svc harbor-core") {
			return nil, nil
		}
		return nil, errors.New("unexpected: " + a)
	})
	// Stub the spawn; serve /health on the "forwarded" port via httptest.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2.0/health" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	var port int
	if _, err := fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port); err != nil {
		t.Skipf("could not parse httptest port from %s", srv.URL)
	}
	prev := startHarborPortForward
	startHarborPortForward = func(int) (int, error) { return 4242, nil }
	t.Cleanup(func() { startHarborPortForward = prev })
	withHarborPFFiles(t)

	env := withGHAEnvFile(t)
	if err := runCIHarborPortForward(port, 2); err != nil {
		t.Fatalf("ready port-forward: %v", err)
	}
	if !ghaEnvContains(t, env, fmt.Sprintf("HARBOR_API_URL=http://localhost:%d", port)) {
		t.Error("HARBOR_API_URL must be exported for the API steps")
	}
	if b, _ := os.ReadFile(harborPFPid); strings.TrimSpace(string(b)) != "4242" {
		t.Errorf("pid file = %q, want 4242", b)
	}

	// Listener never comes up → error after the bounded wait.
	if err := runCIHarborPortForward(1, 0); err == nil {
		t.Error("unreachable listener must fail")
	}
}
