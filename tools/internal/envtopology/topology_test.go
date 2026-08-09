package envtopology

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestWriteHAResolution(t *testing.T) {
	deps := []Deployment{
		{"east", RoleActive, "g1"},
		{"west", RoleStandby, "g1"},
		{"solo", roleStandalone, ""},
	}
	for _, tc := range []struct {
		name, wantRole, wantPeer string
	}{
		{"east", "active", "west"},
		{"west", "standby", "east"},
		{"solo", "standalone", ""}, // standalone → peer empty, not an error
	} {
		out := filepath.Join(t.TempDir(), "output")
		t.Setenv("GITHUB_OUTPUT", out)
		if err := writeHAResolution(deps, tc.name); err != nil {
			t.Fatalf("writeHAResolution(%s): %v", tc.name, err)
		}
		b, _ := os.ReadFile(out)
		got := string(b)
		if !strings.Contains(got, "role="+tc.wantRole) || !strings.Contains(got, "peer="+tc.wantPeer+"\n") {
			t.Errorf("%s → GITHUB_OUTPUT %q, want role=%s peer=%q", tc.name, got, tc.wantRole, tc.wantPeer)
		}
	}
	if err := writeHAResolution(deps, "nope"); err == nil {
		t.Error("unknown Deployment must error")
	}
}

func TestReadTopologyAndHelpers(t *testing.T) {
	dir := haInstance(t, map[string][2]string{
		"east": {"active", "g1"},
		"west": {"standby", "g1"},
		"solo": {"standalone", ""},
	})
	deps, err := ReadTopology(dir)
	if err != nil {
		t.Fatalf("ReadTopology: %v", err)
	}

	if got := HAMembers(deps); !reflect.DeepEqual(got, []string{"east", "west"}) {
		t.Errorf("HAMembers = %v, want [east west]", got)
	}
	if got := byRole(deps, RoleActive); !reflect.DeepEqual(got, []string{"east"}) {
		t.Errorf("byRole(active) = %v, want [east]", got)
	}
	if peer, ok, err := PeerOf(deps, "west"); err != nil || !ok || peer != "east" {
		t.Errorf("PeerOf(west) = %q,%v,%v, want east,true,nil", peer, ok, err)
	}
	if peer, ok, err := PeerOf(deps, "east"); err != nil || !ok || peer != "west" {
		t.Errorf("PeerOf(east) = %q,%v,%v, want west,true,nil", peer, ok, err)
	}
	if _, ok, err := PeerOf(deps, "solo"); ok || err != nil {
		t.Error("PeerOf(solo) ok=true, want false (standalone has no peer)")
	}
}

func TestReadTopologyDefaultsStandalone(t *testing.T) {
	// A cluster tfvars with no ha_* fields defaults to standalone.
	dir := t.TempDir()
	writeCluster(t, dir, map[string]string{"plain.tfvars": "region = \"us-x\"\n"})
	deps, err := ReadTopology(dir)
	if err != nil {
		t.Fatalf("ReadTopology: %v", err)
	}
	if len(deps) != 1 || deps[0].HARole != roleStandalone || deps[0].HAGroup != "" {
		t.Errorf("default = %+v, want standalone/empty", deps[0])
	}
}

// PeerOf must refuse to guess: it used to return the first other group member,
// so a group with two standbys resolved `llz env peer` to an arbitrary cluster —
// the value that tells CI which cluster to seed Harbor creds from.
func TestPeerOfRejectsAmbiguousGroup(t *testing.T) {
	deps := []Deployment{
		{"east", RoleActive, "g1"},
		{"west", RoleStandby, "g1"},
		{"cent", RoleStandby, "g1"},
	}
	_, ok, err := PeerOf(deps, "east")
	if err == nil {
		t.Fatal("PeerOf resolved an ambiguous group, want error")
	}
	if ok {
		t.Error("ok = true on an ambiguous group, want false")
	}
	if !strings.Contains(err.Error(), "more than one peer") {
		t.Errorf("error = %v, want the ambiguous-group message", err)
	}
}

// The half-formed pair must keep working. `llz env add` writes an HA pair one
// half at a time (scaffold.go defers the render and says so), and `llz env
// resolve` runs for every Deployment at the head of the OpenBao bootstrap job —
// so enforcing the whole-set contract at read time would fail bootstrap for
// every cluster in the repo, including unrelated standalone ones.
func TestReadTopologyToleratesHalfAddedPair(t *testing.T) {
	dir := haInstance(t, map[string][2]string{"east": {RoleActive, "g1"}})
	deps, err := ReadTopology(dir)
	if err != nil {
		t.Fatalf("ReadTopology rejected a half-added HA pair: %v", err)
	}
	if len(deps) != 1 || deps[0].HARole != RoleActive {
		t.Fatalf("deps = %+v, want the one active", deps)
	}
	if _, ok, err := PeerOf(deps, "east"); err != nil || ok {
		t.Errorf("PeerOf = (ok %v, err %v), want (false, nil) — no peer yet, but not an error", ok, err)
	}
}

func TestValidateTopology(t *testing.T) {
	good := []Deployment{
		{"a", RoleActive, "g1"}, {"b", RoleStandby, "g1"}, {"solo", roleStandalone, ""},
	}
	if err := ValidateTopology(good); err != nil {
		t.Errorf("ValidateTopology(good) = %v, want nil", err)
	}

	bad := []struct {
		name string
		deps []Deployment
	}{
		{"two actives", []Deployment{{"a", RoleActive, "g1"}, {"b", RoleActive, "g1"}}},
		{"active no standby", []Deployment{{"a", RoleActive, "g1"}}},
		{"role without group", []Deployment{{"a", RoleActive, ""}}},
		{"standalone with group", []Deployment{{"a", roleStandalone, "g1"}}},
		{"invalid role", []Deployment{{"a", "leader", "g1"}}},
	}
	for _, tc := range bad {
		if err := ValidateTopology(tc.deps); err == nil {
			t.Errorf("ValidateTopology(%s) = nil, want error", tc.name)
		}
	}
}

func TestValidateHAFlags(t *testing.T) {
	ok := []struct{ role, group string }{
		{"", ""}, {"standalone", ""}, {"active", "g1"}, {"standby", "g1"},
	}
	for _, c := range ok {
		if err := ValidateHAFlags(c.role, c.group); err != nil {
			t.Errorf("ValidateHAFlags(%q,%q) = %v, want nil", c.role, c.group, err)
		}
	}
	bad := []struct{ role, group string }{
		{"active", ""}, {"standby", ""}, {"standalone", "g1"}, {"leader", "g1"},
	}
	for _, c := range bad {
		if err := ValidateHAFlags(c.role, c.group); err == nil {
			t.Errorf("ValidateHAFlags(%q,%q) = nil, want error", c.role, c.group)
		}
	}
}
