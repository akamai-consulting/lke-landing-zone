package validate

import (
	"strings"
	"testing"
)

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

func TestCIDRList(t *testing.T) {
	// Empty is legitimate: github.com-hosted runners open their own egress IP at
	// runtime, so an empty ACL seed is a real choice, not an oversight.
	for _, ok := range []string{"", "203.0.113.0/24", " 203.0.113.0/24 , 198.51.100.7/32 "} {
		if err := CIDRList("--runner-ipv4-cidrs", ok, IPv4); err != nil {
			t.Errorf("CIDRList(%q, IPv4) = %v, want nil", ok, err)
		}
	}
	if err := CIDRList("--runner-ipv6-cidrs", "2001:db8::/32", IPv6); err != nil {
		t.Errorf("CIDRList(v6) = %v, want nil", err)
	}
	// An open-world prefix is the one value the flag help has always forbidden:
	// LKE-E accepts it, the apply succeeds, and the control plane is then public.
	for _, open := range []string{"0.0.0.0/0", "203.0.113.0/24,0.0.0.0/0", "::/0"} {
		err := CIDRList("--runner-ipv4-cidrs", open, AnyFamily)
		if err == nil {
			t.Fatalf("CIDRList(%q) = nil, want a refusal", open)
		}
		if !strings.Contains(err.Error(), "entire internet") {
			t.Errorf("CIDRList(%q) error %q should say what it does", open, err)
		}
	}
	// A bare address is the common typo, and the fix is one suffix away.
	err := CIDRList("--runner-ipv4-cidrs", "203.0.113.7", AnyFamily)
	if err == nil {
		t.Fatal("a bare address is not a CIDR")
	}
	if !strings.Contains(err.Error(), "203.0.113.7/32") {
		t.Errorf("error %q should suggest the /32 form", err)
	}
	if err := CIDRList("--runner-ipv4-cidrs", "not-an-address", AnyFamily); err == nil {
		t.Error("garbage must be rejected")
	}
}

func TestCIDRListOpenWorldSpellings(t *testing.T) {
	// A /0 has many spellings that all mask to "everything" while reading like a
	// specific host — net.ParseCIDR normalizes 203.0.113.7/0 to 0.0.0.0/0 and
	// 0::/0 to ::/0. A string comparison against "0.0.0.0/0" misses every one of
	// them, and what it lets through disables the control-plane ACL.
	for _, open := range []string{"203.0.113.7/0", "0::/0", "2001:db8::1/0", "10.0.0.0/0"} {
		err := CIDRList("--runner-ipv4-cidrs", open, AnyFamily)
		if err == nil {
			t.Errorf("CIDRList(%q) = nil — an all-addresses mask must be refused however it is spelled", open)
			continue
		}
		if !strings.Contains(err.Error(), "matches every address") {
			t.Errorf("CIDRList(%q) error %q should name what the mask does", open, err)
		}
	}
	// The neighbouring prefix lengths stay legal — only /0 is the trapdoor.
	for _, ok := range []string{"203.0.113.7/1", "2001:db8::1/1", "0.0.0.0/8"} {
		if err := CIDRList("--runner-ipv4-cidrs", ok, AnyFamily); err != nil {
			t.Errorf("CIDRList(%q) = %v, want nil", ok, err)
		}
	}
}

func TestCIDRListBareAddressHintMatchesFamily(t *testing.T) {
	// /32 is a whole host in IPv4 and 79 octillion addresses in IPv6.
	err := CIDRList("--runner-ipv6-cidrs", "2001:db8::1", AnyFamily)
	if err == nil {
		t.Fatal("a bare IPv6 address is not a CIDR")
	}
	if !strings.Contains(err.Error(), "2001:db8::1/128") {
		t.Errorf("IPv6 hint should suggest /128, got %q", err)
	}
	if strings.Contains(err.Error(), "/32") {
		t.Errorf("IPv6 hint must not suggest /32, got %q", err)
	}
}

func TestIsOpenWorldCIDR(t *testing.T) {
	for _, open := range []string{"0.0.0.0/0", "::/0", "0::/0", "198.51.100.4/0"} {
		if !IsOpenWorldCIDR(open) {
			t.Errorf("IsOpenWorldCIDR(%q) = false, want true", open)
		}
	}
	for _, closed := range []string{"203.0.113.0/24", "::1/128", "", "not-a-cidr", "0.0.0.0"} {
		if IsOpenWorldCIDR(closed) {
			t.Errorf("IsOpenWorldCIDR(%q) = true, want false", closed)
		}
	}
}

func TestCIDRListEnforcesTheAddressFamily(t *testing.T) {
	// The ACL is two separate spec fields fed by two separate flags, so a v6
	// prefix handed to the v4 flag is not a harmless mix — it lands in
	// apiServerAllowCIDRs.ipv4 and fails at apply, exactly the "accepted early,
	// fails late" shape these checks exist to remove.
	err := CIDRList("--runner-ipv4-cidrs", "2001:db8::/32", IPv4)
	if err == nil {
		t.Fatal("an IPv6 prefix must not pass the IPv4 flag")
	}
	if !strings.Contains(err.Error(), "is IPv6") {
		t.Errorf("error %q should name the family it found", err)
	}
	if err := CIDRList("--runner-ipv6-cidrs", "203.0.113.0/24", IPv6); err == nil {
		t.Error("an IPv4 prefix must not pass the IPv6 flag")
	}
	// Right family, either notation, still fine — including v4-in-v6 spelling,
	// which To4() classifies as IPv4 (len() would not).
	for _, c := range []struct {
		cidr string
		fam  IPFamily
	}{
		{"203.0.113.0/24", IPv4},
		{"::ffff:203.0.113.0/120", IPv4},
		{"2001:db8::/32", IPv6},
	} {
		if err := CIDRList("f", c.cidr, c.fam); err != nil {
			t.Errorf("CIDRList(%q, %v) = %v, want nil", c.cidr, c.fam, err)
		}
	}
	// AnyFamily keeps the door open for callers that legitimately mix.
	if err := CIDRList("f", "203.0.113.0/24,2001:db8::/32", AnyFamily); err != nil {
		t.Errorf("AnyFamily must accept a mixed list: %v", err)
	}
}
