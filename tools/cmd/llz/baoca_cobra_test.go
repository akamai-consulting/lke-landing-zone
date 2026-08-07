package main

// baoca_cobra_test.go — the peer-CA flag-set tests, which stayed.
//
// They build the LIVE cobra commands, so only package main can. Same call as
// docsguard's six, manifestguard's one, credrotate's and baoseed's — a test that
// walks the command tree lives where the tree is.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/baoca"
)

func TestExtractOpenbaoCAWiring(t *testing.T) {
	c := baoca.ExtractOpenbaoCACmd()
	if c.Use != "extract-openbao-ca" {
		t.Errorf("Use = %q, want extract-openbao-ca", c.Use)
	}
	if c.Flags().Lookup("required") == nil {
		t.Error("missing --required flag")
	}
}

// withKubectlApply swaps the kube.Apply seam, capturing the applied manifest.
