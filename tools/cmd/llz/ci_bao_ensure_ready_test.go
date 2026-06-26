package main

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
// AND so the os.Setenv writes runCIBaoInit makes mid-test are restored on
// cleanup (no leak into sibling tests).
func clearBaoEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{"UNSEAL_K1", "UNSEAL_K2", "UNSEAL_K3", "OPENBAO_ROOT_TOKEN", "GH_TOKEN", "HA_ROLE"} {
		t.Setenv(v, "")
	}
}

// TestRunCIBaoEnsureReadyFirstInit drives the uninitialized path end to end:
// init mints the keys+token, pod-0 then the followers unseal, and the gate
// reports available=true with the fresh root token re-exported.
func TestRunCIBaoEnsureReadyFirstInit(t *testing.T) {
	clearBaoEnv(t)
	t.Setenv("GH_TOKEN", "ghp_write")
	readOutput, readEnv := ghaFiles(t)
	withBaoSleep(t)
	secrets := withGHSetSecret(t, nil)

	inited, pod0Unsealed := false, false
	withBaoExec(t, func(pod, _, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "status":
			if pod == "platform-openbao-0" {
				return statusJSON(inited, !pod0Unsealed), "", nil
			}
			// Followers auto-join via retry_join once the leader is up: initialized
			// follows `inited`, still sealed until their own unseal.
			return statusJSON(inited, true), "", nil
		case strings.HasPrefix(joined, "operator init"):
			inited = true
			return `{"root_token":"s.newroot","unseal_keys_b64":["k1","k2","k3","k4","k5"]}`, "", nil
		case strings.HasPrefix(joined, "operator unseal"):
			if pod == "platform-openbao-0" {
				pod0Unsealed = true
			}
			return "", "", nil
		}
		return "", "unexpected " + joined, fmt.Errorf("unexpected")
	})

	if err := runCIBaoEnsureReady(globalOpts{}, "primary", 30*time.Second, 30*time.Second); err != nil {
		t.Fatalf("runCIBaoEnsureReady (first init): %v", err)
	}
	if got := readOutput(); !strings.Contains(got, "available=true") {
		t.Errorf("GITHUB_OUTPUT = %q, want available=true", got)
	}
	if got := readEnv(); !strings.Contains(got, "OPENBAO_ROOT_TOKEN=s.newroot") {
		t.Errorf("GITHUB_ENV = %q, want the fresh root token re-exported", got)
	}
	joined := strings.Join(*secrets, " ")
	if !strings.Contains(joined, "OPENBAO_UNSEAL_KEY_1") || !strings.Contains(joined, "OPENBAO_ROOT_TOKEN") {
		t.Errorf("persisted secrets = %v, want unseal keys + root token", *secrets)
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
	err := runCIBaoEnsureReady(globalOpts{}, "primary", time.Second, time.Second)
	if err == nil || !strings.Contains(err.Error(), "GH_TOKEN") {
		t.Errorf("err = %v, want a GH_TOKEN-required error on uninitialized cluster", err)
	}
}

// TestRunCIBaoEnsureReadyReUnseal: initialized + sealed, no root token →
// re-unseal every pod (Branch B), available=false (configure/seed skipped).
func TestRunCIBaoEnsureReadyReUnseal(t *testing.T) {
	clearBaoEnv(t)
	t.Setenv("UNSEAL_K1", "k1")
	t.Setenv("UNSEAL_K2", "k2")
	t.Setenv("UNSEAL_K3", "k3")
	readOutput, _ := ghaFiles(t)
	unseals := 0
	withBaoExec(t, func(_, _, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "status":
			return statusJSON(true, true), "", nil
		case strings.HasPrefix(joined, "operator unseal"):
			unseals++
			return "", "", nil
		}
		return "", "unexpected " + joined, fmt.Errorf("unexpected")
	})
	if err := runCIBaoEnsureReady(globalOpts{}, "primary", time.Second, time.Second); err != nil {
		t.Fatalf("runCIBaoEnsureReady (re-unseal): %v", err)
	}
	if unseals != 9 {
		t.Errorf("unseal submissions = %d, want 9 (3 keys × 3 pods)", unseals)
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
	t.Setenv("UNSEAL_K1", "k1")
	t.Setenv("UNSEAL_K2", "k2")
	t.Setenv("UNSEAL_K3", "k3")
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
	if err := runCIBaoEnsureReady(globalOpts{}, "primary", time.Second, time.Second); err != nil {
		t.Fatalf("runCIBaoEnsureReady (reconfigure): %v", err)
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

func TestRunCIBaoEnsureReadyDryRunAndWiring(t *testing.T) {
	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		t.Error("dry-run must not exec")
		return "", "", nil
	})
	if err := runCIBaoEnsureReady(globalOpts{dryRun: true}, "primary", time.Second, time.Second); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if err := runCIBaoEnsureReady(globalOpts{}, "", time.Second, time.Second); err == nil || !strings.Contains(err.Error(), "--region") {
		t.Errorf("missing region = %v, want --region error", err)
	}
	if c := ciBaoEnsureReadyCmd(); c.Use != "bao-ensure-ready" {
		t.Errorf("Use = %q, want bao-ensure-ready", c.Use)
	}
}
