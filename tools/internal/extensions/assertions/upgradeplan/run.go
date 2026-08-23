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
func Run(path string, out, errOut io.Writer, stdin io.Reader) error {
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

	// SAY WHAT WAS EXAMINED, always. "no destructive changes" over 40 resources and
	// over 0 are the same words and very different claims, and the second is what a
	// plan taken against the wrong state key looks like.
	fmt.Fprintf(out, "assert-upgrade-plan: %d resource change(s) examined — %d create, %d update, %d destructive.\n",
		v.Total, v.Creates, v.Updates, len(v.Destructive))
	if v.Total == 0 {
		fmt.Fprintf(out, "  (the plan proposes nothing at all. That is a valid upgrade outcome, but if a change "+
			"WAS expected, check the state key this plan was taken against.)\n")
	}
	if len(v.Destructive) == 0 {
		return nil
	}
	for _, f := range v.Destructive {
		fmt.Fprintf(errOut, "::error::%s would be %sd by this upgrade (actions: %v)\n", f.Address, f.Kind, f.Actions)
	}
	fmt.Fprintf(errOut, "\n%s this upgrade proposes destroying or replacing %d live resource(s):\n",
		color.Red("✗"), len(v.Destructive))
	for _, f := range v.Destructive {
		fmt.Fprintf(errOut, "    %s\n", f)
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
	return fmt.Errorf("assert-upgrade-plan: %d resource(s) would be destroyed or replaced", len(v.Destructive))
}
