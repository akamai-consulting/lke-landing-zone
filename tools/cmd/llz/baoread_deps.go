package main

// baoread_deps.go — wires internal/baoread's two seams.
//
// WHICH POD HOLDS ROOT IS PACKAGE MAIN'S BUSINESS, not the read classifier's:
// rootOpenbaoPod has six callers here, so the installer bakes it into Exec and the
// package's signature carries only the token and the argv. Likewise
// parseBaoPodStatus has four other callers and stays; the package asks
// "answering and unsealed?" through a seam rather than parsing status itself.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/baoread"

func init() {
	// Delegating closures, never direct assignment: baoExecFn and baoKVPutFn are
	// themselves test seams, and capturing their value at init would freeze
	// whatever they pointed at before any test swapped them. That bug has cost
	// this campaign twice.
	baoread.InstallWrites(
		func(token, stdin string, args ...string) (string, string, error) {
			return baoExecFn(rootOpenbaoPod, token, stdin, args...)
		},
		func(path string, fields map[string]string) error { return baoKVPutFn(path, fields) },
	)
	baoread.Install(
		func(token string, args ...string) (string, string, error) {
			return baoExecFn(rootOpenbaoPod, token, "", args...)
		},
		func(statusJSON string) bool {
			st, ok := parseBaoPodStatus(statusJSON)
			return ok && !st.Sealed
		},
	)
}
