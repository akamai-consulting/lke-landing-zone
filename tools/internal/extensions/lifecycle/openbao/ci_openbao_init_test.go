package openbao

import (
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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghsecret"
)

// withGHSetSecret swaps the gh-secret seam, recording "name@env" calls.
func withGHSetSecret(t *testing.T, fail func(name string) error) *[]string {
	t.Helper()
	orig := ghsecret.SetFn
	calls := new([]string)
	ghsecret.SetFn = func(name, ghEnv, value string) error {
		*calls = append(*calls, name+"@"+ghEnv+"="+value)
		if fail != nil {
			return fail(name)
		}
		return nil
	}
	t.Cleanup(func() { ghsecret.SetFn = orig })
	return calls
}

// Fixture key material, DELIBERATELY full length. The disclosure test below
// substring-searches the ESCROW job summary for each of these values, and that
// summary embeds five base64 RSA-OAEP ciphertexts built with a fresh ephemeral
// key on every run — ~1700 characters of fresh random base64. The old fixtures
// were three characters ("uk1".."uk5"), which collided with that random base64
// often enough to redden CI at random: 6 failures in `go test -count=300`, on
// PRs touching nothing in this package. At share length a chance collision is
// not a thing that happens, and these now match the shape `operator init`
// actually mints. Detection is unchanged: the summary writer emits whole
// ciphertext blocks, never a truncated secret, so a real leak still lands here
// in full.
const (
	fxShare1    = "bGx6LXRlc3QtcmVjb3Zlcnktc2hhcmUtMS1ub3QtcmVhbA=="
	fxShare2    = "bGx6LXRlc3QtcmVjb3Zlcnktc2hhcmUtMi1ub3QtcmVhbA=="
	fxShare3    = "bGx6LXRlc3QtcmVjb3Zlcnktc2hhcmUtMy1ub3QtcmVhbA=="
	fxShare4    = "bGx6LXRlc3QtcmVjb3Zlcnktc2hhcmUtNC1ub3QtcmVhbA=="
	fxShare5    = "bGx6LXRlc3QtcmVjb3Zlcnktc2hhcmUtNS1ub3QtcmVhbA=="
	fxRootToken = "s.bGx6LXRlc3Qtcm9vdC10b2tlbi1ub3QtcmVhbA"
)

// fxShares is the same five in index order, for the assertions that walk them.
var fxShares = []string{fxShare1, fxShare2, fxShare3, fxShare4, fxShare5}

// initJSON is derived from the constants above rather than restating them, so a
// fixture change cannot leave the payload and the assertions disagreeing.
var initJSON = fmt.Sprintf(`{"recovery_keys_b64":[%q,%q,%q,%q,%q],"root_token":%q}`,
	fxShare1, fxShare2, fxShare3, fxShare4, fxShare5, fxRootToken)

func TestParseBaoInit(t *testing.T) {
	r, err := ParseInit(initJSON)
	if err != nil || r.RootToken != fxRootToken || len(r.RecoveryKeysB64) != 5 {
		t.Fatalf("ParseInit = (%+v, %v), want full payload", r, err)
	}
	for _, bad := range []string{
		"", "not json",
		`{"recovery_keys_b64":["a","b"],"root_token":"s.x"}`, // too few shares
		`{"recovery_keys_b64":["a","b","c","d","e"]}`,        // no root
	} {
		if _, err := ParseInit(bad); err == nil {
			t.Errorf("ParseInit(%q) = nil error, want failure", bad)
		}
	}
}

// initHarness wires the three GHA output files, the bao exec seam and the
// gh-secret seam, returning the temp dir so a test can read what was written.
func initHarness(t *testing.T, failSet func(name string) error) (string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	for _, v := range []string{"GITHUB_ENV", "GITHUB_OUTPUT", "GITHUB_STEP_SUMMARY"} {
		t.Setenv(v, filepath.Join(dir, v))
	}
	t.Setenv("RUNNER_TEMP", dir)
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		want := "operator init -recovery-shares=5 -recovery-threshold=3 -format=json"
		if pod != "platform-openbao-0" || strings.Join(args, " ") != want {
			t.Errorf("init exec = %s %v", pod, args)
		}
		return initJSON, "", nil
	})
	return dir, withGHSetSecret(t, failSet)
}

