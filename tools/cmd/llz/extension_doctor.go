package main

import (
	"fmt"
	"os"
	"text/tabwriter"
)

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
