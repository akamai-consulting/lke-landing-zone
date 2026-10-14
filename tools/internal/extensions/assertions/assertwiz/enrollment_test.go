package assertwiz

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"sigs.k8s.io/yaml"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func ok(raw string) observed { return observed{state: readOK, raw: []byte(raw)} }

const (
	esReady    = `{"spec":{"data":[{"remoteRef":{"key":"team-a/wiz/connector"}}]},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`
	esNotReady = `{"spec":{"data":[{"remoteRef":{"key":"team-a/wiz/connector"}}]},"status":{"conditions":[{"type":"Ready","status":"False","reason":"SecretSyncedError","message":"could not get secret data from provider"}]}}`
	brokerUp   = `{"status":{"replicas":1,"readyReplicas":1}}`
	brokerDown = `{"status":{"replicas":1,"readyReplicas":0}}`
)

var (
	credFull      = `{"data":{"clientId":"` + b64("id") + `","clientToken":"` + b64("tok") + `"}}`
	regDone       = `{"data":{"connectorData":"` + b64(`{"ConnectorId":"abc","TunnelToken":"x"}`) + `"}}`
	regFreshChart = `{"data":{"connectorData":"` + b64(`{}`) + `"}}`
)

func healthy() observation {
	return observation{ExternalSecret: ok(esReady), Credential: ok(credFull), Registration: ok(regDone), Broker: ok(brokerUp)}
}

// POSITIVE CONTROL FIRST, so the failure arms below cannot all pass for harness
// reasons.
func TestEnrolledClusterPasses(t *testing.T) {
	cs := evaluate(healthy())
	if !allOK(cs) {
		t.Fatalf("a fully enrolled cluster must pass: %+v", cs)
	}
}

func TestEachBrokenLinkFails(t *testing.T) {
	cases := map[string]func(*observation){
		"externalsecret not ready": func(o *observation) { o.ExternalSecret = ok(esNotReady) },
		"externalsecret absent":    func(o *observation) { o.ExternalSecret = observed{state: readAbsent} },
		"credential missing token": func(o *observation) {
			o.Credential = ok(`{"data":{"clientId":"` + b64("id") + `","clientSecret":"` + b64("s") + `"}}`)
		},
		"credential empty value": func(o *observation) {
			o.Credential = ok(`{"data":{"clientId":"` + b64("id") + `","clientToken":""}}`)
		},
		// The chart seeds `{}`; only the registration Job's success replaces it. A
		// gate that accepted the placeholder would pass on a 401 from the Wiz API.
		"registration still placeholder": func(o *observation) { o.Registration = ok(regFreshChart) },
		"registration absent":            func(o *observation) { o.Registration = observed{state: readAbsent} },
		"broker not ready":               func(o *observation) { o.Broker = ok(brokerDown) },
		"broker absent":                  func(o *observation) { o.Broker = observed{state: readAbsent} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			o := healthy()
			mutate(&o)
			if allOK(evaluate(o)) {
				t.Fatalf("%s must fail the gate", name)
			}
		})
	}
}

// "Could not tell" is not "nothing there", and neither is a pass.
func TestUnreadableIsAFailureNotAnAbsence(t *testing.T) {
	o := healthy()
	o.Broker = observed{state: readUnknown, detail: "Unable to connect to the server: i/o timeout"}
	cs := evaluate(o)
	if allOK(cs) {
		t.Fatal("an unreadable broker must fail")
	}
	last := cs[len(cs)-1]
	if !strings.Contains(last.Detail, "NOT evidence it is absent") || !strings.Contains(last.Detail, "i/o timeout") {
		t.Errorf("unreadable verdict must keep the probe's distinction and its text, got %q", last.Detail)
	}
}

func TestMalformedPayloadsFail(t *testing.T) {
	o := observation{ExternalSecret: ok("{"), Credential: ok("{"), Registration: ok("{"), Broker: ok("{")}
	for _, c := range evaluate(o) {
		if c.OK {
			t.Errorf("%s passed on an unparseable payload", c.Name)
		}
	}
}

