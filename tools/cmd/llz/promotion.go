package main

// promotion.go derives an ordered code-promotion pipeline from the per-deployment
// `promotion_rank` declared in cluster tfvars (see
// terraform-iac-bootstrap/cluster/variables.tf). It is the same trick as
// topology.go: a tfvars field that Terraform declares but that llz reads to drive
// CI — here, "what order do I promote a change through my deployments?".
//
// The model: assign ascending positive ranks to the deployments you want to form
// a pipeline (dev=1, staging=2, prod=3). Rank 0 (the default) means "not in any
// pipeline", so the pipeline is explicit opt-in and existing deployments are
// untouched until an operator ranks them. Ranks must be unique — a pipeline is a
// line, not a tie — so `nextStage` is unambiguous.
//
//   • `llz env list --ordered`  — the ranked deployments in promotion order, the
//                                 sequence a promote-on-green workflow walks.
//   • `llz env next <name>`     — the deployment promoted into after <name>; errors
//                                 on the last stage (nothing left to promote to).
//
// Pure helpers (take a tfDir / a []promoStage) so they unit-test against a temp
// dir, mirroring topology.go.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"github.com/spf13/cobra"
)

// promoStage is a deployment's position in the promotion pipeline.
type promoStage struct {
	name string
	rank int
}

// hclIntField matches `field = 123` (an unquoted HCL number) in a tfvars body —
// the numeric twin of topology.go's hclStringField.
func hclIntField(body, field string) (int, bool) {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(field) + `\s*=\s*(-?\d+)`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// readPromotion returns the deployments that declare a positive promotion_rank,
// sorted into promotion order (ascending rank). Rank 0 / absent is omitted (not
// in a pipeline). A rank used by two deployments is a hard error: the pipeline
// must be a strict line so "promote to next" is well-defined. Reuses
// listDeployments so the deployment set is identical to `llz env list`.
func readPromotion(tfDir string) ([]promoStage, error) {
	ranks, err := promotionRanks(tfDir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ranks))
	for name := range ranks {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := map[int]string{}
	stages := []promoStage{}
	for _, name := range names {
		rank := ranks[name]
		if rank <= 0 {
			continue
		}
		if other, dup := seen[rank]; dup {
			return nil, fmt.Errorf("promotion_rank %d is set on both %q and %q — ranks must be unique across the pipeline", rank, other, name)
		}
		seen[rank] = name
		stages = append(stages, promoStage{name: name, rank: rank})
	}
	sort.Slice(stages, func(i, j int) bool { return stages[i].rank < stages[j].rank })
	return stages, nil
}

// promotionRanks returns each deployment's promotion_rank from the LandingZone
// spec when present (the source of truth), else from the committed cluster tfvars.
func promotionRanks(tfDir string) (map[string]int, error) {
	if lz, present, err := loadSpec(); present {
		if err != nil {
			return nil, err
		}
		out := make(map[string]int, len(lz.Spec.Environments))
		for name, e := range lz.Spec.Environments {
			out[name] = e.Cluster.PromotionRank
		}
		return out, nil
	}
	names, err := listDeployments(tfDir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(names))
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(tfDir, "cluster", name+".tfvars"))
		if err != nil {
			return nil, err
		}
		if rank, ok := hclIntField(string(body), "promotion_rank"); ok {
			out[name] = rank
		}
	}
	return out, nil
}

// promotionOrder projects the ordered stages down to their names.
func promotionOrder(stages []promoStage) []string {
	out := make([]string, 0, len(stages))
	for _, s := range stages {
		out = append(out, s.name)
	}
	return out
}

// nextStage returns the deployment promoted into after name — the next-higher
// rank. ok is false when name is unranked/unknown (not in the pipeline) or is the
// final stage (nothing left to promote to).
func nextStage(stages []promoStage, name string) (string, bool) {
	idx := -1
	for i, s := range stages {
		if s.name == name {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(stages) {
		return "", false
	}
	return stages[idx+1].name, true
}

func envNextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "next <deployment>",
		Short: "print the deployment promoted into after <deployment> (the next promotion_rank); errors on the last stage",
		Long: "Reads each deployment's promotion_rank (cluster tfvars) and prints the\n" +
			"next stage in the pipeline — what a promote-on-green CI job builds after\n" +
			"<deployment> goes green. Errors if <deployment> is unranked (not in a\n" +
			"pipeline) or is the final stage. Pair with `llz env list --ordered`.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			tfDir, _, _ := instanceLayout()
			stages, err := readPromotion(tfDir)
			if err != nil {
				return err
			}
			if _, ok := findStage(stages, args[0]); !ok {
				return fmt.Errorf("deployment %q has no promotion_rank — it is not in a promotion pipeline (set promotion_rank in cluster/%s.tfvars)", args[0], args[0])
			}
			next, ok := nextStage(stages, args[0])
			if !ok {
				return fmt.Errorf("deployment %q is the last stage — nothing to promote to", args[0])
			}
			fmt.Println(next)
			return nil
		},
	}
}

// findStage reports whether name is a ranked member of the pipeline.
func findStage(stages []promoStage, name string) (promoStage, bool) {
	for _, s := range stages {
		if s.name == name {
			return s, true
		}
	}
	return promoStage{}, false
}
