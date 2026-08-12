package upstreamupdates

// prtouches.go — decide whether a pull request's diff may change what Terraform
// state should contain.
//
// This exists because of one downstream `if:` — the tf-import step in
// llz-terraform.yml's plan-cluster-pr job, the only thing on the PR path that
// writes cluster/<deployment>/terraform.tfstate. Nothing serializes that write
// against a concurrent apply, so it must not run for a pull request that no human
// opened: a template-upgrade or Renovate PR rewrites llz-*.yml and nothing else,
// is not a draft, and would otherwise take that write on a schedule.
//
// IT IS GO RATHER THAN SHELL FOR TWO REASONS. The obvious one is the budget:
// inline workflow bash is ratcheted (.untestable-budget.yaml). The real one is
// that "could not tell" has to be distinguishable from "nothing changed", and a
// shell pipeline whose grep finds nothing produces the same empty string either
// way — which is precisely the answer that would skip the import silently.

import (
	"fmt"
	"io"
	"strings"
)

// Classification is the verdict for one pull request.
type Classification struct {
	// Touches is true when at least one changed file matched a prefix.
	Touches bool
	// Files is every path the PR changed, in the order the API returned them.
	Files []string
	// Matched is the subset that matched, so a failure message can name what was
	// seen rather than only what was looked for.
	Matched []string
}

// Classify decides whether any of files sits under one of prefixes.
//
// A prefix ending in "/" matches a directory subtree; anything else must match
// the whole path. That distinction is load-bearing: "environments/" is a tree
// while "landingzone.yaml" is one file, and a naive strings.HasPrefix would let
// "landingzone.yaml.example" — a template artifact no plan depends on — select
// the state write.
//
// FAILS CLOSED ON AN EMPTY FILE LIST. A pull request always changes at least one
// file, so an empty list means the query broke rather than that the PR is empty,
// and reporting "touches nothing" for it would launder a broken API call into a
// skipped import on every PR.
func Classify(files, prefixes []string) (Classification, error) {
	if len(files) == 0 {
		return Classification{}, fmt.Errorf("the pull request listed zero changed files, which cannot happen for a real PR — " +
			"treat this as a broken query, not as an empty diff")
	}
	if len(prefixes) == 0 {
		return Classification{}, fmt.Errorf("no --prefix given, so every PR would classify as touching nothing — " +
			"a gate that always answers the same way is not a gate")
	}
	c := Classification{Files: files}
	for _, f := range files {
		for _, p := range prefixes {
			if (strings.HasSuffix(p, "/") && strings.HasPrefix(f, p)) || f == p {
				c.Matched = append(c.Matched, f)
				break
			}
		}
	}
	c.Touches = len(c.Matched) > 0
	return c, nil
}

// Report writes the human half: what was decided and, when the answer is "no",
// what that costs so a reader of the plan is not surprised by it.
func (c Classification) Report(w io.Writer) {
	if c.Touches {
		fmt.Fprintf(w, "::notice title=Terraform diff::This PR changes Terraform roots or the spec (%s) — the VPC/subnet import will run.\n",
			strings.Join(c.Matched, ", "))
		return
	}
	fmt.Fprintf(w, "::notice title=No Terraform diff::None of the %d changed file(s) touch the Terraform roots or the spec, "+
		"so the state-writing import is skipped. The plan still runs, and may show a VPC or subnet as 'to be created' that already exists.\n",
		len(c.Files))
}
