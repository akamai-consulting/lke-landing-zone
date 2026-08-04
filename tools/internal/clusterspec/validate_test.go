package clusterspec

import (
	"strings"
	"testing"
)

// A validation that never fires is worse than none — it reads as coverage. objProxy
// enabled without an object-storage cluster must be rejected at the spec layer,
// because the renderers below it cannot decline: the proxy would start with an empty
// upstream, its certificate would name nothing, and the CoreDNS rewrite would have
// no host to rewrite.
func TestValidateRejectsObjProxyWithoutObjectStorage(t *testing.T) {
	enabled := true
	errs := validateEnv("e2e", Environment{
		Components: map[string]ComponentToggle{"objProxy": {Enabled: &enabled}},
	})
	var found bool
	for _, e := range errs {
		if strings.Contains(e.Error(), "objProxy") && strings.Contains(e.Error(), "objectStorage") {
			found = true
		}
	}
	if !found {
		t.Errorf("objProxy without cluster.objectStorage.cluster must be rejected; got %v", errs)
	}
}

// And it must NOT fire when the component is off, or every env that simply does not
// run the gateway becomes un-renderable.
func TestValidateAllowsObjProxyDisabledWithoutObjectStorage(t *testing.T) {
	off := false
	for _, tog := range []map[string]ComponentToggle{
		{"objProxy": {Enabled: &off}},
		{},
	} {
		for _, e := range validateEnv("e2e", Environment{Components: tog}) {
			if strings.Contains(e.Error(), "objProxy") {
				t.Errorf("objProxy disabled must not be rejected: %v", e)
			}
		}
	}
}
