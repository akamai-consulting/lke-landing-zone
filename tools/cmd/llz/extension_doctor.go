package main

import (
	"encoding/json"
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

// runExtensionConfigDoctor reports declared vars/secrets/ghVars that are unsatisfied
// across the enabled set, exiting non-zero when a required input is missing. The OFFLINE
// findings (local seed-readiness) are the gate; the LIVE GitHub check below is advisory.
func runExtensionConfigDoctor(root string) error {
	exts, err := loadEnabledExtensions(root)
	if err != nil {
		return err
	}
	// Declared tools that aren't installed — surfaced here too (not only in core `llz
	// doctor`), so the standalone `extension doctor` is a COMPLETE Configure-readiness
	// check; otherwise an enabled lint/policy pack whose tool is absent silently skips.
	reportMissingExtTools(exts)
	var findings []configFinding
	for _, e := range exts {
		findings = append(findings, manifestConfigFindings(e.Name, e.Manifest, os.Getenv)...)
	}
	fatal := 0
	if len(findings) == 0 {
		fmt.Fprintln(os.Stderr, "extension config: all declared vars/secrets/ghVars locally satisfied")
	} else {
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "EXTENSION\tKIND\tNAME\tSEVERITY\tSTATUS")
		for _, f := range findings {
			sev := "info"
			if f.Fatal {
				sev = "REQUIRED"
				fatal++
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", f.Ext, f.Kind, f.Name, sev, f.Status)
		}
		tw.Flush()
	}
	// Best-effort live GitHub check — advisory, never fails doctor on infra absence.
	if live := liveGHVarFindings(exts, liveGHVarsFn); len(live) > 0 {
		fmt.Fprintln(os.Stderr, "\nlive GitHub variables (advisory — verified against the repo/Environment):")
		for _, x := range live {
			fmt.Fprintf(os.Stderr, "  %s %s\n", yellow("⚠"), x)
		}
	}
	if fatal > 0 {
		return fmt.Errorf("%d required input(s) unsatisfied", fatal)
	}
	return nil
}

// liveGHVarsFn returns the GitHub Actions variables (name→value) actually set for a scope
// (ghEnv=="" → repo-level; otherwise the named Environment), and ok=false when the lookup
// cannot run (no gh, unknown repo, network/auth/404). BEST-EFFORT: doctor never fails just
// because GitHub was unreachable. Seam for tests.
var liveGHVarsFn = func(ghEnv string) (map[string]string, bool) {
	if !lookable("gh") {
		return nil, false
	}
	repo, err := resolveInstanceRepo("", false)
	if err != nil || repo == "" {
		return nil, false
	}
	path := "repos/" + repo + "/actions/variables"
	if ghEnv != "" {
		path = "repos/" + repo + "/environments/" + ghEnv + "/variables"
	}
	raw := ghAPI(path)
	if raw == nil {
		return nil, false
	}
	var parsed struct {
		Variables []ghVar `json:"variables"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return nil, false
	}
	out := map[string]string{}
	for _, v := range parsed.Variables {
		out[v.Name] = v.Value
	}
	return out, true
}

// liveGHVarFindings is the live GitHub trust check: for each enabled extension's declared
// ghVars, is the variable actually SET on GitHub, and for image: true, is the LIVE value
// digest-pinned? This is what closes the gap the offline lint can't — lintManifest pins a
// declared Default, but the value an operator actually stored could be a mutable tag.
// Report-only/advisory: lookup is injected (tests stub it); a scope it can't read is
// skipped (no false alarms when GitHub is unreachable).
func liveGHVarFindings(exts []Extension, lookup func(string) (map[string]string, bool)) []string {
	var out []string
	for _, e := range exts {
		vals := varValues(e.Manifest, os.Getenv)
		for _, gv := range e.Manifest.GHVars {
			gv = resolveGHVarEnv(gv, vals)
			live, ok := lookup(gv.GHEnv)
			if !ok {
				continue
			}
			scope := "repo-level"
			if gv.GHEnv != "" {
				scope = "env " + gv.GHEnv
			}
			v, set := live[gv.Name]
			switch {
			case !set:
				out = append(out, fmt.Sprintf("%s: ghVar %s is not set on GitHub (%s) — `gh variable set %s`", e.Name, gv.Name, scope, gv.Name))
			case gv.Image && !reImageDigest.MatchString(v):
				out = append(out, fmt.Sprintf("%s: ghVar %s (image: true) live value %q is not digest-pinned (…@sha256:<64hex>)", e.Name, gv.Name, v))
			}
		}
	}
	return out
}