// allInitSecrets is every value `operator init` mints. NOTHING in this list may
// appear in the job summary on any path — see the next test for why.
var allInitSecrets = append(append([]string{}, fxShares...), fxRootToken)

func TestRunCIBaoInit(t *testing.T) {
	dir, ghCalls := initHarness(t, nil)

	if err := RunInit(false, "primary", ""); err != nil {
		t.Fatal(err)
	}

	env, _ := os.ReadFile(filepath.Join(dir, "GITHUB_ENV"))
	wantEnv := fmt.Sprintf("OPENBAO_ROOT_TOKEN=%s\nRECOVERY_K1=%s\nRECOVERY_K2=%s\nRECOVERY_K3=%s\n",
		fxRootToken, fxShare1, fxShare2, fxShare3)
	if string(env) != wantEnv {
		t.Errorf("GITHUB_ENV = %q, want %q", env, wantEnv)
	}
	out, _ := os.ReadFile(filepath.Join(dir, "GITHUB_OUTPUT"))
	if string(out) != "did_init=true\n" {
		t.Errorf("GITHUB_OUTPUT = %q, want did_init=true", out)
	}
	// Shares 4 and 5 have no other home on this path, so they must be persisted.
	want := []string{
		"OPENBAO_RECOVERY_KEY_1@infra-primary=" + fxShare1,
		"OPENBAO_RECOVERY_KEY_2@infra-primary=" + fxShare2,
		"OPENBAO_RECOVERY_KEY_3@infra-primary=" + fxShare3,
		"OPENBAO_ROOT_TOKEN@infra-primary=" + fxRootToken,
		"OPENBAO_RECOVERY_KEY_4@infra-primary=" + fxShare4,
		"OPENBAO_RECOVERY_KEY_5@infra-primary=" + fxShare5,
	}
	if strings.Join(*ghCalls, " ") != strings.Join(want, " ") {
		t.Errorf("gh calls = %v, want %v", *ghCalls, want)
	}
}

// THE REGRESSION TEST FOR THE DISCLOSURE. Both paths are checked, so neither can
// regress alone.
func TestRunCIBaoInitNeverWritesKeyMaterialToSummary(t *testing.T) {
	for _, tc := range []struct{ name, escrow string }{
		{"fallback", ""},
		{"escrow", testEscrowPubKeyB64(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := initHarness(t, nil)
			if err := RunInit(false, "primary", tc.escrow); err != nil {
				t.Fatal(err)
			}
			summary, _ := os.ReadFile(filepath.Join(dir, "GITHUB_STEP_SUMMARY"))
			if strings.Contains(string(summary), initJSON) {
				t.Error("the raw operator-init payload is in the job summary")
			}
			for _, secret := range allInitSecrets {
				if strings.Contains(string(summary), secret) {
					t.Errorf("secret %q appears in the job summary: %s", secret, summary)
				}
			}
		})
	}
}

// A gh-secret write failure is still fatal — shares 1-3 ARE the quorum, so the
// bootstrap must not report success having failed to persist them.
func TestRunCIBaoInitQuorumWriteFailureIsFatal(t *testing.T) {
	initHarness(t, func(string) error { return errors.New("403 secrets: write denied") })
	if err := RunInit(false, "primary", ""); err == nil {
		t.Fatal("want error when the gh secret set for a quorum share fails")
	}
}

