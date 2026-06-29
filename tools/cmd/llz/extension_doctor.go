package main

import (
	"fmt"
	"os"
	"text/tabwriter"
)

// missingExtTools returns the declared tools an extension needs whose executable is not
// on PATH. This is the readiness gap that otherwise hides behind a silent check-skip
// (haveTool): an enabled lint pack whose tool is absent does nothing and says nothing.
func missingExtTools(m extManifest) []extTool {
	var miss []extTool
	for _, t := range m.Tools {
		if !lookable(t.Name) {
			miss = append(miss, t)
		}
	}
	return miss
}

// fixHint tells the operator how to get a missing tool: provision it (if the extension
// declared a mise spec) or install it themselves.
func fixHint(t extTool) string {
	if t.Via != "" {
		return "run `llz extension provision`"
	}
	return "install it"
}

// reportMissingExtTools prints a loud readiness warning for every enabled extension whose
// declared tool is absent — surfaced by doctor. Report-only: the check still skips (an
// opt-in capability should not wedge bootstrap), but the gap is now visible.
func reportMissingExtTools(exts []Extension) {
	for _, e := range exts {
		for _, t := range missingExtTools(e.Manifest) {
			fmt.Printf("  %s %s requires %q — not installed; %s (its check skips until then)\n", yellow("⚠"), e.Name, t.Name, fixHint(t))
		}
	}
}

// warnMissingExtTools warns at enable time that an extension's declared tool is absent —
// so "enabled lint-yaml but yamllint is missing" surfaces immediately, not buried in a
// later `llz lint` skip line.
func warnMissingExtTools(m extManifest) {
	for _, t := range missingExtTools(m) {
		fmt.Fprintf(os.Stderr, "  %s requires %q — not installed; %s, or its check will silently skip\n", m.Name, t.Name, fixHint(t))
	}
}

func checkExtensionConfig(root string, env func(string) string) ([]configFinding, error) {
	exts, err := loadEnabledExtensions(root)
	if err != nil {
		return nil, err
	}
	var out []configFinding
	for _, e := range exts {
		out = append(out, manifestConfigFindings(e.Name, e.Manifest, env)...)
	}
	return out, nil
}

// runExtensionConfigDoctor reports declared vars/secrets that are unsatisfied
// across the enabled set, exiting non-zero when a required secret is missing.
func runExtensionConfigDoctor(root string) error {
	findings, err := checkExtensionConfig(root, os.Getenv)
	if err != nil {
		return err
	}
	if len(findings) == 0 {
		fmt.Fprintln(os.Stderr, "extension config: all declared vars/secrets satisfied")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "EXTENSION\tKIND\tNAME\tSEVERITY\tSTATUS")
	fatal := 0
	for _, f := range findings {
		sev := "info"
		if f.Fatal {
			sev = "REQUIRED"
			fatal++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", f.Ext, f.Kind, f.Name, sev, f.Status)
	}
	tw.Flush()
	if fatal > 0 {
		return fmt.Errorf("%d required input(s) unsatisfied", fatal)
	}
	return nil
}
