package assertreconciler

// THE GRANT NOW CONSTRAINS. Both of this extension's bindings declare
// `cluster-read` and nothing else, and until the capability layer existed that
// line was a comment: the Deps carried a general `Exec(name, args...)` that could
// invoke `kubectl delete`, `bao`, or anything else on PATH.
//
// This is the first test in the tree that can fail because a declaration was
// exceeded rather than because behaviour changed.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestDeclaredGrantsAreReadOnly(t *testing.T) {
	for _, b := range Extension().Bindings {
		for _, g := range b.Grants {
			if g == extension.ClusterWrite {
				t.Fatalf("%s declares cluster-write — if that is now true the test below is "+
					"asserting the wrong thing and this package's handle must be re-scoped", b)
			}
		}
	}
}

func TestTheHandleRefusesEveryWriteThisExtensionDidNotDeclare(t *testing.T) {
	h := capability.For(Extension().Bindings[0]).Cluster
	for _, argv := range [][]string{
		{"-n", "kube-system", "delete", "configmap", "llz-linode-cidr-firewall-config"},
		{"-n", "argocd", "annotate", "application", "x", "argocd.argoproj.io/refresh=hard"},
		{"apply", "-f", "-"},
		{"-n", "llz-reconciler", "rollout", "restart", "deploy/llz-reconciler"},
		{"exec", "pod/x", "--", "sh", "-c", "rm -rf /"},
	} {
		if err := h.Permits(argv...); err == nil {
			t.Errorf("assert-reconciler's cluster-read handle permitted `kubectl %s`",
				strings.Join(argv, " "))
		}
	}
}

func TestTheHandleStillPermitsWhatTheAssertionsActuallyDo(t *testing.T) {
	h := capability.For(Extension().Bindings[0]).Cluster
	// The six real call sites in this package, by shape.
	for _, argv := range [][]string{
		{"get", "storageclass", "-o", "json"},
		{"-n", "llz-reconciler", "get", "configmap", "llz-token-inventory", "-o", "json", "--ignore-not-found"},
		{"-n", "kube-system", "get", "configmap", "llz-linode-cidr-firewall-config", "-o", "json"},
		{"-n", "llz-reconciler", "get", "deploy", "llz-reconciler", "-o", "json"},
		{"-n", "llz-reconciler", "get", "lease", "llz-reconciler-leader", "-o", "json"},
		{"-n", "llz-reconciler", "describe", "pod", "x"},
	} {
		if err := h.Permits(argv...); err != nil {
			t.Errorf("refused a read this package actually performs: kubectl %s: %v",
				strings.Join(argv, " "), err)
		}
	}
}
