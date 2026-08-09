package teardown

// deps.go — what this package is HANDED, and what it deliberately is not.
//
// The pattern is `volumes.Deps`: a cloud credential, a cluster client, a shell-out
// seam, and a place to write. The fourth extension predicted the next candidates
// would need the same four, and teardown does — with one addition that says
// something new. See Deps.Confirm.

import (
	"context"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
)

// Kubectl runs kubectl and reports its combined output plus whether it succeeded.
// Named as a type rather than inlined because three files here take it.
type Kubectl func(args ...string) (string, bool)

// Deps carries the capabilities the caller holds and this package does not.
type Deps struct {
	// Client opens the Linode API. Returned with a context because the caller
	// owns the deadline for a destroy, which can legitimately run for minutes.
	Client func() (*linode.Client, context.Context, error)

	// Token is the raw API token, for the paths that build their own client
	// (bucket draining signs its own S3 requests).
	Token func() (string, error)

	// Kubectl drives the cluster during unwedge. Nil is legal: a cluster that is
	// already gone cannot be unwedged, and that is a SKIP rather than an error.
	Kubectl Kubectl

	// Exec is the raw shell-out seam, for the two probes that need a command's
	// exit status rather than its output.
	Exec func(name string, args ...string) ([]byte, error)

	// TempKubeconfig writes a kubeconfig to a temp file and returns a cleanup.
	TempKubeconfig func(pattern string, raw []byte) (string, func(), error)

	// RegionTFVars reads a region's tfvars — where the cluster id and the object
	// storage prefix come from when no flag supplies them.
	RegionTFVars func(tfDir, region string) (tf.TFVars, string, error)

	// Combined runs kubectl against a named kubeconfig and returns its COMBINED
	// output. Separate from Exec because unwedge diagnoses failures from stderr,
	// and Exec's contract is stdout only — a distinction the two seams' original
	// comment in package main called out explicitly.
	Combined func(kubeconfigPath string, args ...string) (string, bool)

	// Summary appends to the GitHub step summary — the destroy job's record of
	// what it deleted, which outlives the log.
	Summary func(envVar string, lines ...string) error

	// TFBin names the terraform/tofu binary. A seam because the repo migrated to
	// OpenTofu and the destroy path must call whichever this instance pins.
	TFBin func() string

	// Confirm reports whether the operator has authorised destructive action —
	// package main's `--yes`.
	//
	// THIS FIELD IS THE ONE THAT IS NOT LIKE THE OTHERS, and it is worth naming.
	// Every other seam here delivers a CAPABILITY: a token, a client, a writer.
	// This one delivers an AUTHORISATION — the answer to "may I", not the means to
	// do it. The grant vocabulary has no way to express it: `cloud-mutate` says
	// this binding may delete cloud resources, and says nothing about whether a
	// human agreed to this particular deletion.
	//
	// That distinction did not come up in the first five extensions because none
	// of them destroys anything. It is a live question for the action ABI: a
	// binding that holds `cloud-mutate` at `destroyed` is the one place where
	// "granted" and "confirmed" must not be the same bit.
	Confirm func() bool
}

// KubectlFor builds a Kubectl bound to a specific kubeconfig. The unwedge path
// resolves a kubeconfig at run time (from $KUBECONFIG_B64, $KUBECONFIG, or the
// Linode API), so it cannot be handed a ready-made client the way an in-cluster
// lane can — the capability it needs is "make me one for THIS file".
func (d Deps) KubectlFor(kubeconfigPath string) Kubectl {
	return func(args ...string) (string, bool) {
		return d.Combined(kubeconfigPath, args...)
	}
}
