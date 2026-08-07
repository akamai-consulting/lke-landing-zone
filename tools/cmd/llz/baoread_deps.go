package main

// baoread_deps.go — wires internal/baoread's two seams.
//
// WHICH POD HOLDS ROOT IS PACKAGE MAIN'S BUSINESS, not the read classifier's:
// baoread.RootPod has six callers here, so the installer bakes it into Exec and the
// package's signature carries only the token and the argv. Likewise
// baoread.ParsePodStatus has four other callers and stays; the package asks
// "answering and unsealed?" through a seam rather than parsing status itself.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/baoread"

func init() {
	// Delegating closures, never direct assignment: baoread.ExecFn is
	// themselves test seams, and capturing their value at init would freeze
	// whatever they pointed at before any test swapped them. That bug has cost
	// this campaign twice.
	baoread.InstallWrites(
		func(token, stdin string, args ...string) (string, string, error) {
			return baoread.ExecFn(baoread.RootPod, token, stdin, args...)
		},
	)
	baoread.Install(
		func(token string, args ...string) (string, string, error) {
			return baoread.ExecFn(baoread.RootPod, token, "", args...)
		},
		func(statusJSON string) bool {
			st, ok := baoread.ParsePodStatus(statusJSON)
			return ok && !st.Sealed
		},
	)
}
