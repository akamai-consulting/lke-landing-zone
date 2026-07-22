package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rsaPubB64 returns the base64(PEM PKIX public key) an operator would paste, plus
// the private key for round-trip decryption in tests.
func rsaPubB64(t *testing.T, bits int) (string, *rsa.PrivateKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return base64.StdEncoding.EncodeToString(pemBytes), priv
}

func TestParseRecipientRSAPubKey(t *testing.T) {
	good, _ := rsaPubB64(t, 2048)
	if _, err := parseRecipientRSAPubKey(good); err != nil {
		t.Fatalf("valid 2048-bit key rejected: %v", err)
	}
	// Whitespace/newlines are tolerated (operators sometimes leave a trailing \n).
	if _, err := parseRecipientRSAPubKey("  " + good + "\n"); err != nil {
		t.Errorf("surrounding whitespace should be tolerated: %v", err)
	}

	// A pasted PRIVATE key is the classic footgun — reject it by name.
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	if _, err := parseRecipientRSAPubKey(base64.StdEncoding.EncodeToString(privPEM)); err == nil || !strings.Contains(err.Error(), "PRIVATE key") {
		t.Errorf("private key = %v, want PRIVATE-key rejection", err)
	}

	// Too-small RSA key.
	small, _ := rsaPubB64(t, 1024)
	if _, err := parseRecipientRSAPubKey(small); err == nil || !strings.Contains(err.Error(), "2048") {
		t.Errorf("1024-bit key = %v, want >= 2048-bit rejection", err)
	}

	// Non-RSA (EC) key.
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKIXPublicKey(&ec.PublicKey)
	ecPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: ecDER})
	if _, err := parseRecipientRSAPubKey(base64.StdEncoding.EncodeToString(ecPEM)); err == nil || !strings.Contains(err.Error(), "not RSA") {
		t.Errorf("EC key = %v, want not-RSA rejection", err)
	}

	// Not base64, and base64 of non-PEM garbage.
	if _, err := parseRecipientRSAPubKey("!!!not base64!!!"); err == nil {
		t.Error("invalid base64 should error")
	}
	if _, err := parseRecipientRSAPubKey(base64.StdEncoding.EncodeToString([]byte("hello"))); err == nil {
		t.Error("base64 of non-PEM should error")
	}
}

// The delivered ciphertext must round-trip back to the exact token with the
// operator's private key using the same OAEP/SHA-256 parameters as the documented
// openssl decrypt recipe.
func TestBreakglassEncryptRoundTrip(t *testing.T) {
	b64, priv := rsaPubB64(t, 2048)
	pub, err := parseRecipientRSAPubKey(b64)
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	t.Setenv("RUNNER_TEMP", tmp)
	summary := filepath.Join(tmp, "summary")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)

	const token = "s.breakglass-root"
	if err := breakglassEncryptAndDeliver("primary", "generate", pub, token); err != nil {
		t.Fatal(err)
	}

	// Ciphertext file exists and decrypts back to the token.
	raw, err := os.ReadFile(filepath.Join(tmp, "root-token.b64"))
	if err != nil {
		t.Fatalf("ciphertext file missing: %v", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil {
		t.Fatalf("artifact is not base64: %v", err)
	}
	plain, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, priv, ciphertext, nil)
	if err != nil {
		t.Fatalf("decrypt failed: %v", err)
	}
	if string(plain) != token {
		t.Errorf("round-trip = %q, want %q", plain, token)
	}

	// The summary carries the ciphertext + decrypt recipe, never the plaintext.
	sum, _ := os.ReadFile(summary)
	if !strings.Contains(string(sum), string(raw)) {
		t.Error("summary should embed the ciphertext")
	}
	if strings.Contains(string(sum), token) {
		t.Error("summary must NOT contain the plaintext token")
	}
	if !strings.Contains(string(sum), "rsa_oaep_md:sha256") {
		t.Error("summary should include the openssl decrypt recipe")
	}
}

