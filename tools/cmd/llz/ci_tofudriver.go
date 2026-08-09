package main

// ci_tofudriver.go — the capability wiring for the `tofu-driver` extension
// (internal/tofudriver).

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/tofudriver"
)

func init() {
	tofudriver.Install(tofudriver.Deps{Summary: ghaout.Append})
}
