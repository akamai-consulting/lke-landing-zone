package main

import (
	"reflect"
	"testing"
)

// haInstance seeds a temp tfDir with cluster/<name>.tfvars carrying the given
// ha_role/ha_group and returns the dir.
func haInstance(t *testing.T, clusters map[string][2]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"terraform.tfvars.example": "# template\n"}
	for name, rg := range clusters {
		files[name+".tfvars"] = "region = \"us-x\"\nha_role = \"" + rg[0] + "\"\nha_group = \"" + rg[1] + "\"\n"
	}
	writeCluster(t, dir, files)
	return dir
}

func TestReadTopologyAndHelpers(t *testing.T) {
	dir := haInstance(t, map[string][2]string{
		"east": {"active", "g1"},
		"west": {"standby", "g1"},
		"solo": {"standalone", ""},
	})
	deps, err := readTopology(dir)
	if err != nil {
		t.Fatalf("readTopology: %v", err)
	}

	if got := haMembers(deps); !reflect.DeepEqual(got, []string{"east", "west"}) {
		t.Errorf("haMembers = %v, want [east west]", got)
	}
	if got := byRole(deps, roleActive); !reflect.DeepEqual(got, []string{"east"}) {
		t.Errorf("byRole(active) = %v, want [east]", got)
	}
	if peer, ok := peerOf(deps, "west"); !ok || peer != "east" {
		t.Errorf("peerOf(west) = %q,%v, want east,true", peer, ok)
	}
	if peer, ok := peerOf(deps, "east"); !ok || peer != "west" {
		t.Errorf("peerOf(east) = %q,%v, want west,true", peer, ok)
	}
	if _, ok := peerOf(deps, "solo"); ok {
		t.Error("peerOf(solo) ok=true, want false (standalone has no peer)")
	}
}

func TestReadTopologyDefaultsStandalone(t *testing.T) {
	// A cluster tfvars with no ha_* fields defaults to standalone.
	dir := t.TempDir()
	writeCluster(t, dir, map[string]string{"plain.tfvars": "region = \"us-x\"\n"})
	deps, err := readTopology(dir)
	if err != nil {
		t.Fatalf("readTopology: %v", err)
	}
	if len(deps) != 1 || deps[0].haRole != roleStandalone || deps[0].haGroup != "" {
		t.Errorf("default = %+v, want standalone/empty", deps[0])
	}
}

func TestValidateTopology(t *testing.T) {
	good := []deployment{
		{"a", roleActive, "g1"}, {"b", roleStandby, "g1"}, {"solo", roleStandalone, ""},
	}
	if err := validateTopology(good); err != nil {
		t.Errorf("validateTopology(good) = %v, want nil", err)
	}

	bad := []struct {
		name string
		deps []deployment
	}{
		{"two actives", []deployment{{"a", roleActive, "g1"}, {"b", roleActive, "g1"}}},
		{"active no standby", []deployment{{"a", roleActive, "g1"}}},
		{"role without group", []deployment{{"a", roleActive, ""}}},
		{"standalone with group", []deployment{{"a", roleStandalone, "g1"}}},
		{"invalid role", []deployment{{"a", "leader", "g1"}}},
	}
	for _, tc := range bad {
		if err := validateTopology(tc.deps); err == nil {
			t.Errorf("validateTopology(%s) = nil, want error", tc.name)
		}
	}
}

func TestValidateHAFlags(t *testing.T) {
	ok := []struct{ role, group string }{
		{"", ""}, {"standalone", ""}, {"active", "g1"}, {"standby", "g1"},
	}
	for _, c := range ok {
		if err := validateHAFlags(c.role, c.group); err != nil {
			t.Errorf("validateHAFlags(%q,%q) = %v, want nil", c.role, c.group, err)
		}
	}
	bad := []struct{ role, group string }{
		{"active", ""}, {"standby", ""}, {"standalone", "g1"}, {"leader", "g1"},
	}
	for _, c := range bad {
		if err := validateHAFlags(c.role, c.group); err == nil {
			t.Errorf("validateHAFlags(%q,%q) = nil, want error", c.role, c.group)
		}
	}
}
