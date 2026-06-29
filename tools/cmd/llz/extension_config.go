package main

import "strings"

func varOverrideEnv(name string) string {
	return "LLZ_VAR_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}

// varValues resolves an extension's declared vars to template values: each var's
// Default, overridable by its LLZ_VAR_<NAME> env var.
func varValues(m extManifest, env func(string) string) map[string]string {
	out := map[string]string{}
	for _, v := range m.Vars {
		val := v.Default
		if o := env(varOverrideEnv(v.Name)); o != "" {
			val = o
		}
		out[v.Name] = val
	}
	return out
}

// configFinding is one unsatisfied declared input. Fatal marks a missing required
// secret (doctor exits non-zero); everything else is informational.
type configFinding struct {
	Ext, Kind, Name, Status string
	Fatal                   bool
}

// manifestConfigFindings checks one manifest's declared inputs. Pure (env
// injected) so it table-tests.
func manifestConfigFindings(ext string, m extManifest, env func(string) string) []configFinding {
	var out []configFinding
	for _, v := range m.Vars {
		if v.Default == "" && env(varOverrideEnv(v.Name)) == "" {
			out = append(out, configFinding{ext, "var", v.Name,
				"no default; set " + varOverrideEnv(v.Name) + " or templates render it empty", false})
		}
	}
	for _, s := range m.Secrets {
		if env(s.Name) == "" {
			status := "not set"
			if s.Doc != "" {
				status += " — " + s.Doc
			}
			out = append(out, configFinding{ext, "secret", s.Name, status, s.Required})
		}
	}
	// A GitHub Actions variable's LOCAL seed-readiness is satisfied if it has a Default
	// (seedable) or an LLZ_VAR_<NAME> override is present; otherwise `seed` has nothing to
	// push and the scaffolded workflow reads it empty in CI. NOTE: this is seed-readiness,
	// NOT a live check that the variable is actually set on the GitHub repo/Environment —
	// that needs a `gh variable list` lookup (see the doctor live-lookup open question). A
	// required ghVar with neither default nor override is a fatal finding.
	for _, gv := range m.GHVars {
		if gv.Default == "" && env(varOverrideEnv(gv.Name)) == "" {
			status := "no default/override to seed; set it on GitHub (`gh variable set " + gv.Name + "`) or it renders empty in CI"
			if gv.Doc != "" {
				status += " — " + gv.Doc
			}
			out = append(out, configFinding{ext, "gh-var", gv.Name, status, gv.Required})
		}
	}
	return out
}
