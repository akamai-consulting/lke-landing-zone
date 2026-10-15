package clusterspec

import (
	"strings"
	"testing"
)

// objProxyErrors returns the validation errors that mention the retired component.
func objProxyErrors(t *testing.T, tog map[string]ComponentToggle) []string {
	t.Helper()
	var out []string
	for _, e := range validateEnv("e2e", Environment{Components: tog}) {
		if strings.Contains(e.Error(), "components.objProxy") {
			out = append(out, e.Error())
		}
	}
	return out
}

// THE RETIRED GATEWAY IS REFUSED, NOT IGNORED. A spec still naming objProxy came
// from an instance that may hold SSE-C objects only the proxy could read, so the
// key must stop the render with an actionable message — the bare "unknown
// component" an unrecognised key gets would not tell the operator what they stand
// to lose.
func TestValidateRefusesTheRetiredObjProxyComponent(t *testing.T) {
	on, off := true, false
	for name, tog := range map[string]map[string]ComponentToggle{
		"enabled":  {"objProxy": {Enabled: &on}},
		"disabled": {"objProxy": {Enabled: &off}},
		"default":  {"objProxy": {}},
	} {
		errs := objProxyErrors(t, tog)
		if len(errs) != 1 {
			t.Errorf("%s: want exactly one objProxy error, got %v", name, errs)
			continue
		}
		if !strings.Contains(errs[0], "no longer exists") || !strings.Contains(errs[0], "Delete this key") {
			t.Errorf("%s: the error must say the component is gone and what to do: %q", name, errs[0])
		}
		if strings.Contains(errs[0], "unknown component") {
			t.Errorf("%s: a retired component must get the migration message, not the generic one: %q", name, errs[0])
		}
	}
}

// ONLY AN ENABLED GATEWAY carries the data-loss warning. Printing it for a key that
// says `enabled: false` would teach operators to skim past it — and the one time it
// matters is the instance that actually wrote SSE-C objects.
func TestValidateWarnsAboutSSECObjectsOnlyWhenTheGatewayWasEnabled(t *testing.T) {
	on, off := true, false
	if errs := objProxyErrors(t, map[string]ComponentToggle{"objProxy": {Enabled: &on}}); len(errs) != 1 ||
		!strings.Contains(errs[0], "secret/obj/ssec") || !strings.Contains(errs[0], "cannot read them") {
		t.Errorf("an ENABLED gateway must warn that its SSE-C objects become unreadable: %v", errs)
	}
	if errs := objProxyErrors(t, map[string]ComponentToggle{"objProxy": {Enabled: &off}}); len(errs) != 1 ||
		strings.Contains(errs[0], "secret/obj/ssec") {
		t.Errorf("a disabled gateway wrote nothing under SSE-C and must not carry the warning: %v", errs)
	}
}

// And a spec that never named it is untouched.
func TestValidateIsSilentAboutObjProxyWhenAbsent(t *testing.T) {
	if errs := objProxyErrors(t, map[string]ComponentToggle{}); len(errs) != 0 {
		t.Errorf("a spec without objProxy must not be told about it: %v", errs)
	}
}
