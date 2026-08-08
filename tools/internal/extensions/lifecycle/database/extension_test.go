package database

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	e := Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "database-provisioner" {
		t.Errorf("identity drifted: %q", e.Name)
	}
	if e.Always {
		t.Error("an instance with no declared database has nothing to seed")
	}
}

// SEEDING READS THE CLOUD; ROTATION WRITES IT. Linode's admin-password reset is an
// API write against a LIVE database, and seeding only reads the cluster list.
// Collapsing the two grants would hide that seeding cannot break a running
// database and rotation can.
func TestSeedReadsAndRotateWrites(t *testing.T) {
	byName := map[string]extension.Binding{}
	for _, b := range Extension().Bindings {
		byName[b.Name] = b
	}
	seed, ok := byName["seed-admin"]
	if !ok {
		t.Fatal("no seed-admin binding")
	}
	rot, ok := byName["rotate-admin"]
	if !ok {
		t.Fatal("no rotate-admin binding")
	}
	if has(seed, extension.CloudMutate) {
		t.Error("seed-admin claimed cloud-mutate — it reads the cluster list and writes only OpenBao")
	}
	if !has(rot, extension.CloudMutate) {
		t.Error("rotate-admin must declare cloud-mutate: the reset is an API write against a live database")
	}
	for _, b := range []extension.Binding{seed, rot} {
		if !has(b, extension.SecretCustody) {
			t.Errorf("%s: both PLACE credential material", b.Name)
		}
	}
}

// The assertion holds READ grants only, which is what makes it an assertion. If it
// ever needed to write, it would have to become a transition — the forced spelling
// six other extensions in this campaign have had to accept.
func TestAssertionStaysReadOnly(t *testing.T) {
	for _, b := range Extension().Bindings {
		if b.Kind != extension.Assertion {
			continue
		}
		for _, g := range b.Grants {
			switch g {
			case extension.CloudRead, extension.SecretRead, extension.ReadRepo, extension.ClusterRead:
			default:
				t.Errorf("%s declared %q — an assertion may hold read grants only", b.Name, g)
			}
		}
	}
}

func has(b extension.Binding, g extension.Grant) bool {
	for _, have := range b.Grants {
		if have == g {
			return true
		}
	}
	return false
}

// THE PUSHED BINDING, PINNED — AND NOW IN THE OPPOSITE DIRECTION. This test used
// to end by failing if the Incomplete note was emptied, because the note was the
// only record that rotate-admin's state was an approximation. The note is gone and
// this test is not: `Requires: operating` says the same thing in the declaration,
// so what needs guarding is no longer the note's presence but the PRECONDITION's.
//
// Flipping it rather than deleting it is deliberate. A test that pins a workaround
// becomes, the moment the workaround is fixed, a test that pins the FIX — and
// deleting it would have thrown away the one place that records why `seeded` is
// not the whole story.
func TestRotationDeclaresOperatingAsItsPrecondition(t *testing.T) {
	// Neither refusal has been relaxed, and both must stay: Requires exists BECAUSE
	// a transition still cannot reach `operating`, not as a step toward letting it.
	probe := extension.Extension{
		Name: "probe", Short: "x",
		Bindings: []extension.Binding{{Kind: extension.Transition, State: extension.Operating}},
	}
	if errs := probe.Validate(); len(errs) == 0 {
		t.Error("a transition can now reach `operating` — that is the rule Requires was " +
			"built to avoid spending; something claiming to MOVE the platform to " +
			"`operating` is what the restriction exists to prevent")
	}
	// The OTHER refusal, and the one that made the tables disagree: `converged` is
	// the obvious fallback, and grantStates bars custody there — while its own
	// comment says `operating` is in the custody row FOR rotation.
	custodyAtConverged := extension.Extension{
		Name: "probe", Short: "x",
		Bindings: []extension.Binding{{
			Kind: extension.Transition, State: extension.Converged,
			Grants: []extension.Grant{extension.SecretCustody},
		}},
	}
	if errs := custodyAtConverged.Validate(); len(errs) == 0 {
		t.Error("secret-custody became legal at `converged` — the two-table disagreement " +
			"this binding exposed was resolved with Requires, not by widening grantStates")
	}

	var rotate extension.Binding
	for _, b := range Extension().Bindings {
		if b.Name == "rotate-admin" {
			rotate = b
		}
	}
	if rotate.Name == "" {
		t.Fatal("rotate-admin binding is gone")
	}
	if rotate.Requires != extension.Operating {
		t.Errorf("rotate-admin declares Requires=%q, want %q — a scheduled rotation runs "+
			"against a platform that is already up, and `seeded` alone does not say so",
			rotate.Requires, extension.Operating)
	}
	if rotate.State != extension.Seeded {
		t.Errorf("rotate-admin declares State=%q, want %q — the rotation re-places credential "+
			"material, which is what seeding means; only the PRECONDITION was ever missing",
			rotate.State, extension.Seeded)
	}
}

// The two `seeded` transitions must stay addressable BY NAME, because each lane's
// OpenBao handle is built from its own binding. If a name goes stale the panic in
// namedBinding fires at run time; this is what keeps that unreachable.
func TestNamedBindingsResolveForBothLanes(t *testing.T) {
	for _, n := range []string{"seed-admin", "rotate-admin"} {
		b := namedBinding(n)
		if b.Name != n {
			t.Fatalf("namedBinding(%q) returned %q", n, b.Name)
		}
		var custody bool
		for _, g := range b.Grants {
			if g == extension.SecretCustody {
				custody = true
			}
		}
		if !custody {
			t.Errorf("%s no longer declares secret-custody — its KVPut is built from this "+
				"binding, so the write would be refused at run time", n)
		}
	}
}