// Shares 4 and 5 are redundancy, not quorum, and by the time they are written
// the shares are already minted — so a failure there warns and the bootstrap
// carries on. Failing would wedge a run over a loss no retry can repair.
func TestRunCIBaoInitRedundantShareWriteFailureIsNotFatal(t *testing.T) {
	initHarness(t, func(name string) error {
		if name == "OPENBAO_RECOVERY_KEY_4" || name == "OPENBAO_RECOVERY_KEY_5" {
			return errors.New("403 secrets: write denied")
		}
		return nil
	})
	if err := RunInit(false, "primary", ""); err != nil {
		t.Fatalf("RunInit = %v, want nil — losing share 4/5 leaves the 3-of-5 quorum intact", err)
	}
}

// testEscrowPubKeyB64 generates a throwaway 2048-bit key and returns
// base64(PEM) of its public half, matching what an operator pastes.
func testEscrowPubKeyB64(t *testing.T) string {
	t.Helper()
	if testEscrowKey == nil {
		var err error
		if testEscrowKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			t.Fatal(err)
		}
	}
	der, err := x509.MarshalPKIXPublicKey(&testEscrowKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return base64.StdEncoding.EncodeToString(pemBytes)
}

// Generated once per test binary: 2048-bit keygen is slow enough that doing it
// per subtest showed up in the package's runtime.
var testEscrowKey *rsa.PrivateKey

func TestRunCIBaoInitEscrowDeliversCiphertextOnly(t *testing.T) {
	pub := testEscrowPubKeyB64(t)
	dir, ghCalls := initHarness(t, nil)

	if err := RunInit(false, "primary", pub); err != nil {
		t.Fatal(err)
	}

	// ALL FIVE, in index order, recoverable ONLY with the private key.
	//
	// FIVE, not "the two GitHub does not hold": the threshold is 3, so an escrow
	// copy of fewer than 3 authorizes nothing — useless in the one scenario escrow
	// exists for, which is losing the infra-<region> environment.
	raw, err := os.ReadFile(filepath.Join(dir, "openbao-recovery-keys.b64"))
	if err != nil {
		t.Fatalf("escrow file not written: %v", err)
	}
	blocks := strings.Fields(string(raw))
	if len(blocks) != 5 {
		t.Fatalf("escrow file has %d blocks, want all 5 — an escrow copy below the 3-of-5 threshold authorizes nothing", len(blocks))
	}
	summary, _ := os.ReadFile(filepath.Join(dir, "GITHUB_STEP_SUMMARY"))
	for i, b := range blocks {
		ct, err := base64.StdEncoding.DecodeString(b)
		if err != nil {
			t.Fatalf("block %d is not base64: %v", i+1, err)
		}
		got, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, testEscrowKey, ct, nil)
		if err != nil {
			t.Fatalf("block %d did not decrypt: %v", i+1, err)
		}
		if want := fxShares[i]; string(got) != want {
			t.Errorf("block %d = %q, want %q", i+1, got, want)
		}
		// The same ciphertext must ALSO be inline in the summary: the artifact
		// upload is a separate step a caller can omit, and these do not come round
		// again.
		if !strings.Contains(string(summary), b) {
			t.Errorf("ciphertext block %d is not in the job summary", i+1)
		}
	}

	// The summary must state the threshold, so an operator who decrypts three
	// blocks and stops knows that is enough — and one who keeps two knows it is not.
	if !strings.Contains(string(summary), "3 of 5") {
		t.Error("the escrow summary does not state the 3-of-5 threshold")
	}

	// On this path shares 4 and 5 live in the ciphertext, NOT in GitHub — that is
	// the whole point of supplying a key.
	for _, call := range *ghCalls {
		if strings.HasPrefix(call, "OPENBAO_RECOVERY_KEY_4") || strings.HasPrefix(call, "OPENBAO_RECOVERY_KEY_5") {
			t.Errorf("escrow path persisted %q to GitHub; it should exist only as ciphertext", call)
		}
	}
}

