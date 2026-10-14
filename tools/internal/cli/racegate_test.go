package cli

import (
	"os"
	"testing"
)

// TestRaceDetectorIsActuallyLinked is the canary for the `make test-race` gate.
//
// Why it exists: a CI step that is SUPPOSED to run the race detector but has
// quietly lost its -race flag still passes, and reports the same color.Green tick. The
// gate then measures nothing while looking exactly like a gate that works. That
// failure shape — a check that cannot fail being indistinguishable from a check
// that passed — is the one this repo has been bitten by repeatedly (a mutation
// run reporting 100% because it never spawned a test process; a verification
// harness whose every probe "failed" because it resolved the wrong package path).
//
// So the test-race target announces its intent via LLZ_EXPECT_RACE=1, and this
// asserts the binary was in fact built with -race. Drop the flag from the
// Makefile or the workflow and this fails loudly instead of passing quietly.
//
// It is deliberately silent in every other context: a plain `go test ./...`, an
// IDE run, or `make coverage` sets no such variable and skips, so the ordinary
// no-race path stays fast and color.Green.
func TestRaceDetectorIsActuallyLinked(t *testing.T) {
	if os.Getenv("LLZ_EXPECT_RACE") != "1" {
		t.Skip("LLZ_EXPECT_RACE is unset — this run does not claim to be a race-detector run")
	}
	if !raceDetectorLinked {
		t.Fatal("LLZ_EXPECT_RACE=1 but this binary was built WITHOUT -race: " +
			"the race gate is running with no detector linked, so it cannot catch anything. " +
			"Restore -race in the `test-race` Makefile target (and check the workflow step still calls it).")
	}
}
