package configreadiness

// Followed isMissingBinary here. It classifies "the tool is not installed" apart
// from "the tool ran and failed" — readiness reports the first as a fixable finding
// and the second as a real fault, and conflating them tells an operator to debug a
// credential when the answer is `brew install`.

import (
	"errors"
	"os/exec"
	"testing"
)

func TestIsMissingBinary(t *testing.T) {
	if !isMissingBinary(&exec.Error{Name: "tflint", Err: exec.ErrNotFound}) {
		t.Error("isMissingBinary(*exec.Error) = false, want true")
	}
	if isMissingBinary(errors.New("some other error")) {
		t.Error("isMissingBinary(generic) = true, want false")
	}
}
