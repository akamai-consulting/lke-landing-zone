package clusterspec

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func wizOn(secretPath string) map[string]ComponentToggle {
	on := true
	return map[string]ComponentToggle{
		"wiz":             {Enabled: &on, SecretPath: secretPath},
		"externalSecrets": {Enabled: &on},
	}
}

var wizTeams = []Team{{Name: "team-a", OpenbaoSubtree: "secret/team-a"}}

func TestWizIsOptInAndCarved(t *testing.T) {
	c, ok := LookupComponent("wiz")
	if !ok {
		t.Fatal("wiz component missing")
	}
	if !c.DefaultDisabled {
		t.Error("wiz must be default-disabled: it needs a per-org tenant and a credential nothing here can mint")
	}
	if c.CarvedApp == nil || c.CarvedApp.Namespace != WizNamespace {
		t.Fatalf("wiz must be carved into its own health-inert App in %q, got %+v", WizNamespace, c.CarvedApp)
	}
	es, _ := LookupComponent("externalSecrets")
	if es.CarvedApp.AppWave >= c.CarvedApp.AppWave {
		t.Errorf("externalSecrets wave %d must floor below wiz wave %d", es.CarvedApp.AppWave, c.CarvedApp.AppWave)
	}
	k := RenderCarvedAppKustomization(c, "", "")
	for _, want := range []string{"../../../../../platform-apl/components/wiz", "path: wiz-externalsecret-patch.yaml", "path: wiz-application-patch.yaml", "name: " + WizCredentialSecret, "name: " + WizConnectorApplication} {
		if !strings.Contains(k, want) {
			t.Errorf("carved kustomization missing %q:\n%s", want, k)
		}
	}
}

func TestValidateWizSecretPath(t *testing.T) {
	cases := []struct {
		name, path string
		teams      []Team
		wantErr    string
	}{
		{"under a team subtree", "secret/team-a/wiz/connector", wizTeams, ""},
		{"missing", "", wizTeams, "requires secretPath"},
		{"not the absolute bao form", "team-a/wiz/connector", wizTeams, "must be secret/"},
		{"glob", "secret/team-a/*", wizTeams, "must be secret/"},
		// A platform path validates as a string and is denied on every refresh.
		{"outside every subtree", "secret/linode/wiz", wizTeams, "not under any declared team subtree"},
		// Prefix, not substring: secret/team-ab is NOT under secret/team-a.
		{"sibling prefix", "secret/team-ab/wiz", wizTeams, "not under any declared team subtree"},
		{"the subtree itself", "secret/team-a", wizTeams, "not under any declared team subtree"},
		{"no teams declared", "secret/team-a/wiz/connector", nil, "no spec.teams entry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateWizSecretPath("lab", wizOn(tc.path), tc.teams)
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("want valid, got %v", errs)
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), tc.wantErr) {
				t.Fatalf("want one error containing %q, got %v", tc.wantErr, errs)
			}
		})
	}
	// Disabled: secretPath is not required.
	if errs := validateWizSecretPath("lab", map[string]ComponentToggle{}, nil); len(errs) != 0 {
		t.Errorf("a disabled wiz must not require secretPath, got %v", errs)
	}
}

func TestWizSecretPathIsOnlyAWizKnob(t *testing.T) {
	errs := validateComponentSizing("lab", "harbor", ComponentToggle{SecretPath: "secret/team-a/x"})
	if len(errs) == 0 {
		t.Error("secretPath on a component that does not read it must be rejected")
	}
	if errs := validateComponentSizing("lab", "wiz", ComponentToggle{SecretPath: "secret/team-a/x"}); len(errs) != 0 {
		t.Errorf("secretPath is wiz's knob, got %v", errs)
	}
	merged := mergeToggle(ComponentToggle{SecretPath: "secret/team-a/x"}, ComponentToggle{})
	if merged.SecretPath != "secret/team-a/x" {
		t.Error("spec.defaults must carry secretPath into an env that does not restate it")
	}
}

