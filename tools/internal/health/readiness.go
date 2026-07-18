package health

import (
	"regexp"
	"strings"
)

// readiness.go holds the predicates for the two cluster readiness gates:
// assert-loki-bootstrapped.sh (Loki Ready + S3-backed) and wait-for-harbor.sh
// (the Harbor workloads to roll out). The kubectl orchestration lives in cmd/llz.

// LokiPodReady reports whether a Loki pod is Ready: phase Running/Succeeded with
// every container ready — the inverse of assert-loki-bootstrapped.sh's not_ready
// selector.
func LokiPodReady(s PodStatus) bool {
	if s.Phase != "Running" && s.Phase != "Succeeded" {
		return false
	}
	for _, c := range s.ContainerStatuses {
		if !c.Ready {
			return false
		}
	}
	return true
}

// lokiS3MarkerRe matches an S3-storage marker in a rendered Loki config (the
// per-line grep alternation from the script; (?m) so `$` anchors a line end).
var lokiS3MarkerRe = regexp.MustCompile(`(?m)object_store:[ \t]*s3|storage:[ \t]*$|aws:|s3:|bucketnames:|endpoint:`)

// LokiConfigUsesS3 reports whether concatenated Loki ConfigMap data references S3
// object storage rather than the read-only-filesystem default — the real signal
// that log persistence works (the kyverno loki-s3-object-store policy mutates
// object_store filesystem→s3). Mirrors the script's two greps: an s3-storage
// marker AND a case-insensitive "s3" mention.
func LokiConfigUsesS3(configText string) bool {
	if !strings.Contains(strings.ToLower(configText), "s3") {
		return false
	}
	return lokiS3MarkerRe.MatchString(configText)
}

// (The Harbor workload sets lived here: HarborRegistryDeployments, and before it
// HarborDeployments/HarborStatefulSets. They fed `llz ci wait-harbor`, whose two
// halves were retired in turn — the pre-seed control-plane gate when robot
// provisioning moved in-cluster (f0aa68f) took its only caller, and the post-seed
// registry wait when converge proved sufficient. Argo app health is the single
// adjudicator of Harbor now.)