func TestEmptyCheckSetIsNotAPass(t *testing.T) {
	if allOK(nil) {
		t.Fatal("zero checks examined must never read as success")
	}
}

// Never print the credential — the verdict names KEYS, not values.
func TestCredentialVerdictNeverPrintsValues(t *testing.T) {
	o := healthy()
	o.Credential = ok(`{"data":{"clientId":"` + b64("super-secret-id") + `"}}`)
	for _, c := range evaluate(o) {
		if strings.Contains(c.Detail, "super-secret-id") {
			t.Fatalf("verdict leaked a credential value: %q", c.Detail)
		}
	}
}

func TestPlaceholderKeyNamesTheRenderStep(t *testing.T) {
	o := healthy()
	o.ExternalSecret = ok(strings.Replace(esNotReady, "team-a/wiz/connector", "TEAM_SUBTREE/wiz/connector", 1))
	c := evaluate(o)[0]
	if c.OK || !c.Final || !strings.Contains(c.Detail, "llz render") {
		t.Errorf("an unpatched placeholder key must fail finally and name `llz render`, got %+v", c)
	}
}

func TestRunPollsThenFailsAndSkipsByTheSpec(t *testing.T) {
	origRead, origExp, origSleep, origSet := readObject, wizExpected, sleep, setResult
	t.Cleanup(func() { readObject, wizExpected, sleep, setResult = origRead, origExp, origSleep, origSet })
	sleep = func(time.Duration) {}
	var result string
	setResult = func(r string) { result = r }

	// Spec says disabled → skip, and nothing is read.
	reads := 0
	readObject = func(string, string) observed { reads++; return observed{state: readAbsent} }
	wizExpected = func(string) (bool, string) { return false, "spec.components.wiz is disabled for lab" }
	if err := runAssertWizEnrollment("lab", 0, 0); err != nil {
		t.Fatalf("disabled by spec must skip, got %v", err)
	}
	if reads != 0 {
		t.Errorf("a spec skip must not read the cluster, read %d objects", reads)
	}
	// A skip exits 0 like a pass; the step output is what tells them apart.
	if result != "skipped" {
		t.Errorf("a skip must publish result=skipped, got %q", result)
	}

	// Spec says enabled, nothing deployed → FAIL (enabled-but-absent is a deploy failure).
	wizExpected = func(string) (bool, string) { return true, "spec.components.wiz IS enabled for lab" }
	if err := runAssertWizEnrollment("lab", 0, 0); err == nil {
		t.Fatal("enabled but absent must fail, not skip")
	}
	if result != "fail" {
		t.Errorf("a failure must publish result=fail, got %q", result)
	}

	// Enabled and healthy → pass.
	h := healthy()
	readObject = func(kind, name string) observed {
		switch {
		case strings.HasPrefix(kind, "externalsecret"):
			return h.ExternalSecret
		case kind == "deployment":
			return h.Broker
		case name == clusterspec.WizCredentialSecret:
			return h.Credential
		default:
			return h.Registration
		}
	}
	if err := runAssertWizEnrollment("lab", 0, 0); err != nil {
		t.Fatalf("healthy enrollment must pass: %v", err)
	}
	if result != "pass" {
		t.Errorf("a pass must publish result=pass, got %q", result)
	}
}

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		t.Fatalf("assert-wiz does not validate: %v", errs)
	}
	if Extension().Component != "wiz" {
		t.Error("assert-wiz must follow the wiz component, or the battery runs it (and fails) on every deployment without a Wiz tenant")
	}
	b := Extension().MustBinding("wiz-enrollment")
	hasRead := false
	for _, g := range b.Grants {
		if g == extension.SecretCustody || g == extension.ClusterWrite {
			t.Errorf("an assertion holds read grants only, got %s", g)
		}
		if g == extension.SecretRead {
			hasRead = true
		}
	}
	if !hasRead {
		t.Error("the gate decodes Secrets; it must declare secret-read")
	}
}

