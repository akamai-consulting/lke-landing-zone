package kube

// secretapply.go holds the generic "apply a K8s Secret" helpers.
//
// THEY HAVE NOW MOVED TWICE FOR THE SAME REASON. They started in
// ci_seed_approle.go, beside a seeder that was later deleted; they were rescued
// into secret_apply.go because they are provider-agnostic; and they are here
// because rendering and applying a Kubernetes Secret is what this package is for.
// Twice they were left behind by the code they happened to be typed next to,
// which is the same filename-as-subject failure this campaign has found stranding
// tests seven times.

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Apply pipes a rendered manifest to `kubectl apply -f -` — the native
// form of the scripts' `kubectl create … --dry-run=client -o yaml | kubectl
// apply -f -` idempotent-apply idiom. Seamed for tests.
var Apply = func(manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// SecretManifest renders an Opaque Secret with one key. The value is
// base64-encoded into `data:` so no YAML escaping of the secret is needed.
func SecretManifest(ns, name, key, value string) string {
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
