package main

import (
	"os"
	"path/filepath"
	"testing"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestVarValuesDefaultAndOverride(t *testing.T) {
	m := extManifest{Vars: []extVar{
		{Name: "team", Default: "platform"},
		{Name: "region", Default: "us-sea"},
	}}
	// defaults
	got := varValues(m, envFrom(nil))
	if got["team"] != "platform" || got["region"] != "us-sea" {
		t.Fatalf("defaults wrong: %v", got)
	}
	// LLZ_VAR_<NAME> override wins
	got = varValues(m, envFrom(map[string]string{"LLZ_VAR_REGION": "eu-west"}))
	if got["region"] != "eu-west" {
		t.Fatalf("override should win, got %q", got["region"])
	}
}

func TestManifestConfigFindings(t *testing.T) {
	m := extManifest{
		Vars: []extVar{
			{Name: "team", Default: "platform"}, // satisfied
			{Name: "domain"},                    // no default → info finding
		},
		Secrets: []extSecret{
			{Name: "OHTTP_KEY_SEED", Required: true, Doc: "HPKE seed"}, // missing → fatal
			{Name: "OPTIONAL_TOKEN"},                                   // missing → info
			{Name: "PRESENT_TOKEN"},                                    // set → satisfied
		},
	}
	env := envFrom(map[string]string{"PRESENT_TOKEN": "x"})
	fs := manifestConfigFindings("ohttp", m, env)
	if len(fs) != 3 {
		t.Fatalf("want 3 findings (domain, OHTTP_KEY_SEED, OPTIONAL_TOKEN), got %d: %+v", len(fs), fs)
	}
	var fatal int
	for _, f := range fs {
		if f.Fatal {
			fatal++
			if f.Name != "OHTTP_KEY_SEED" {
				t.Fatalf("only the required secret should be fatal, got %q", f.Name)
			}
		}
	}
	if fatal != 1 {
		t.Fatalf("want exactly 1 fatal, got %d", fatal)
	}
}

// Configure → Scaffold: a declared var default renders into a scaffolded file.
func TestScaffoldRendersDeclaredVar(t *testing.T) {
	ext := writeExt(t,
		"schemaVersion: 2\nname: cfg\nshort: x\nkind: tool\nvars:\n  - {name: team, default: platform}\nfiles:\n  - {src: f, dst: OUT}\n",
		map[string]string{"f": "owner: <@ .team @>\n"})
	root := t.TempDir()
	if err := runExtensionApply(globalOpts{}, ext, root, false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "OUT"))
	if string(got) != "owner: platform\n" {
		t.Fatalf("declared var should render; got %q", got)
	}
}

func TestConfigDoctorFatalOnRequiredSecret(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "ohttp",
		"schemaVersion: 2\nname: ohttp\nshort: x\nkind: tool\nsecrets:\n  - {name: ZZ_NEVER_SET_SEED, required: true, doc: seed}\n", nil)
	if err := saveExtConfig(root, extConfig{Enabled: []string{"ohttp"}}); err != nil {
		t.Fatal(err)
	}
	if err := runExtensionConfigDoctor(root); err == nil {
		t.Fatal("doctor should fail when a required secret is unset")
	}
}

func TestLintRequiresVarSecretNames(t *testing.T) {
	m := extManifest{Name: "x", Short: "y", Kind: "tool", Stage: StageUniversal,
		Vars: []extVar{{Default: "d"}}, Secrets: []extSecret{{Required: true}}}
	if findings := lintManifest(m); len(findings) != 2 {
		t.Fatalf("an unnamed var + secret should yield 2 findings, got %v", findings)
	}
}