// THE STATIC HALF OF THE CONTRACT. The gate reads objects by the names in
// clusterspec; the bundle in platform-apl/components/wiz creates them. Read the
// SHIPPED manifests and require they produce exactly those names — a rename on
// one side fails here at PR time instead of as a red lane on a real tenant.
func TestShippedBundleProducesTheObjectsTheGateReads(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..", "platform-apl", "components", "wiz", "wiz")

	var es struct {
		Metadata struct{ Name, Namespace string }
		Spec     struct {
			Target struct{ Name string }
			Data   []struct{ SecretKey string }
		}
	}
	mustYAML(t, filepath.Join(dir, "externalsecret.yaml"), &es)
	if es.Metadata.Name != clusterspec.WizCredentialSecret || es.Spec.Target.Name != clusterspec.WizCredentialSecret || es.Metadata.Namespace != clusterspec.WizNamespace {
		t.Errorf("externalsecret.yaml produces %s/%s → %s; the gate reads %s/%s", es.Metadata.Namespace, es.Metadata.Name, es.Spec.Target.Name, clusterspec.WizNamespace, clusterspec.WizCredentialSecret)
	}
	var keys []string
	for _, d := range es.Spec.Data {
		keys = append(keys, d.SecretKey)
	}
	if strings.Join(keys, ",") != strings.Join(clusterspec.WizCredentialFields, ",") {
		t.Errorf("externalsecret.yaml writes keys %v; the chart and the gate read %v", keys, clusterspec.WizCredentialFields)
	}

	var app struct {
		Metadata struct{ Name string }
		Spec     struct {
			Destination struct{ Namespace string }
			Source      struct {
				Helm struct {
					ReleaseName  string
					ValuesObject map[string]any `json:"valuesObject"`
				}
			}
		}
	}
	mustYAML(t, filepath.Join(dir, "application.yaml"), &app)
	if app.Metadata.Name != clusterspec.WizConnectorApplication {
		t.Errorf("application.yaml is %q; the render patch targets %q", app.Metadata.Name, clusterspec.WizConnectorApplication)
	}
	if app.Spec.Source.Helm.ReleaseName != clusterspec.WizReleaseName || app.Spec.Destination.Namespace != clusterspec.WizNamespace {
		t.Errorf("release %q into %q; the gate derives %s and %s from release %q in %q",
			app.Spec.Source.Helm.ReleaseName, app.Spec.Destination.Namespace,
			clusterspec.WizBrokerDeployment(), clusterspec.WizRegistrationSecret(), clusterspec.WizReleaseName, clusterspec.WizNamespace)
	}
	global, _ := app.Spec.Source.Helm.ValuesObject["global"].(map[string]any)
	tok, _ := global["wizApiToken"].(map[string]any)
	sec, _ := tok["secret"].(map[string]any)
	if sec["name"] != clusterspec.WizCredentialSecret || sec["create"] != false {
		t.Errorf("the chart must read the ExternalSecret's Secret %q and never create its own, got %v", clusterspec.WizCredentialSecret, sec)
	}

	// And the rendered per-env patch must target the same objects.
	for _, patch := range []string{clusterspec.RenderWizExternalSecretPatch("secret/team-a/wiz/connector"), clusterspec.RenderWizApplicationPatch("lab")} {
		var m struct{ Metadata struct{ Name string } }
		if err := yaml.Unmarshal([]byte(patch), &m); err != nil {
			t.Fatal(err)
		}
		if m.Metadata.Name != clusterspec.WizCredentialSecret && m.Metadata.Name != clusterspec.WizConnectorApplication {
			t.Errorf("render patch targets %q, which the bundle does not ship", m.Metadata.Name)
		}
	}
}

func mustYAML(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
