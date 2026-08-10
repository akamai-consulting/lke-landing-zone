package assertsecrets

// nowUnix is this package's clock seam for cache-busting annotations.
//
// It travelled to internal/converge with nudge.go, and now here with the harbor
// provisioner kick — the last caller, so package main keeps no clock at all.
// Each package keeps its OWN rather than sharing one (internal/reconcilelanes has
// a third): a shared clock seam would mean one test's fake time silently applying
// to another package's asserts.
import "time"

var nowUnix = func() int64 { return time.Now().Unix() }
