package forge

// lane.go — forge-scoped naming for resources shared across forge lanes (the
// e2e case: a github.com lane and a GHES lane on ONE Linode account + TF-state
// bucket). Identifiers that embed only a deployment/region name — the TF state
// key cluster/<env>/…, the LKE cluster label, llz-incluster-<region>, the
// infra-<env> environment — collide across lanes unless the lane name itself is
// forge-unique. LaneSuffix is the single discriminator the harness appends so
// every downstream identifier separates by construction.
//
// GitHub.com deliberately gets the EMPTY suffix so the existing github.com lane's
// identifiers (and its live Linode labels / TF state) are unchanged — no
// migration. Only the newer forges carry a suffix. See
// docs/designs/forge-abstraction.md (§Open questions — the e2e gate).

// LaneSuffix is the stable slug appended to a shared identifier to make it
// forge-unique: "" for GitHub.com, "-ghec"/"-ghes"/"-gitlab" otherwise. It is
// intended to be concatenated onto a base name that is already a legal
// deployment name, e.g. base "e2e" -> "e2e-ghes", which then cascades into
// infra-e2e-ghes, cluster/e2e-ghes/…, and llz-incluster-e2e-ghes.
func LaneSuffix(flavor Flavor) string {
	switch flavor {
	case GitHub:
		return ""
	case GHEC:
		return "-ghec"
	case GHES:
		return "-ghes"
	case GitLab:
		return "-gitlab"
	default:
		return ""
	}
}

// LaneName appends LaneSuffix(flavor) to base. base is assumed to already be a
// legal deployment name (validate.EnvNameRe); callers that need the result to
// stay within the 31-char deployment-name limit must keep base short enough for
// the suffix. Returns base unchanged for GitHub.com.
func LaneName(flavor Flavor, base string) string {
	return base + LaneSuffix(flavor)
}
