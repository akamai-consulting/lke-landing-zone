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
	// SettledBuckets are the labels of the Object Storage buckets this plan is
	// NOT renaming. They exist for the rename remedy, which is a claim about the
	// whole instance and cannot be made from the renamed buckets alone — see
	// RenameRemedy's "half-migrated" header.
	SettledBuckets []string
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
		// A bucket this instance ALREADY HAS and this plan leaves where it is —
		// evidence about where the rest of the instance sits, for the rename remedy.
		//
		// A NON-EMPTY `before` LABEL IS THE WHOLE TEST, and it is doing two jobs.
		// It excludes the destructive entries (those are the renames themselves), and
		// it excludes a bucket being CREATED, whose `before` is null. A create is not
		// evidence of anything: the plan would create it under whichever prefix is
		// configured, so pinning the old one costs nothing and the remedy is still
		// correct. Keying on `classify() == ""` alone counted creates as settled and
		// suppressed a correct remedy for any release that added a bucket — while
		// reporting that the plan "leaves alone" a bucket it was creating.
		if rc.Type == objBucketType && kind == "" && rc.Change.Before != nil && rc.Change.Before.Label != "" {
			v.SettledBuckets = append(v.SettledBuckets, rc.Change.Before.Label)
		}
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
	sort.Strings(v.SettledBuckets)
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

// ── The bucket census ─────────────────────────────────────────────────────────
//
// WHY A DESTRUCTIVE PLAN IS NOT ALWAYS DATA LOSS, and why this gate could not
// tell. A bucket label is create-time only, so honouring a changed prefix means
// destroy-and-create. That is catastrophic for a bucket holding 63,345 Loki chunks
// and completely inert for one holding nothing — and the plan JSON says the same
// thing about both. So the gate refused every rename, which is safe and also makes
// the legitimate move unperformable: an operator who pins the RIGHT prefix, to
// keep the buckets that hold data, is then blocked by the empty ones the pin moves
// the other way. gsap-apl is exactly that shape, and there is no prefix that
// empties its plan.
//
// Linode answers this in the same call that lists the buckets — /v4/object-storage
// /buckets carries `objects` per bucket — so the evidence costs one request and no
// S3 credentials.
//
// FAIL CLOSED ON THE UNKNOWN. A bucket the census does not mention is treated as
// holding data. No token, a failed request and a bucket in another account all
// land there, which keeps the old refusal as the default and makes this an
// exemption granted on evidence rather than a relaxation applied by default.

// BucketCensus maps an Object Storage bucket label to how many objects it holds.
// A nil census knows nothing, which is not the same as knowing everything is
// empty — see partition.
type BucketCensus map[string]int

// Empty reports whether label is KNOWN to hold nothing.
func (c BucketCensus) Empty(label string) bool {
	n, ok := c[label]
	return ok && n == 0
}

