package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every in-cluster workload that talks to OpenBao must VERIFY its TLS. The
// transport is chosen by openbao.InClusterHTTPClient() from two env vars, so the
// posture lives in the manifests — and a manifest that quietly drops
// OPENBAO_CA_FILE silently downgrades to unverified TLS while still looking
// configured (OPENBAO_SKIP_VERIFY is deliberately kept as the cold-start
// fallback, so its presence alone proves nothing).
//
// Each consumer namespace gets `ca.crt` from its own openbao-ca-bundle
// Certificate, issued from the cluster-scoped `openbao-ca` ClusterIssuer. This
// asserts the full chain per workload: the Certificate exists, it is registered
// in the kustomization (an unregistered file is never applied), and the workload
// mounts it and names it.
func TestInClusterOpenBaoConsumersVerifyTLS(t *testing.T) {
	root := repoRootForTLSTest(t)
	for _, c := range []struct{ name, workload, bundle, kustomization string }{
		{
			name:          "llz-reconciler",
			workload:      "platform-apl/components/llzReconciler/llz-reconciler/deployment.yaml",
			bundle:        "platform-apl/components/llzReconciler/llz-reconciler/openbao-ca-bundle.yaml",
			kustomization: "platform-apl/components/llzReconciler/llz-reconciler/kustomization.yaml",
		},
		{
			name:          "harbor-robot-provisioner",
			workload:      "platform-apl/components/harbor/harbor-robot-provisioner/cronjob.yaml",
			bundle:        "platform-apl/components/harbor/harbor-robot-provisioner/openbao-ca-bundle.yaml",
			kustomization: "platform-apl/components/harbor/kustomization.yaml",
		},
		{
			name:          "broad-pat-rotator",
			workload:      "platform-apl/components/broadPatRotator/broad-pat-rotator/cronjob.yaml",
			bundle:        "platform-apl/components/broadPatRotator/broad-pat-rotator/openbao-ca-bundle.yaml",
			kustomization: "platform-apl/components/broadPatRotator/broad-pat-rotator/kustomization.yaml",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			workload := readForTLSTest(t, root, c.workload)
			// Match the env DECLARATION, not the bare name: these manifests also
			// discuss OPENBAO_CA_FILE in comments, and a substring check on the
			// name alone stays color.Green after the actual env entry is deleted (it
			// did, the first time this test was written).
			for _, want := range []string{
				"- name: OPENBAO_CA_FILE",
				"value: /etc/openbao-ca/ca.crt",
				"mountPath: /etc/openbao-ca",
				"secretName: openbao-ca-bundle",
			} {
				if !strings.Contains(workload, want) {
					t.Errorf("%s does not contain %q — pod→OpenBao traffic falls back to UNVERIFIED TLS", c.workload, want)
				}
			}

			bundle := readForTLSTest(t, root, c.bundle)
			if !strings.Contains(bundle, "kind: Certificate") || !strings.Contains(bundle, "name: openbao-ca") {
				t.Errorf("%s is not a Certificate issued from the openbao-ca ClusterIssuer", c.bundle)
			}
			// The bundle must not be a CA itself — it is a throwaway leaf whose only
			// purpose is the ca.crt cert-manager stamps onto its Secret.
			if strings.Contains(bundle, "isCA: true") {
				t.Errorf("%s sets isCA: true; it should be a disposable leaf", c.bundle)
			}

			// An unregistered manifest is dead YAML: it renders nothing and the
			// workload's optional mount stays empty, silently unverified.
			// Comments are stripped first for the same reason as above — these
			// kustomizations describe the entry in prose right beside it.
			if !listsResource(readForTLSTest(t, root, c.kustomization), "openbao-ca-bundle.yaml") {
				t.Errorf("%s does not list openbao-ca-bundle.yaml as a resource — the Certificate is never applied", c.kustomization)
			}
		})
	}
}

func readForTLSTest(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// repoRootForTLSTest walks up from the package dir to the repo root (the dir
// holding platform-apl/), so the test is independent of where `go test` runs.
func repoRootForTLSTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "platform-apl")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("repo root (platform-apl/) not found — running outside a source checkout")
	return ""
}

// listsResource reports whether a kustomization declares `name` as a list entry,
// ignoring comment lines. A bare strings.Contains would be configreadiness.Satisfied by a comment
// that merely mentions the file, which is exactly how the first version of this
// guard stayed color.Green after the resource entry was deleted.
func listsResource(kustomization, name string) bool {
	for _, line := range strings.Split(kustomization, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") && strings.HasSuffix(trimmed, name) {
			return true
		}
	}
	return false
}
