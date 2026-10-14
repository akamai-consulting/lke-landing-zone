package cli

// chdir_test.go — package main's copy, kept when checks_test.go went to
// internal/extensions/lint with the lint steps. Eight lines with no production
// caller; a shared testutil package would cost more than the duplication.

import (
	"os"
	"testing"
)

func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}
