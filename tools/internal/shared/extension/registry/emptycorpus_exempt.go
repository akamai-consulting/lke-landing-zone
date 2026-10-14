package registry

// emptyCorpusExempt lists driven gates that legitimately report clean over an
// empty tree, and WHY. Measured, not chosen; entries are deleted the moment they
// stop being needed (TestEveryDrivenGateFailsOnAnEmptyCorpus fails on a stale
// one, the same both-directions rule as the raw-exec ratchets).
//
// IT CARRIES REASONS BECAUSE IT USED TO CARRY BOOLS. An empty map with a
// "shrinks only" note is easy to keep honest while it is empty and useless the
// moment it is not: `"pin-coherence": true` says nothing a reader can weigh, and
// a silent gate registered without a reason is indistinguishable from a gate
// somebody could not be bothered to fix. That is the distinction this whole file
// exists to preserve, so the value is the argument.
var emptyCorpusExempt = map[string]string{
	// ITS SUBJECT DOES NOT EXIST IN A CORPUS. pin-coherence reads ONE optional
	// file — an instance's .copier-answers.yml — and compares the two template
	// pins recorded there. There is no tree to walk and no count to gate on, so
	// "examined nothing" is not a state it can be in.
	//
	// Silence is its designed answer three ways: no answers file (not an
	// instance), either pin absent, or either pin not an exact release tag. The
	// template repo has no answers file at all, so this gate speaks only in an
	// instance — which is a real limit on what `gates: N ran, all clean` means
	// here, and the reason this entry is written out rather than assumed.
	"pin-coherence/pin-coherence": "reads one optional .copier-answers.yml, not a corpus — no instance is a legitimate silent pass",
}
