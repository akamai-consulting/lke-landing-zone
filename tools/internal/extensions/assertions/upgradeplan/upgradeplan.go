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
		// Before/After carry ONE attribute, and only for the sake of the remedy
		// text below: a destroyed-and-recreated Object Storage bucket is almost
		// always a RENAME, and the two labels are what turn "this destroys a live
		// resource" into "set spec.instance.objLabelPrefix to X". Everything else
		// in the before/after objects is deliberately not modelled — the verdict
		// must not depend on attributes this gate has no opinion about.
		Before *changeAttrs `json:"before"`
		After  *changeAttrs `json:"after"`
	} `json:"change"`
}

// changeAttrs is the sliver of a plan's before/after object this reads.
type changeAttrs struct {
	Label string `json:"label"`
}

// Verdict is what a plan proposes.
type Verdict struct {
	Total       int       // resource_changes entries examined
	Destructive []Finding // the ones that delete something
	Creates     int
	Updates     int
	// Changed is every entry that proposes ANY action, destructive or not —
	// what --expect-no-changes judges. Kept separate from the counters because a
	// count cannot name the resource, and naming it is the whole value of the
	// report.
	Changed []Finding
}

// Finding is one resource the plan would destroy or replace.
type Finding struct {
	Address string
	Actions []string
	Kind    string // "destroy" or "replace", for a message that names what happens
	// Type and the two labels exist for the rename remedy, not for the verdict.
	Type        string
	BeforeLabel string
	AfterLabel  string
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
		// no-op entries are the bulk of any plan and are not changes. Terraform
		// lists every resource it read, so counting them would make an empty plan
		// look like a busy one.
		if !isNoOp(rc.Change.Actions) {
			v.Changed = append(v.Changed, Finding{Address: rc.Address, Actions: rc.Change.Actions})
		}
		kind := classify(rc.Change.Actions)
		if kind != "" {
			f := Finding{Address: rc.Address, Actions: rc.Change.Actions, Kind: kind, Type: rc.Type}
			if rc.Change.Before != nil {
				f.BeforeLabel = rc.Change.Before.Label
			}
			if rc.Change.After != nil {
				f.AfterLabel = rc.Change.After.Label
			}
			v.Destructive = append(v.Destructive, f)
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
	sort.Slice(v.Changed, func(i, j int) bool { return v.Changed[i].Address < v.Changed[j].Address })
	return v
}

// isNoOp reports whether a planned action list proposes nothing. `read` counts
// as nothing: a data source being re-read is not a change to anything an apply
// would write.
func isNoOp(actions []string) bool {
	for _, a := range actions {
		if a != "no-op" && a != "read" {
			return false
		}
	}
	return true
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

// ── The rename remedy ─────────────────────────────────────────────────────────
//
// WHY A SPECIAL CASE AT ALL. The generic advice this gate prints — make the
// change non-forcing, add a moved{} block, or say so in the release notes — is
// right for a module change and useless for the one destructive plan adopters
// actually meet: an Object Storage bucket whose LABEL changed, because a bucket
// label is ForceNew and a bucket cannot be renamed. There is no moved{} for it
// and no non-forcing spelling of it.
//
// THE INCIDENT. `spec.instance.objLabelPrefix` moved the bucket prefix off the
// hardcoded module default `platform` onto a per-instance value, which is the
// right fix for a namespace shared globally per region. For an instance already
// provisioned under `platform-*` it is a rename, so v0.0.44 and v0.0.45 both
// planned `2 to add, 0 to change, 2 to destroy` against a live prod deployment:
//
//	~ label = "platform-loki-chunks-prod" -> "gsap-apl-loki-chunks-prod" # forces replacement
//
// Both applies failed only because Linode refuses to delete a non-empty bucket.
// An instance whose buckets happened to be EMPTY would have had them deleted and
// recreated with no error at all — and the failure is not the run that goes red,
// it is the one that goes green.

const objBucketType = "linode_object_storage_bucket"

// RenameRemedy returns the operator-facing remedy for a plan that renames Object
// Storage buckets, or "" when no finding is one.
//
// Pure over the findings, so every arm is reachable from a table test.
func RenameRemedy(findings []Finding) string {
	var was, now string
	var renamed []Finding
	for _, f := range findings {
		if f.Type != objBucketType || f.BeforeLabel == "" || f.AfterLabel == "" || f.BeforeLabel == f.AfterLabel {
			continue
		}
		renamed = append(renamed, f)
		oldPrefix, newPrefix := splitPrefix(f.BeforeLabel, f.AfterLabel)
		// AGREEMENT ACROSS EVERY RENAMED BUCKET, or no prefix claim at all. One
		// prefix is the whole remedy; two different ones mean something other than
		// a prefix change is going on and naming either would be a guess.
		switch {
		case was == "" && now == "":
			was, now = oldPrefix, newPrefix
		case was != oldPrefix || now != newPrefix:
			was, now = "", ""
		}
	}
	if len(renamed) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nTHESE ARE BUCKET RENAMES, WHICH ARE NOT RECOVERABLE BY RETRYING.\n" +
		"A bucket label is create-time only, so Terraform's only way to honour the new\n" +
		"name is to DELETE the bucket holding your data and create an empty one. Linode\n" +
		"refuses to delete a non-empty bucket, which is the only reason this is a failed\n" +
		"run and not a silent one — an empty bucket would have been deleted cleanly.\n\n")
	for _, f := range renamed {
		fmt.Fprintf(&b, "    %s: %s -> %s\n", f.Address, f.BeforeLabel, f.AfterLabel)
	}
	if was != "" && now != "" {
		fmt.Fprintf(&b, `
WHAT TO DO. This instance's buckets were created under the prefix %q and the
spec now derives %q. Keep the buckets you have by pinning the prefix in
landingzone.yaml:

    spec:
      instance:
        objLabelPrefix: %s

then re-run this apply — the plan goes empty and no bucket is touched. The
prefix also names this instance's Object Storage KEY labels, which `+"`llz reap`"+`
and the rotation table match exactly, so check those before you commit it.

Adopting the new names instead means COPYING the objects across and repointing
Loki and Harbor first. Deleting the old buckets is a destroy, and the destroy
path is where the confirmation for that lives.
`, was, now, was)
	}
	return b.String()
}

// splitPrefix returns the heads of two labels that share a suffix — for
// "platform-loki-chunks-prod" and "gsap-apl-loki-chunks-prod", ("platform",
// "gsap-apl").
//
// THE LONGEST COMMON SUFFIX, rather than a table of the module's bucket names
// ("-loki-chunks-", "-harbor-registry-", …). That table is exactly the kind of
// second copy of a fact that drifts from the module it describes; a bucket added
// there later would silently stop producing a remedy. The suffix is derivable
// from the two labels themselves, which cannot drift.
func splitPrefix(before, after string) (string, string) {
	i := 0
	for i < len(before) && i < len(after) && before[len(before)-1-i] == after[len(after)-1-i] {
		i++
	}
	return strings.TrimSuffix(before[:len(before)-i], "-"), strings.TrimSuffix(after[:len(after)-i], "-")
}
