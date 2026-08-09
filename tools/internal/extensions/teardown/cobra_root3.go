package teardown

import "strconv"

// itoaOrUnknown came with the sweep loops that are its only callers.

// itoaOrUnknown renders a count, mapping the -1 "list failed" sentinel to "".
func itoaOrUnknown(n int) string {
	if n < 0 {
		return ""
	}
	return strconv.Itoa(n)
}
