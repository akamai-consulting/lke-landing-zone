package objenc

// objprefix.go — resolve the instance's Object Storage label prefix for the CI
// verbs that take a deployment name rather than a loaded spec.
//
// The prefix namespaces every bucket and key label an instance creates
// (clusterspec/objlabels.go). It USED to be the constant `platform` in a handful
// of places, which is precisely why every instance collided on the same global
// bucket names — so the replacement must not have a "just use platform" fallback
// hiding in it. Resolution is from the spec or nothing: these commands all run
// inside the instance checkout, and a wrong prefix here does not fail, it points
// rotation and teardown at ANOTHER instance's buckets and keys.

import (
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// LabelPrefixFor loads the instance spec and returns its label prefix. The
// error names the spec, because that is the only place the answer can come from.
func LabelPrefixFor(what string) (string, error) {
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return "", errObjPrefixUnresolved(what, err)
	}
	p := lz.ObjLabelPrefix()
	if p == "" {
		return "", errObjPrefixUnresolved(what, nil)
	}
	return p, nil
}

func errObjPrefixUnresolved(what string, cause error) error {
	if cause != nil {
		//lint:ignore ST1005 multi-line operator diagnostic: the trailing period closes an embedded remediation line, not a sentence fragment
		return fmt.Errorf("%s needs the instance's Object Storage label prefix, which comes from the LandingZone spec: %w\n"+
			"  Run it from the instance root (the spec is landingzone.yaml).", what, cause)
	}
	//lint:ignore ST1005 multi-line operator diagnostic: the trailing period closes an embedded remediation line, not a sentence fragment
	return fmt.Errorf("%s needs the instance's Object Storage label prefix, but the spec yields an empty one.\n"+
		"  Set spec.instance.objLabelPrefix in landingzone.yaml.", what)
}
