package clusteraccess

// acl_classify_test.go — moved from package main's uncovered_helpers_test.go.
// isAlreadyExists decides whether a failed ConfigMap apply is benign, and it lives
// with the ACL now.

import "testing"

// isAlreadyExists decides whether a failed apply is benign. Both spellings occur:
// the Go-style API error and kubectl's prose.
func TestIsAlreadyExists(t *testing.T) {
	for _, s := range []string{
		`Error from server (AlreadyExists): configmaps "x" already exists`,
		"AlreadyExists",
		`Error from server: object already exists`,
	} {
		if !isAlreadyExists(s) {
			t.Errorf("must be treated as benign: %q", s)
		}
	}
	for _, s := range []string{
		"", "Error from server (Forbidden)",
		// Near-misses: neither spelling present. Case matters — a
		// case-insensitive match would wrongly swallow this one.
		"ALREADY EXISTS", "alreadyexists",
	} {
		if isAlreadyExists(s) {
			t.Errorf("must NOT be swallowed as benign: %q", s)
		}
	}
}
