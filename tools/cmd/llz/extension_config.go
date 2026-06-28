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
	return out
}
