package cli

// ci_tofudriver.go — the capability wiring for the `tofu-driver` extension
// (internal/tofudriver).

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/tofudriver"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
)

func init() {
	tofudriver.Install(tofudriver.Deps{Summary: ghaout.Append})
}
