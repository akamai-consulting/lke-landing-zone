package main

// In-cluster Linode token resolution (secrets-before-apps Phase 2). The
// llz-reconciler Deployment used to consume the ESO-synced linode-api-token
// Secret via an env secretKeyRef — a hard reference that (a) held the pod in
// CreateContainerConfigError until the OpenBao store served (the circular
// dependency that kept the argo-nudge/store-recovery lanes offline during
// bootstrap, and forced the Deployment to sync-wave 6), and (b) served a STALE
// token after every rotation, because Kubernetes never injects env into a
// running pod. The Deployment now mounts the Secret as an OPTIONAL volume and
// the consumers resolve the token lazily per pass: env first (CronJob/CI
// compatibility — those always set LINODE_TOKEN), then the mounted file, which
// kubelet refreshes (~1m) on Secret create/rotate.

import (
	"os"
	"strings"
)

// linodeTokenFile is where the Deployment mounts the optional linode-api-token
// Secret volume. Package var so tests can point it at a fixture.
var linodeTokenFile = "/var/run/secrets/llz/linode-api-token/token"

// inclusterLinodeToken resolves the in-cluster Linode token: LINODE_TOKEN env,
// else the optional Secret volume, else "" (not yet synced — callers no-op or
// error per their contract).
func inclusterLinodeToken() string {
	return inclusterToken("LINODE_TOKEN", linodeTokenFile)
}

// inclusterToken is the shared secrets-before-apps token resolver: the named env
// var first (CronJob/CI compatibility), else the optional Secret volume mounted at
// file (kubelet-refreshed on rotate), else "" (not yet synced). Backs both the
// linode and apl-values-repo resolvers.
func inclusterToken(envVar, file string) string {
	if t := os.Getenv(envVar); t != "" {
		return t
	}
	if b, err := os.ReadFile(file); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}
