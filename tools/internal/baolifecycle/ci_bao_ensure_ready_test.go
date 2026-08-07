package baolifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ghaFiles points GITHUB_OUTPUT/ENV/STEP_SUMMARY at temp files and returns a
// reader for each. ensure-ready writes the availability output + re-exports the
// root token through these, exactly as the inline steps did.
func ghaFiles(t *testing.T) (readOutput, readEnv func() string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "output")
	env := filepath.Join(t.TempDir(), "env")
	sum := filepath.Join(t.TempDir(), "summary")
	t.Setenv("GITHUB_OUTPUT", out)
	t.Setenv("GITHUB_ENV", env)
	t.Setenv("GITHUB_STEP_SUMMARY", sum)
	read := func(p string) func() string {
		return func() string { b, _ := os.ReadFile(p); return string(b) }
	}
	return read(out), read(env)
}

func statusJSON(initialized, sealed bool) string {
	return fmt.Sprintf(`{"initialized":%t,"sealed":%t}`, initialized, sealed)
}

// clearBaoEnv zeroes the key/token env vars via t.Setenv so a test starts clean
// AND so the os.Setenv writes RunInit makes mid-test are restored on
// cleanup (no leak into sibling tests).
func clearBaoEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{"RECOVERY_K1", "RECOVERY_K2", "RECOVERY_K3", "OPENBAO_ROOT_TOKEN", "GH_TOKEN", "HA_ROLE"} {
		t.Setenv(v, "")
	}
}

// TestRunCIBaoEnsureReadyFirstInit drives the uninitialized path end to end:
// init mints the recovery keys+token, every pod then auto-unseals from the
// static seal key, and the gate reports available=true with the fresh root
// token re-exported.
func TestRunCIBaoEnsureReadyFirstInit(t *testing.T) {
	clearBaoEnv(t)
	t.Setenv("GH_TOKEN", "ghp_write")
	readOutput, readEnv := ghaFiles(t)
	withBaoSleep(t)
	secrets := withGHSetSecret(t, nil)

	inited := false
	withBaoExec(t, func(pod, _, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "status":
			// Before init: uninitialized+sealed. After init each pod auto-unseals
			// from the static seal key (no key-submission step).
			return statusJSON(inited, !inited), "", nil
		case strings.HasPrefix(joined, "operator init"):
			inited = true
			return `{"root_token":"s.newroot","recovery_keys_b64":["k1","k2","k3","k4","k5"]}`, "", nil
		}
		return "", "unexpected " + joined, fmt.Errorf("unexpected")
	})

	if err := RunEnsureReady(false, "primary", 30*time.Second, 30*time.Second); err != nil {
		t.Fatalf("RunEnsureReady (first init): %v", err)
	}
	if got := readOutput(); !strings.Contains(got, "available=true") {
		t.Errorf("GITHUB_OUTPUT = %q, want available=true", got)
	}
	if got := readEnv(); !strings.Contains(got, "OPENBAO_ROOT_TOKEN=s.newroot") {
		t.Errorf("GITHUB_ENV = %q, want the fresh root token re-exported", got)
	}
	joined := strings.Join(*secrets, " ")
	if !strings.Contains(joined, "OPENBAO_RECOVERY_KEY_1") || !strings.Contains(joined, "OPENBAO_ROOT_TOKEN") {
		t.Errorf("persisted secrets = %v, want recovery keys + root token", *secrets)
	}
}

// TestRunCIBaoEnsureReadyFirstInitNeedsGHToken fails fast (friendly) when an
// uninitialized cluster has no secrets-write PAT to persist the keys.
func TestRunCIBaoEnsureReadyFirstInitNeedsGHToken(t *testing.T) {
	clearBaoEnv(t) // GH_TOKEN cleared
	ghaFiles(t)
	withBaoExec(t, func(_, _, _ string, args ...string) (string, string, error) {
		return statusJSON(false, true), "", nil // uninitialized
	})
	err := RunEnsureReady(false, "primary", time.Second, time.Second)
	if err == nil || !strings.Contains(err.Error(), "GH_TOKEN") {
		t.Errorf("err = %v, want a GH_TOKEN-required error on uninitialized cluster", err)
	}
}

// TestRunCIBaoEnsureReadyReseal: initialized + sealed after a restart, no root
// token → wait for the pods to self-unseal from the static seal key (Branch B),
// available=false (configure/seed skipped). No keys are submitted.
func TestRunCIBaoEnsureReadyReseal(t *testing.T) {
	clearBaoEnv(t)
	readOutput, _ := ghaFiles(t)
	withBaoSleep(t)
	probes := 0
	withBaoExec(t, func(_, _, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "status":
			// The first sweep (aggregate probe over 3 pods) still sees the pods
			// sealed; by the time baoread.WaitForAutoUnseal polls they've self-unsealed.
			probes++
			return statusJSON(true, probes <= 3), "", nil
		}
		return "", "unexpected " + joined, fmt.Errorf("unexpected")
	})
	if err := RunEnsureReady(false, "primary", 30*time.Second, 30*time.Second); err != nil {
		t.Fatalf("RunEnsureReady (reseal): %v", err)
	}
	if got := readOutput(); !strings.Contains(got, "available=false") {
		t.Errorf("GITHUB_OUTPUT = %q, want available=false (no root token)", got)
	}
}

