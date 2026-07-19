package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
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

// tfOutputRunFn runs `terraform output -json` (all outputs) and returns stdout.
// Package var so tests stub the terraform exec. stderr is discarded — a
// zero-output state prints a warning there that must not reach the value.
var tfOutputRunFn = func() (string, error) {
	cmd := exec.Command("terraform", "output", "-json")
	out, err := cmd.Output() // stdout only; stderr (the warning) dropped
	return string(out), err
}

func ciTFOutputCmd() *cobra.Command {
	var asJSON, allowMissing bool
	var outKey, outFile string
	c := &cobra.Command{
		Use:   "tf-output <name>",
		Short: "read one terraform output cleanly (-json internally; no warning leak)",
		Long: "Assimilates the inline `terraform output -raw/-json <name>` reads.\n" +
			"Reads the whole output set via `terraform output -json` (once), so a\n" +
			"zero-output state yields a clean absence instead of leaking Terraform's\n" +
			"'No outputs found' warning into the value. Renders the named output's\n" +
			"value: raw (a string value verbatim; a complex value as compact JSON) by\n" +
			"default, or --json to force compact JSON. Destination: --out-key K appends\n" +
			"`K=<value>` to $GITHUB_OUTPUT; --out-file PATH writes the value there\n" +
			"(e.g. kubeconfig_raw → $KUBECONFIG); otherwise it prints to stdout. A\n" +
			"missing output is an error unless --allow-missing (then it is empty).",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runCITFOutput(args[0], asJSON, allowMissing, outKey, outFile)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "render the value as compact JSON (default: raw for strings)")
	c.Flags().BoolVar(&allowMissing, "allow-missing", false, "a missing output yields an empty value instead of an error")
	c.Flags().StringVar(&outKey, "out-key", "", "append `<key>=<value>` to $GITHUB_OUTPUT instead of printing")
	c.Flags().StringVar(&outFile, "out-file", "", "write the value to this file instead of printing")
	return c
}

func runCITFOutput(name string, asJSON, allowMissing bool, outKey, outFile string) error {
	raw, err := tfOutputRunFn()
	if err != nil {
		return fmt.Errorf("tf-output: terraform output -json: %w", err)
	}
	value, err := tfOutputValue(raw, name, asJSON, allowMissing)
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
		return appendGHAFile("GITHUB_OUTPUT", outKey+"="+value)
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

// tfOutputValue extracts output `name` from a `terraform output -json` blob and
// renders it. The blob is `{name: {value, type, sensitive}}` (or `{}` when the
// state has no outputs). A string value renders raw unless asJSON; any other
// value always renders as compact JSON.
func tfOutputValue(outputsJSON, name string, asJSON, allowMissing bool) (string, error) {
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
