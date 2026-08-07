package main

// baoseed_sealkey_cobra_test.go — the seal-key flag-set test, which stayed.
//
// It builds the LIVE cobra command, so only package main can. Same call as
// docsguard's six, manifestguard's one and credrotate's — a test that walks the
// command tree lives where the tree is.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/baoseed"
)

func TestRunCIBaoSeedSealKeyDryRunAndWiring(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		t.Error("dry-run must not exec kubectl")
		return nil, nil
	})
	if err := baoseed.RunSeedSealKey(true, "primary"); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if err := baoseed.RunSeedSealKey(false, ""); err == nil || !strings.Contains(err.Error(), "--region") {
		t.Errorf("missing region = %v, want --region error", err)
	}
	if c := baoseed.BaoSeedSealKeyCmd(); c.Use != "bao-seed-seal-key" {
		t.Errorf("Use = %q, want bao-seed-seal-key", c.Use)
	}
}

func TestRunCIBaoSeedAllRequiresRegion(t *testing.T) {
	if err := baoseed.RunSeedAll(""); err == nil || !strings.Contains(err.Error(), "--region") {
		t.Errorf("baoseed.RunSeedAll(\"\") = %v, want --region error", err)
	}
	if c := baoseed.BaoSeedAllCmd(); c.Use != "bao-seed-all" {
		t.Errorf("Use = %q, want bao-seed-all", c.Use)
	}
}
