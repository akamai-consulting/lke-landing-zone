package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoHardcodedTerraformExec is the regression guard for the OpenTofu
// migration's long tail (ADR 0008).
//
// #356 converted seven call sites from a hardcoded "terraform" to tfBin(), and
// MISSED two — `tf-import` and `tf-apply`. Nothing caught it: the unit tests stub
// the exec seams, `make lint` never shells out, and a local checkout resolves a
// `tofu` on PATH (often via `alias terraform=tofu`), so a clean local run proved
// nothing. It surfaced only when a real e2e apply ran inside the ci-tofu image,
// which deliberately carries no `terraform` binary:
//
//	llz: could not run terraform apply: exec: "terraform": executable file not found in $PATH
//
// That cost a full provisioning cycle to discover. This test makes the eighth
// call site fail at PR time instead.
//
// It scans for the SHELL-OUT forms only — exec.Command / exec.CommandContext /
// runTeed / execOutput with a literal "terraform" as the binary. Prose, flag
// defaults, file paths and directory names that merely contain the word are not
// matched, because forbidding the string outright would be unmaintainable and
// would push people to obfuscate it.
func TestNoHardcodedTerraformExec(t *testing.T) {
	// The binary is always the FIRST argument to these helpers, so anchoring on
	// `(` + optional ctx + the literal keeps this from matching a "terraform"
	// that appears later as a subcommand or flag value.
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`exec\.Command\(\s*"terraform"`),
		regexp.MustCompile(`exec\.CommandContext\(\s*\w+\s*,\s*"terraform"`),
		regexp.MustCompile(`runTeed\(\s*"terraform"`),
		regexp.MustCompile(`execOutput\(\s*"terraform"`),
		regexp.MustCompile(`runCombined\(\s*"terraform"`),
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for _, re := range patterns {
			if loc := re.FindIndex(b); loc != nil {
				line := 1 + strings.Count(string(b[:loc[0]]), "\n")
				t.Errorf("%s:%d execs a hardcoded \"terraform\". The landing zone runs OpenTofu and the CI image carries no `terraform` binary, so this fails at runtime in CI while passing locally. Use tfCommand/tfCommandContext, or tfBin() for the helpers that take a binary name.", name, line)
			}
		}
	}
	// A guard that scanned nothing reports the same color.Green as one that scanned
	// everything — the contract this repo's other guards share.
	if scanned == 0 {
		t.Fatal("scanned 0 Go files — the guard's corpus is empty, so its color.Green means nothing")
	}
}
