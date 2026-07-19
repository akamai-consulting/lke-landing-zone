package terraform

import (
	"regexp"
	"strings"
)

// moveconflict.go detects the one terraform plan diagnostic that is a WARNING to
// terraform but is always a bug in an automated pipeline: a `moved` block whose
// destination address is already occupied.
//
// Terraform's own wording is explicit that it degrades into a destroy:
//
//	Warning: Unresolved resource instance address changes
//	  Terraform tried to adjust resource instance addresses in the prior state
//	  … but some adjustments did not succeed due to existing objects already at
//	  the intended addresses:
//	    - module.cluster.module.node_firewall.linode_firewall.this could not move
//	      to module.cluster.linode_firewall.this
//	  Terraform has planned to destroy these objects.
//
// Interactively an operator reads that and stops. A pipeline applies straight
// through it — and when both addresses alias the SAME cloud object (an importer
// that resolves by label having already adopted it at the destination), the
// "destroy the old address" step deletes the live resource.
//
// That is exactly how akamai/gsap-apl run 29701131691 destroyed its node firewall
// 0.7ms before starting a 14m29s LKE create, then failed the node pool with
// `[400] [firewall_id] The provided ID did not match any existing Firewalls`. The
// module's `depends_on = [linode_firewall.this]` fail-fast could not catch it:
// that edge names the DESTINATION address (already satisfied by the import, so no
// work to do), while the destroy targeted the orphaned source address — and
// `depends_on` orders creates, not destroys.
//
// So this is gated at PLAN time, where it costs seconds, rather than being left to
// surface as a resource error after the expensive create.

const unresolvedMoveHeading = "Unresolved resource instance address changes"

// moveConflictRe captures the "<source> could not move to <destination>" bullets
// terraform lists under the heading. Terraform hard-wraps its diagnostics, so the
// two addresses can land on separate lines — callers must join the block before
// matching (see UnresolvedMoveConflicts).
var moveConflictRe = regexp.MustCompile(`([^\s]+)\s+could not move to\s+([^\s]+)`)

// MoveConflict is one blocked `moved` migration: the state address terraform
// wanted to move From, and the already-occupied address it wanted to move To.
// Terraform plans to DESTROY the object at From.
type MoveConflict struct {
	From string
	To   string
}

// UnresolvedMoveConflicts returns every blocked `moved` migration in a
// `terraform plan -no-color` output, and whether the diagnostic is present at
// all. A plan carrying any of these exits 0 — the caller is expected to treat a
// non-empty result as fatal.
//
// The heading is required rather than matching "could not move to" alone, so an
// unrelated string in a resource body (a description, a tag) cannot trip the gate.
func UnresolvedMoveConflicts(planOutput string) ([]MoveConflict, bool) {
	i := strings.Index(planOutput, unresolvedMoveHeading)
	if i < 0 {
		return nil, false
	}
	// Collapse whitespace across the diagnostic block so a hard-wrapped
	// "could not move\n  to <addr>" still matches as one bullet.
	block := strings.Join(strings.Fields(planOutput[i:]), " ")

	var out []MoveConflict
	seen := map[MoveConflict]bool{}
	for _, m := range moveConflictRe.FindAllStringSubmatch(block, -1) {
		c := MoveConflict{From: strings.TrimPrefix(m[1], "-"), To: strings.TrimSuffix(m[2], ",")}
		if c.From == "" || c.To == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out, true
}
