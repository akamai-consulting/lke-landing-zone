package cli

// assertsuite_enablement.go — the one place that can see both the registry and
// the assert battery, so it is where they are joined.
//
// assertsuite cannot ask the registry itself: the registry IMPORTS assertsuite to
// declare it, so a call the other way is a cycle. main holds both and injects the
// answer, which is the same seam pattern every capability in this tree uses.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertsuite"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension/registry"
)

func init() { assertsuite.InstallDisabled(laneDisabledForInstance) }

// laneDisabledForInstance answers whether this instance still has the extension a
// lane belongs to.
//
// EVERY FAILURE PATH ANSWERS "RUN IT". No spec, an unreadable spec, an
// unresolvable registry — all mean the lane runs. An assertion battery that fell
// silent because it could not read a file would report a green run over nothing,
// and the whole reason the skip state exists is that a lane which did not run must
// never look like one that passed. Refusing to skip is the safe direction here,
// exactly inverse to the seeder rule where refusing to WRITE is.
func laneDisabledForInstance(ext string) (string, bool) {
	lz, ok, err := clusterspec.Detected()
	if err != nil || !ok || lz == nil {
		return "", false
	}
	res, err := registry.EnabledFor(lz.Spec.Defaults.Components)
	if err != nil {
		return "", false
	}
	for _, e := range res {
		if e.Extension.Name == ext {
			return e.Reason, !e.Enabled
		}
	}
	// A lane naming an extension the registry does not know is a wiring mistake,
	// not a disabled capability. Running it surfaces that as the lane's own
	// failure rather than hiding it as a skip.
	return "", false
}