// A malformed key must fail BEFORE `operator init` runs. The shares are minted
// exactly once, so discovering a bad key afterwards means they can never be
// escrowed.
func TestRunCIBaoInitRejectsBadEscrowKeyBeforeInit(t *testing.T) {
	ran := false
	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		ran = true
		return initJSON, "", nil
	})
	err := RunInit(false, "primary", "not-base64-at-all!!")
	if err == nil || !strings.Contains(err.Error(), "escrow public key rejected") {
		t.Errorf("err = %v, want the escrow-key rejection", err)
	}
	if ran {
		t.Error("`operator init` ran despite a malformed escrow key — the shares were minted and cannot be escrowed")
	}
}

func TestRunCIBaoInitRequiresRegionAndInitSuccess(t *testing.T) {
	if err := RunInit(false, "", ""); err == nil {
		t.Error("missing --region accepted")
	}
	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		return "", "Error initializing: Vault is already initialized", errors.New("exit status 2")
	})
	err := RunInit(false, "primary", "")
	if err == nil || !strings.Contains(err.Error(), "already initialized") {
		t.Errorf("err = %v, want operator-init failure with stderr", err)
	}
}

func TestRunCIBaoRegenRootValidTokenSkips(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.current")
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		switch args[0] {
		case "status":
			return `{"initialized":true,"sealed":false}`, "", nil
		case "token":
			if token != "s.current" {
				t.Errorf("lookup used token %q", token)
			}
			return `{"data":{"policies":["root"]}}`, "", nil
		}
		t.Errorf("unexpected exec %v", args)
		return "", "", nil
	})
	ghCalls := withGHSetSecret(t, nil)
	if err := RunRegenRootCI(false, "primary"); err != nil {
		t.Fatal(err)
	}
	if len(*ghCalls) != 0 {
		t.Errorf("valid token must not touch gh secrets: %v", *ghCalls)
	}
}

func TestRunCIBaoRegenRootSealedLeaderFails(t *testing.T) {
	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		return `{"initialized":true,"sealed":true}`, "", errors.New("exit status 2")
	})
	if err := RunRegenRootCI(false, "primary"); err == nil || !strings.Contains(err.Error(), "not unsealed") {
		t.Errorf("err = %v, want sealed-leader refusal", err)
	}
}

func TestRunCIBaoRegenRootFullQuorumFlow(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.revoked")
	t.Setenv("RECOVERY_K1", "k1")
	t.Setenv("RECOVERY_K2", "k2")
	t.Setenv("RECOVERY_K3", "k3")
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("GITHUB_ENV", envFile)

	keysSubmitted := 0
	cancelled := false
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		cmd := strings.Join(args, " ")
		switch {
		case args[0] == "status":
			return `{"initialized":true,"sealed":false}`, "", nil
		case args[0] == "token": // revoked
			return "", "Code: 403. * permission denied", errors.New("exit status 2")
		case strings.Contains(cmd, "-cancel"):
			cancelled = true
			return "", "", nil
		case strings.Contains(cmd, "-init"):
			return `{"nonce":"n-1","otp":"otp-1"}`, "", nil
		case strings.Contains(cmd, "-nonce=n-1"):
			if args[len(args)-1] != "-" || stdin == "" {
				t.Errorf("unseal key must ride stdin, got args=%v stdin=%q", args, stdin)
			}
			keysSubmitted++
			if keysSubmitted == 3 {
				return fmt.Sprintf(`{"complete":true,"progress":3,"required":3,"encoded_token":"enc-%s"}`, strings.TrimSpace(stdin)), "", nil
			}
			return fmt.Sprintf(`{"complete":false,"progress":%d,"required":3}`, keysSubmitted), "", nil
		case strings.Contains(cmd, "-decode=enc-k3"):
			return `{"token":"s.newroot"}`, "", nil
		}
		t.Errorf("unexpected exec %v", args)
		return "", "", errors.New("unexpected")
	})
	ghCalls := withGHSetSecret(t, nil)

	if err := RunRegenRootCI(false, "secondary"); err != nil {
		t.Fatal(err)
	}
	if !cancelled || keysSubmitted != 3 {
		t.Errorf("cancelled=%v keysSubmitted=%d, want cancel + 3 submissions", cancelled, keysSubmitted)
	}
	env, _ := os.ReadFile(envFile)
	if string(env) != "OPENBAO_ROOT_TOKEN=s.newroot\n" {
		t.Errorf("GITHUB_ENV = %q, want new root export", env)
	}
	if len(*ghCalls) != 1 || (*ghCalls)[0] != "OPENBAO_ROOT_TOKEN@infra-secondary=s.newroot" {
		t.Errorf("gh calls = %v, want one root-token write to infra-secondary", *ghCalls)
	}
}

