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

// HarborDeployments are the Harbor Deployments wait-for-harbor rolls out before
// declaring Harbor ready (apl-core 5.0.0; the upstream harbor-nginx ingress is
// absent — the Istio Gateway handles ingress).
func HarborDeployments() []string {
	return []string{"harbor-core", "harbor-jobservice", "harbor-portal", "harbor-registry"}
}

// HarborStatefulSets are the Harbor StatefulSets (Redis + Trivy) harbor-core
// depends on — a belt-and-suspenders rollout check.
func HarborStatefulSets() []string { return []string{"harbor-redis", "harbor-trivy"} }
