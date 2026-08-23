package kube

// secretapply.go holds the generic "apply a K8s Secret" helpers.
//
// THEY HAVE NOW MOVED TWICE FOR THE SAME REASON. They started in
// ci_seed_approle.go, beside a seeder that was later deleted; they were rescued
// into secret_apply.go because they are provider-agnostic; and they are here
// because rendering and applying a Kubernetes Secret is what this package is for.
// Twice they were left behind by the code they happened to be typed next to,
// which is the same filename-as-subject failure that keeps stranding tests.

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

// Create pipes a rendered manifest to `kubectl create -f -`, which FAILS with
// AlreadyExists rather than overwriting.
//
// Apply above is the right verb for a manifest LLZ owns and re-asserts. It is the
// wrong one for a manifest that must never replace what is already there — the
// OpenBao static seal key being the sharpest case: apply is an upsert, so two
// runs that both find the Secret absent both write, and the second silently
// destroys the key that decrypts the first one's seal. AlreadyExists is the
// answer that lets a caller say "someone else got there; leave it alone".
var Create = func(manifest string) (string, error) {
	cmd := exec.Command("kubectl", "create", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// IsAlreadyExists reports whether a kubectl failure was the API refusing to
// create over an existing object.
func IsAlreadyExists(out string) bool {
	return strings.Contains(strings.ToLower(out), "already exists")
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
