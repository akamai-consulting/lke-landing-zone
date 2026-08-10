package cli

// extensions_init.go — call every extension's dependency wiring.
//
// FOUR EXTENSIONS EXPORTED AN Init() AND NOTHING CALLED ANY OF THEM. Each was
// written correctly, sat in the package it belonged to, and was simply never
// invoked after the commands moved out of package main — the sort of gap that a
// compiler cannot see, because an uncalled exported function is a perfectly legal
// program.
//
// The three failure modes it produced are worth keeping in one place, because
// they are ranked in the opposite order to how alarming they look:
//
//	openbao          `llz ci bao-seed-seal-key` died on "SetGitHubSecret not
//	                 installed". LOUD, and the cheapest: it stopped an e2e run at
//	                 the OpenBao bootstrap with a message naming the exact seam.
//	database         "OpenBaoForward not installed" — same shape, same loudness.
//	statepassphrase  THE DANGEROUS ONE. Its defaults were chosen to "do something
//	                 harmless and non-nil": Exec returns (nil, nil) and
//	                 GHJSONPaged returns nil. Unwired, `llz ci
//	                 rotate-state-passphrase` therefore REPORTS SUCCESS HAVING
//	                 ROTATED NOTHING — a credential rotation that leaves every
//	                 secret in place and says it is done.
//
// That inversion is the lesson. The two that fail closed cost a cluster and were
// fixed in an hour; the one that fails OPEN would have been believed. An
// erroring default is not defensive clutter, it is what turns an un-wired seam
// into a bug report instead of a false green.
//
// `func init()` rather than an explicit call from newRootCmd: the wiring must be
// in place before ANY command runs, including one a test constructs directly, and
// baoread_deps.go in this same package already establishes the pattern.
//
// TestEveryExtensionInitIsCalled pins this file against the tree, so a fifth
// extension cannot add an Init() that nothing invokes.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/bootstrapcluster"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/database"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/openbao"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/statepassphrase"
)

func init() {
	openbao.Init()
	database.Init()
	statepassphrase.Init()
	// Currently an empty body. Called anyway, and deliberately: the coupling test
	// requires every exported Init() to be invoked, and exempting the empty one
	// would mean the day it grows a body is the day it silently stops being wired.
	bootstrapcluster.Init()
}
