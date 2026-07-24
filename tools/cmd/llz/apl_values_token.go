package main

// In-cluster apl-values-repo token resolution (secrets-before-apps Phase 2),
// mirroring linode_token.go. The llz-reconciler Deployment runs at sync-wave 0 —
// BEFORE the OpenBao store serves — so the apl-overlay push token must NOT be a
// hard env secretKeyRef: that would (a) hold the pod in CreateContainerConfigError
// until the store served (re-introducing the wave-6 inversion the design retired,
// and starving the whole wave when ordered wrong), and (b) serve a STALE token
// after every rotation, because Kubernetes never injects env into a running pod.
// The Deployment mounts the Secret as an OPTIONAL volume and the reconciler
// resolves the token lazily per pass: env first (tests/CI), then the mounted file,
// which kubelet refreshes (~1m) on Secret create/rotate.

import (
	"os"
	"strings"
)

// aplValuesRepoTokenFile is where the Deployment mounts the optional
// apl-values-repo-token Secret volume. Package var so tests can point it at a fixture.
var aplValuesRepoTokenFile = "/var/run/secrets/llz/apl-values-repo-token/token"

// inclusterAplValuesRepoToken resolves the apl-overlay push token:
// APL_VALUES_REPO_TOKEN env (tests/CI), else the optional Secret volume, else ""
// (not yet synced — the apl-overlay pass no-ops until it appears).
func inclusterAplValuesRepoToken() string {
	if t := os.Getenv("APL_VALUES_REPO_TOKEN"); t != "" {
		return t
	}
	if b, err := os.ReadFile(aplValuesRepoTokenFile); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}
