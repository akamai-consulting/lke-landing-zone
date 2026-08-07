package database

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
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

// THE PUSHED BINDING, PINNED. If bindableStates ever lets a Transition reach
// `operating`, this fails — and the right response is to move rotate-admin there
// and delete the Incomplete note, not to leave both.
func TestRotationStaysAtTheNearestLegalState(t *testing.T) {
	probe := extension.Extension{
		Name: "probe", Short: "x",
		Bindings: []extension.Binding{{Kind: extension.Transition, State: extension.Operating}},
	}
	if errs := probe.Validate(); len(errs) == 0 {
		t.Error("a transition can now reach `operating` — rotate-admin belongs there: " +
			"a scheduled rotation runs against a platform that is already up, and this " +
			"extension's Incomplete note is the record of why it could not say so")
	}
	// The OTHER refusal, and the one that makes the tables disagree: `converged` is
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
		t.Error("secret-custody became legal at `converged` — re-read this extension's " +
			"Incomplete note before moving rotate-admin there")
	}
	if len(Extension().Incomplete) == 0 {
		t.Error("Incomplete emptied — rotate-admin's state is an approximation, and nothing else says so")
	}
}