// The patch replaces spec.data WHOLESALE (CR lists are atomic), so it must carry
// every credential field, each keyed relative to the store's mount.
func TestRenderWizExternalSecretPatch(t *testing.T) {
	p := RenderWizExternalSecretPatch("secret/team-a/wiz/connector")
	var es struct {
		Metadata struct{ Name, Namespace string }
		Spec     struct {
			Data []struct {
				SecretKey string `json:"secretKey"`
				RemoteRef struct{ Key, Property string }
			}
		}
	}
	if err := yaml.Unmarshal([]byte(p), &es); err != nil {
		t.Fatal(err)
	}
	if es.Metadata.Name != WizCredentialSecret || es.Metadata.Namespace != WizNamespace {
		t.Errorf("patch targets %s/%s", es.Metadata.Namespace, es.Metadata.Name)
	}
	if len(es.Spec.Data) != len(WizCredentialFields) {
		t.Fatalf("patch must carry the complete data list (%v), got %d entries", WizCredentialFields, len(es.Spec.Data))
	}
	for i, d := range es.Spec.Data {
		if d.SecretKey != WizCredentialFields[i] || d.RemoteRef.Property != WizCredentialFields[i] {
			t.Errorf("entry %d maps %s←%s", i, d.SecretKey, d.RemoteRef.Property)
		}
		if d.RemoteRef.Key != "team-a/wiz/connector" {
			t.Errorf("remoteRef.key %q must be relative to the KV mount (no secret/ prefix)", d.RemoteRef.Key)
		}
	}
	if strings.Contains(p, "TEAM_SUBTREE") {
		t.Error("the rendered patch must replace the base placeholder")
	}
}

func TestRenderWizApplicationPatch(t *testing.T) {
	p := RenderWizApplicationPatch("prod-east")
	for _, want := range []string{"name: " + WizConnectorApplication, `clusterDisplayName: "prod-east"`, `connectorName: "prod-east"`} {
		if !strings.Contains(p, want) {
			t.Errorf("application patch missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "REPLACE_ME") {
		t.Error("the rendered patch must not carry the base placeholder")
	}
}

// END TO END ACROSS THE KUSTOMIZE BOUNDARY: build the shipped component with the
// rendered patches and require that no placeholder survives and every object
// lands in the namespace the gate reads. Skips (visibly) without kustomize.
func TestWizComponentBuildsWithRenderedPatches(t *testing.T) {
	build := func(dir string) *exec.Cmd {
		if kz, err := exec.LookPath("kustomize"); err == nil {
			return exec.Command(kz, "build", "--load-restrictor", "LoadRestrictionsNone", dir)
		}
		if kc, err := exec.LookPath("kubectl"); err == nil {
			return exec.Command(kc, "kustomize", "--load-restrictor", "LoadRestrictionsNone", dir)
		}
		return nil
	}
	if build("") == nil {
		t.Skip("neither kustomize nor kubectl on PATH — the build half of this test did not run")
	}
	_, file, _, _ := runtime.Caller(0)
	comp, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "platform-apl", "components", "wiz"))
	// Both sides symlink-resolved: macOS temp dirs and /tmp are symlinks, and a
	// relative path computed between unresolved forms walks out of the tree.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if comp, err = filepath.EvalSymlinks(comp); err != nil {
		t.Fatal(err)
	}
	c, _ := LookupComponent("wiz")
	rel, err := filepath.Rel(dir, comp)
	if err != nil {
		t.Fatal(err)
	}
	k := strings.Replace(RenderCarvedAppKustomization(c, "", ""), "../../../../../platform-apl/components/wiz", rel, 1)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("kustomization.yaml", k)
	write("wiz-externalsecret-patch.yaml", RenderWizExternalSecretPatch("secret/team-a/wiz/connector"))
	write("wiz-application-patch.yaml", RenderWizApplicationPatch("lab"))
	out, err := build(dir).CombinedOutput()
	if err != nil {
		t.Fatalf("kustomize build failed: %v\n%s", err, out)
	}
	s := string(out)
	for _, bad := range []string{"TEAM_SUBTREE", "REPLACE_ME"} {
		if strings.Contains(s, bad) {
			t.Errorf("placeholder %s survived the build:\n%s", bad, s)
		}
	}
	for _, want := range []string{"key: team-a/wiz/connector", "connectorName: lab", "clusterDisplayName: lab", "releaseName: " + WizReleaseName, "seccompProfile"} {
		if !strings.Contains(s, want) {
			t.Errorf("built output missing %q", want)
		}
	}
}
