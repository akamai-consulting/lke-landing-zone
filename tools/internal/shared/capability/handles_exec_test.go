package capability_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// The handles' RUN paths, exercised through the process seams rather than left to
// a floor adjustment. Permits() is what every other test asserts; these prove the
// permitted call actually reaches the seam and the refused one never does — which
// is the difference between a permission check and a permission.
func TestForgeRunReachesTheSeamOnlyWhenPermitted(t *testing.T) {
	var got []string
	prev := kubectlprobe.Exec
	kubectlprobe.Exec = func(name string, args ...string) ([]byte, error) {
		got = append(got, name+" "+strings.Join(args, " "))
		return []byte("out"), nil
	}
	t.Cleanup(func() { kubectlprobe.Exec = prev })

	h := capability.For(binding(extension.CloudRead))
	b, err := h.Forge.Run("api", "repos/o/r")
	if err != nil || string(b) != "out" {
		t.Fatalf("permitted read failed: %v (%q)", err, b)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], "gh api") {
		t.Errorf("seam saw %v, want one `gh api …`", got)
	}

	// The refused call must not reach the seam at all — a check that runs the
	// command and then complains has already done the thing.
	if _, err := h.Forge.Run("secret", "set", "X"); err == nil {
		t.Error("`gh secret set` succeeded without secret-custody")
	}
	if len(got) != 1 {
		t.Errorf("a REFUSED call still reached the process seam: %v", got)
	}
}

func TestSecretsGetReachesTheStoreAndCarriesTheVerdict(t *testing.T) {
	// KVGetFieldOK is a func, not a var; the swappable seam is one layer down.
	// Going through the real KVGetFieldOK is better anyway: it means this test
	// exercises the VERDICT logic the handle's whole contract rests on, rather
	// than a stub that could disagree with it.
	prev := baoread.Exec
	baoread.Exec = func(token string, args ...string) (string, string, error) {
		return "value-for-token", "", nil
	}
	t.Cleanup(func() { baoread.Exec = prev })

	h := capability.For(binding(extension.SecretRead))
	v, verdict := h.Secrets.Get("secret/x", "token")
	if verdict != baoread.Found || v != "value-for-token" {
		t.Errorf("Get = (%q, %v), want the store's answer passed through unchanged", v, verdict)
	}
}

func TestCustodianPutReachesTheStoreOnlyWithCustody(t *testing.T) {
	var puts int
	prev := baoread.KVPut
	baoread.KVPut = func(string, map[string]string) error { puts++; return nil }
	t.Cleanup(func() { baoread.KVPut = prev })

	if err := capability.For(binding(extension.SecretCustody)).
		Custodian.Put("secret/x", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("custody Put failed: %v", err)
	}
	if puts != 1 {
		t.Errorf("permitted Put reached the store %d times, want 1", puts)
	}

	if err := capability.For(binding(extension.SecretRead)).
		Custodian.Put("secret/x", nil); !errors.Is(err, capability.ErrNoSecretCustody) {
		t.Errorf("read-only Put returned %v, want ErrNoSecretCustody", err)
	}
	if puts != 1 {
		t.Error("a REFUSED Put still reached the store")
	}
}

// String() is what every refusal message and test failure renders, so a wrong or
// empty name is a message nobody can act on.
func TestForgeActionNamesAreDistinctAndNonEmpty(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range []capability.ForgeAction{
		capability.ForgeRead, capability.ForgeMutate,
		capability.ForgeCustody, capability.ForgeUnclassified,
	} {
		s := a.String()
		if s == "" {
			t.Errorf("ForgeAction(%d) renders empty", a)
		}
		if seen[s] {
			t.Errorf("two actions both render %q — a refusal message could not distinguish them", s)
		}
		seen[s] = true
	}
}