// A lookup that never got an answer is not a revoked token. Taking the quorum
// branch on an exec failure mints a SECOND root token and overwrites the
// infra-<region> env secret, leaving the current root live and untracked.
func TestRunCIBaoRegenRootInconclusiveLookupDoesNotRegenerate(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.current")
	t.Setenv("RECOVERY_K1", "k1")
	t.Setenv("RECOVERY_K2", "k2")
	t.Setenv("RECOVERY_K3", "k3")
	generateRootCalled := false
	withBaoExec(t, func(_, _, _ string, args ...string) (string, string, error) {
		switch {
		case args[0] == "status":
			return `{"initialized":true,"sealed":false}`, "", nil
		case args[0] == "token":
			// The exec never landed — no bao verdict at all.
			return "", "error dialing backend: No agent available", errors.New("exit status 1")
		case args[0] == "operator":
			generateRootCalled = true
		}
		return "", "", nil
	})
	secrets := withGHSetSecret(t, nil)

	err := RunRegenRootCI(false, "primary")
	if err == nil || !strings.Contains(err.Error(), "inconclusive") {
		t.Fatalf("err = %v, want an inconclusive-validation failure", err)
	}
	if generateRootCalled {
		t.Error("burned a recovery-key quorum on the strength of an exec failure")
	}
	if len(*secrets) != 0 {
		t.Errorf("overwrote the OPENBAO_ROOT_TOKEN env secret (%v) without evidence the old one was revoked", *secrets)
	}
}

func TestRunCIBaoRegenRootQuorumWithoutToken(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.revoked")
	t.Setenv("RECOVERY_K1", "k1")
	t.Setenv("RECOVERY_K2", "k2")
	t.Setenv("RECOVERY_K3", "k3")
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		cmd := strings.Join(args, " ")
		switch {
		case args[0] == "status":
			return `{"initialized":true,"sealed":false}`, "", nil
		case args[0] == "token":
			// OpenBao's own answer for a revoked token. An EMPTY stderr here would
			// mean "the lookup never got an answer", which no longer regenerates.
			return "", "permission denied", errors.New("exit status 2")
		case strings.Contains(cmd, "-init"):
			return `{"nonce":"n-1","otp":"otp-1"}`, "", nil
		case strings.Contains(cmd, "-nonce"):
			// Wrong keys: progress advances but never completes.
			return `{"complete":false,"progress":1,"required":3}`, "", nil
		}
		return "", "", nil
	})
	withGHSetSecret(t, nil)
	err := RunRegenRootCI(false, "primary")
	if err == nil || !strings.Contains(err.Error(), "encoded_token") {
		t.Errorf("err = %v, want missing-encoded_token failure", err)
	}
}

func TestRunCIBaoRegenRootMissingKeys(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.revoked")
	t.Setenv("RECOVERY_K1", "")
	t.Setenv("RECOVERY_K2", "")
	t.Setenv("RECOVERY_K3", "")
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		if args[0] == "status" {
			return `{"initialized":true,"sealed":false}`, "", nil
		}
		return "", "permission denied", errors.New("exit status 2")
	})
	if err := RunRegenRootCI(false, "primary"); err == nil || !strings.Contains(err.Error(), "RECOVERY_K1") {
		t.Errorf("err = %v, want missing-keys error", err)
	}
}
