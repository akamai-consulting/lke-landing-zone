package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/buildpreflight"
)

// Helpers the returned tests use, copied back across the boundary.

// writeMiniInstance lays down the smallest tree Run recognizes: a
// landingzone.yaml (so InstancePresent is true) and one environments/<env>.yaml.
func writeMiniInstance(t *testing.T, dir string, envs ...string) {
	t.Helper()
	lz := "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: mini\nspec:\n  instance:\n    repo: my-org/mini\n"
	if err := os.WriteFile(filepath.Join(dir, "landingzone.yaml"), []byte(lz), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "environments"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range envs {
		body := fmt.Sprintf("apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: ClusterDefinition\nmetadata:\n  name: %s\nspec:\n  cluster:\n    region: us-sea\n", e)
		if err := os.WriteFile(filepath.Join(dir, "environments", e+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "terraform-iac-bootstrap", "cluster"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// stubGitHub makes every gh API call return the given JSON bodies, keyed by a
// substring of the request path. A path with no entry 404s (returns an error).
func stubGitHub(t *testing.T, bodies map[string]any) {
	t.Helper()
	orig := buildpreflight.GHAPIJSON
	t.Cleanup(func() { buildpreflight.GHAPIJSON = orig })
	buildpreflight.GHAPIJSON = func(path string, out any) error {
		// Longest match wins: "repos/<r>" is a prefix of "repos/<r>/contents/…",
		// and map iteration order would otherwise pick between them at random.
		best, found := "", false
		for frag := range bodies {
			if strings.Contains(path, frag) && len(frag) > len(best) {
				best, found = frag, true
			}
		}
		if !found {
			return fmt.Errorf("gh api %s: HTTP 404", path)
		}
		b, _ := json.Marshal(bodies[best])
		return json.Unmarshal(b, out)
	}
}
