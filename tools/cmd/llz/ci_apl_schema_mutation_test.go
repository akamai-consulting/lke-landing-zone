package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The var-contract check must PASS a file whose leftover placeholders are all
// wired — here, a file with none at all. Only the failing direction was covered
// (TestRunValidateAplValuesVarContractFails), so a `len(unwired) > 0` boundary
// slip to `>= 0` — which rejects every values file, including clean ones —
// went unnoticed.
func TestRunValidateAplValuesVarContractPassesOnACleanFile(t *testing.T) {
	dir := t.TempDir()
	values := filepath.Join(dir, "values.yaml")
	mustWrite(t, values, "apps:\n  loki:\n    enabled: true\n")

	var err error
	out := captureStdout(t, func() { err = runValidateAplValues(values, "", true) })
	if err != nil {
		t.Fatalf("a values file with no unwired placeholders must pass, got %v", err)
	}
	if !strings.Contains(out, "runtime-placeholder var-contract ok") {
		t.Errorf("missing the var-contract ok line: %q", out)
	}
	if !strings.Contains(out, "schema check skipped (--skip-schema)") {
		t.Errorf("--skip-schema should have short-circuited the helm half: %q", out)
	}
}
