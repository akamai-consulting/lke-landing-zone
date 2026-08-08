package main

// A COPY of assertreconciler's containsArg fixture — five lines of substring
// search over an argv, which ci_upgrade_test_gate_test.go also needs. Copied
// rather than exported, for the reason the converge extraction settled: a fixture
// shared across an extraction boundary makes the extracted package a dependency
// of the CLI's own tests.

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
