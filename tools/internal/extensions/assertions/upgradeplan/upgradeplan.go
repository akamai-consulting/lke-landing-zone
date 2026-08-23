package upgradeplan

// upgradeplan.go — an upgrade must not propose destroying or replacing anything
// on a cluster that already exists.
//
// ── THE CLASS OF BUG THIS CATCHES, AND WHY NOTHING ELSE CAN ───────────────────
//
// Every e2e lane force-pushes a FRESH instantiation at the commit under test and
// tears it down afterwards (e2e-instantiate.yml). So the only configuration the
// release gate has ever exercised is greenfield: a cluster created by the code
// being tested. No lane plans or applies a new template against state created by
// an OLD one, which is the configuration every adopter after their first day is
// in.
//
// The failure that hides there is not a broken apply. It is an apply that
// SUCCEEDS and recycles infrastructure. A field added to a module, a resource
// renamed, an attribute Terraform treats as ForceNew — each reads as a small,
// correct diff and each proposes destroying a live cluster.
//
// It is not hypothetical. `vpc_id`/`subnet_id` on linode_lke_cluster are
// create-time only — the Linode API's cluster PUT accepts neither — but the
// provider schema marks them as ordinary optional attributes, so a cluster whose
// live VPC differs from the module's plans as a calm `update in-place`:
//
//	~ subnet_id = 814117 -> 806378
//	~ vpc_id    = 580281 -> 575244
//
// An operator reads that as a small correction. Any instance created before the
// module gained `vpc_id` hits it on its FIRST PLAN AFTER UPGRADING, on a cluster
// that has been healthy for months. That one was caught by a hand-written unit
// test in tfroots — not by any lane, because no lane could see it.
//
// ── WHAT THIS ASSERTS ─────────────────────────────────────────────────────────
//
// Given `tofu show -json` output, every resource whose planned actions include a
// delete is a finding: a bare `delete`, and both spellings of a replace
// (`["delete","create"]` and, under create_before_destroy,
// `["create","delete"]`). Creates and in-place updates pass — an upgrade
// legitimately adds resources and changes attributes.
//
// ── WHAT IT CANNOT SEE ────────────────────────────────────────────────────────
//
// A destructive change Terraform models as an in-place update, which is exactly
// what the vpc_id case is: the plan says `update`, and only knowledge of the
// Linode API says otherwise. This gate is the second line for that class, not the
// first — the first is the coupling test in tfroots. What this catches is
// everything Terraform is HONEST about and nobody was looking at.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Plan is the subset of `tofu show -json` this reads.
//
// FormatVersion is parsed for one reason: to tell a real plan from any other JSON
// document. Without it `{}` unmarshals happily into a Plan with no changes and
// the gate reports "nothing destructive" about a file that is not a plan.
type Plan struct {
	FormatVersion   string           `json:"format_version"`
	ResourceChanges []ResourceChange `json:"resource_changes"`
}

// ResourceChange is one entry of the plan's resource_changes array.
type ResourceChange struct {
	Address      string `json:"address"`
	Type         string `json:"type"`
	ProviderName string `json:"provider_name"`
	Change       struct {
		Actions []string `json:"actions"`
	} `json:"change"`
}

// Verdict is what a plan proposes.
type Verdict struct {
	Total       int       // resource_changes entries examined
	Destructive []Finding // the ones that delete something
	Creates     int
	Updates     int
}

// Finding is one resource the plan would destroy or replace.
type Finding struct {
	Address string
	Actions []string
	Kind    string // "destroy" or "replace", for a message that names what happens
}

func (f Finding) String() string {
	return fmt.Sprintf("%s — %s (%s)", f.Address, f.Kind, strings.Join(f.Actions, ","))
}

// classify names what a set of planned actions does. Returns "" for anything
// that harms nothing.
//
// A DELETE ANYWHERE IN THE LIST, rather than an equality test against known
// orderings. `["delete","create"]` is a replace and `["create","delete"]` is the
// same replace under create_before_destroy; matching the pair explicitly means a
// third spelling — or a future action list this code has not met — passes
// silently. Asking "does this destroy something" is the question, and it has one
// answer regardless of ordering.
func classify(actions []string) string {
	deletes, creates := false, false
	for _, a := range actions {
		switch a {
		case "delete":
			deletes = true
		case "create":
			creates = true
		}
	}
	switch {
	case deletes && creates:
		return "replace"
	case deletes:
		return "destroy"
	}
	return ""
}

// Evaluate judges a parsed plan. Pure, so every arm is reachable from a table
// test with no Terraform and no cluster.
func Evaluate(p Plan) Verdict {
	v := Verdict{Total: len(p.ResourceChanges)}
	for _, rc := range p.ResourceChanges {
		kind := classify(rc.Change.Actions)
		if kind != "" {
			v.Destructive = append(v.Destructive, Finding{
				Address: rc.Address, Actions: rc.Change.Actions, Kind: kind,
			})
			continue
		}
		for _, a := range rc.Change.Actions {
			switch a {
			case "create":
				v.Creates++
			case "update":
				v.Updates++
			}
		}
	}
	sort.Slice(v.Destructive, func(i, j int) bool { return v.Destructive[i].Address < v.Destructive[j].Address })
	return v
}

// Parse reads `tofu show -json` output.
//
// FAILS CLOSED ON ANYTHING THAT IS NOT A PLAN. An empty resource_changes array is
// a legitimate verdict — an upgrade that proposes nothing — and is accepted,
// because the array being PRESENT is evidence the document was understood. A
// missing format_version is not: that is any other JSON, or a truncated file, or
// `tofu show` having written an error to the same path, and calling it "nothing
// destructive" is how a gate reports the strongest verdict on the weakest
// evidence.
func Parse(raw []byte) (Plan, error) {
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return Plan{}, fmt.Errorf("parse the plan JSON: %w "+
			"(this wants `tofu show -json <planfile>` output, not the human-readable plan)", err)
	}
	if strings.TrimSpace(p.FormatVersion) == "" {
		return Plan{}, fmt.Errorf("no format_version in the plan JSON — this is not `tofu show -json` " +
			"output. Refusing to report a verdict on a document that was not understood: an empty " +
			"parse and a clean plan are the same green line, and only one of them is evidence")
	}
	return p, nil
}
