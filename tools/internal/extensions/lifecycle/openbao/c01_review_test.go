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
	"time"

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
		err = RunCILogin(false, "kubernetes", "reconciler", srv.URL, "kubernetes", saFile, "OPENBAO_TOKEN", "")
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
	// THE `&& !contains(mask)` FORM WAS VACUOUS: the ::add-mask:: line CONTAINS the
	// token, so the conjunction could never be true and the assertion could never
	// fail. Mutation-confirmed by the review: adding fmt.Print(token) to the
	// $GITHUB_ENV branch left both tests green. Assert on what must NOT be there
	// instead — a bare token line, i.e. the token with no `::add-mask::` prefix.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "s.the-minted-token") && !strings.HasPrefix(line, "::add-mask::") {
			t.Errorf("inside Actions the token belongs in $GITHUB_ENV, not bare on stdout — found %q", line)
		}
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
		case strings.Contains(joined, "token lookup"):
			return `{"data":{"policies":["root"]}}`, "", nil
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
	sleeps := withBaoSleepSeam(t)
	quorum := 0
	withBaoExecRaw(t, func(_, _, _ string, args ...string) (string, string, error) {
		// ONE transient, ON THE FIRST QUORUM SUBMISSION rather than on the first call
		// of any kind. Making the SEALED CHECK transient instead stops the flow there
		// and never reaches the loop this is about, so reverting the quorum
		// submissions to ExecPod leaves it green. Counting quorum calls specifically
		// (rather than every call, mod 2) makes which call fails independent of how
		// many the rest of the flow makes.
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "generate-root -nonce=") {
			quorum++
			if quorum == 1 {
				return "", "error dialing backend: No agent available", errors.New("exit 1")
			}
		}
		switch {
		case strings.Contains(joined, "status"):
			return `{"sealed":false,"t":1}`, "", nil
		case strings.Contains(joined, "-init"):
			return `{"nonce":"N","otp":"OTP"}`, "", nil
		case strings.Contains(joined, "-decode="):
			return `{"token":"s.new-root"}`, "", nil
		case strings.Contains(joined, "token lookup"):
			return `{"data":{"policies":["root"]}}`, "", nil
		case strings.Contains(joined, "generate-root -nonce="):
			return `{"complete":true,"progress":1,"required":1,"encoded_token":"ENC"}`, "", nil
		}
		return "", "", nil
	})
	withRegenRootKeyReader(t, "unseal-key-1")
	withKubectlProbeStub(t)
	withGHSetSecret(t, nil)

	err := RunRegenRoot(false, "primary", RegenRootOpts{})
	if err != nil {
		t.Fatalf("a transient mid-quorum must be ridden out, not aborted: %v", err)
	}
	if *sleeps == 0 {
		t.Error("no backoff happened — the quorum submission is not going through the resilient wrapper, " +
			"so a konnectivity blip mid-quorum still aborts and reports itself as a key mismatch")
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

// withRegenRootKeyReader feeds the interactive unseal-key prompt.
func withRegenRootKeyReader(t *testing.T, keys ...string) {
	t.Helper()
	prev := readSecretLine
	i := 0
	readSecretLine = func() (string, error) {
		if i < len(keys) {
			k := keys[i]
			i++
			return k, nil
		}
		return keys[len(keys)-1], nil
	}
	t.Cleanup(func() { readSecretLine = prev })
}

// withKubectlProbeStub stops the `kubectl config current-context` call from
// shelling out to the host's real kubectl — which the first cut of
// TestRegenRootGoesThroughTheRetryingExec did, on whatever cluster the machine
// running the tests happened to be pointed at.
func withKubectlProbeStub(t *testing.T) {
	t.Helper()
	prev := kubectlprobe.Exec
	kubectlprobe.Exec = func(string, ...string) ([]byte, error) { return []byte("test-context\n"), nil }
	t.Cleanup(func() { kubectlprobe.Exec = prev })
}

// withBaoSleepSeam counts the resilient wrapper's backoffs.
func withBaoSleepSeam(t *testing.T) *int {
	t.Helper()
	prev := baoread.Sleep
	n := new(int)
	baoread.Sleep = func(time.Duration) { *n++ }
	t.Cleanup(func() { baoread.Sleep = prev })
	return n
}

// ── from the code review of this PR ─────────────────────────────────────────

// TestRegenRootDryRunTouchesNothing. findLeaderPod probes three pods, and now
// that this file goes through the RESILIENT exec each probe carries the 24-try
// transient budget — so under a konnectivity outage a --dry-run sat silent for
// ~16 minutes before printing its first line. A dry run must not touch the
// cluster at all, and it now returns before anything does.
func TestRegenRootDryRunTouchesNothing(t *testing.T) {
	var execs int
	withBaoExecRaw(t, func(string, string, string, ...string) (string, string, error) {
		execs++
		return "", "", errors.New("a dry run must not reach the cluster")
	})
	withKubectlProbeStub(t)
	if err := RunRegenRoot(true, "primary", RegenRootOpts{}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if execs != 0 {
		t.Errorf("a --dry-run made %d exec(s) — findLeaderPod alone is three pods x the 24-try transient "+
			"budget, so an unreachable cluster makes this sit silent for ~16 minutes", execs)
	}
}

// TestRegenRootReadsASealedPod. `bao status` exits 2 when sealed and still prints
// valid JSON, so bailing on the exec error made the `if sealed` branch DEAD in
// production: an operator with a sealed cluster was told "cannot reach OpenBao …
// via the current kubectl context" and sent to check a kubeconfig that was fine.
// The same defect this PR fixes in healthsla, in the file it was already editing.
func TestRegenRootReadsASealedPod(t *testing.T) {
	withBaoExecRaw(t, func(_, _, _ string, args ...string) (string, string, error) {
		if strings.Contains(strings.Join(args, " "), "status") {
			return `{"sealed":true,"t":3}`, "", errors.New("command terminated with exit code 2")
		}
		return "", "", nil
	})
	withKubectlProbeStub(t)
	err := RunRegenRoot(false, "primary", RegenRootOpts{})
	if err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("a sealed pod must be reported as SEALED, not as unreachable; got %v", err)
	}
}

// TestSeedSealKeyLeavesAConcurrentCreatorsKeyAlone. ExistsOK closes the case
// where the probe could not SEE the Secret; it cannot close the case where two
// seed runs both looked before either wrote. `apply` is an upsert, so both would
// write and the second would destroy the key that decrypts the first one's seal —
// with the escrowed copy already pointing at the loser. `create` fails
// AlreadyExists instead, which is an answer this path can act on.
func TestSeedSealKeyLeavesAConcurrentCreatorsKeyAlone(t *testing.T) {
	withSeedNamespace(t, true)
	t.Setenv("OPENBAO_SEAL_KEY", "")
	t.Setenv("GH_TOKEN", "ghp_write")
	withSeedRand(t, 0x9)
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return nil, errors.New("Error from server (NotFound)")
	})
	creates := withSeedKubectlCreateConflict(t)
	withGHSetSecretErr(t, nil)

	if err := RunSeedSealKey(false, "primary"); err != nil {
		t.Fatalf("losing the create race is not an error — the live key is intact: %v", err)
	}
	if *creates == 0 {
		t.Error("the seal key must be written with create, not apply: apply is an upsert and would have " +
			"overwritten the winner's key")
	}
}

