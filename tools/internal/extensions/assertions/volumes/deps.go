package volumes

// deps.go — what this package needs from its caller, and why each one is a
// parameter rather than something the package reaches for itself.
//
// THIS IS THE FIRST EXTRACTION THAT HAD TO NAME ITS SEAMS. The three gates before
// it (`guard-budgets`, `guard-docs`, `posture-at-rest`) all read files and nothing
// else, so extracting them needed no injection at all — which is exactly why they
// were cheap, and exactly why they proved nothing about extensions that touch a
// cluster or a cloud. Everything below is the cost of the first one that does.
//
// Four seams, and every high-coupling candidate in the catalog's closure census
// needs some subset of the same four: a cloud credential, a cluster client, a
// kubectl shell-out, and a CI-summary sink. That is the action ABI's requirements
// document, derived by extracting rather than by guessing.

import "context"

// KubeGetter is the slice of the in-cluster Kubernetes client this package reads
// through. Declared here rather than imported so the package depends on the SHAPE
// it uses and not on the caller's client type — package main's kubeAPI satisfies
// it structurally, and so does a test fake.
type KubeGetter interface {
	GetJSON(ctx context.Context, path string) (map[string]any, int, error)
}

// Deps carries the capabilities the caller holds and this package does not.
//
// It is one struct rather than four parameters because the SET is the interesting
// thing: read it as the answer to "what does a cloud-mutating extension have to be
// handed?", which is the question the action ABI exists to answer. A binding's
// declared grants should eventually decide which of these fields are populated —
// `cloud-mutate` fills Token, `cluster-read` fills Kube — and a field left nil is
// a capability the extension was not granted.
type Deps struct {
	// Token is the Linode API token. Empty is a hard error at every call site
	// rather than a skip: a storage check that cannot reach the API and passes
	// anyway is the vacuous green this whole gate family exists to refuse.
	Token string

	// Kube is the in-cluster client. Populated on the in-pod reconciler lanes;
	// nil on a CI runner, which uses Kubectl instead. The two paths are not
	// interchangeable — see the comment in AssertEncryption.
	Kube KubeGetter

	// Kubectl shells out to kubectl and returns stdout. Injected rather than
	// called directly so package main keeps owning the one exec seam it routes
	// every shell-out through, and so tests here need no kubectl binary.
	Kubectl func(args ...string) ([]byte, error)

	// Summary appends lines to the GitHub step summary. Injected because the
	// failure REPORT is the product of an assertion — the operator reads it
	// instead of the logs — and a report nothing can capture cannot be tested.
	Summary func(lines ...string) error
}
