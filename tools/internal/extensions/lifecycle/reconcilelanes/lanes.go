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
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// THE FOUR-METHOD `Client` INTERFACE IS GONE. Every lane here now takes
// capability.KubeAPI, which is the same surface minus Watch and plus a fence: a
// lane that did not declare cluster-write gets a handle that refuses to patch, at
// the transport, rather than a comment saying it should not.
//
// Watch is deliberately not on the fenced handle — it is a read, and its signature
// carries kube.WatchEvent, which would put kube in front of every extension. The
// daemon still owns the watches and calls the lanes on each event.

// DefaultSecretStore is the ClusterSecretStore the platform's ExternalSecrets
// point at. Shared with `llz ci nudge-argo` in package main, which annotates the
// same store — a second copy of the name would be a second thing to keep in step.
const DefaultSecretStore = "openbao"

// nowUnix is a seam so the revalidation annotation value is deterministic in tests.
var nowUnix = func() int64 { return time.Now().Unix() }

// laneBinding is the declaration a lane acts under, looked up rather than
// reconstructed so a second binding cannot silently widen what it may do.
//
// SELF-SERVICE, LIKE THE GUARDS. capability.RepoForGate established the pattern:
// the binding comes FROM the declaration at the point of use, so the handle a lane
// holds is provably the one the model published. A binding built next to the call
// site would be the lane grading its own homework.
//
// It PANICS on a missing name for the same reason RepoForGate does: a lane whose
// declaration lost its binding is a wiring bug, and a refusing handle would express
// it as the cluster being unreachable — which reads as an outage rather than as a
// missing declaration.
func laneBinding(name string) extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Invariant && b.Name == name {
			return b
		}
	}
	panic("reconcile-actions: no invariant binding named " + name +
		" — its lane builds its cluster handle from one, so its absence is a wiring bug")
}

// Fenced wraps a raw in-cluster client in the grants the named lane declared.
//
// The reconcile daemon hands every lane the same client; this is where that client
// becomes as narrow as the lane's declaration. A lane holding only cluster-read
// gets a client that refuses to patch, at the transport, rather than a comment
// saying it does not.
func Fenced(name string, client capability.KubeClient) capability.KubeAPI {
	return capability.KubeFor(laneBinding(name), client)
}
