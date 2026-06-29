package main

import (
	"fmt"
	"os"
	"text/tabwriter"
)

// missingExtTools returns the declared tools: an extension needs that are not on PATH.
// This is the readiness gap that otherwise hides behind a silent check-skip (haveTool):
// an enabled lint pack whose tool is absent does nothing and, without this, says nothing.
func missingExtTools(m extManifest) []string {
	var miss []string
	for _, t := range m.Tools {
		if !lookable(t) {
			miss = append(miss, t)
		}
	}
	return miss
}

// reportMissingExtTools prints a loud readiness warning for every enabled extension whose
// declared tool is absent — surfaced by doctor. Report-only: the check still skips (an
// opt-in capability should not wedge bootstrap), but the gap is now visible.
func reportMissingExtTools(exts []Extension) {
	for _, e := range exts {
		for _, t := range missingExtTools(e.Manifest) {
			fmt.Printf("  %s %s requires %q — not installed; its check will skip until you install it\n", yellow("⚠"), e.Name, t)
		}
	}
}

// warnMissingExtTools warns at enable time that an extension's declared tool is absent —
// so "enabled lint-yaml but yamllint is missing" surfaces immediately, not buried in a
// later `llz lint` skip line.
func warnMissingExtTools(m extManifest) {
	for _, t := range missingExtTools(m) {
		fmt.Fprintf(os.Stderr, "  %s requires %q — not installed; install it or its check will silently skip\n", m.Name, t)
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
		return fmt.Errorf("%d required secret(s) unsatisfied", fatal)
	}
	return nil
}
