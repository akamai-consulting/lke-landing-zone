// Package reconcilelanes holds the llzReconciler's ACTION lanes — the work each
// lane does — separated from the runtime that schedules them.
//
// THE SPLIT IS THE CATALOG'S, NOT AN INVENTION HERE. It lists `reconcile-actions`
// (the lanes) and `reconciler-runtime` (the loop, leader election, the manager) as
// two entries, and calls the lanes "seven separate invariants whose needs differ —
// the clearest case for one-invariant-per-extension". This package is the half that
// could be moved; the runtime stays in package main, where the elector, the health
// port and `func main` live.
//
// WHAT A LANE IS, structurally: a free function taking a Client and returning an
// error. Not a method on the runtime type — which is why this extraction was
// possible at all, and worth saying because the closure census predicted it would
// not be. The census counted 62 references for the reconciler family; most were the
// word "reconciler" inside comments, and the real coupling for these lanes is the
// four-method Client interface below.
package reconcilelanes

import (
	"context"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
)

// Client is the slice of the Kubernetes API the lanes drive. Declared here rather
// than imported so the package depends on the SHAPE it uses; package main's
// Client satisfies it structurally, and so does a test fake.
type Client interface {
	GetJSON(ctx context.Context, path string) (map[string]any, int, error)
	CreateJSON(ctx context.Context, path string, obj any) (int, error)
	MergePatch(ctx context.Context, path string, patch any) error
	Watch(ctx context.Context, path, resourceVersion string, fn func(kube.WatchEvent) error) error
}

// DefaultSecretStore is the ClusterSecretStore the platform's ExternalSecrets
// point at. Shared with `llz ci nudge-argo` in package main, which annotates the
// same store — a second copy of the name would be a second thing to keep in step.
const DefaultSecretStore = "openbao"

// nowUnix is a seam so the revalidation annotation value is deterministic in tests.
var nowUnix = func() int64 { return time.Now().Unix() }
