package cli

// ci_assertobs.go — the capability wiring for the `assert-observability`
// extension (internal/assertobs).

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertobs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/objenc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

func init() { installAssertObsDeps() }

func installAssertObsDeps() {
	assertobs.Install(assertobs.Deps{
		Exec:       execOutput,
		KubectlOut: kubectlprobe.Out,
		Summary:    ghaout.Append,
		ObjEncDeps: func() objenc.Deps { return objenc.ObjencDeps() },
	})
}
