package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// propagateEnv sets the step's full env contract and a summary capture file.
func propagateEnv(t *testing.T, token, hash string) string {
	t.Helper()
	sum := filepath.Join(t.TempDir(), "sum")
	t.Setenv("GITHUB_STEP_SUMMARY", sum)
	t.Setenv("REGION", "primary")
	t.Setenv("OPENBAO_PROPAGATOR_ROLE_ID", "role-id")
	t.Setenv("OPENBAO_PROPAGATOR_SECRET_ID", "secret-id")
	t.Setenv("NEW_TOKEN", token)
	t.Setenv("NEW_PAT_ID", "12345")
	t.Setenv("NEW_TOKEN_HASH", hash)
	return sum
}

// stubPropagateBao answers the AppRole login and records the kv put.
func stubPropagateBao(t *testing.T, loginToken string) *[][]string {
	t.Helper()
	var calls [][]string
	prev := baoExecFn
	baoExecFn = func(pod, token, stdin string, args ...string) (string, string, error) {
		calls = append(calls, append([]string{"pod=" + pod, "token=" + token, "stdin=" + stdin}, args...))
		if len(args) > 1 && args[0] == "write" {
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
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.Contains(a, "get pod "+rootOpenbaoPod) {
			return nil, nil
		}
		return nil, errors.New("unexpected: " + a)
	})
	calls := stubPropagateBao(t, "approle-token")

	if err := runCIPropagatePAT(); err != nil {
		t.Fatalf("propagate-pat: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("bao exec calls = %d, want login + kv put", len(*calls))
	}
	put := (*calls)[1]
	if put[1] != "token=approle-token" {
		t.Errorf("kv put must use the AppRole token, got %s", put[1])
	}
	if put[2] != `stdin={"token":"linode-pat-value"}` {
		t.Errorf("token must ride stdin as JSON, got %s", put[2])
	}
	for _, arg := range put[3:] {
		if strings.Contains(arg, token) {
			t.Errorf("raw token leaked onto bao argv: %v", put[3:])
		}
	}
	summary, _ := os.ReadFile(sum)
	if !strings.Contains(string(summary), "new_pat_id=`12345`") {
		t.Errorf("summary missing the audit line:\n%s", summary)
	}
}

func TestPropagatePATHashMismatchAborts(t *testing.T) {
	propagateEnv(t, "stale-token", fmt.Sprintf("%x", sha256.Sum256([]byte("fresh-token"))))
	calls := stubPropagateBao(t, "approle-token")
	if err := runCIPropagatePAT(); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("stale token must abort before any write, got err=%v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("no bao exec may run on a hash mismatch, got %v", *calls)
	}
}

func TestPropagatePATSkipsAndFailures(t *testing.T) {
	// Missing AppRole creds → warn-skip (exit 0), nothing executed.
	sum := propagateEnv(t, "tok", "")
	t.Setenv("OPENBAO_PROPAGATOR_ROLE_ID", "")
	calls := stubPropagateBao(t, "x")
	if err := runCIPropagatePAT(); err != nil {
		t.Errorf("missing AppRole creds must skip cleanly: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("skip must not exec bao, got %v", *calls)
	}
	if s, _ := os.ReadFile(sum); !strings.Contains(string(s), "OPENBAO_PROPAGATOR_*") {
		t.Errorf("skip must note the missing AppRole seed:\n%s", s)
	}

	// Empty NEW_TOKEN → hard error.
	propagateEnv(t, "", "")
	if err := runCIPropagatePAT(); err == nil {
		t.Error("empty NEW_TOKEN must fail")
	}

	// OpenBao pod absent → warn-skip.
	propagateEnv(t, "tok", "")
	withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("NotFound") })
	calls = stubPropagateBao(t, "x")
	if err := runCIPropagatePAT(); err != nil {
		t.Errorf("absent pod must skip cleanly: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("skip must not exec bao, got %v", *calls)
	}

	// AppRole login failure → hard error, no kv put.
	propagateEnv(t, "tok", "")
	withKubectl(t, func(string) ([]byte, error) { return nil, nil })
	calls = stubPropagateBao(t, "")
	if err := runCIPropagatePAT(); err == nil || !strings.Contains(err.Error(), "AppRole login failed") {
		t.Errorf("login failure must error, got %v", err)
	}
	if len(*calls) != 1 {
		t.Errorf("kv put must not run after a failed login, got %v", *calls)
	}
}
