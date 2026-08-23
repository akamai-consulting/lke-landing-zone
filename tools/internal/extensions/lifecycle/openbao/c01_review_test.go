package openbao

// c01_review_test.go — the gates for the five C01 findings of the 2026-08-13
// full-codebase review. Grouped in one file because they are one class read five
// ways: a command that cannot tell what happened, and says nothing.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// withBaoExecRaw stubs the RAW exec so the resilient wrapper stays live and its
// retry can be observed. execseams_test.go's header explains why this is a
// different seam from withBaoExec and not a substitute for it.
func withBaoExecRaw(t *testing.T, fn func(pod, token, stdin string, args ...string) (string, string, error)) {
	t.Helper()
	orig := baoread.ExecRaw
	baoread.ExecRaw = fn
	t.Cleanup(func() { baoread.ExecRaw = orig })
}

// runCILoginCapturingStdout drives the real RunCILogin against a fake OpenBao —
// the same harness TestOpenBaoLoginKubernetesExportsToken uses — and returns what
// landed on stdout.
func runCILoginCapturingStdout(t *testing.T, token string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"auth":{"client_token":"` + token + `"}}`))
	}))
	t.Cleanup(srv.Close)
	saFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(saFile, []byte("sa-jwt-abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubInClusterBaoClient(t, srv.Client())
	var err error
	out := captureStdout(t, func() {
		err = RunCILogin(false, "kubernetes", "reconciler", srv.URL, "kubernetes", saFile, "OPENBAO_TOKEN")
	})
	if err != nil {
		t.Fatalf("kubernetes login: %v", err)
	}
	return out
}

// runOpenBaoGetCapturingStdout drives the real RunGet against a fake OpenBao.
// ClientForward honours OPENBAO_ADDR_<ROLE>, so no seam is needed — an explicitly
// set address always wins over the port-forward path.
func runOpenBaoGetCapturingStdout(t *testing.T, value string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"data":{"k":"` + value + `"}}}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENBAO_ADDR_ACTIVE", srv.URL)
	t.Setenv("OPENBAO_TOKEN_ACTIVE", "s.test")
	var err error
	out := captureStdout(t, func() { err = RunGet("active", "secret/platform/x", "k") })
	if err != nil {
		t.Fatalf("openbao get: %v", err)
	}
	return out
}

// ── sealkey.go: an unanswerable probe must not read as "no key here yet" ──────

// TestSeedSealKeyRefusesWhenItCannotSeeTheSecret is the highest-stakes arm in
// this package. `Exists` folds "the apiserver did not answer" into "absent", and
// absent here means GENERATE a fresh 32-byte key and apply it — over a live one.
// The static seal key is what decrypts OpenBao's seal, so that is a cluster whose
// secret store can never be unsealed again, with the original key gone.
//
// The window is a probe that fails for a reason the following apply does not:
// a throttled or timed-out get, RBAC that permits patch but not get, one
// konnectivity blip. Each reads as "absent" and is answered by minting.
func TestSeedSealKeyRefusesWhenItCannotSeeTheSecret(t *testing.T) {
	prevRetries, prevDelay := kubectlprobe.Retries, kubectlprobe.Delay
	kubectlprobe.Retries, kubectlprobe.Delay = 1, 0
	t.Cleanup(func() { kubectlprobe.Retries, kubectlprobe.Delay = prevRetries, prevDelay })

	for _, transient := range []string{
		"Unable to connect to the server: dial tcp: i/o timeout",
		"error: You must be logged in to the server (Unauthorized)",
		"Error from server (TooManyRequests): the server has received too many requests",
	} {
		t.Run(transient[:20], func(t *testing.T) {
			withSeedNamespace(t, true)
			t.Setenv("OPENBAO_SEAL_KEY", "")
			withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New(transient) })
			applied := withSeedKubectlApply(t)
			gh := withGHSetSecretErr(t, nil)

			err := RunSeedSealKey(false, "primary")
			if err == nil {
				t.Fatal("an unanswerable probe must not be read as `no seal key yet` — the next step " +
					"generates one and applies it over whatever is live, and there is no recovery from that")
			}
			if !strings.Contains(err.Error(), "did not answer") {
				t.Errorf("the error must say the apiserver did not answer, got: %v", err)
			}
			if *applied != "" {
				t.Errorf("nothing may be applied when we cannot tell: %q", *applied)
			}
			if len(*gh) != 0 {
				t.Errorf("nothing may be persisted when we cannot tell: %v", *gh)
			}
		})
	}
}

// TestSeedSealKeyStillGeneratesOnADefiniteAbsence pins the exclusion. A guard
// that only proves it refuses would break every first bootstrap, which is the
// path this command exists for.
func TestSeedSealKeyStillGeneratesOnADefiniteAbsence(t *testing.T) {
	withSeedNamespace(t, true)
	t.Setenv("OPENBAO_SEAL_KEY", "")
	withSeedRand(t, 0x5)
	t.Setenv("GH_TOKEN", "ghp_write")
	// "NotFound" is in kubectlprobe's absenceMarkers: kubectl ASKED and ANSWERED.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("Error from server (NotFound)") })
	applied := withSeedKubectlApply(t)
	withGHSetSecretErr(t, nil)
	if err := RunSeedSealKey(false, "primary"); err != nil {
		t.Fatalf("a definite absence is the first-bootstrap path and must still seed: %v", err)
	}
	if *applied == "" {
		t.Error("a definite absence must still generate and apply the key")
	}
}

// ── cilogin.go: the CI-agnostic verb whose only output was GitHub-specific ────

// TestCILoginPutsTheTokenOnStdoutWithoutGithubEnv. ghaout.Append is a silent
// no-op when its env var is unset, so outside GitHub Actions this minted a real
// OpenBao token, wrote it nowhere, printed "exported to $GITHUB_ENV" and exited
// 0. Outside Actions is the PRIMARY case: the file's own header argues
// `--method kubernetes` is the default precisely because it works from an Argo
// Workflow, a CronJob or the reconciler — none of which set $GITHUB_ENV.
func TestCILoginPutsTheTokenOnStdoutWithoutGithubEnv(t *testing.T) {
	t.Setenv("GITHUB_ENV", "")
	t.Setenv("GITHUB_ACTIONS", "")
	out := runCILoginCapturingStdout(t, "s.the-minted-token")
	if strings.TrimSpace(out) != "s.the-minted-token" {
		t.Fatalf("stdout must carry the bare token so `T=$(llz ci openbao-login …)` works, got %q", out)
	}
	// Not masked on this path: ::add-mask:: goes to STDOUT and would land inside
	// the capture. teamlogin.go records the same trade.
	if strings.Contains(out, "::add-mask::") {
		t.Errorf("a mask line on stdout corrupts the capture it exists inside: %q", out)
	}
}

// TestCILoginStillWritesGithubEnvWhenItIsSet pins the exclusion — the Actions
// path is the one that already worked and must keep working, masked.
func TestCILoginStillWritesGithubEnvWhenItIsSet(t *testing.T) {
	env := t.TempDir() + "/gh-env"
	t.Setenv("GITHUB_ENV", env)
	t.Setenv("GITHUB_ACTIONS", "true")
	out := runCILoginCapturingStdout(t, "s.the-minted-token")
	if strings.Contains(out, "s.the-minted-token") && !strings.Contains(out, "::add-mask::") {
		t.Errorf("inside Actions the token belongs in $GITHUB_ENV, not bare on stdout: %q", out)
	}
	b, err := os.ReadFile(env)
	if err != nil {
		t.Fatalf("read $GITHUB_ENV: %v", err)
	}
	if !strings.Contains(string(b), "OPENBAO_TOKEN=s.the-minted-token") {
		t.Errorf("$GITHUB_ENV must carry the export, got %q", b)
	}
}

// ── ci_openbao_init.go: stop feeding keys to a completed quorum ──────────────

// TestGenerateRootStopsWhenTheQuorumCompletes. The unseal THRESHOLD is not
// necessarily three. At a threshold of two the second key completes
// generate-root and OpenBao MINTS the root token; a third submission against a
// completed nonce errors, and this loop returned that error — after the token
// existed and before anything decoded it. A live root token nobody holds, no
// record it was created, and a message pointing the operator at unseal-key
// correctness, which was the one thing that was fine.
func TestGenerateRootStopsWhenTheQuorumCompletes(t *testing.T) {
	t.Setenv("RECOVERY_K1", "k1")
	t.Setenv("RECOVERY_K2", "k2")
	t.Setenv("RECOVERY_K3", "k3")
	t.Setenv("OPENBAO_ROOT_TOKEN", "")
	t.Setenv("GITHUB_ENV", "")
	withGHSetSecret(t, nil)

	var submissions int
	withBaoExec(t, func(_, _, stdin string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "-init"):
			return `{"nonce":"N","otp":"OTP"}`, "", nil
		case strings.Contains(joined, "-decode="):
			return `{"token":"s.new-root"}`, "", nil
		case strings.Contains(joined, "-cancel"):
			return "", "", nil
		case joined == "status -format=json":
			return `{"sealed":false,"initialized":true}`, "", nil
		case stdin != "":
			submissions++
			if submissions == 2 { // threshold 2: the SECOND key completes it
				return `{"complete":true,"progress":2,"required":2,"encoded_token":"ENC"}`, "", nil
			}
			if submissions > 2 {
				return "", "no root generation in progress", errors.New("exit 2")
			}
			return `{"complete":false,"progress":1,"required":2}`, "", nil
		}
		return "", "", nil
	})

	if err := RunRegenRootCI(false, "primary"); err != nil {
		t.Fatalf("the quorum completed at key 2 and the flow must finish, got %v", err)
	}
	if submissions != 2 {
		t.Errorf("submitted %d keys; the loop must stop the moment the quorum completes, or it "+
			"errors on a nonce that has already minted a token nobody holds", submissions)
	}
}

// ── regenroot.go: the interactive path skipped the retry wrapper ─────────────

// TestRegenRootGoesThroughTheRetryingExec. RunRegenRoot called baoread.ExecPod —
// the RAW kubectl exec — directly, so a konnectivity blip mid-quorum aborted a
// flow that the resilient wrapper would have ridden out, and reported it as a key
// mismatch. Calling ExecPod also made the function unstubabble, which is why it
// had no test at all: this one exists because the fix made it possible.
func TestRegenRootGoesThroughTheRetryingExec(t *testing.T) {
	sleeps := withBaoSleep(t)
	var raw int
	withBaoExecRaw(t, func(_, _, _ string, _ ...string) (string, string, error) {
		raw++
		if raw == 1 {
			// A transient the wrapper is meant to absorb.
			return "", "error dialing backend: No agent available", errors.New("exit 1")
		}
		return `{"sealed":true,"t":3}`, "", nil
	})
	err := RunRegenRoot(false, "primary", RegenRootOpts{})
	if raw < 2 {
		t.Fatalf("the first exec was not retried (%d raw call(s)) — RunRegenRoot is not going through "+
			"the resilient wrapper, so a blip mid-quorum still aborts the flow", raw)
	}
	if *sleeps == 0 {
		t.Error("a retry must back off; zero sleeps means the wrapper was bypassed")
	}
	// Sealed → it stops there, which is the point: the retry happened first.
	if err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Errorf("expected the sealed check to be reached after the retry, got %v", err)
	}
}

// ── cli.go: the mask line that corrupted every documented capture ────────────

// TestOpenBaoGetWritesOnlyTheValue. ghsecret.Mask writes `::add-mask::<value>` to
// STDOUT, and this command's stdout IS its return value. Under GITHUB_ACTIONS
// every documented use — `diff <(llz openbao get …) <(…)`,
// `llz openbao get … | shasum`, `V=$(llz openbao get …)` — received two lines
// where one was asked for, and compared or hashed the wrong thing.
func TestOpenBaoGetWritesOnlyTheValue(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	out := runOpenBaoGetCapturingStdout(t, "the-secret-value")
	if out != "the-secret-value" {
		t.Fatalf("stdout must carry the value and nothing else, got %q", out)
	}
}
