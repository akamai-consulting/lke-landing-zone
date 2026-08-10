package capability_test

import (
	"errors"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// `gh api` IS THE WHOLE REASON THIS IS CLASSIFIED RATHER THAN LISTED. The same
// subcommand reads or writes depending on one flag, so a table keyed by name alone
// would have to call it one or the other and be wrong half the time.
func TestGhAPIIsClassifiedByHTTPMethod(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want capability.ForgeAction
	}{
		{[]string{"api", "repos/o/r"}, capability.ForgeRead}, // gh's default is GET
		{[]string{"api", "-X", "GET", "repos/o/r"}, capability.ForgeRead},
		{[]string{"api", "--method", "HEAD", "x"}, capability.ForgeRead},
		{[]string{"api", "-X", "PUT", "repos/o/r/x"}, capability.ForgeMutate},
		{[]string{"api", "--method=POST", "x"}, capability.ForgeMutate},
		{[]string{"api", "-X", "DELETE", "x"}, capability.ForgeMutate},
		{[]string{"api", "-X", "patch", "x"}, capability.ForgeMutate}, // case-insensitive
		// A method this table does not know is refused, not assumed safe.
		{[]string{"api", "-X", "TRACE", "x"}, capability.ForgeUnclassified},
		// A dangling flag is malformed; its intent is unknown.
		{[]string{"api", "-X"}, capability.ForgeUnclassified},
	} {
		if got := capability.ClassifyForge(tc.argv); got != tc.want {
			t.Errorf("ClassifyForge(%v) = %s, want %s", tc.argv, got, tc.want)
		}
	}
}

// CUSTODY IS NOT MERELY MUTATION. `gh secret set` places credential material into
// a GitHub environment; `gh workflow run` dispatches a pipeline. Collapsing them
// would hand every workflow-dispatching binding the ability to write secrets,
// which is the over-granting argument reconcile-actions made when it split into
// four invariants.
func TestSettingASecretIsCustodyNotMutation(t *testing.T) {
	if got := capability.ClassifyForge([]string{"secret", "set", "NAME"}); got != capability.ForgeCustody {
		t.Errorf("`gh secret set` classified as %s, want custody", got)
	}
	if got := capability.ClassifyForge([]string{"secret", "delete", "NAME"}); got != capability.ForgeCustody {
		t.Errorf("`gh secret delete` classified as %s, want custody", got)
	}
	if got := capability.ClassifyForge([]string{"workflow", "run", "x.yml"}); got != capability.ForgeMutate {
		t.Errorf("`gh workflow run` classified as %s, want mutate", got)
	}
	// Listing secrets returns NAMES, never values — the API does not expose them —
	// so it is a read, and token-inventory depends on that being true.
	if got := capability.ClassifyForge([]string{"secret", "list"}); got != capability.ForgeRead {
		t.Errorf("`gh secret list` classified as %s, want read — the API returns names, not values", got)
	}
}

// A binding holding cloud-mutate may dispatch a workflow and MUST NOT set a
// secret. This is the pair the three-way split exists for.
func TestMutateDoesNotConferCustody(t *testing.T) {
	h := capability.For(binding(extension.CloudMutate))

	if err := h.Forge.Permits("workflow", "run", "terraform.yml"); err != nil {
		t.Errorf("cloud-mutate was refused a workflow dispatch: %v", err)
	}
	err := h.Forge.Permits("secret", "set", "LINODE_TOKEN")
	if !errors.Is(err, capability.ErrNoForgeCustody) {
		t.Errorf("cloud-mutate was allowed `gh secret set` (err=%v) — dispatching a pipeline "+
			"is not permission to place a credential in it", err)
	}
}

func TestCustodyDoesNotConferGeneralMutation(t *testing.T) {
	h := capability.For(binding(extension.SecretCustody))

	if err := h.Forge.Permits("secret", "set", "X"); err != nil {
		t.Errorf("secret-custody was refused `gh secret set`: %v", err)
	}
	if err := h.Forge.Permits("repo", "create", "o/r"); !errors.Is(err, capability.ErrNoForgeMutate) {
		t.Errorf("secret-custody was allowed `gh repo create` (err=%v) — holding a credential "+
			"is not permission to reshape the forge", err)
	}
}

// Read comes from EITHER cloud-read or read-repo. Both appear on the four packages
// this handle was built for, and requiring one specific grant would have meant
// re-declaring working code to satisfy a new handle.
func TestReadComesFromEitherReadGrant(t *testing.T) {
	for _, g := range []extension.Grant{extension.CloudRead, extension.ReadRepo} {
		h := capability.For(binding(g))
		if err := h.Forge.Permits("api", "repos/o/r"); err != nil {
			t.Errorf("%s was refused a forge read: %v", g, err)
		}
		if err := h.Forge.Permits("api", "-X", "PUT", "repos/o/r"); !errors.Is(err, capability.ErrNoForgeMutate) {
			t.Errorf("%s was allowed a forge WRITE (err=%v)", g, err)
		}
	}
}

// AN UNKNOWN SUBCOMMAND IS REFUSED, NOT ASSUMED READ. A `gh` verb nobody has
// classified must not arrive holding the safest permission — the same rule the
// kubectl classifier follows, and the reason `gh api -X TRACE` fails above.
func TestUnclassifiedArgvIsRefusedEvenWithEveryGrant(t *testing.T) {
	h := capability.For(binding(
		extension.CloudRead, extension.CloudMutate, extension.SecretCustody, extension.ReadRepo))

	for _, argv := range [][]string{
		{"gist", "create"},
		{"codespace", "ssh"},
		{},
	} {
		if err := h.Forge.Permits(argv...); !errors.Is(err, capability.ErrForgeUnclassified) {
			t.Errorf("argv %v with every grant returned %v — an unclassified command must be "+
				"refused so the table stays the record of what is understood", argv, err)
		}
	}
}

// A binding with no forge-relevant grant gets a handle that refuses, never a nil.
func TestNoForgeGrantYieldsARefusingHandleNotNil(t *testing.T) {
	h := capability.For(binding(extension.ClusterRead))
	if h.Forge == nil {
		t.Fatal("Forge is nil — every handle must be present and refusing")
	}
	if err := h.Forge.Permits("api", "repos/o/r"); !errors.Is(err, capability.ErrNoForgeRead) {
		t.Errorf("Permits = %v, want ErrNoForgeRead", err)
	}
	if _, err := h.Forge.Run("api", "repos/o/r"); err == nil {
		t.Error("Run succeeded on a denied handle")
	}
}

func TestDeniedForgeIsExportedAndRefuses(t *testing.T) {
	if _, err := capability.DeniedForge().Run("api", "x"); err == nil {
		t.Error("DeniedForge().Run succeeded")
	}
}

// The three tables must not overlap. An argv in two groups would classify by
// whichever branch ran first, which is a rule nobody could read off the source.
func TestClassificationTablesAreDisjoint(t *testing.T) {
	reads, mutations, custody := capability.ForgeActions()
	seen := map[string]string{}
	for group, keys := range map[string][]string{
		"read": reads, "mutate": mutations, "custody": custody,
	} {
		for _, k := range keys {
			if prev, dup := seen[k]; dup {
				t.Errorf("%q is in both %s and %s — classification would depend on branch order",
					k, prev, group)
			}
			seen[k] = group
		}
	}
	if len(seen) == 0 {
		t.Fatal("no classified argv at all — the tables emptied and every gh call would be refused")
	}
}
