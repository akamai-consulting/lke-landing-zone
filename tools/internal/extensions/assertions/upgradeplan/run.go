package upgradeplan

// run.go — the reporting half, kept out of upgradeplan.go so the judgement there
// stays a pure function over parsed input.

import (
	"fmt"
	"io"
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// Run reads a plan JSON from `path` ("-" for stdin) and reports.
//
// `expectNoChanges` tightens the predicate from "proposes no destruction" to
// "proposes NOTHING". See the header of noChangesFailure for why that stricter
// question is worth asking separately.
//
// `allowReplace` names resource TYPES whose destruction is a routine operator
// action rather than a finding — see PartitionAllowed. It is an allowlist and not
// a denylist on purpose, and that choice is the fail-closed direction: a resource
// type this gate has never met is REFUSED by default, so a module that starts
// recycling something new is loud on the first apply instead of silent until
// somebody notices the class was never listed.
func Run(path string, expectNoChanges bool, allowReplace []string, out, errOut io.Writer, stdin io.Reader) error {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return fmt.Errorf("read the plan: %w", err)
	}
	p, err := Parse(raw)
	if err != nil {
		return err
	}
	v := Evaluate(p)
	// The census is only consulted when there is something destructive to weigh, so
	// the ordinary clean plan makes no network call at all.
	var census BucketCensus
	if len(v.Destructive) > 0 {
		census = LookupBuckets()
	}

	// SAY WHAT WAS EXAMINED, always. "no destructive changes" over 40 resources and
	// over 0 are the same words and very different claims, and the second is what a
	// plan taken against the wrong state key looks like.
	fmt.Fprintf(out, "assert-upgrade-plan: %d resource change(s) examined — %d create, %d update, %d destructive.\n",
		v.Total, v.Creates, v.Updates, len(v.Destructive))
	if v.Total == 0 {
		fmt.Fprintf(out, "  (the plan proposes nothing at all. That is a valid upgrade outcome, but if a change "+
			"WAS expected, check the state key this plan was taken against.)\n")
	}
	// THE EXEMPTION, AND WHY IT IS NOT A RELAXATION. A bucket replace that deletes
	// an EMPTY bucket destroys nothing, and refusing it made the correct move
	// unperformable: an operator who pins the prefix that keeps their data-bearing
	// buckets is then blocked by the empty ones that pin moves the other way. Every
	// other destructive finding — a cluster, a bucket with objects, a bucket the
	// census could not see — still blocks. Reported loudly rather than silently
	// tolerated, because "2 buckets replaced" should never scroll past unread.
	blocking, harmless := v.partition(census)
	if len(harmless) > 0 {
		fmt.Fprintf(out, "%s %d bucket(s) will be REPLACED, and each is empty — the rename loses nothing:\n",
			color.Yellow("!"), len(harmless))
		for _, f := range harmless {
			fmt.Fprintf(out, "    %s: %s -> %s (0 objects)\n", f.Address, f.BeforeLabel, f.AfterLabel)
		}
		fmt.Fprintf(out, "  Verified against the Object Storage API just now, not inferred from the plan.\n")
	}
	refused, allowed := PartitionAllowed(blocking, allowReplace)

	// SAY WHAT WAS WAVED THROUGH, every time, and say it on the ERROR stream so it
	// survives a collapsed log group. An allowlist that stays silent is how a gate
	// quietly stops being one: the entry that was added for a node-pool resize is
	// the same entry that lets the next unexpected recycle past, and nobody reads a
	// flag they cannot see in the output.
	for _, f := range allowed {
		fmt.Fprintf(errOut, "::warning::%s would be %sd — PERMITTED because --allow-replace names %s\n",
			f.Address, f.Kind, f.Type)
	}
	if len(refused) == 0 {
		// len(v.Changed), NOT v.Total: Total counts every resource_changes entry,
		// and Terraform lists every resource it READ. Gating on it made a settled
		// cluster look like a busy one — the gate would have been red on every
		// correct run, which is how a gate gets turned off.
		if expectNoChanges && len(v.Changed) > 0 {
			return noChangesFailure(v, errOut)
		}
		return nil
	}
	for _, f := range refused {
		fmt.Fprintf(errOut, "::error::%s would be %sd by this upgrade (actions: %v)\n", f.Address, f.Kind, f.Actions)
	}
	fmt.Fprintf(errOut, "\n%s this upgrade proposes destroying or replacing %d live resource(s):\n",
		color.Red("✗"), len(refused))
	for _, f := range refused {
		fmt.Fprintf(errOut, "    %s\n", f)
	}
	// THE SPECIFIC REMEDY WINS. The generic advice below is about module changes —
	// moved{} blocks, non-forcing spellings, release notes — and none of it applies
	// to a bucket rename, which has no moved{} and no non-forcing form. Printing
	// both would bury the one instruction that works under four that do not.
	if remedy := RenameRemedy(v, census, KeyLabels()); remedy != "" {
		fmt.Fprint(errOut, remedy)
		return fmt.Errorf("assert-upgrade-plan: %d resource(s) would be destroyed or replaced", len(refused))
	}
	fmt.Fprintf(errOut, `
WHAT THIS MEANS. The plan was taken against state an EARLIER release created, so
these are resources an adopter already has. An apply would recycle them — for a
linode_lke_cluster that is every node and every workload on it.

An upgrade is allowed to create and to update in place. It is not allowed to
destroy, because an adopter takes it expecting continuity and nothing in the
pull request shows them otherwise: the roots are gitignored and generated from
the pin, and no pull request runs a plan.

WHAT TO DO. Find which attribute forces it — the human-readable plan prints
"# forces replacement" beside it. Then either make the change non-forcing, or
give the resource a moved{}/import path, or decide the recycle is intended and
say so in the release notes, loudly, before the tag is cut.
`)
	return fmt.Errorf("assert-upgrade-plan: %d resource(s) would be destroyed or replaced", len(refused))
}

