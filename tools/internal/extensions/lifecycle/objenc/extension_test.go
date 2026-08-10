package objenc

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("obj-encryption does not validate: %v", err)
		}
	}
}

// THE `→ seeded` GROUP, FINALLY OCCUPIED. PR #15's kind: check|tool menu had no
// seeder skeleton, so 6,874 lines of credential provisioning were inexpressible by
// omission — the single defect the declaration model exists to fix. This is the
// first extension to bind the state, so it is the test of whether the fix was real
// or only argued.
func TestTheSeedBindingIsExpressible(t *testing.T) {
	var seed *extension.Binding
	for i, b := range Extension().Bindings {
		if b.State == extension.Seeded {
			seed = &Extension().Bindings[i]
		}
	}
	if seed == nil {
		t.Fatal("no binding at `seeded` — this extension exists to occupy that state")
	}
	if seed.Kind != extension.Transition {
		t.Errorf("the seed ACTS: kind = %q, want %q", seed.Kind, extension.Transition)
	}
	one := extension.Extension{Name: "probe", Short: "x", Bindings: []extension.Binding{*seed}}
	if errs := one.Validate(); len(errs) > 0 {
		t.Errorf("a transition to seeded holding secret-custody must be expressible: %v", errs)
	}
}

// THE GATE MUST NOT HOLD THE KEY IT PROVES IS WORKING.
//
// A HEAD carrying no SSE-C header returns 400 for an encrypted object and 200 for a
// plaintext one, so the check needs no key at all. An assertion that held custody
// could read what it was meant to prove unreadable — and would pass either way.
func TestTheAssertionHoldsNoCustody(t *testing.T) {
	for _, b := range Extension().Bindings {
		if b.Kind != extension.Assertion {
			continue
		}
		for _, g := range b.Grants {
			if g == extension.SecretCustody {
				t.Errorf("%s holds secret-custody — the encryption gate proves the key works "+
					"by NOT having it; a keyless HEAD is what separates the two states", b)
			}
		}
	}
}

// The three bindings differ in what they touch because they differ in risk. Collapse
// them and the union hands the one-shot seeder a cluster grant and the read-only gate
// the key — which is the over-granting the per-binding scoping exists to prevent.
func TestTheThreeMomentsStaySeparate(t *testing.T) {
	e := Extension()
	if len(e.Bindings) != 3 {
		t.Fatalf("want three bindings (seed, proxy, gate), got %d", len(e.Bindings))
	}
	seen := map[extension.State]bool{}
	for _, b := range e.Bindings {
		if seen[b.State] {
			t.Errorf("two bindings at %q — each moment is its own", b.State)
		}
		seen[b.State] = true
	}
	for _, want := range []extension.State{extension.Seeded, extension.Operating, extension.Verified} {
		if !seen[want] {
			t.Errorf("no binding at %q", want)
		}
	}
}

// Opt-in, like import-brownfield: objProxy is a spec component an instance turns
// on. `llz ci seed-ssec-key` no-ops when it is off, which is the behaviour this
// field records.
func TestObjEncryptionIsOptIn(t *testing.T) {
	if Extension().Always {
		t.Error("obj-encryption follows spec.components.objProxy; it must not ship enabled")
	}
}

// seedBinding() panics if it finds nothing, so this is what keeps that
// unreachable in a built binary.
func TestSeedBindingIsFound(t *testing.T) {
	b := seedBinding()
	if b.Kind != extension.Transition || b.State != extension.Seeded {
		t.Fatalf("seedBinding returned %s — ObjencDeps builds its OpenBao custody handle "+
			"from this, so the wrong binding means the wrong grants", b)
	}
	var custody bool
	for _, g := range b.Grants {
		if g == extension.SecretCustody {
			custody = true
		}
	}
	if !custody {
		t.Error("the seed binding no longer declares secret-custody — ObjencDeps.KVPut is " +
			"built from it, so this would ship a seeder whose writes are refused at runtime")
	}
}

// THE CONVERSION IS LOAD-BEARING, and this is what says so. ObjencDeps used to
// call baoread.KVGetFieldOK / baoread.KVPut directly, which meant `secret-custody`
// on the binding above was a claim with nothing behind it. Now the handles come
// from capability.For(seedBinding()) — so if the grant were removed, the writes
// would refuse.
//
// Asserted by building handles from a binding with the grant STRIPPED, rather than
// by editing the real declaration: the point is that the wiring reads the grant,
// not that today's declaration happens to carry it.
func TestCustodyHandleIsBuiltFromTheGrant(t *testing.T) {
	stripped := seedBinding()
	stripped.Grants = []extension.Grant{extension.ClusterRead}

	h := capability.For(stripped)
	if err := h.Custodian.Put("secret/objenc/ssec", map[string]string{"key": "x"}); err == nil {
		t.Error("a binding without secret-custody was allowed to place the SSE-C key — the " +
			"grant is not reaching the handle, so the declaration is decorative again")
	}

	// And the half that matters most: a refused READ must not look like an empty
	// path. Absent is what tells this extension to MINT, and minting a second
	// SSE-C key makes every object written under the first unreadable.
	if _, v := h.Secrets.Get("secret/objenc/ssec", "key"); v == baoread.Absent {
		t.Fatal("a refused read reported Absent — this extension reads Absent as `no key " +
			"yet` and mints one. Every object under the previous key would become " +
			"unreadable, and the trigger would be a NARROWER grant.")
	}
}
