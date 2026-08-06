package extension

// grantstates_internal_test.go — the one test that has to see the table itself.
//
// The rest of this package's tests are `package extension_test`, deliberately: they
// exercise the model the way a caller does. This one cannot, because grantStates is
// unexported and pinning it is the whole point — an external test could only assert
// the table's EFFECTS, and a widened row has no effect until someone declares
// against it, which is exactly the delay this guards against.

import "testing"

// grantStates is judgement transcribed, and the fourth extension changed a row in
// it. Pin the whole table: a widening should be an argued edit to this test and
// its comment, not a quiet one that nothing notices.
//
// The specific regression this guards is the one that produced the change. Adding
// `operating` to cloud-mutate was correct — two shipping reconciler lanes mutate
// Linode Volumes continuously — but the four states it was added to are NOT
// therefore negotiable: `scaffolded` and `configured` have no cloud to mutate, and
// a cloud-mutating binding at `verified` is an assertion that changes what it
// measures.
func TestGrantStatesTableIsPinned(t *testing.T) {
	want := map[Grant][]State{
		SecretCustody: {Seeded, Operating},
		CloudMutate:   {Provisioned, Seeded, Converged, Operating, Destroyed},
		ClusterWrite:  {Provisioned, Seeded, Converged, Operating, Destroyed},
	}
	if len(grantStates) != len(want) {
		t.Fatalf("grantStates has %d rows, want %d — only the MUTATING grants belong here; "+
			"read grants are unrestricted because reading is not what the reviewer is protected from",
			len(grantStates), len(want))
	}
	for g, wantStates := range want {
		got, ok := grantStates[g]
		if !ok {
			t.Errorf("grantStates lost its %q row", g)
			continue
		}
		if len(got) != len(wantStates) {
			t.Errorf("%s: states = %v, want %v", g, got, wantStates)
			continue
		}
		for i := range wantStates {
			if got[i] != wantStates[i] {
				t.Errorf("%s: states = %v, want %v", g, got, wantStates)
				break
			}
		}
	}
	// And the states that must NEVER carry a mutating grant, named rather than
	// implied — these are the ones a widening would reach for first.
	for _, s := range []State{Scaffolded, Configured, Verified} {
		for _, g := range []Grant{CloudMutate, ClusterWrite, SecretCustody} {
			if containsState(grantStates[g], s) {
				t.Errorf("%q became legal at %q — %q is where a mutating grant must not go", g, s, s)
			}
		}
	}
}

// Incomplete is prose, and the only thing worth validating about prose is that it
// is not blank. A `[]string{""}` would mark an extension partial while saying
// nothing about how — worse than not marking it, because the marker implies a
// reader can find out more.
func TestIncompleteRejectsBlankEntries(t *testing.T) {
	base := Extension{
		Name: "probe", Short: "x",
		Bindings: []Binding{{Kind: Gate, State: Scaffolded, Grants: []Grant{ReadRepo}}},
	}
	if errs := base.Validate(); len(errs) != 0 {
		t.Fatalf("fixture should validate: %v", errs)
	}
	base.Incomplete = []string{"the seeder half has not moved"}
	if errs := base.Validate(); len(errs) != 0 {
		t.Errorf("a real note must validate: %v", errs)
	}
	base.Incomplete = []string{"  "}
	if errs := base.Validate(); len(errs) == 0 {
		t.Error("a blank note must be refused — it marks the extension partial without saying how")
	}
}