func TestRunCIBaoBreakglassBadInputs(t *testing.T) {
	if err := runCIBaoBreakglass(globalOpts{}, "", "generate", "x"); err == nil {
		t.Error("empty region should error")
	}
	if err := runCIBaoBreakglass(globalOpts{}, "primary", "bogus", ""); err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("bogus action = %v, want unknown-action error", err)
	}
	// generate/rotate without a recipient key must fail before touching the cluster.
	if err := runCIBaoBreakglass(globalOpts{}, "primary", "generate", ""); err == nil || !strings.Contains(err.Error(), "recipient-pubkey-b64 is required") {
		t.Errorf("generate w/o key = %v, want required-key error", err)
	}
	if err := runCIBaoBreakglass(globalOpts{}, "primary", "rotate", ""); err == nil || !strings.Contains(err.Error(), "recipient-pubkey-b64 is required") {
		t.Errorf("rotate w/o key = %v, want required-key error", err)
	}
}

func TestRunCIBaoBreakglassRevoke(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.live-root")
	summary := filepath.Join(t.TempDir(), "summary")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)

	revoked := false
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		if strings.Join(args, " ") == "token revoke -self" {
			if token != "s.live-root" {
				t.Errorf("revoke used token %q, want the stored value", token)
			}
			revoked = true
			return "", "", nil
		}
		t.Errorf("unexpected exec %v", args)
		return "", "", nil
	})

	var deleted []string
	origDel := ghDeleteSecretFn
	ghDeleteSecretFn = func(name, ghEnv string) error {
		deleted = append(deleted, name+"@"+ghEnv)
		return nil
	}
	t.Cleanup(func() { ghDeleteSecretFn = origDel })

	if err := runCIBaoBreakglass(globalOpts{}, "primary", "revoke", ""); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Error("revoke should have revoked the current token")
	}
	if len(deleted) != 1 || deleted[0] != "OPENBAO_ROOT_TOKEN@infra-primary" {
		t.Errorf("delete calls = %v, want one infra-primary root-token delete", deleted)
	}
	if sum, _ := os.ReadFile(summary); !strings.Contains(string(sum), "revoke (primary)") {
		t.Errorf("summary = %q, want revoke note", sum)
	}
}

// revoke tolerates a missing stored token and a delete failure — it must not
// leave the cleanup half-done nor hard-fail the run.
func TestRunCIBaoBreakglassRevokeNoTokenDeleteFails(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "") // empty = no token, and auto-restored (no leak into later tests)
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "s"))
	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		t.Error("no token means nothing to revoke — exec must not run")
		return "", "", nil
	})
	origDel := ghDeleteSecretFn
	ghDeleteSecretFn = func(string, string) error { return errors.New("404") }
	t.Cleanup(func() { ghDeleteSecretFn = origDel })

	if err := runCIBaoBreakglass(globalOpts{}, "primary", "revoke", ""); err != nil {
		t.Errorf("revoke should be best-effort, got %v", err)
	}
}

// generate with a still-valid stored token: bao-regen-root skips regeneration and
// breakglass delivers the existing (encrypted) token — idempotent, no quorum burn.
func TestRunCIBaoBreakglassGenerateSkipPath(t *testing.T) {
	b64, priv := rsaPubB64(t, 2048)
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.still-valid")
	tmp := t.TempDir()
	t.Setenv("RUNNER_TEMP", tmp)
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(tmp, "summary"))

	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		switch args[0] {
		case "status":
			return `{"initialized":true,"sealed":false}`, "", nil
		case "token": // lookup succeeds → token still valid → skip regen
			return `{"data":{"policies":["root"]}}`, "", nil
		}
		t.Errorf("unexpected exec %v (skip path must not run the quorum)", args)
		return "", "", nil
	})
	// A regeneration would call gh secret set; assert it does NOT here.
	ghCalls := withGHSetSecret(t, nil)

	if err := runCIBaoBreakglass(globalOpts{}, "primary", "generate", b64); err != nil {
		t.Fatal(err)
	}
	if len(*ghCalls) != 0 {
		t.Errorf("skip path must not write gh secrets: %v", *ghCalls)
	}
	raw, err := os.ReadFile(filepath.Join(tmp, "root-token.b64"))
	if err != nil {
		t.Fatalf("ciphertext file missing: %v", err)
	}
	ct, _ := base64.StdEncoding.DecodeString(string(raw))
	plain, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, priv, ct, nil)
	if err != nil || string(plain) != "s.still-valid" {
		t.Errorf("delivered token = (%q, %v), want the still-valid stored token", plain, err)
	}
}

