package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// The committed instance-template _shared apl-overlay files are `llz render`
// output (headerless), so they MUST stay byte-identical to the renderers — a
// `render --check` in a scaffolded instance compares against exactly these. This
// pins them so a change to the render functions that forgets to regenerate the
// template is caught here, not in a ~40-min e2e. (The per-env overlay files are
// generated at instantiation, so only _shared ships in the template.)
func TestTemplateSharedOverlayMatchesRenderers(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "instance-template", "apl-values", "_shared", "apl-overlay")
	cases := []struct {
		file string
		want string
	}{
		{clusterspec.OverlayObjFile, clusterspec.RenderObjOverlayShared()},
		{clusterspec.OverlayAppsFile, clusterspec.RenderAppsOverlayShared()},
	}
	for _, c := range cases {
		got, err := os.ReadFile(filepath.Join(dir, c.file))
		if err != nil {
			t.Fatalf("read committed template %s: %v", c.file, err)
		}
		if string(got) != c.want {
			t.Errorf("committed template _shared/apl-overlay/%s drifted from the renderer — re-run `llz render` and commit.\n--- committed ---\n%s\n--- renderer ---\n%s", c.file, got, c.want)
		}
	}
}
