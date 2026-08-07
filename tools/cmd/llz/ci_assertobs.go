package main

// ci_assertobs.go — the capability wiring for the `assert-observability`
// extension (internal/assertobs).

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertobs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/objenc"
)

func init() { installAssertObsDeps() }

func installAssertObsDeps() {
	assertobs.Install(assertobs.Deps{
		Exec:       func(n string, a ...string) ([]byte, error) { return execOutput(n, a...) },
		KubectlOut: kubectlprobe.Out,
		Summary:    ghaout.Append,
		ObjEncDeps: func() objenc.Deps { return objencDeps() },
	})
}
