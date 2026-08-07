package main

// Helpers the moved login tests use, copied across the package boundary.

import (
	"io"
	"strings"
)

// readSnippet, copied back: decodejson_test.go asserts on it and its subject is
// JSON decoding, not OpenBao login.
func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(b))
}
