package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oidcServer points the GitHub Actions OIDC request env at a fake endpoint that
// returns a fixed token, asserting the audience + bearer are sent.
func oidcServer(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("audience") == "" {
			http.Error(w, "missing audience", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer req-tok" {
			http.Error(w, "bad bearer", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"value":"oidc-jwt"}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "req-tok")
}

// propagateEnv sets the step's env contract and a summary capture file.
func propagateEnv(t *testing.T, token, hash string) string {
	t.Helper()
	sum := filepath.Join(t.TempDir(), "sum")
	t.Setenv("GITHUB_STEP_SUMMARY", sum)
	t.Setenv("REGION", "primary")
	t.Setenv("GITHUB_REPOSITORY", "acme/platform")
	t.Setenv("NEW_TOKEN", token)
	t.Setenv("NEW_PAT_ID", "12345")
	t.Setenv("NEW_TOKEN_HASH", hash)
	return sum
}

// stubPropagateBao answers the jwt login and records the kv put.
func stubPropagateBao(t *testing.T, loginToken string) *[][]string {
	t.Helper()
	var calls [][]string
	prev := baoExecFn
	baoExecFn = func(pod, token, stdin string, args ...string) (string, string, error) {
		calls = append(calls, append([]string{"pod=" + pod, "token=" + token, "stdin=" + stdin}, args...))
		if len(args) > 1 && args[0] == "write" { // the auth/jwt/login call
			if loginToken == "" {
				return "", "permission denied", errors.New("exit 2")
			}
			return fmt.Sprintf(`{"auth":{"client_token":%q}}`, loginToken), "", nil
		}
		return "", "", nil
	}
	t.Cleanup(func() { baoExecFn = prev })
	return &calls
}

func TestPropagatePATHappyPath(t *testing.T) {
	token := "linode-pat-value"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	sum := propagateEnv(t, token, hash)
	oidcServer(t)
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.Contains(a, "get pod "+rootOpenbaoPod) {
			return nil, nil
		}
		return nil, errors.New("unexpected: " + a)
	})
	calls := stubPropagateBao(t, "propagator-token")

	if err := runCIPropagatePAT(); err != nil {
		t.Fatalf("propagate-pat: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("bao exec calls = %d, want jwt login + kv put: %v", len(*calls), *calls)
	}
	// The login must be a repo-bound jwt login with the minted OIDC token.
	login := (*calls)[0]
	joined := strings.Join(login, " ")
	if !strings.Contains(joined, "auth/jwt/login") || !strings.Contains(joined, "role=secret-propagator") || !strings.Contains(joined, "jwt=oidc-jwt") {
		t.Errorf("login is not the expected jwt login: %v", login)
	}
	put := (*calls)[1]
	if put[1] != "token=propagator-token" {
		t.Errorf("kv put must use the jwt-issued token, got %s", put[1])
	}
	if put[2] != `stdin={"token":"linode-pat-value"}` {
		t.Errorf("token must ride stdin as JSON, got %s", put[2])
	}
	for _, arg := range put[3:] {
		if strings.Contains(arg, token) {
			t.Errorf("raw token leaked onto bao argv: %v", put[3:])
		}
	}
	if summary, _ := os.ReadFile(sum); !strings.Contains(string(summary), "new_pat_id=`12345`") {
		t.Errorf("summary missing the audit line:\n%s", summary)
	}
}

func TestPropagatePATHashMismatchAborts(t *testing.T) {
	propagateEnv(t, "stale-token", fmt.Sprintf("%x", sha256.Sum256([]byte("fresh-token"))))
	calls := stubPropagateBao(t, "propagator-token")
	if err := runCIPropagatePAT(); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("stale token must abort before any write, got err=%v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("no bao exec may run on a hash mismatch, got %v", *calls)
	}
}

func TestPropagatePATSkipsAndFailures(t *testing.T) {
	// Empty NEW_TOKEN → hard error.
	t.Run("empty token", func(t *testing.T) {
		propagateEnv(t, "", "")
		if err := runCIPropagatePAT(); err == nil {
			t.Error("empty NEW_TOKEN must fail")
		}
	})

	// OpenBao pod absent → warn-skip (exit 0), nothing executed.
	t.Run("absent pod", func(t *testing.T) {
		propagateEnv(t, "tok", "")
		oidcServer(t)
		withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("NotFound") })
		calls := stubPropagateBao(t, "x")
		if err := runCIPropagatePAT(); err != nil {
			t.Errorf("absent pod must skip cleanly: %v", err)
		}
		if len(*calls) != 0 {
			t.Errorf("skip must not exec bao, got %v", *calls)
		}
	})

	// No id-token permission (OIDC request env unset) → hard error before any bao call.
	t.Run("no oidc permission", func(t *testing.T) {
		propagateEnv(t, "tok", "")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		withKubectl(t, func(string) ([]byte, error) { return nil, nil })
		calls := stubPropagateBao(t, "x")
		if err := runCIPropagatePAT(); err == nil || !strings.Contains(err.Error(), "OIDC") {
			t.Errorf("missing id-token permission must error, got %v", err)
		}
		if len(*calls) != 0 {
			t.Errorf("no bao exec before a token is minted, got %v", *calls)
		}
	})

	// jwt login failure → hard error, no kv put.
	t.Run("login failure", func(t *testing.T) {
		propagateEnv(t, "tok", "")
		oidcServer(t)
		withKubectl(t, func(string) ([]byte, error) { return nil, nil })
		calls := stubPropagateBao(t, "")
		if err := runCIPropagatePAT(); err == nil || !strings.Contains(err.Error(), "jwt login failed") {
			t.Errorf("login failure must error, got %v", err)
		}
		if len(*calls) != 1 {
			t.Errorf("kv put must not run after a failed login, got %v", *calls)
		}
	})
}

func TestGithubActionsOIDCToken(t *testing.T) {
	t.Run("mints with audience + bearer", func(t *testing.T) {
		oidcServer(t)
		got, err := githubActionsOIDCToken("https://github.com/acme", nil)
		if err != nil || got != "oidc-jwt" {
			t.Fatalf("got %q, err=%v; want oidc-jwt", got, err)
		}
	})
	t.Run("errors without request env", func(t *testing.T) {
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		if _, err := githubActionsOIDCToken("aud", nil); err == nil {
			t.Error("missing request env must error")
		}
	})
}

func TestOIDCAudienceForRepo(t *testing.T) {
	if got := oidcAudienceForRepo("acme/platform"); got != "https://github.com/acme" {
		t.Errorf("got %q", got)
	}
	if got := oidcAudienceForRepo("noslash"); got != "https://github.com/noslash" {
		t.Errorf("got %q", got)
	}
}
