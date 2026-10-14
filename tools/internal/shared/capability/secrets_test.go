package capability_test

import (
	"errors"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func binding(grants ...extension.Grant) extension.Binding {
	return extension.Binding{
		Kind: extension.Transition, State: extension.Seeded, Grants: grants,
	}
}

// THE ASSERTION THIS WHOLE FILE EXISTS FOR.
//
// baoread's Verdict distinguishes "bao answered: absent" (a write is safe) from
// "nothing answered" (a write is not). A denied capability is a NEW way to fail to
// read, and if it reported Absent then reducing a binding's permission would make
// a seeder MORE likely to overwrite a live credential — the exact failure baoread
// exists to prevent, reintroduced by the layer meant to make grants real, in the
// direction of data loss.
func TestDeniedReadIsUnknownNeverAbsent(t *testing.T) {
	h := capability.For(binding(extension.ClusterRead)) // no secret grant at all

	val, verdict := h.Secrets.Get("secret/infra/pat", "token")
	if verdict == baoread.Absent {
		t.Fatal("a REFUSED read reported Absent. A seeder reads that as `the path is " +
			"free` and writes over whatever is there. Absent must mean bao answered; a " +
			"refusal is Unknown.")
	}
	if verdict != baoread.Unknown {
		t.Errorf("verdict = %v, want Unknown — a refusal is not an answer about the path", verdict)
	}
	if val != "" {
		t.Errorf("returned %q from a refused read", val)
	}
}

// The same property stated the other way: whatever else changes, a caller that
// only writes on Absent can never be made to write by removing a grant.
func TestNoGrantCombinationMakesARefusalLookWritable(t *testing.T) {
	for _, grants := range [][]extension.Grant{
		{},
		{extension.ClusterRead},
		{extension.ReadRepo, extension.CloudRead},
		{extension.ClusterWrite},
	} {
		h := capability.For(binding(grants...))
		if _, v := h.Secrets.Get("secret/x", "y"); v == baoread.Absent {
			t.Errorf("grants %v produced Absent from a binding with no secret grant", grants)
		}
	}
}

func TestSecretReadGetsReadOnly(t *testing.T) {
	h := capability.For(binding(extension.SecretRead))

	if err := h.Secrets.Permits(); err != nil {
		t.Errorf("secret-read was refused a read: %v", err)
	}
	if err := h.Custodian.PermitsCustody(); !errors.Is(err, capability.ErrNoSecretCustody) {
		t.Errorf("secret-read was allowed custody (err=%v) — reading a credential is not "+
			"permission to place one", err)
	}
	if err := h.Custodian.Put("secret/x", map[string]string{"a": "b"}); err == nil {
		t.Error("Put succeeded without secret-custody")
	}
}

// Custody implies read, for the reason cluster-write implies cluster-read: every
// path that places material reads it back to verify. The converse must not hold,
// and TestSecretReadGetsReadOnly is what pins that.
func TestCustodyImpliesRead(t *testing.T) {
	h := capability.For(binding(extension.SecretCustody))

	if err := h.Secrets.Permits(); err != nil {
		t.Errorf("secret-custody was refused a read: %v — every seeder reads back what it "+
			"placed, so requiring both grants would make the grant line noisier without "+
			"making it more informative", err)
	}
	if err := h.Custodian.PermitsCustody(); err != nil {
		t.Errorf("secret-custody was refused custody: %v", err)
	}
}

// GRANTS ARE READ PER BINDING, NOT PER EXTENSION. An extension holding a
// read-only assertion beside a custody transition must not be able to reach the
// transition's grant from inside the assertion — which is why For takes a Binding
// and why passing the whole extension would hand back the union and undo it.
func TestGrantsAreScopedToTheBindingNotTheExtension(t *testing.T) {
	e := extension.Extension{
		Name: "x", Short: "y", Always: true,
		Bindings: []extension.Binding{
			{Kind: extension.Assertion, State: extension.Verified, Name: "check",
				Grants: []extension.Grant{extension.SecretRead}},
			{Kind: extension.Transition, State: extension.Seeded, Name: "seed",
				Grants: []extension.Grant{extension.SecretCustody}},
		},
	}
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("fixture does not validate: %v", errs)
	}

	check := capability.For(e.Bindings[0])
	if err := check.Custodian.PermitsCustody(); err == nil {
		t.Error("the read-only assertion got custody from its sibling transition — that is " +
			"the union, and moving grants onto Binding was what stopped it")
	}
	seed := capability.For(e.Bindings[1])
	if err := seed.Custodian.PermitsCustody(); err != nil {
		t.Errorf("the seeding transition was refused its own grant: %v", err)
	}
}

// Zero values that work. `Handles{}` must never appear, but a caller assembling
// its own Deps needs somewhere safe to default to — and the recorded scar is that
// a nil seam panics on the first path that needs it, or worse reports success.
func TestExportedDeniedHandlesRefuseRatherThanPanic(t *testing.T) {
	if _, v := capability.DeniedSecrets().Get("p", "f"); v != baoread.Unknown {
		t.Errorf("DeniedSecrets().Get returned %v, want Unknown", v)
	}
	if err := capability.DeniedSecrets().Permits(); !errors.Is(err, capability.ErrNoSecretRead) {
		t.Errorf("DeniedSecrets().Permits() = %v, want ErrNoSecretRead", err)
	}
	if err := capability.DeniedCustodian().Put("p", nil); err == nil {
		t.Error("DeniedCustodian().Put succeeded — a default that silently does nothing is " +
			"the tautology this repo has caught six times in fixtures")
	}
}

// Every field of Handles is populated for every binding, so no caller can meet a
// nil. Checked reflectively-in-spirit rather than by listing fields, because the
// failure this guards is someone ADDING a field and populating it in one of For's
// two return paths.
func TestEveryHandleIsNonNilOnBothPaths(t *testing.T) {
	for _, grants := range [][]extension.Grant{
		{extension.ReadRepo},                             // the early-return path
		{extension.ClusterRead, extension.SecretCustody}, // the full path
		{extension.ClusterWrite},                         // write-implies-read path
	} {
		h := capability.For(binding(grants...))
		if h.Cluster == nil || h.Writer == nil || h.Secrets == nil || h.Custodian == nil {
			t.Errorf("grants %v produced a nil handle: %+v — For has two return paths and a "+
				"new field must be populated on both", grants, h)
		}
	}
}
