package envtopology

// envlist.go implements `llz env list` — the topo.Deployment inventory the CI
// workflows fan their per-topo.Deployment matrices out over. The set of deployments
// is whatever `llz env add` has scaffolded: one `<name>.tfvars` per topo.Deployment
// under terraform-iac-bootstrap/cluster/ (see scaffold.go). Terraform is the
// single source of truth — there is deliberately no hardcoded env list — so a
// `discover` job runs `llz env list --json` and feeds the result straight into
// every `strategy.matrix.region`, and the credential-rotation propagation and
// the scheduled health checks can no longer drift apart on which deployments
// they cover.

import (
	"encoding/json"
	"fmt"

	topo "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envtopology"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/promote"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
)

func runEnvList(jsonOut, haOnly, ordered bool, role string) error {
	tfDir, _, _ := instancelayout.Detect()
	var names []string
	switch {
	case ordered:
		// Promotion order, not alphabetical: the sequence a promote-on-color.Green
		// workflow walks (dev → staging → prod). Only ranked deployments appear.
		stages, err := promote.ReadPromotion(caps.PromoteDeps(), tfDir)
		if err != nil {
			return err
		}
		names = promote.PromotionOrder(stages)
	case haOnly || role != "":
		deps, err := topo.ReadTopology(tfDir)
		if err != nil {
			return err
		}
		names = topo.HAFilter(deps, haOnly, role)
	default:
		n, err := topo.ListDeployments(tfDir)
		if err != nil {
			return err
		}
		names = n
	}
	if jsonOut {
		// A bare JSON array drops straight into `fromJSON(...)` →
		// strategy.matrix.region with no wrapper to unpick. names is never nil
		// (topo.ListDeployments seeds it to []), so this prints `[]`, never `null`.
		b, err := json.Marshal(names)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	for _, n := range names {
		fmt.Println(n)
	}
	return nil
}

// writeHAResolution emits role=/peer= to $GITHUB_OUTPUT for one topo.Deployment
// (peer empty for a standalone) and prints a human line. Split from ResolveCmd
// so the output logic is unit-testable without an on-disk instance layout.
func writeHAResolution(deps []topo.Deployment, name string) error {
	d, ok := topo.FindDeployment(deps, name)
	if !ok {
		return fmt.Errorf("no such topo.Deployment %q (run `llz env list`)", name)
	}
	// Empty peer is expected for a standalone or a half-added pair; an ambiguous
	// group is not — this value drives which cluster CI pairs with.
	peer, _, err := topo.PeerOf(deps, name)
	if err != nil {
		return err
	}
	shown := peer
	if shown == "" {
		shown = "<none>"
	}
	fmt.Printf("Resolved %s: role=%s peer=%s\n", name, d.HARole, shown)
	return caps.Summary("GITHUB_OUTPUT", "role="+d.HARole, "peer="+peer)
}
