package capability_test

import (
	"errors"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// WRITING A POLICY IS NOT A READ, and this is the pair the split exists for.
// `policy write` can grant access to every path in the store — it is custody in
// the strongest sense the model has, and a binding that may only READ credentials
// must not be able to rewrite who can reach them.
func TestPolicyWriteNeedsCustodyAndReadDoesNot(t *testing.T) {
	ro := capability.For(binding(extension.SecretRead))

	if err := ro.BaoAdmin.Permits("policy", "list"); err != nil {
		t.Errorf("secret-read was refused `bao policy list`: %v", err)
	}
	if err := ro.BaoAdmin.Permits("status"); err != nil {
		t.Errorf("secret-read was refused `bao status`: %v", err)
	}
	if err := ro.BaoAdmin.Permits("policy", "write", "x"); !errors.Is(err, capability.ErrNoBaoAdmin) {
		t.Errorf("secret-read was allowed `bao policy write` (err=%v) — reading a credential "+
			"is not permission to rewrite who may reach every path", err)
	}
	for _, argv := range [][]string{
		{"auth", "enable"}, {"token", "revoke"}, {"operator", "init"},
		{"operator", "generate-root"}, {"audit", "enable"},
	} {
		if err := ro.BaoAdmin.Permits(argv...); err == nil {
			t.Errorf("secret-read was allowed `bao %v`", argv)
		}
	}
}

func TestCustodyGetsBothHalves(t *testing.T) {
	h := capability.For(binding(extension.SecretCustody))
	for _, argv := range [][]string{
		{"status"}, {"policy", "list"}, {"policy", "write", "x"}, {"auth", "enable"},
	} {
		if err := h.BaoAdmin.Permits(argv...); err != nil {
			t.Errorf("secret-custody refused `bao %v`: %v", argv, err)
		}
	}
}

// KV IS THE OTHER SURFACE AND MUST NOT BE REACHABLE HERE. Secrets.Get returns a
// three-valued VERDICT so a refused read can never be mistaken for an absent path
// — the property obj-encryption's SSE-C key depends on. A general argv that could
// spell `kv get` would route around that and hand back a bare string.
func TestKVIsNotReachableThroughTheAdminHandle(t *testing.T) {
	h := capability.For(binding(extension.SecretCustody))
	for _, argv := range [][]string{
		{"kv", "get", "-field=token", "secret/x"},
		{"kv", "put", "secret/x", "k=v"},
		{"kv", "list", "secret/"},
	} {
		if err := h.BaoAdmin.Permits(argv...); !errors.Is(err, capability.ErrBaoUnclassified) {
			t.Errorf("`bao %v` was reachable through BaoAdmin (err=%v) — KV has typed "+
				"operations whose fail-closed verdict this would bypass", argv, err)
		}
	}
}

// An unknown subcommand is refused even with every grant: a bao verb nobody has
// classified must not arrive holding the safest permission.
func TestUnclassifiedBaoArgvIsRefusedWithEveryGrant(t *testing.T) {
	h := capability.For(binding(extension.SecretCustody, extension.SecretRead))
	for _, argv := range [][]string{{"lease", "revoke"}, {"plugin", "register"}, {}} {
		if err := h.BaoAdmin.Permits(argv...); !errors.Is(err, capability.ErrBaoUnclassified) {
			t.Errorf("argv %v returned %v, want refused", argv, err)
		}
	}
}

// A flag directly after the verb still classifies — `bao status --porcelain` is
// two of the counted call sites.
func TestAVerbFollowedByAFlagStillClassifies(t *testing.T) {
	if got := capability.ClassifyBao([]string{"status", "--porcelain"}); got != capability.BaoRead {
		t.Errorf("`bao status --porcelain` classified as %s, want read", got)
	}
	if got := capability.ClassifyBao([]string{"write", "-force", "sys/x"}); got != capability.BaoAdmin {
		t.Errorf("`bao write` classified as %s, want admin", got)
	}
}

func TestNoSecretGrantYieldsARefusingBaoHandleNotNil(t *testing.T) {
	h := capability.For(binding(extension.ClusterRead))
	if h.BaoAdmin == nil {
		t.Fatal("BaoAdmin is nil — every handle must be present and refusing")
	}
	if _, _, err := h.BaoAdmin.Run("pod", "tok", "", "status"); err == nil {
		t.Error("Run succeeded on a denied handle")
	}
	if _, _, err := capability.DeniedBaoAdmin().Run("p", "t", "", "status"); err == nil {
		t.Error("DeniedBaoAdmin().Run succeeded")
	}
}

// The two tables must not overlap: an argv in both would classify by branch order,
// which is a rule nobody could read off the source.
func TestBaoTablesAreDisjoint(t *testing.T) {
	reads, admins := capability.BaoActions()
	seen := map[string]bool{}
	for _, k := range reads {
		seen[k] = true
	}
	for _, k := range admins {
		if seen[k] {
			t.Errorf("%q is both a read and an admin operation", k)
		}
	}
	if len(reads) == 0 || len(admins) == 0 {
		t.Fatal("a classification table emptied — every bao call would be refused")
	}
}

// The RUN path, and that a refused call never reaches the process at all — the
// difference between a permission check and a permission.
func TestBaoAdminRunReachesTheStoreOnlyWhenPermitted(t *testing.T) {
	var calls int
	prev := baoread.ExecFn
	baoread.ExecFn = func(pod, token, stdin string, args ...string) (string, string, error) {
		calls++
		return "sealed=false", "", nil
	}
	t.Cleanup(func() { baoread.ExecFn = prev })

	ro := capability.For(binding(extension.SecretRead))
	out, _, err := ro.BaoAdmin.Run("pod-0", "tok", "", "status")
	if err != nil || out != "sealed=false" {
		t.Fatalf("permitted read failed: %v (%q)", err, out)
	}
	if calls != 1 {
		t.Fatalf("store saw %d calls, want 1", calls)
	}

	if _, _, err := ro.BaoAdmin.Run("pod-0", "tok", "", "policy", "write", "x"); err == nil {
		t.Error("`policy write` succeeded without secret-custody")
	}
	if calls != 1 {
		t.Error("a REFUSED admin call still reached the store")
	}
}

// String() renders in every refusal message; an empty or duplicated name is a
// message nobody can act on.
func TestBaoActionNamesAreDistinctAndNonEmpty(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range []capability.BaoAction{
		capability.BaoRead, capability.BaoAdmin, capability.BaoUnclassified,
	} {
		s := a.String()
		if s == "" || seen[s] {
			t.Errorf("BaoAction(%d) renders %q — empty or duplicated", a, s)
		}
		seen[s] = true
	}
}