// TestCILoginWritesTheTokenToAFileWhenAsked. stdout is a LOG for the caller this
// fallback was added for: `llz ci openbao-login` as a container ENTRYPOINT has
// its stdout collected by the kubelet and shipped to Loki, so writing a live
// OpenBao credential there is worse than the silence the fallback replaced.
func TestCILoginWritesTheTokenToAFileWhenAsked(t *testing.T) {
	t.Setenv("GITHUB_ENV", "")
	t.Setenv("GITHUB_ACTIONS", "")
	path := filepath.Join(t.TempDir(), "token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"auth":{"client_token":"s.file-token"}}`))
	}))
	t.Cleanup(srv.Close)
	saFile := filepath.Join(t.TempDir(), "sa")
	if err := os.WriteFile(saFile, []byte("jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubInClusterBaoClient(t, srv.Client())

	out := captureStdout(t, func() {
		if err := RunCILogin(false, "kubernetes", "reconciler", srv.URL, "kubernetes", saFile, "OPENBAO_TOKEN", path); err != nil {
			t.Fatalf("login: %v", err)
		}
	})
	if strings.Contains(out, "s.file-token") {
		t.Error("with --output-file the token must NOT also go to stdout — that is the pod log")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "s.file-token" {
		t.Fatalf("token file = (%q, %v), want the token", b, err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %v, want 0600", st.Mode().Perm())
	}
}
