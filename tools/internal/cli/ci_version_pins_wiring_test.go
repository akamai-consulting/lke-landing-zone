package cli

import "testing"

// The test that asserts version-pins is REGISTERED in the ci tree. That is a fact
// about the tree, not about the gate, so it stays with the tree — and it is why
// the naive "pull whatever the moved test references" loop tried to drag every
// cobra constructor in llz into internal/versionpins.

func TestVersionPinsCommandWiring(t *testing.T) {
	var found bool
	for _, c := range ciCmd().Commands() {
		if c.Name() == "version-pins" {
			found = true
		}
	}
	if !found {
		t.Error("`llz ci version-pins` is not registered")
	}
}
