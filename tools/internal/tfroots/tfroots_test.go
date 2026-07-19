package tfroots

import (
	"path/filepath"
	"strings"
	"testing"
)

// wantRoots is the day-0 TF root set the embed must produce, asserted here as
// test-local expected data rather than read back from the package (which would
// make the test echo the implementation). The former cluster-bootstrap root was
// retired — its in-cluster bootstrap runs natively via `llz ci bootstrap-cluster`.
var wantRoots = []string{"cluster", "object-storage", "vpc"}

// TestFilesProducesTokenFreeTF is the former TestRenderWritesTokenFreeTF,
// retargeted at Files (the live API) after the write-to-disk Render half of this
// package was retired — the render engine iterates Files() and writes via its own
// writeTargets. The assertions are unchanged: every root lands its full *.tf set,
// only .tf files are produced, no copier token survives, a static file is
// byte-identical to the embed, and a token-bearing file reflects the values.
func TestFilesProducesTokenFreeTF(t *testing.T) {
	dst := t.TempDir()
	files := Files(dst, "akamai-consulting", "v9.9.9", "acme/inst")
	if len(files) == 0 {
		t.Fatal("Files produced nothing")
	}

	byRoot := map[string]map[string]bool{}
	for p, content := range files {
		if !strings.HasSuffix(p, ".tf") {
			t.Errorf("Files produced a non-.tf file: %s", p)
		}
		if strings.HasSuffix(p, "terraform.tfvars.example") {
			t.Errorf("Files must not produce a tfvars.example: %s", p)
		}
		if strings.HasSuffix(p, "README.md") {
			t.Errorf("Files must not produce a README.md: %s", p)
		}
		rel, err := filepath.Rel(filepath.Join(dst, "terraform-iac-bootstrap"), p)
		if err != nil {
			t.Fatalf("rel: %v", err)
		}
		parts := strings.SplitN(rel, string(filepath.Separator), 2)
		if byRoot[parts[0]] == nil {
			byRoot[parts[0]] = map[string]bool{}
		}
		byRoot[parts[0]][parts[1]] = true

		if strings.Contains(content, "<@") {
			t.Errorf("%s still has an unsubstituted copier token:\n%s", p, content)
		}
	}

	for _, root := range wantRoots {
		f := byRoot[root]
		if f == nil {
			t.Errorf("root %q produced no .tf files", root)
			continue
		}
		for _, must := range []string{"backend.tf", "versions.tf", "main.tf"} {
			if !f[must] {
				t.Errorf("root %q missing %s", root, must)
			}
		}
	}
	if len(byRoot) != len(wantRoots) {
		t.Errorf("Files produced roots %v, want %v", byRoot, wantRoots)
	}

	// backend.tf is static (no token) → substitution leaves it identical to the embed.
	rawBackend, err := embedded.ReadFile("roots/cluster/backend.tf")
	if err != nil {
		t.Fatal(err)
	}
	if got := files[filepath.Join(dst, "terraform-iac-bootstrap", "cluster", "backend.tf")]; string(rawBackend) != got {
		t.Errorf("static backend.tf changed under substitution")
	}

	// A token-bearing file must reflect the substituted values.
	clusterMain := files[filepath.Join(dst, "terraform-iac-bootstrap", "cluster", "main.tf")]
	if !strings.Contains(clusterMain, "akamai-consulting/lke-landing-zone.git//terraform-modules/llz-cluster?ref=v9.9.9") {
		t.Errorf("cluster/main.tf tokens not substituted:\n%s", clusterMain)
	}
}

func TestTfvarsExample(t *testing.T) {
	for _, root := range wantRoots {
		b, err := TfvarsExample(root)
		if err != nil {
			t.Errorf("TfvarsExample(%q): %v", root, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("TfvarsExample(%q) is empty", root)
		}
	}
	if _, err := TfvarsExample("nope"); err == nil {
		t.Errorf("TfvarsExample of a bogus root should error")
	}
}

func TestSubstitute(t *testing.T) {
	in := "org=<@ upstream_org @> ref=<@ llz_version @> repo=<@ instance_repo @>"
	got := Substitute(in, "akamai-consulting", "v1.2.3", "acme/inst")
	want := "org=akamai-consulting ref=v1.2.3 repo=acme/inst"
	if got != want {
		t.Errorf("Substitute = %q, want %q", got, want)
	}
}

// TestDefaultVPCSubnetCIDRMatchesRoot ties DefaultVPCSubnetCIDR to the HCL it
// claims to mirror. internal/terraform and internal/clusterspec both alias the
// constant, so a change to the root's default that is not reflected in Go — or
// vice versa — fails here rather than silently producing a VPC_CIDR that
// disagrees with what Terraform actually applied.
func TestDefaultVPCSubnetCIDRMatchesRoot(t *testing.T) {
	raw, err := embedded.ReadFile("roots/cluster/variables.tf")
	if err != nil {
		t.Fatalf("read embedded cluster/variables.tf: %v", err)
	}

	// Scan forward from the `vpc_subnet_cidr` block header to its `default =`.
	lines := strings.Split(string(raw), "\n")
	idx := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), `variable "vpc_subnet_cidr"`) {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal(`no variable "vpc_subnet_cidr" block in roots/cluster/variables.tf`)
	}

	want := ""
	for _, l := range lines[idx:] {
		s := strings.TrimSpace(l)
		if s == "}" {
			break
		}
		if strings.HasPrefix(s, "default") {
			if _, v, ok := strings.Cut(s, "="); ok {
				want = strings.Trim(strings.TrimSpace(v), `"`)
			}
			break
		}
	}
	if want == "" {
		t.Fatal("vpc_subnet_cidr block has no default = ... line")
	}
	if want != DefaultVPCSubnetCIDR {
		t.Errorf("DefaultVPCSubnetCIDR = %q, but roots/cluster/variables.tf defaults to %q", DefaultVPCSubnetCIDR, want)
	}
}
