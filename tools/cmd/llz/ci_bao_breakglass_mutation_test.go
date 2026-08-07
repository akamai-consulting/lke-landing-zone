package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghsecret"
)

// The revoke step's two arms report OPPOSITE facts to the operator ("the token
// is dead" vs "it may still be live"), so which one runs has to follow the exec
// result rather than being interchangeable.
func TestBreakglassRevokeCurrentReportsRevokeOutcome(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.live")

	withBaoExec(t, func(pod, token, _ string, args ...string) (string, string, error) {
		if got := strings.Join(args, " "); got != "token revoke -self" {
			t.Errorf("exec = %q, want `token revoke -self`", got)
		}
		if token != "s.live" {
			t.Errorf("revoke must present the stored token, got %q", token)
		}
		return "", "", nil
	})
	out := captureStdout(t, func() { breakglassRevokeCurrent("primary") })
	if !strings.Contains(out, "Current root token revoked.") {
		t.Errorf("a successful revoke must be reported as revoked, got %q", out)
	}
	if strings.Contains(out, "::warning::") {
		t.Errorf("a successful revoke must not warn, got %q", out)
	}

	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		return "", "permission denied", errors.New("exit status 2")
	})
	out = captureStdout(t, func() { breakglassRevokeCurrent("primary") })
	if !strings.Contains(out, "::warning::token revoke -self failed") {
		t.Errorf("a failed revoke must warn, got %q", out)
	}
	if strings.Contains(out, "Current root token revoked.") {
		t.Errorf("a failed revoke must not claim the token was revoked, got %q", out)
	}
}

// The job summary is the audit record: it must not say the secret was deleted
// when the delete warned, and must not warn when it succeeded.
func TestBreakglassDeleteStoredReportsTheDeleteHonestly(t *testing.T) {
	run := func(t *testing.T, delErr error) (stdout, summary string) {
		t.Helper()
		sum := filepath.Join(t.TempDir(), "summary")
		t.Setenv("GITHUB_STEP_SUMMARY", sum)
		t.Setenv("GITHUB_ACTOR", "octocat")
		orig := ghsecret.DeleteFn
		ghsecret.DeleteFn = func(name, ghEnv string) error {
			if name != "OPENBAO_ROOT_TOKEN" || ghEnv != "infra-primary" {
				t.Errorf("delete called with (%q, %q)", name, ghEnv)
			}
			return delErr
		}
		t.Cleanup(func() { ghsecret.DeleteFn = orig })
		out := captureStdout(t, func() {
			if err := breakglassDeleteStored("primary"); err != nil {
				t.Fatalf("breakglassDeleteStored: %v", err)
			}
		})
		b, _ := os.ReadFile(sum)
		return out, string(b)
	}

	t.Run("delete succeeds", func(t *testing.T) {
		out, summary := run(t, nil)
		if !strings.Contains(out, "Deleted infra-primary::OPENBAO_ROOT_TOKEN.") {
			t.Errorf("stdout %q missing the deleted note", out)
		}
		if strings.Contains(out, "::warning::") {
			t.Errorf("a successful delete must not warn, got %q", out)
		}
		if !strings.Contains(summary, "revoked and `infra-primary::OPENBAO_ROOT_TOKEN` deleted.") {
			t.Errorf("summary %q should record the delete", summary)
		}
		if strings.Contains(summary, "Could NOT delete") {
			t.Errorf("summary must not claim a failure that did not happen: %q", summary)
		}
	})

	t.Run("delete warns", func(t *testing.T) {
		out, summary := run(t, errors.New("404 Not Found"))
		if !strings.Contains(out, "::warning::Could not delete") {
			t.Errorf("a failed delete must warn, got %q", out)
		}
		if !strings.Contains(summary, "Could NOT delete") {
			t.Errorf("summary must not claim the secret was deleted: %q", summary)
		}
	})
}

// The actor line is the WHO of the break-glass audit trail; "unknown" is the
// fallback for a missing $GITHUB_ACTOR, never the normal answer.
func TestBreakglassActorLineUsesTheDispatchingActor(t *testing.T) {
	t.Setenv("GITHUB_ACTOR", "octocat")
	if got := breakglassActorLine(); got != "Dispatched by **@octocat**." {
		t.Errorf("actor line = %q, want the real actor", got)
	}
	t.Setenv("GITHUB_ACTOR", "")
	if got := breakglassActorLine(); got != "Dispatched by **@unknown**." {
		t.Errorf("actor line = %q, want the unknown fallback", got)
	}
}

// The rejection message quotes the key's size so an operator can see how far
// short it fell — rsa.PublicKey.Size() is BYTES, so it must be scaled up to
// bits, not down.
func TestParseRecipientRSAPubKeyNamesTheActualBitLength(t *testing.T) {
	small, _ := rsaPubB64(t, 1024)
	_, err := parseRecipientRSAPubKey(small)
	if err == nil {
		t.Fatal("a 1024-bit key must be rejected")
	}
	if !strings.Contains(err.Error(), "1024-bit") {
		t.Errorf("error should report the key as 1024-bit, got %v", err)
	}
}