func TestRunCIBaoBreakglassDryRun(t *testing.T) {
	b64, _ := rsaPubB64(t, 2048)
	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		t.Error("dry-run must not touch the cluster")
		return "", "", nil
	})
	if err := runCIBaoBreakglass(globalOpts{dryRun: true}, "primary", "generate", b64); err != nil {
		t.Errorf("dry-run generate = %v, want nil", err)
	}
}

// quorumRegenExec returns a fake baoExecFn that drives the full generate-root
// quorum flow (dead lookup → cancel/init/3 keys → decode) and mints `newRoot`.
// `revoke` handling + call-order are recorded via the closures.
func quorumRegenExec(t *testing.T, newRoot string, onRevoke func(token string), onRegenInit func()) func(string, string, string, ...string) (string, string, error) {
	keys := 0
	return func(pod, token, stdin string, args ...string) (string, string, error) {
		cmd := strings.Join(args, " ")
		switch {
		case cmd == "token revoke -self":
			if onRevoke != nil {
				onRevoke(token)
			}
			return "", "", nil
		case args[0] == "status":
			return `{"initialized":true,"sealed":false}`, "", nil
		case args[0] == "token" && len(args) > 1 && args[1] == "lookup":
			return "", "Code: 403. * permission denied", errors.New("exit status 2") // revoked → regen
		case strings.Contains(cmd, "-cancel"):
			return "", "", nil
		case strings.Contains(cmd, "-init"):
			if onRegenInit != nil {
				onRegenInit()
			}
			return `{"nonce":"n-1","otp":"otp-1"}`, "", nil
		case strings.Contains(cmd, "-nonce=n-1"):
			keys++
			if keys == 3 {
				return fmt.Sprintf(`{"complete":true,"progress":3,"required":3,"encoded_token":"enc-%s"}`, strings.TrimSpace(stdin)), "", nil
			}
			return fmt.Sprintf(`{"complete":false,"progress":%d,"required":3}`, keys), "", nil
		case strings.Contains(cmd, "-decode=enc-k3"):
			return fmt.Sprintf(`{"token":%q}`, newRoot), "", nil
		}
		t.Errorf("unexpected exec %v", args)
		return "", "", errors.New("unexpected")
	}
}

func setBreakglassEnv(t *testing.T, storedToken string) (tmp string) {
	t.Setenv("OPENBAO_ROOT_TOKEN", storedToken)
	t.Setenv("RECOVERY_K1", "k1")
	t.Setenv("RECOVERY_K2", "k2")
	t.Setenv("RECOVERY_K3", "k3")
	tmp = t.TempDir()
	t.Setenv("RUNNER_TEMP", tmp)
	t.Setenv("GITHUB_ENV", filepath.Join(tmp, "env"))
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(tmp, "summary"))
	return tmp
}

func decryptDelivered(t *testing.T, tmp string, priv *rsa.PrivateKey) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(tmp, "root-token.b64"))
	if err != nil {
		t.Fatalf("ciphertext file missing: %v", err)
	}
	ct, _ := base64.StdEncoding.DecodeString(string(raw))
	plain, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, priv, ct, nil)
	if err != nil {
		t.Fatalf("decrypt failed: %v", err)
	}
	return string(plain)
}

