package main

// secret_apply.go holds the small, generic "apply a K8s Secret" helpers shared
// across the CI commands (cert bootstrap, OpenBao peer CA, …). They used to live
// in ci_seed_approle.go alongside the AppRole seeder; that command was removed
// with the retired AppRole subsystem (ESO now uses Kubernetes auth), but these
// two helpers are provider-agnostic and stayed.

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// kubectlApplyFn pipes a rendered manifest to `kubectl apply -f -` — the native
// form of the scripts' `kubectl create … --dry-run=client -o yaml | kubectl
// apply -f -` idempotent-apply idiom. Seamed for tests.
var kubectlApplyFn = func(manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// genericSecretManifest renders an Opaque Secret with one key. The value is
// base64-encoded into `data:` so no YAML escaping of the secret is needed.
func genericSecretManifest(ns, name, key, value string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
data:
  %s: %s
`, name, ns, key, base64.StdEncoding.EncodeToString([]byte(value)))
}
