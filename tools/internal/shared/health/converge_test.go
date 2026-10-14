package health

import "testing"

func TestConvergeStep(t *testing.T) {
	cases := map[int]ConvergeAction{
		0:   ConvergeDone,
		2:   ConvergePoll,
		1:   ConvergeRetryHard,
		3:   ConvergeUnreachable,
		4:   ConvergeAbort,
		255: ConvergeAbort,
	}
	for code, want := range cases {
		if got := ConvergeStep(code); got != want {
			t.Errorf("ConvergeStep(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestPhaseAwareExitCode(t *testing.T) {
	// phase1: a hard-fail (1) is still-installing infra → in-progress (2).
	if got := PhaseAwareExitCode(1, true); got != 2 {
		t.Errorf("phase1 hard-fail = %d, want 2 (in-progress, keep polling)", got)
	}
	// phase1: every other code is unchanged.
	for _, code := range []int{0, 2, 3} {
		if got := PhaseAwareExitCode(code, true); got != code {
			t.Errorf("phase1 code %d = %d, want unchanged", code, got)
		}
	}
	// not phase1: a hard-fail stays terminal (real post-install failures fail fast).
	if got := PhaseAwareExitCode(1, false); got != 1 {
		t.Errorf("non-phase1 hard-fail = %d, want 1 (terminal)", got)
	}
}