// noChangesFailure reports a plan that proposes changes where none were expected.
//
// ── WHY THE STRICTER QUESTION IS WORTH ASKING SEPARATELY ──────────────────────
//
// Destruction is the question that matters when comparing releases. Immediately
// after an apply, a DIFFERENT question is answerable and cheaper: the state was
// just made to match the configuration, so a plan taken now must be empty. Any
// change it proposes is a resource Terraform cannot bring to rest — a perpetual
// diff.
//
// That is not a cosmetic complaint. A perpetual diff means every subsequent
// apply churns the resource, every plan an operator reads is noisy enough that
// they stop reading it, and the class hides the worst version of itself:
// linode_lke_cluster's create-time-only vpc_id/subnet_id plan as a calm
// `update in-place` that the API silently refuses, so the apply "succeeds" and
// the same diff returns forever. That one is caught today by a hand-written
// coupling test naming those two attributes. This catches the NEXT such
// attribute without anyone having to know about it in advance.
//
// Only reachable when the destructive check already passed, so the two verdicts
// never compete for the reader: a plan that destroys something is reported as
// destroying something, not as "unexpected changes".
func noChangesFailure(v Verdict, errOut io.Writer) error {
	for _, rc := range v.Changed {
		fmt.Fprintf(errOut, "::error::%s still proposes %v immediately after an apply\n", rc.Address, rc.Actions)
	}
	fmt.Fprintf(errOut, "\n%s a plan taken straight after an apply proposes %d change(s):\n",
		color.Red("✗"), len(v.Changed))
	for _, rc := range v.Changed {
		fmt.Fprintf(errOut, "    %s — %v\n", rc.Address, rc.Actions)
	}
	fmt.Fprintf(errOut, `
WHAT THIS MEANS. The apply just made the state match the configuration, so this
plan should be empty. A resource that still wants to change is one Terraform
cannot bring to rest: every future apply will churn it and every plan an operator
reads will carry this noise.

THE WORST VERSION OF THIS CLASS looks exactly like the mildest. An attribute the
Linode API accepts on create and silently ignores on update plans as a calm
"update in-place" forever — the apply reports success and changes nothing. That
is what linode_lke_cluster's vpc_id/subnet_id do, and a resource caught here may
be the same shape.

WHAT TO DO. Either the attribute should not be managed (add it to the resource's
lifecycle ignore_changes, as the cluster module does for its VPC binding), or the
value the module computes genuinely differs from what the API returns and the
module should compute the API's form.
`)
	return fmt.Errorf("assert-upgrade-plan: %d resource(s) still propose changes immediately after an apply", len(v.Changed))
}
