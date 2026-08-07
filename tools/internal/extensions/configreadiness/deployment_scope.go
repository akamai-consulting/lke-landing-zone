package configreadiness

import (
	"fmt"
	"os"
	"path/filepath"

	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/terraform"
)

// deployment_scope.go — shared by `llz ci preflight` and `llz ci assert-no-orphans`.
//
// It lives here rather than in either caller because both had the identical bug:
// each was handed the DEPLOYMENT name as --volume-region, which is a Linode-REGION
// filter, so no Volume ever matched and both censuses read 0 however many were
// stranded. Deriving the region in ONE place is what stops the two disagreeing
// again.

// resolveDeploymentScope fills the Linode-region and deployment scoping that the
// orphan census needs, from the rendered cluster tfvars.
//
// WHY IT EXISTS. Both callers — `llz ci preflight` (before an apply) and
// `llz ci assert-no-orphans` (after a destroy) — were handed the DEPLOYMENT name
// as --volume-region. That flag is a Linode-REGION filter, so no Volume ever
// matched it and both censuses reported 0 Volumes however many were stranded:
// the apply-side guard could not warn, and the destroy-side gate could go green
// on a deployment that leaked every one of its Volumes. Deriving the region from
// the tfvars in ONE place is what stops the two sites disagreeing again.
//
// Every returned value keeps an explicit flag if the caller set one; deployment
// only fills the gaps. An unreadable tfvars degrades to the caller's own values
// (and the callers say so) rather than inventing a scope.
func ResolveDeploymentScope(deployment, region, volumeRegion, env string) (string, string, string) {
	if deployment == "" {
		return region, volumeRegion, env
	}
	if env == "" {
		env = deployment
	}
	path := filepath.Join("terraform-iac-bootstrap", "cluster", deployment+".tfvars")
	content, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::warning::--deployment %s but %s is unreadable (%v) — falling back to the flags as given.\n",
			deployment, path, err)
		return region, volumeRegion, env
	}
	v := tf.ParseTFVars(string(content))
	if region == "" {
		region = v.Region
	}
	if volumeRegion == "" {
		volumeRegion = v.Region
	}
	return region, volumeRegion, env
}
