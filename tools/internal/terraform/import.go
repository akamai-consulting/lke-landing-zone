package terraform

import (
	"encoding/base64"
	"regexp"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// SelectNodePoolID picks the node pool to import by reproducing the script's
// two-pass match: an exact label match first, then a tag match (the
// `select((.tags // [])|index($t))` fallback). Returns (id, true) on a hit.
func SelectNodePoolID(pools []map[string]any, label string) (uint64, bool) {
	for _, p := range pools {
		if linode.MapString(p, "label") == label {
			return linode.MapUint(p, "id"), true
		}
	}
	for _, p := range pools {
		for _, t := range linode.MapTags(p) {
			if t == label {
				return linode.MapUint(p, "id"), true
			}
		}
	}
	return 0, false
}

// stateIDRe matches the `id = "<value>"` attribute line of `terraform state
// show` output, mirroring the script's `awk -F'"' '/^\s*id\s*=/'`.
var stateIDRe = regexp.MustCompile(`^\s*id\s*=\s*"([^"]*)"`)

// ParseStateID extracts the `id` attribute from `terraform state show` output,
// or "" if the resource is absent / has no id line.
func ParseStateID(stateShowOutput string) string {
	for _, line := range strings.Split(stateShowOutput, "\n") {
		if m := stateIDRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// StubKubeconfig is the minimal valid kubeconfig the script writes so the
// kubernetes/helm/kubectl providers can initialise when the real one is
// unavailable (fresh workspace, cluster gone, or API error).
const StubKubeconfig = "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\ncurrent-context: \"\"\n"

// KubeconfigContent decodes the base64 kubeconfig returned by the Linode API,
// falling back to StubKubeconfig (with stub=true) when the input is empty or not
// decodable — exactly the script's "fetch returned empty → write stub" branch.
func KubeconfigContent(b64 string) (content []byte, stub bool) {
	if b64 == "" {
		return []byte(StubKubeconfig), true
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(dec) == 0 {
		return []byte(StubKubeconfig), true
	}
	return dec, false
}
