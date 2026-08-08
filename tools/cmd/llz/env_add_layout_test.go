package main

import (
	"os"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/envadd"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envdef"
)

// Tests that travelled with the resolve family but exercise envadd.Run, which is
// scaffold.go's and stays in main. The naive puller tried to drag a PRODUCTION
// function across the boundary to satisfy them — the guard against ciCmd/cliopts.Global/
// globalOpts/main needs to cover production symbols too, not just main-only ones.

func TestRunEnvAddRefusesOutsideAnInstance(t *testing.T) {
	// The whole point of the gate: no files are written. Before it, this call
	// authored a full landingzone.yaml + environments/ + apl-values/ tree here.
	dir := t.TempDir()
	chdir(t, dir)

	err := envadd.Run(false, "lab", envdef.Opts{Region: "us-sea", ObjCluster: "us-sea-1"})
	if err == nil {
		t.Fatal("expected `llz env add` to refuse outside an instance root")
	}
	ents, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(ents) != 0 {
		t.Errorf("nothing must be written outside an instance root; found %v", ents)
	}
}
