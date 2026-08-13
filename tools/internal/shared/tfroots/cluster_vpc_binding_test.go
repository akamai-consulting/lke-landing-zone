package tfroots

// cluster_vpc_binding_test.go — the llz-cluster module must not try to move a
// live cluster between VPCs, and must report where the cluster actually is.
//
// ────────────────────────────────────────────────────────────────────────────
// THE PLAN THAT CANNOT DO WHAT IT SAYS.
//
// `vpc_id`/`subnet_id` on linode_lke_cluster are create-time only — the Linode
// API's cluster PUT does not accept either — but the provider schema marks them
// as ordinary optional attributes. So a cluster whose live VPC differs from the
// module's plans as a calm `update in-place`:
//
//	~ subnet_id = 814117 -> 806378
//	~ vpc_id    = 580281 -> 575244
//
// An operator reads that as a small correction. It is not: the apply either
// no-ops and re-proposes the same diff on every future plan, or (should Linode
// ever implement it) recycles every node in the cluster.
//
// The state it comes from is not exotic. The module gained the `vpc_id`
// argument after it was first written; a cluster created before that passed
// subnet_id alone, which does NOT attach the VPC — LKE-E provisions its own
// `lke<clusterID>` VPC instead — so the module's VPC is orphaned and the cluster
// lives elsewhere. Any instance that predates the fix hits this on its first
// plan after upgrading, on a cluster that has been healthy for months.
//
// WHY A TEST AND NOT `tofu validate`. All three properties below are valid HCL
// in either direction. validate, fmt, checkov and the plan itself are all happy
// with a module that silently proposes the move and points its outputs at a VPC
// containing no nodes — that combination is exactly what shipped. The regression
// shape is someone tidying the lifecycle block or "simplifying" the outputs back
// to the local, so the check has to be on the source text.
// ────────────────────────────────────────────────────────────────────────────

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const clusterModuleDir = "../../../../terraform-modules/llz-cluster"

// ignoreChangesList returns the contents of the ignore_changes list, or "" if
// there is none.
//
// Bracket-MATCHED rather than regexed to the first `]`, because the list's own
// first entry is `control_plane[0].acl` — a `\[([^\]]*)\]` reads that inner
// bracket as the end of the list and reports the remaining entries missing. The
// first cut of this test did exactly that and failed on a correct module.
func ignoreChangesList(hcl string) string {
	i := strings.Index(hcl, "ignore_changes")
	if i < 0 {
		return ""
	}
	open := strings.Index(hcl[i:], "[")
	if open < 0 {
		return ""
	}
	open += i
	depth := 0
	for j, r := range hcl[open:] {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return hcl[open+1 : open+j]
			}
		}
	}
	return ""
}

// readModuleFile returns a module file's text, failing closed when it is missing
// or empty — an unreadable file would otherwise let every assertion below pass
// by examining nothing.
func readModuleFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.FromSlash(clusterModuleDir), name))
	if err != nil {
		t.Fatalf("llz-cluster/%s is what this test exists to check and must be readable: %v", name, err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		t.Fatalf("llz-cluster/%s is empty; every check below would pass vacuously", name)
	}
	return string(b)
}

// The create-time-only attributes must be ignored, or the module proposes a move
// the API cannot perform.
func TestClusterIgnoresCreateTimeOnlyVPCBinding(t *testing.T) {
	main := readModuleFile(t, "main.tf")

	ignored := ignoreChangesList(main)
	if strings.TrimSpace(ignored) == "" {
		t.Fatal("linode_lke_cluster.this has no ignore_changes at all — it needs one for " +
			"control_plane acl and pool as well as the VPC binding; see the lifecycle block's comment")
	}

	for _, attr := range []string{"vpc_id", "subnet_id"} {
		if !regexp.MustCompile(`\b` + attr + `\b`).MatchString(ignored) {
			t.Errorf("ignore_changes does not cover %s, so a cluster whose live VPC differs from this "+
				"module's plans as an `update in-place` that the Linode API cannot honour.\n"+
				"\tThe apply either no-ops — and the identical diff returns on every plan forever — or "+
				"recycles every node.\n\tignore_changes is currently: [%s]",
				attr, strings.Join(strings.Fields(ignored), " "))
		}
	}
}

// Ignoring the drift silently is how it went unnoticed, so the report is part of
// the behavior, not a nicety.
func TestClusterReportsAVPCBindingMismatch(t *testing.T) {
	main := readModuleFile(t, "main.tf")

	if !strings.Contains(main, `check "`) {
		t.Fatal("the module has no `check` block. ignore_changes silences the VPC-binding drift, so " +
			"without a check nothing anywhere says the cluster is in a different VPC than the one " +
			"whose CIDR built the node firewall and whose id fills the databases' vpcId.")
	}
	// The report is worthless if it cannot name the live VPC, which is where any
	// diagnosis has to start — a message that only says "mismatch" sends the
	// reader to the Linode console to find the number by hand.
	if !strings.Contains(main, "linode_lke_cluster.this.vpc_id") {
		t.Error("the check block never reads linode_lke_cluster.this.vpc_id, so it cannot report which " +
			"VPC the cluster is actually in — the one fact the fix starts from")
	}
}

// The networking outputs feed a VPC-only Managed Postgres. Naming the module's
// VPC rather than the cluster's yields a database the cluster cannot reach.
func TestClusterNetworkingOutputsReadTheClusterNotTheModulesVPC(t *testing.T) {
	outputs := readModuleFile(t, "outputs.tf")

	for _, o := range []struct{ name, want string }{
		{"vpc_id", "linode_lke_cluster.this.vpc_id"},
		{"vpc_subnet_id", "linode_lke_cluster.this.subnet_id"},
	} {
		body := outputBody(t, outputs, o.name)
		if !strings.Contains(body, o.want) {
			t.Errorf("output %q does not read %s.\n"+
				"\tIt must report where the cluster ACTUALLY is, not what this module built — the two "+
				"differ on any cluster created before the module passed vpc_id.\n"+
				"\tThe cluster root re-exports this to fill spec.cluster.databases.<name>.vpcId/subnetId, "+
				"and a Managed Postgres has no public endpoint: attached to a VPC the nodes are not in, "+
				"it is unreachable, and the symptom is a connection timeout in a pod several layers away.\n"+
				"\tcurrent body: %s",
				o.name, o.want, strings.Join(strings.Fields(body), " "))
		}
	}
}

// outputBody returns the text of a named output block. Fails closed: a renamed or
// deleted output is a failure, not an empty string that trivially passes.
func outputBody(t *testing.T, hcl, name string) string {
	t.Helper()
	head := `output "` + name + `"`
	i := strings.Index(hcl, head)
	if i < 0 {
		t.Fatalf("output %q is gone from llz-cluster/outputs.tf; the cluster root re-exports it "+
			"(roots/cluster/outputs.tf) and would fail to render", name)
	}
	rest := hcl[i+len(head):]
	if j := strings.Index(rest, "\noutput "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