// TestRunCIBaoBreakglassGenerateRegenPath: generate with a REVOKED stored token
// runs the quorum and delivers the FRESHLY regenerated token — the os.Getenv
// freshness property (no $GITHUB_ENV shadowing), which the skip-path test can't cover.
func TestRunCIBaoBreakglassGenerateRegenPath(t *testing.T) {
	b64, priv := rsaPubB64(t, 2048)
	tmp := setBreakglassEnv(t, "s.revoked")
	withBaoExec(t, quorumRegenExec(t, "s.newroot", nil, nil))
	withGHSetSecret(t, nil)

	if err := runCIBaoBreakglass(globalOpts{}, "primary", "generate", b64); err != nil {
		t.Fatal(err)
	}
	if got := decryptDelivered(t, tmp, priv); got != "s.newroot" {
		t.Errorf("delivered %q, want the freshly regenerated s.newroot (not the stale s.revoked)", got)
	}
}

// TestRunCIBaoBreakglassRotate: rotate must REVOKE before regenerating (load-bearing
// ordering — else a live untracked root lingers), and deliver the fresh token.
func TestRunCIBaoBreakglassRotate(t *testing.T) {
	b64, priv := rsaPubB64(t, 2048)
	tmp := setBreakglassEnv(t, "s.old-root")
	seq, revokeAt, regenInitAt := 0, 0, 0
	withBaoExec(t, quorumRegenExec(t, "s.newroot",
		func(token string) {
			seq++
			revokeAt = seq
			if token != "s.old-root" {
				t.Errorf("revoke used token %q, want the stored old root", token)
			}
		},
		func() { seq++; regenInitAt = seq },
	))
	withGHSetSecret(t, nil)

	if err := runCIBaoBreakglass(globalOpts{}, "primary", "rotate", b64); err != nil {
		t.Fatal(err)
	}
	if revokeAt == 0 || regenInitAt == 0 || revokeAt > regenInitAt {
		t.Errorf("rotate must REVOKE (seq %d) before regenerating (seq %d)", revokeAt, regenInitAt)
	}
	if got := decryptDelivered(t, tmp, priv); got != "s.newroot" {
		t.Errorf("rotate delivered %q, want fresh s.newroot", got)
	}
}

// TestRunCIBaoBreakglassRotateRefusesRedelivery: if the revoke silently fails
// (transient exec error) while the stored token is STILL live, regen's lookup
// succeeds and takes the "valid — skipping regeneration" branch, leaving the
// token unchanged. Rotate must NOT redeliver that un-rotated (possibly
// compromised) token — it must fail loudly.
func TestRunCIBaoBreakglassRotateRefusesRedelivery(t *testing.T) {
	b64, _ := rsaPubB64(t, 2048)
	setBreakglassEnv(t, "s.old-root")
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		cmd := strings.Join(args, " ")
		switch {
		case cmd == "token revoke -self":
			return "", "connection reset", errors.New("exit status 1") // transient — swallowed as a warning
		case args[0] == "status":
			return `{"initialized":true,"sealed":false}`, "", nil
		case args[0] == "token" && len(args) > 1 && args[1] == "lookup":
			return `{"data":{"id":"s.old-root"}}`, "", nil // STILL VALID → regen skips
		}
		t.Errorf("unexpected exec %v (regen must not run once the token looks valid)", args)
		return "", "", errors.New("unexpected")
	})
	withGHSetSecret(t, nil)

	err := runCIBaoBreakglass(globalOpts{}, "primary", "rotate", b64)
	if err == nil {
		t.Fatal("rotate must FAIL when the token is unchanged (revoke did not take), not redeliver it")
	}
	if !strings.Contains(err.Error(), "did not produce a fresh root token") {
		t.Errorf("want the redelivery-guard error, got %v", err)
	}
}
