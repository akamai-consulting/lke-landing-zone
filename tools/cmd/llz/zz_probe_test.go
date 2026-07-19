package main

import "testing"

// Probe: half-added HA pair (active written, standby not yet).
func TestProbeHalfAddedPair(t *testing.T) {
	dir := haInstance(t, map[string][2]string{
		"east": {roleActive, "g1"},
	})
	deps, err := readTopology(dir)
	t.Logf("one-active-only: deps=%+v err=%v", deps, err)
	if err != nil {
		t.Logf("REGRESSION: readTopology fails mid-add")
	}
}

// Probe: empty deployment list.
func TestProbeEmptyTopology(t *testing.T) {
	dir := t.TempDir()
	writeCluster(t, dir, map[string]string{})
	deps, err := readTopology(dir)
	t.Logf("empty: deps=%+v err=%v", deps, err)
}

// Probe: unknown ha_role value in tfvars.
func TestProbeUnknownRole(t *testing.T) {
	dir := haInstance(t, map[string][2]string{
		"east": {"primary", "g1"},
	})
	deps, err := readTopology(dir)
	t.Logf("bogus role: deps=%+v err=%v", deps, err)
}

// Probe: standalone that carries a leftover ha_group.
func TestProbeStandaloneWithGroup(t *testing.T) {
	dir := haInstance(t, map[string][2]string{
		"solo": {roleStandalone, "g1"},
	})
	deps, err := readTopology(dir)
	t.Logf("standalone+group: deps=%+v err=%v", deps, err)
}
