package validate

import "testing"

func TestEnvName(t *testing.T) {
	for _, ok := range []string{"e2e", "prod", "myteam-dev", "a1"} {
		if err := EnvName(ok); err != nil {
			t.Errorf("EnvName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "A", "1x", "-x", "x_y", "Bad_Name"} {
		if err := EnvName(bad); err == nil {
			t.Errorf("EnvName(%q) = nil, want error", bad)
		}
	}
}

func TestOBJClusterID(t *testing.T) {
	if err := OBJClusterID(""); err != nil {
		t.Errorf("empty obj cluster should be allowed, got %v", err)
	}
	for _, ok := range []string{"us-ord-1", "us-ord-10", "ap-south-2"} {
		if err := OBJClusterID(ok); err != nil {
			t.Errorf("OBJClusterID(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"us-ord", "10.0.0.0/13", "REPLACE_ME"} {
		if err := OBJClusterID(bad); err == nil {
			t.Errorf("OBJClusterID(%q) = nil, want error", bad)
		}
	}
}

func TestForge(t *testing.T) {
	for _, ok := range []string{ForgeGitHub, ForgeGitHubEnterprise, ForgeGitLab} {
		if err := Forge(ok); err != nil {
			t.Errorf("Forge(%q) = %v, want nil", ok, err)
		}
	}
	if err := Forge("bitbucket"); err == nil {
		t.Error("Forge(bitbucket) = nil, want error")
	}
	if err := Forge(""); err == nil {
		t.Error("Forge(empty) = nil, want error")
	}
}

func TestHATopology(t *testing.T) {
	ok := []struct{ role, group string }{
		{"", ""}, {RoleStandalone, ""}, {RoleActive, "prod"}, {RoleStandby, "prod"},
	}
	for _, c := range ok {
		if err := HATopology(c.role, c.group, "role", "group"); err != nil {
			t.Errorf("HATopology(%q,%q) = %v, want nil", c.role, c.group, err)
		}
	}
	bad := []struct{ role, group string }{
		{RoleStandalone, "prod"}, // group with standalone
		{RoleActive, ""},         // active without group
		{RoleStandby, ""},        // standby without group
		{"bogus", ""},            // invalid role
	}
	for _, c := range bad {
		if err := HATopology(c.role, c.group, "role", "group"); err == nil {
			t.Errorf("HATopology(%q,%q) = nil, want error", c.role, c.group)
		}
	}
}
