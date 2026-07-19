package terraform

import "testing"

// The verbatim diagnostic from akamai/gsap-apl run 29701131691, which applied
// straight through this warning and deleted node firewall 75288222.
const gsapPlanWarning = `
Plan: 3 to add, 0 to change, 1 to destroy.

Warning: Unresolved resource instance address changes

Terraform tried to adjust resource instance addresses in the prior state
based on change information recorded in the configuration, but some
adjustments did not succeed due to existing objects already at the intended
addresses:
  - module.cluster.module.node_firewall.linode_firewall.this could not move to module.cluster.linode_firewall.this

Terraform has planned to destroy these objects. If Terraform's proposed
changes aren't appropriate, you must first resolve the conflicts using the
"terraform state" subcommands and then create a new plan.
`

func TestUnresolvedMoveConflictsDetectsGsapIncident(t *testing.T) {
	got, found := UnresolvedMoveConflicts(gsapPlanWarning)
	if !found {
		t.Fatal("want the diagnostic to be found")
	}
	if len(got) != 1 {
		t.Fatalf("want 1 conflict, got %d: %+v", len(got), got)
	}
	want := MoveConflict{
		From: "module.cluster.module.node_firewall.linode_firewall.this",
		To:   "module.cluster.linode_firewall.this",
	}
	if got[0] != want {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
}

// Terraform hard-wraps diagnostics, so a long address pair splits across lines.
func TestUnresolvedMoveConflictsHandlesWrapping(t *testing.T) {
	wrapped := `Warning: Unresolved resource instance address changes

addresses:
  - module.cluster.module.node_firewall.linode_firewall.this could not move
    to module.cluster.linode_firewall.this

Terraform has planned to destroy these objects.`
	got, found := UnresolvedMoveConflicts(wrapped)
	if !found || len(got) != 1 {
		t.Fatalf("wrapped bullet not parsed: found=%v got=%+v", found, got)
	}
	if got[0].To != "module.cluster.linode_firewall.this" {
		t.Errorf("To = %q", got[0].To)
	}
}

func TestUnresolvedMoveConflictsMultiple(t *testing.T) {
	in := `Warning: Unresolved resource instance address changes
addresses:
  - module.a.linode_firewall.this could not move to linode_firewall.a
  - module.b.linode_vpc.this could not move to linode_vpc.b
Terraform has planned to destroy these objects.`
	got, _ := UnresolvedMoveConflicts(in)
	if len(got) != 2 {
		t.Fatalf("want 2 conflicts, got %d: %+v", len(got), got)
	}
}

// A clean plan must not trip the gate — including one that merely mentions a
// successful move, which terraform reports without the warning heading.
func TestUnresolvedMoveConflictsCleanPlan(t *testing.T) {
	for _, in := range []string{
		"Plan: 3 to add, 0 to change, 0 to destroy.",
		"",
		`  # module.cluster.linode_firewall.this has moved to module.cluster.linode_firewall.this
Plan: 0 to add, 0 to change, 0 to destroy.`,
	} {
		if got, found := UnresolvedMoveConflicts(in); found || len(got) != 0 {
			t.Errorf("clean plan flagged: found=%v got=%+v (input %q)", found, got, in)
		}
	}
}

// The heading is required: "could not move to" appearing in unrelated text (a
// resource description, a tag value) must not fail an otherwise-valid plan.
func TestUnresolvedMoveConflictsRequiresHeading(t *testing.T) {
	in := `  + description = "this could not move to another region"
Plan: 1 to add, 0 to change, 0 to destroy.`
	if got, found := UnresolvedMoveConflicts(in); found || len(got) != 0 {
		t.Errorf("matched without the heading: found=%v got=%+v", found, got)
	}
}
