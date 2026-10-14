package cli

// firstNonEmpty stays in this package because three files here still use it
// (ci.go, ci_assert_adopter_pin.go, credentials_flagsets.go). Other packages keep
// their own copy of these three lines rather than importing one.

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
