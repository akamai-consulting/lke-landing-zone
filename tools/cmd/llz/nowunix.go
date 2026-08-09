package main

// nowUnix is package main's clock seam for cache-busting annotations.
//
// It travelled to internal/converge with nudge.go, whose force-sync annotation
// used it — but ci_kick_harbor_provisioner.go uses it too and stayed. Rather than
// export a one-line clock from an extension, each package keeps its own: this is
// already the pattern (internal/reconcilelanes has one), and a shared clock seam
// would mean one test's fake time silently applying to another package's asserts.
import "time"

var nowUnix = func() int64 { return time.Now().Unix() }
