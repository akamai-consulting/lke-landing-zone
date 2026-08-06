package extension

// grantstates_internal_test.go — the one test that has to see the table itself.
//
// The rest of this package's tests are `package extension_test`, deliberately: they
// exercise the model the way a caller does. This one cannot, because grantStates is
// unexported and pinning it is the whole point — an external test could only assert
// the table's EFFECTS, and a widened row has no effect until someone declares
// against it, which is exactly the delay this guards against.

import "testing"

// grantStates is judgement transcribed, and TWO extensions have now changed a row
// in it. Pin the whole table: a widening should be an argued edit to this test and
// its comment, not a quiet one that nothing notices. Both widenings arrived the
// same way — an extraction of code that already shipped and could not be described
// — so treat a failure here as evidence about the table, not about the caller.
//
// The regressions this guards, in the order they were found:
//
//   - cloud-mutate gained `operating` (fourth extension). Correct: two shipping
//     reconciler lanes mutate Linode Volumes continuously. But the four states it
//     was added to are NOT therefore negotiable — `scaffolded` and `configured`
//     have no cloud to mutate, and a cloud-mutating binding at `verified` is an
//     assertion that changes what it measures.
//   - secret-custody gained `provisioned` (eleventh, `cluster-access`). Correct:
//     the cluster-admin kubeconfig is issued by the cloud at provisioning time and
//     holding it is what MAKES seeding possible. The states it was NOT given still
//     matter — custody at `scaffolded` or `configured` would mean a credential
//     exists before anything has been built to issue one, which is the shape of a
//     hardcoded secret rather than a fetched one.
//   - write-repo was ADDED (twenty-eighth, `deliver-docs`) — the first new row
//     rather than a widened one, and the first whose states sit entirely OUTSIDE
//     the mutating middle. `scaffolded` and `upgraded` are the two moments copier
//     runs, which is the only reason a repo write happens at all. It holds exactly
//     the states one shipping extension proved and not the `promoted` that looks
//     obvious; see the row's comment.
func TestGrantStatesTableIsPinned(t *testing.T) {
	want := map[Grant][]State{
		SecretCustody: {Provisioned, Seeded, Operating},
		CloudMutate:   {Provisioned, Seeded, Converged, Operating, Destroyed},
		ClusterWrite:  {Provisioned, Seeded, Converged, Operating, Destroyed},
		WriteRepo:     {Scaffolded, Upgraded},
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
	// And the states that must never carry a SUBSTRATE-mutating grant, named
	// rather than implied — these are the ones a widening would reach for first.
	//
	// THE THREE GRANTS ARE LISTED EXPLICITLY, and after write-repo that is load-
	// bearing rather than incidental. This loop used to read as "no mutating grant
	// belongs at scaffolded/configured/verified", and write-repo is a mutating
	// grant that is legal at `scaffolded`. The rule it was always expressing is
	// narrower: you cannot mutate a SUBSTRATE that does not exist yet, and you
	// cannot mutate anything at `verified` without changing what you are measuring.
	// A repo exists before any substrate does, so it is not covered — and saying so
	// out loud is cheaper than discovering it when the next grant is added.
	for _, s := range []State{Scaffolded, Configured, Verified} {
		for _, g := range []Grant{CloudMutate, ClusterWrite, SecretCustody} {
			if containsState(grantStates[g], s) {
				t.Errorf("%q became legal at %q — %q is where a substrate-mutating grant must not go", g, s, s)
			}
		}
	}
	// write-repo is bound by the same `verified` rule, for the same reason: an
	// assertion that rewrites the repo it is judging has changed its own evidence.
	if containsState(grantStates[WriteRepo], Verified) {
		t.Errorf("write-repo became legal at %q — a binding that rewrites the repo cannot also be evidence about it", Verified)
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