// TestRunCIBaoEnsureReadyReconfigureValidToken: initialized + unsealed with a
// valid loaded token → no init, no unseal, regen validates and skips, gate
// reports available=true with the token re-exported.
func TestRunCIBaoEnsureReadyReconfigureValidToken(t *testing.T) {
	clearBaoEnv(t)
	t.Setenv("RECOVERY_K1", "k1")
	t.Setenv("RECOVERY_K2", "k2")
	t.Setenv("RECOVERY_K3", "k3")
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.valid")
	readOutput, readEnv := ghaFiles(t)
	var sawInit, sawUnseal bool
	withBaoExec(t, func(_, token, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "status":
			return statusJSON(true, false), "", nil
		case args[0] == "token" && args[1] == "lookup":
			return `{"data":{"id":"s.valid"}}`, "", nil // valid → no regeneration
		case strings.HasPrefix(joined, "operator init"):
			sawInit = true
			return "", "", nil
		case strings.HasPrefix(joined, "operator unseal"):
			sawUnseal = true
			return "", "", nil
		}
		return "", "unexpected " + joined, fmt.Errorf("unexpected")
	})
	if err := RunEnsureReady(false, "primary", time.Second, time.Second); err != nil {
		t.Fatalf("RunEnsureReady (reconfigure): %v", err)
	}
	if sawInit || sawUnseal {
		t.Errorf("initialized+unsealed must not init (%v) or unseal (%v)", sawInit, sawUnseal)
	}
	if got := readOutput(); !strings.Contains(got, "available=true") {
		t.Errorf("GITHUB_OUTPUT = %q, want available=true", got)
	}
	if got := readEnv(); !strings.Contains(got, "OPENBAO_ROOT_TOKEN=s.valid") {
		t.Errorf("GITHUB_ENV = %q, want the loaded token re-exported", got)
	}
}

func TestRunCIBaoEnsureReadyRegeneratesFromQuorumWithoutARootToken(t *testing.T) {
	clearBaoEnv(t)
	t.Setenv("RECOVERY_K1", "k1")
	t.Setenv("RECOVERY_K2", "k2")
	t.Setenv("RECOVERY_K3", "k3")
	readOutput, _ := ghaFiles(t)
	withBaoSleep(t)

	genRootInit := false
	withBaoExec(t, func(_, _, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "status":
			return statusJSON(true, false), "", nil // initialized + unsealed
		case strings.HasPrefix(joined, "operator generate-root -cancel"):
			return "", "", nil
		case strings.HasPrefix(joined, "operator generate-root -init"):
			genRootInit = true
			return `{"nonce":"n1","otp":"otp1"}`, "", nil
		case strings.HasPrefix(joined, "operator generate-root -nonce="):
			return `{"complete":true,"encoded_token":"enc"}`, "", nil
		case joined == "token lookup":
			t.Error("validated a token that was never set")
			return "", "", fmt.Errorf("unexpected")
		}
		return "", "unexpected " + joined, fmt.Errorf("unexpected")
	})

	// The decode + GitHub write are past the point this test is about; it only has
	// to prove the quorum path is ENTERED, which the old gate made impossible.
	_ = RunEnsureReady(false, "primary", 30*time.Second, 30*time.Second)

	if !genRootInit {
		t.Fatal("no root token + a full recovery quorum must regenerate, not skip to available=false")
	}
	if got := readOutput(); strings.Contains(got, "available=false") {
		t.Errorf("GITHUB_OUTPUT = %q — reported unavailable despite a usable quorum", got)
	}
}

// Without the quorum there is nothing to regenerate FROM, so the old
// skip-and-report behaviour must survive: this is the one case where
// available=false is the honest answer, and turning it into a hard failure would
// break a re-run that only wanted the cluster applied.
func TestRunCIBaoEnsureReadyStillSkipsWithNeitherTokenNorQuorum(t *testing.T) {
	clearBaoEnv(t)
	readOutput, _ := ghaFiles(t)
	withBaoSleep(t)
	withBaoExec(t, func(_, _, _ string, args ...string) (string, string, error) {
		if args[0] == "status" {
			return statusJSON(true, false), "", nil
		}
		return "", "unexpected " + strings.Join(args, " "), fmt.Errorf("unexpected")
	})
	if err := RunEnsureReady(false, "primary", 30*time.Second, 30*time.Second); err != nil {
		t.Fatalf("no token and no quorum must skip, not fail: %v", err)
	}
	if got := readOutput(); !strings.Contains(got, "available=false") {
		t.Errorf("GITHUB_OUTPUT = %q, want available=false", got)
	}
}
