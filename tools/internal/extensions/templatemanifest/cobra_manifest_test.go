package templatemanifest

// The command-wiring test, and the ONLY test left in this package. Everything else
// in manifest_test.go was about the model and followed it to
// internal/shared/manifest; this one asks whether the cobra command still has its
// three flags, which is a question about the verb that stayed.

import (
	"testing"
)

func TestTemplateManifestCommandWiring(t *testing.T) {
	c := Cmd()
	for _, flag := range []string{"root", "classify", "list"} {
		if c.Flags().Lookup(flag) == nil {
			t.Fatalf("missing --%s flag", flag)
		}
	}
	if err := c.Args(c, []string{"extra"}); err == nil {
		t.Fatal("template-manifest accepted positional args")
	}
}
