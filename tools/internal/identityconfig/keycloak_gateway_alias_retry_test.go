package identityconfig

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

// The real apiserver rejection observed on two bootstrap runs. The step already
// waited for Argo to CREATE the StatefulSet, which is a different readiness question
// and says nothing about admission — so the patch failed the whole job on a
// transient that clears in under a minute.
const kyvernoWebhookRejection = `Error from server (InternalError): Internal error occurred: ` +
	`failed calling webhook "validate.kyverno.svc-fail": failed to call webhook: ` +
	`Post "https://kyverno-svc.kyverno.svc:443/validate/fail?timeout=10s": No agent available:`

func withPatchStub(t *testing.T, outs []string, errs []error) *int {
	t.Helper()
	prevExec, prevNow, prevSleep := kubectlprobe.Exec, keycloakPinNow, keycloakPinSleep
	t.Cleanup(func() { kubectlprobe.Exec, keycloakPinNow, keycloakPinSleep = prevExec, prevNow, prevSleep })
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	keycloakPinNow = func() time.Time { return now }
	keycloakPinSleep = func(d time.Duration) { now = now.Add(d) }
	calls := 0
	kubectlprobe.Exec = func(_ string, _ ...string) ([]byte, error) {
		i := calls
		calls++
		if i >= len(outs) {
			i = len(outs) - 1
		}
		return []byte(outs[i]), errs[min(i, len(errs)-1)]
	}
	return &calls
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// The webhook race must be ridden out, not failed on.
func TestKeycloakPinRetriesThroughTheKyvernoWebhookRace(t *testing.T) {
	calls := withPatchStub(t,
		[]string{kyvernoWebhookRejection, kyvernoWebhookRejection, ""},
		[]error{errors.New("exit status 1"), errors.New("exit status 1"), nil})

	if err := patchWithWebhookRetry(`{}`); err != nil {
		t.Fatalf("a transient webhook rejection must be retried, not returned: %v", err)
	}
	if *calls != 3 {
		t.Errorf("patched %d time(s), want 3 (two rejections then success)", *calls)
	}
}

// A NON-race failure must surface immediately. Retrying a bad field or an RBAC
// denial just hides a real error behind a three-minute timeout.
func TestKeycloakPinDoesNotRetryARealPatchFailure(t *testing.T) {
	calls := withPatchStub(t,
		[]string{`Error from server (Forbidden): statefulsets.apps "platform-openbao" is forbidden`},
		[]error{errors.New("exit status 1")})

	err := patchWithWebhookRetry(`{}`)
	if err == nil {
		t.Fatal("a forbidden patch must fail")
	}
	if *calls != 1 {
		t.Errorf("retried a non-race failure %d time(s) — that delays a real error behind a timeout", *calls)
	}
}

// A Kyverno that never recovers must fail the job rather than hang it.
func TestKeycloakPinGivesUpOnAPermanentlyUnreachableWebhook(t *testing.T) {
	withPatchStub(t, []string{kyvernoWebhookRejection}, []error{errors.New("exit status 1")})

	err := patchWithWebhookRetry(`{}`)
	if err == nil {
		t.Fatal("an unreachable webhook must eventually fail")
	}
	if !strings.Contains(err.Error(), "still unreachable") {
		t.Errorf("the error must say the webhook never came back: %v", err)
	}
}
