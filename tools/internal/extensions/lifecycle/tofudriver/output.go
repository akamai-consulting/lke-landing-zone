package tofudriver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfbin"
)

// ci_tfoutput.go — `llz ci tf-output <name>`, the assimilation of the scattered
// inline `terraform output -raw/-json <name>` reads across llz-terraform.yml and
// llz-secret-rotation.yml (Phase 5 of docs/designs/forge-abstraction.md /
// instance-slimming.md Lever 2). It centralizes the "No outputs found" hardening:
// each raw read used to risk leaking Terraform's "Warning: No outputs found"
// text into the captured value when state had zero outputs (observed after a
// partial destroy — it broke s5cmd with a bad --endpoint-url). Reading the whole
// output set as `-json` once and extracting the named value returns clean data
// or a clean absence, with no inline warnings.

// EXPORTED, and it is a SEAM as much as a function: the db-report and
// rotate-dbadmin verbs in package main read Terraform outputs through it, and
// their tests stub it. Exporting the var rather than duplicating the exec keeps
// one place where "how this repo asks Tofu for an output" is decided.
//
// OutputRunFn runs `terraform output -json` (all outputs) and returns stdout.
// Package var so tests stub the terraform exec. stderr is discarded — a
// zero-output state prints a warning there that must not reach the value.
var OutputRunFn = func() (string, error) {
	cmd := tfbin.Command("output", "-json")
	out, err := cmd.Output() // stdout only; stderr (the warning) dropped
	return string(out), err
}

func runCITFOutput(name string, asJSON, allowMissing bool, outKey, outFile string) error {
	raw, err := OutputRunFn()
	if err != nil {
		return fmt.Errorf("tf-output: terraform output -json: %w", err)
	}
	value, err := OutputValue(raw, name, asJSON, allowMissing)
	if err != nil {
		return err
	}
	switch {
	case outKey != "":
		if strings.ContainsAny(value, "\n\r") {
			// A multi-line value in a single-line GITHUB_OUTPUT assignment would
			// corrupt the file; those (e.g. kubeconfig_raw) must use --out-file.
			return fmt.Errorf("tf-output: value of %q is multi-line; use --out-file, not --out-key", name)
		}
		return caps.Summary("GITHUB_OUTPUT", outKey+"="+value)
	case outFile != "":
		if err := os.WriteFile(outFile, []byte(value), 0o600); err != nil {
			return fmt.Errorf("tf-output: write %s: %w", outFile, err)
		}
		return nil
	default:
		fmt.Println(value)
		return nil
	}
}

// OutputValue extracts output `name` from a `terraform output -json` blob and
// renders it. The blob is `{name: {value, type, sensitive}}` (or `{}` when the
// state has no outputs). A string value renders raw unless asJSON; any other
// value always renders as compact JSON.
func OutputValue(outputsJSON, name string, asJSON, allowMissing bool) (string, error) {
	var outputs map[string]struct {
		Value json.RawMessage `json:"value"`
	}
	trimmed := strings.TrimSpace(outputsJSON)
	if trimmed == "" {
		trimmed = "{}"
	}
	if err := json.Unmarshal([]byte(trimmed), &outputs); err != nil {
		return "", fmt.Errorf("tf-output: parse terraform output json: %w", err)
	}
	o, ok := outputs[name]
	if !ok {
		if allowMissing {
			return "", nil
		}
		return "", fmt.Errorf("tf-output: no output %q in terraform state", name)
	}
	if !asJSON {
		// A JSON string value renders as its raw contents (the `-raw` contract);
		// a non-string value has no raw form, so fall through to compact JSON.
		var s string
		if err := json.Unmarshal(o.Value, &s); err == nil {
			return s, nil
		}
	}
	return compactJSON(o.Value), nil
}

// compactJSON returns v with insignificant whitespace removed; on any error it
// returns the input verbatim (it is already valid JSON from the decoder).
func compactJSON(v json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, v); err != nil {
		return string(v)
	}
	return b.String()
}
