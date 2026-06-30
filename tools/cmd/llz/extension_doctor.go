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
	// ghVars are verified live (their source of truth is GitHub, not local env). A finding
	// here is fatal ONLY when the live lookup confirms a required, non-seedable variable is
	// absent; an unreachable GitHub yields a non-fatal "unverified" row, so a correctly-
	// configured instance passes and an offline doctor never fails on unknown live state.
	findings = append(findings, liveGHVarFindings(exts, liveGHVarsFn)...)

	fatal := 0
	if len(findings) == 0 {
		fmt.Fprintln(os.Stderr, "extension config: all declared vars/secrets/ghVars satisfied")
		return nil
	}
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

// liveGHVarFindings is the AUTHORITATIVE ghVar check (ghVars are not checked offline): for
// each declared ghVar, is the variable actually SET on GitHub, and for image: true, is the
// LIVE value digest-pinned? The severity rules implement "fail only on a confirmed problem":
//
//   - lookup unavailable (gh down / 404 / unknown repo) → "unverified" advisory, NEVER fatal
//     (an offline doctor cannot prove GitHub state) — only for ghVars with no local material;
//   - required + live-confirmed ABSENT + no local default/override → FATAL (genuinely missing
//     and not auto-seedable);
//   - required + absent but seedable (has a default) → advisory "run seed";
//   - optional + absent → advisory;
//   - image: true + present-but-unpinned → advisory.
//
// So a correctly-configured instance (variable set live) yields NO finding and doctor
// passes; doctor fails only when the live lookup CONFIRMS a required, non-seedable variable
// is absent. lookup is injected for tests.
func liveGHVarFindings(exts []Extension, lookup func(string) (map[string]string, bool)) []configFinding {
	var out []configFinding
	for _, e := range exts {
		vals := varValues(e.Manifest, os.Getenv)
		for _, gv := range e.Manifest.GHVars {
			gv = resolveGHVarEnv(gv, vals)
			seedable := gv.Default != "" || os.Getenv(varOverrideEnv(gv.Name)) != ""
			scope := "repo-level"
			if gv.GHEnv != "" {
				scope = "env " + gv.GHEnv
			}
			live, ok := lookup(gv.GHEnv)
			if !ok {
				if !seedable { // a live-only ghVar we couldn't verify — surface, don't fail
					out = append(out, configFinding{e.Name, "gh-var", gv.Name, "unverified — gh unavailable; ensure it is set on GitHub (" + scope + ")", false})
				}
				continue
			}
			v, set := live[gv.Name]
			switch {
			case !set && gv.Required && !seedable:
				out = append(out, configFinding{e.Name, "gh-var", gv.Name, "REQUIRED but not set on GitHub (" + scope + ") — `gh variable set " + gv.Name + "`", true})
			case !set && gv.Required:
				out = append(out, configFinding{e.Name, "gh-var", gv.Name, "not set on GitHub (" + scope + ") — run `llz extension seed`", false})
			case !set:
				out = append(out, configFinding{e.Name, "gh-var", gv.Name, "not set on GitHub (" + scope + ", optional)", false})
			case gv.Image && !reImageDigest.MatchString(v):
				out = append(out, configFinding{e.Name, "gh-var", gv.Name, "live value not digest-pinned (image: true): " + v, false})
			}
		}
	}
	return out
}
