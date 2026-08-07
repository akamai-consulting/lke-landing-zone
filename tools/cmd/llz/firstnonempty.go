package main

// firstNonEmpty stays in package main because three files here still use it
// (ci.go, ci_assert_adopter_pin.go, credentials_flagsets.go). internal/onboard
// keeps its own copy — the seventeenth in this campaign to do so.

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
