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
// ONE COPY PER PACKAGE, AND THIS IS THIS PACKAGE'S. Other packages keep their own
// three lines rather than import a helper for it — but where a copy and its
// original land in the SAME package, the copy goes.
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
