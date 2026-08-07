package clusterspec

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Load reads, decodes, defaults, and returns the LandingZone at path. It does
// NOT validate (call Validate separately) so callers can inspect a structurally
// loadable-but-invalid spec. sigs.k8s.io/yaml decodes via the json tags, matching
// how a future CRD apiserver would unmarshal the same document.
func Load(path string) (*LandingZone, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(b)
}

// Decode parses LandingZone YAML/JSON bytes and applies defaults.
func Decode(b []byte) (*LandingZone, error) {
	var lz LandingZone
	if err := yaml.UnmarshalStrict(b, &lz); err != nil {
		return nil, fmt.Errorf("parse LandingZone: %w", err)
	}
	lz.applyInheritance() // fold spec.defaults under inline envs (no-op when unset)
	lz.Defaults()
	return &lz, nil
}

// Exists reports whether a LandingZone file is present at path.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
