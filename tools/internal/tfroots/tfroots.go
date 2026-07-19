// Package tfroots embeds the four Terraform root directories an instance builds
// on (cluster, cluster-bootstrap, object-storage, vpc) and generates them on the
// fly. An instance commits ZERO Terraform: `llz render` writes the roots' *.tf
// files (gitignored build artifacts) the same way it writes each <env>.tfvars,
// from this single embedded copy — so the roots live ONCE, in the binary, and an
// instance never vendors ~700 lines of byte-identical HCL.
//
// The embedded files keep their copier tokens verbatim (<@ upstream_org @>,
// <@ llz_version @>, <@ instance_repo @>); Render/Substitute fill them at
// generation time from values the command layer resolves (upstream_org is the
// constant akamai-consulting — no forks; ref is the template version the instance
// tracks; instance_repo is the instance's own owner/name). backend.tf is a static
// partial `backend "s3" {}` (all per-env params arrive via -backend-config at
// init), so it carries no token and lands identically everywhere.
package tfroots

import (
	"embed"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed roots
var embedded embed.FS

// The three copier token types the roots carry (established: 5 upstream_org, 4
// llz_version, 2 instance_repo across all four roots). Substituted verbatim.
const (
	tokUpstreamOrg  = "<@ upstream_org @>"
	tokLLZVersion   = "<@ llz_version @>"
	tokInstanceRepo = "<@ instance_repo @>"
)

// DefaultVPCSubnetCIDR is the cluster root's `vpc_subnet_cidr` default. It lives
// here because this package embeds the roots, so it is the one place that can be
// checked against the HCL itself — TestDefaultVPCSubnetCIDRMatchesRoot parses
// roots/cluster/variables.tf and fails if the two drift.
//
// internal/terraform and internal/clusterspec both alias this rather than
// restating the literal: terraform resolves an omitted <region>.tfvars value to
// it (so the firewall-controller's VPC_CIDR matches what `terraform output
// vpc_subnet_cidr` would have returned), and clusterspec resolves an unset
// SubnetCIDR to it when checking peer overlap (so two envs that BOTH omit it
// still collide loudly). Three copies of one literal, each claiming to mirror
// the HCL, with nothing enforcing it — that is the drift this prevents.
const DefaultVPCSubnetCIDR = "10.0.0.0/13"

// Substitute fills the three copier tokens in a root file's content. Any token a
// given file does not contain is a harmless no-op.
func Substitute(content, upstreamOrg, ref, instanceRepo string) string {
	r := strings.NewReplacer(
		tokUpstreamOrg, upstreamOrg,
		tokLLZVersion, ref,
		tokInstanceRepo, instanceRepo,
	)
	return r.Replace(content)
}

// tfRel is the sorted set of embedded *.tf files (relative to roots/, e.g.
// "cluster/backend.tf"), enumerated ONCE at package init. Only *.tf files are
// roots to lay down: terraform.tfvars.example (rendered per-env into <env>.tfvars
// by the render engine) and any root README.md are NOT written as
// roots. The embed is fixed at compile time, so this walk cannot fail at runtime —
// a panic here means the binary embedded something unexpected.
var tfRel = func() []string {
	var rel []string
	err := fs.WalkDir(embedded, "roots", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".tf") {
			return nil
		}
		rel = append(rel, strings.TrimPrefix(p, "roots/"))
		return nil
	})
	if err != nil {
		panic("tfroots: walking embedded roots: " + err.Error())
	}
	sort.Strings(rel)
	return rel
}()

// Files returns the generated *.tf as an absolute-path → substituted-content map
// under dst, WITHOUT writing anything — the render engine's --diff uses it to
// preview the roots alongside the tfvars. Reads are from the compile-time embed, so
// they cannot fail at runtime.
func Files(dst, upstreamOrg, ref, instanceRepo string) map[string]string {
	out := make(map[string]string, len(tfRel))
	for _, r := range tfRel {
		raw, _ := embedded.ReadFile("roots/" + r) // enumerated at init — cannot fail
		out[filepath.Join(dst, "terraform-iac-bootstrap", r)] = Substitute(string(raw), upstreamOrg, ref, instanceRepo)
	}
	return out
}

// TfvarsExample returns the embedded roots/<root>/terraform.tfvars.example bytes,
// tokens intact — the render engine reads it (instead of the instance filesystem,
// where the .example no longer ships) as the base for each <env>.tfvars, applies
// the spec's assignments, and substitutes any remaining tokens itself.
func TfvarsExample(root string) ([]byte, error) {
	return embedded.ReadFile("roots/" + root + "/terraform.tfvars.example")
}
