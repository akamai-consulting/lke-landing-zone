package baolifecycle

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The recovery shares are generated exactly once, so a half-failed init has to
// be diagnosable from the message alone: it reports WHICH half arrived. Getting
// the root-token half backwards sends the operator hunting the wrong failure.
func TestParseBaoInitErrorReportsWhichHalfArrived(t *testing.T) {
	_, err := ParseInit(`{"recovery_keys_b64":["a","b"],"root_token":"s.x"}`)
	if err == nil {
		t.Fatal("two shares is an incomplete payload")
	}
	if !strings.Contains(err.Error(), "root=true") || !strings.Contains(err.Error(), "2 recovery shares") {
		t.Errorf("message should report root=true with 2 shares, got %v", err)
	}

	_, err = ParseInit(`{"recovery_keys_b64":["a","b","c","d","e"]}`)
	if err == nil {
		t.Fatal("a missing root token is an incomplete payload")
	}
	if !strings.Contains(err.Error(), "root=false") {
		t.Errorf("message should report root=false, got %v", err)
	}
}

// A regenerated root token that never reached infra-<region> is NOT a
// successful regeneration: the quorum is spent, the old token is dead, and the
// only copy of the new one is in this job's memory. Reporting success there
// leaves the next run with no way in, so the write failure must surface.
func TestBaoRegenRootFailsWhenTheGitHubSecretWriteFails(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.revoked")
	t.Setenv("RECOVERY_K1", "k1")
	t.Setenv("RECOVERY_K2", "k2")
	t.Setenv("RECOVERY_K3", "k3")
	t.Setenv("GITHUB_ENV", filepath.Join(t.TempDir(), "env"))

	keysSubmitted := 0
	withBaoExec(t, func(_, _, stdin string, args ...string) (string, string, error) {
		cmd := strings.Join(args, " ")
		switch {
		case args[0] == "status":
			return `{"initialized":true,"sealed":false}`, "", nil
		case args[0] == "token": // the stored token is revoked → regenerate
			return "", "Code: 403. * permission denied", errors.New("exit status 2")
		case strings.Contains(cmd, "-cancel"):
			return "", "", nil
		case strings.Contains(cmd, "-init"):
			return `{"nonce":"n-1","otp":"otp-1"}`, "", nil
		case strings.Contains(cmd, "-nonce=n-1"):
			keysSubmitted++
			if keysSubmitted == 3 {
				return fmt.Sprintf(`{"complete":true,"progress":3,"required":3,"encoded_token":"enc-%s"}`, strings.TrimSpace(stdin)), "", nil
			}
			return fmt.Sprintf(`{"complete":false,"progress":%d,"required":3}`, keysSubmitted), "", nil
		case strings.Contains(cmd, "-decode=enc-k3"):
			return `{"token":"s.newroot"}`, "", nil
		}
		return "", "", errors.New("unexpected exec: " + cmd)
	})
	calls := withGHSetSecret(t, func(string) error { return errors.New("403: PAT lacks Environments admin") })

	var err error
	out := captureStdout(t, func() { err = RunRegenRootCI(false, "secondary") })
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want the GitHub-secret write failure surfaced", err)
	}
	if len(*calls) != 1 {
		t.Errorf("gh calls = %v, want the one attempted write", *calls)
	}
	if strings.Contains(out, "New root token written") {
		t.Errorf("must not claim the token was stored when the write failed:\n%s", out)
	}
}