// partition splits the destructive findings into the ones that must still block
// and the ones the census proves are harmless.
//
// A finding is exempt only if it is a BUCKET, only if it is being replaced rather
// than plainly destroyed, and only if the census says the bucket it would delete
// is empty. Everything else — a cluster, a bucket with objects, a bucket nothing
// knows about — blocks exactly as before.
//
// THE `before` LABEL IS THE ONE THAT MATTERS: it names the bucket that would be
// DELETED. Checking the after label would ask whether the new, not-yet-existing
// bucket is empty, which is both trivially true and completely beside the point.
func (v Verdict) partition(census BucketCensus) (blocking, harmless []Finding) {
	for _, f := range v.Destructive {
		if f.Type == objBucketType && f.Kind == "replace" && f.BeforeLabel != "" && census.Empty(f.BeforeLabel) {
			harmless = append(harmless, f)
			continue
		}
		blocking = append(blocking, f)
	}
	return blocking, harmless
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
// Pure over the verdict, so every arm is reachable from a table test.
//
// ── THE HALF-MIGRATED INSTANCE, AND WHY THE PREFIX CLAIM NEEDS THE WHOLE PLAN ─
//
// `objLabelPrefix: <old>` is a claim about EVERY bucket this instance has, and
// the renamed ones are not the whole set. gsap-apl's real prod plan is the case:
//
//	harbor_registry  replace  platform-harbor-registry-prod -> gsap-apl-harbor-registry-prod
//	loki_chunks      replace  platform-loki-chunks-prod     -> gsap-apl-loki-chunks-prod
//	loki_admin       no-op    gsap-apl-loki-admin-prod
//	loki_ruler       no-op    gsap-apl-loki-ruler-prod
//
// Both renamed buckets agree on platform -> gsap-apl, so the agreement rule below
// is satisfied and the remedy confidently said `objLabelPrefix: platform`. Pinning
// that produces `2 to add, 0 to change, 2 to destroy` again — on loki_admin and
// loki_ruler, which were already where the spec wanted them and which the operator
// was never warned about. The advice does not fix the plan; it moves the
// destruction onto two different live buckets, and it reads as the safe option.
//
// So a settled bucket already carrying the prefix being moved AWAY FROM is
// disqualifying evidence: there is no single value of objLabelPrefix that leaves
// this instance's buckets alone, and saying so is the only honest answer. The
// renames are still reported — the operator still has to act — but nothing is
// named as the fix, because nothing is.
func RenameRemedy(v Verdict, census BucketCensus, keyLabels []string) string {
	var was, now string
	var seenPrefix, prefixesDisagree bool
	var renamed []Finding
	for _, f := range v.Destructive {
		if f.Type != objBucketType || f.BeforeLabel == "" || f.AfterLabel == "" || f.BeforeLabel == f.AfterLabel {
			continue
		}
		renamed = append(renamed, f)
		oldPrefix, newPrefix := splitPrefix(f.BeforeLabel, f.AfterLabel)
		// AGREEMENT ACROSS EVERY RENAMED BUCKET, or no prefix claim at all. One
		// prefix is the whole remedy; two different ones mean something other than
		// a prefix change is going on and naming either would be a guess.
		//
		// DISAGREEMENT IS STICKY, and that is the whole reason this is a flag rather
		// than the obvious `was, now = "", ""`. Clearing them put the loop back in its
		// "nothing seen yet" state, so the NEXT rename re-seeded from itself and the
		// disagreement was forgotten: three renames whose odd one out is not last
		// printed a confident prefix plus "the plan goes empty and no bucket is
		// touched", while pinning it would destroy the non-conforming bucket. Two
		// findings can never reach the re-seed, which is why the arm that covers this
		// never saw it.
		switch {
		case !seenPrefix:
			seenPrefix, was, now = true, oldPrefix, newPrefix
		case was != oldPrefix || now != newPrefix:
			prefixesDisagree = true
		}
	}
	if prefixesDisagree {
		was, now = "", ""
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
	// The disqualifying evidence, checked before the prefix is offered rather than
	// alongside it: a remedy printed and then qualified is a remedy people act on.
	if stranded := bucketsNotUnder(was, v.SettledBuckets); was != "" && now != "" && len(stranded) > 0 {
		fmt.Fprintf(&b, `
NO SINGLE PREFIX FIXES THIS INSTANCE — it is half-migrated. The renames above
move %q -> %q, but this plan LEAVES these ALONE, and they are not under
%q — so pinning it would rename them in the other direction:

`, was, now, was)
		for _, label := range stranded {
			fmt.Fprintf(&b, "    %s\n", label)
		}
		// THE SPELLING IS DELIBERATE: this paragraph names the prefix but never in
		// the `objLabelPrefix: <value>` form, which appears ONLY in the arm that is
		// recommending it. A hurried operator copies the line that looks like
		// config, and the whole point here is that the config line is wrong.
		fmt.Fprintf(&b, `
So pinning the prefix back to %q — the advice that fits the renames above,
and what this check used to print here — would propose destroying THOSE
instead. It does not empty the plan; it moves the destruction onto different
live buckets, and it reads like the safe option.
`, was)
		// THE CENSUS TURNS THIS FROM A SHRUG INTO A RECOMMENDATION. "Decide per
		// bucket, the tooling cannot help" is true only while nobody has asked how
		// much data each bucket holds — and Linode answers that in the same call that
		// lists them. With counts in hand there is usually one obviously right
		// answer: keep the buckets with data, let the empty ones move.
		b.WriteString(recommendPrefix(was, renamed, stranded, census, keyLabels))
	} else if was != "" && now != "" {
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
	} else {
		// NO PREFIX AGREED, AND run.go RETURNS ON ANY NON-EMPTY REMEDY — so without
		// this the operator got the rename list and NO instruction of any kind: the
		// specific advice does not apply and the generic block below it never prints.
		// Reachable whenever the renames do not share one prefix move, which the
		// sticky-disagreement fix made considerably more reachable than it looks.
		b.WriteString(`
NO SINGLE PREFIX EXPLAINS THESE RENAMES — they do not all move the same way, so
there is no one spec value to pin and this check will not guess at one.

WHAT TO DO. Read the moves above and decide per bucket. If most of them share a
prefix, ` + "`spec.instance.objLabelPrefix`" + ` is still the lever for those — it is only
the odd ones out that need handling on their own. For any bucket you do intend to
rename, the objects have to be COPIED across and the consumer (Loki, Harbor)
repointed first; removing the old bucket is a destroy, and the destroy path is
where the confirmation for that lives. Check each one's contents before assuming
it is empty — Linode refuses to delete a non-empty bucket, and an empty one it
deletes without complaint.
`)
	}
	return b.String()
}

// recommendPrefix turns the two candidate prefixes into advice, using how much
// data each side actually holds.
//
// It recommends the prefix whose renames are ALL empty, because that is the move
// with nothing to lose: every bucket carrying objects stays where it is, and the
// gate's own census-based exemption will then let the apply through. When both
// sides would destroy data — or the census is silent — it says so and stops,
// which is the honest answer and the one that was always printed before.
func recommendPrefix(was string, renamed []Finding, stranded []string, census BucketCensus, keyLabels []string) string {
	// Pinning `was` renames the STRANDED buckets. Leaving the spec alone renames
	// the ones already in the plan. Each side is safe only if everything it moves
	// is known-empty.
	pinSafe, pinKnown := allEmpty(stranded, census)
	var current []string
	for _, f := range renamed {
		current = append(current, f.BeforeLabel)
	}
	keepSafe, keepKnown := allEmpty(current, census)

	var b strings.Builder
	if len(census) == 0 {
		// NO EVIDENCE AT ALL is a different sentence from "this token cannot see that
		// bucket", and printing the latter for the former sends an operator to audit
		// permissions they have not got a problem with. Run by hand outside an
		// instance checkout, the honest answer is that nothing was asked.
		// NAMES WHAT TO DO ABOUT IT. "Run this from your instance checkout" was the
		// first wording and it misleads the operator who IS in their checkout and
		// simply has no cached token — sending them to re-read a path instead of to
		// the command that fills it.
		b.WriteString("\nHOW MUCH DATA EACH BUCKET HOLDS could not be established — no Linode credential\n" +
			"was available, so nothing is exempted and no prefix is recommended. Run this from\n" +
			"your instance checkout, where `llz tokens` caches one, or export LINODE_TOKEN.\n")
	} else {
		b.WriteString("\nWHAT EACH BUCKET ACTUALLY HOLDS:\n\n")
		for _, label := range append(append([]string{}, current...), stranded...) {
			if n, ok := census[label]; ok {
				fmt.Fprintf(&b, "    %-44s %s\n", label, objectCount(n))
				continue
			}
			fmt.Fprintf(&b, "    %-44s (unknown — not visible to this token)\n", label)
		}
	}

	switch {
	case pinKnown && pinSafe:
		fmt.Fprintf(&b, `
WHAT TO DO. Pin the old prefix in landingzone.yaml:

    spec:
      instance:
        objLabelPrefix: %s

then re-render and re-run. Every bucket holding data stays exactly where it is;
the ones that move are EMPTY, so the replacement loses nothing and this check
passes them for that reason.
`, was)
		b.WriteString(keyLabelNote(was, keyLabels))
	case keepKnown && keepSafe:
		fmt.Fprintf(&b, `
WHAT TO DO. Leave the spec as it is and re-run. The buckets this plan renames are
all EMPTY, so the replacement loses nothing and this check passes them for that
reason — it was only the half-migrated shape that made the advice above look
necessary.
`)
	default:
		b.WriteString(`
WHAT TO DO. Both directions would destroy a bucket that holds objects, so there is
no prefix to pin and this check will not guess. Decide per bucket: for each one you
do intend to rename, COPY the objects across and repoint the consumer (Loki,
Harbor) first, then remove the old bucket down the destroy path, where the
confirmation for that lives.

Step by step: docs/runbooks/bucket-prefix-rename.md
`)
	}
	return b.String()
}

// keyLabelNote reports whether the prefix being recommended also matches this
// account's Object Storage KEY labels.
//
// THE PREFIX NAMES BOTH, and a recommendation checked against the buckets alone is
// half an answer: `llz reap` and the credential-rotation table match key labels
// exactly, so a prefix that is right for the buckets and wrong for the keys moves
// the data problem into a rotation problem nobody is looking for. This used to be
// a caveat telling the operator to go and check. The same call that counts the
// objects lists the keys, so it is checkable, and a caveat that can be checked and
// is not is just a way of being wrong more slowly.
func keyLabelNote(prefix string, keyLabels []string) string {
	if len(keyLabels) == 0 {
		return "\nThe prefix also names this instance's Object Storage KEY labels, which `llz reap`\n" +
			"and the rotation table match exactly. This check could not list them — confirm\nthem yourself before you commit.\n"
	}
	var match, other []string
	for _, l := range keyLabels {
		if strings.HasPrefix(l, prefix+"-") {
			match = append(match, l)
			continue
		}
		other = append(other, l)
	}
	if len(match) == 0 {
		return fmt.Sprintf("\nHEADS UP: the prefix also names this instance's Object Storage KEY labels, and none\n"+
			"of this account's keys are under %q. `llz reap` and the rotation table match those\n"+
			"labels exactly, so check them before you commit.\n", prefix)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nThe prefix also names this instance's Object Storage KEY labels, and these already\n"+
		"agree with it — so pinning %q lines the keys up too, rather than moving them:\n\n", prefix)
	for _, l := range match {
		fmt.Fprintf(&b, "    %s\n", l)
	}
	if len(other) > 0 {
		fmt.Fprintf(&b, "\n  (%d other key(s) on this account are under a different prefix; they may belong to\n"+
			"  another instance, so this check does not reason about them.)\n", len(other))
	}
	return b.String()
}

// allEmpty reports whether every label is known-empty, and whether the census
// knew about all of them. An unknown label makes the answer unknown rather than
// false — the caller must not read "we could not check" as "there is data".
func allEmpty(labels []string, census BucketCensus) (empty, known bool) {
	if len(labels) == 0 {
		return false, false // nothing to move is not a recommendation
	}
	for _, l := range labels {
		n, ok := census[l]
		if !ok {
			return false, false
		}
		if n > 0 {
			return false, true
		}
	}
	return true, true
}

// objectCount renders a count the way the reader needs to weigh it.
func objectCount(n int) string {
	if n == 0 {
		return "EMPTY"
	}
	if n == 1 {
		return "1 object"
	}
	return fmt.Sprintf("%d objects", n)
}

// bucketsNotUnder returns the labels that do NOT sit under prefix — for a
// candidate `objLabelPrefix: <prefix>`, exactly the buckets that pin would rename.
//
// THE QUESTION IS ASKED IN TERMS OF THE VALUE BEING RECOMMENDED, not of the one
// being moved away from. Every bucket label in the module derives from
// label_prefix, so pinning a prefix renames precisely the buckets not already
// under it — which makes this the direct test of "would following this advice
// destroy something", rather than an inference about how the instance got here.
//
// prefix+"-" and not a bare HasPrefix, because the labels are
// `<prefix>-<role>-<env>`: without the separator, prefix "acme" would count
// "acme2-loki-chunks-prod" as already migrated and the advice would be offered on
// evidence that does not exist.
func bucketsNotUnder(prefix string, labels []string) []string {
	if prefix == "" {
		return nil
	}
	var out []string
	for _, l := range labels {
		if !strings.HasPrefix(l, prefix+"-") {
			out = append(out, l)
		}
	}
	return out
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

// ── The allowlist ─────────────────────────────────────────────────────────────
//
// WHY ONE IS NEEDED AT ALL. "An apply must not destroy live infrastructure" is
// the right rule for a bucket holding Loki chunks and the wrong rule for a node
// pool: `linode_lke_node_pool.type` is ForceNew, so changing the node size — an
// ordinary, deliberate operator action taken through the spec — plans as a
// replace. A guard that refuses it unconditionally would block a routine resize
// with no way through, and a guard operators cannot get past is one they turn
// off.
//
// AN ALLOWLIST AND NOT A DENYLIST, and the direction is the whole design. With a
// denylist ("refuse these types"), a resource added to a module later is
// unprotected until somebody remembers to list it — the silent failure. With an
// allowlist, that same new resource is REFUSED on its first destructive plan:
// loud, on the apply, naming the type, and the fix is either a real bug or one
// entry added by someone who looked at it.
//
// The allowlist is per-lane, set where the lane knows what is routine for it: the
// cluster apply permits the node pool and the time_sleep helper; object-storage,
// databases and the shared VPC permit nothing, because nothing they hold is
// routinely recycled.

// PartitionAllowed splits destructive findings into the ones this lane refuses
// and the ones its allowlist permits.
//
// Pure over the findings, so both halves are reachable from a table test.
// An empty allowlist refuses everything, which is what every lane except the
// cluster passes.
func PartitionAllowed(findings []Finding, allowReplace []string) (refused, allowed []Finding) {
	ok := make(map[string]bool, len(allowReplace))
	for _, t := range allowReplace {
		// TrimSpace only. An empty entry is NOT filtered out here, deliberately:
		// filtering it would give the "untyped finding" invariant below a second,
		// redundant guard, and two guards for one property means a mutation to
		// either is caught by neither. The single check that enforces it lives in
		// the loop, where the test can land on it.
		ok[strings.TrimSpace(t)] = true
	}
	for _, f := range findings {
		// A finding with NO TYPE is refused whatever the allowlist says. Type comes
		// from the plan document; an entry this gate could not type is an entry it
		// did not understand, and matching "" against an allowlist would let one
		// through on the strength of a field that was never read.
		if f.Type != "" && ok[f.Type] {
			allowed = append(allowed, f)
			continue
		}
		refused = append(refused, f)
	}
	return refused, allowed
}
