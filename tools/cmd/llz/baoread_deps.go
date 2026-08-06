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
