package cli

// Helpers the moved tests use, copied across the new package boundary.

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// orAll renders an empty scope as "(all)".
//
// A COPY IN THIS PACKAGE HAS JUST BEEN DELETED IN FAVOUR OF THIS ONE. teardown.go
// carried its own, with a comment saying it was "a local copy of package main's
// helper" — and this is that helper, arriving with reap.go. Ten packages in this
// campaign kept a three-line copy rather than import one for it; this is the first
// time a copy and its original have ended up in the same package, and the copy is
// the one that goes.
func orAll(s string) string {
	if s == "" {
		return "(all)"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "(none — skipping cluster/firewall/BYO-VPC steps)"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
