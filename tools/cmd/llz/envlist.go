package main

// envlist.go implements `llz env list` — the deployment inventory the CI
// workflows fan their per-deployment matrices out over. The set of deployments
// is whatever `llz env add` has scaffolded: one `<name>.tfvars` per deployment
// under terraform-iac-bootstrap/cluster/ (see scaffold.go). Terraform is the
// single source of truth — there is deliberately no hardcoded env list — so a
// `discover` job runs `llz env list --json` and feeds the result straight into
// every `strategy.matrix.region`, and the credential-rotation propagation and
// the scheduled health checks can no longer drift apart on which deployments
// they cover.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// listDeployments returns the sorted deployment names discovered under
// <tfDir>/cluster/*.tfvars. The cluster root is the canonical one: every
// deployment owns a Linode cluster, so its cluster tfvars is the authoritative
// marker (the other roots can drift if a file is hand-removed). The template's
// own terraform.tfvars[.example] and any non-conforming basename are skipped —
// the latter with a stderr warning, so a stray file can never inject a poisoned
// value into a CI matrix. Pure (takes tfDir) so it is unit-testable against a
// temp dir.
func listDeployments(tfDir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(tfDir, "cluster", "*.tfvars"))
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, p := range matches {
		name := strings.TrimSuffix(filepath.Base(p), ".tfvars")
		// `terraform.tfvars` (a non-suffixed local override) and
		// `terraform.tfvars.example` (the template) are never deployments.
		if name == "terraform" || name == "terraform.example" {
			continue
		}
		if err := validateEnvName(name); err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s — %v\n", p, err)
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func runEnvList(jsonOut, haOnly, ordered bool, role string) error {
	tfDir, _, _ := instanceLayout()
	var names []string
	switch {
	case ordered:
		// Promotion order, not alphabetical: the sequence a promote-on-green
		// workflow walks (dev → staging → prod). Only ranked deployments appear.
		stages, err := readPromotion(tfDir)
		if err != nil {
			return err
		}
		names = promotionOrder(stages)
	case haOnly || role != "":
		deps, err := readTopology(tfDir)
		if err != nil {
			return err
		}
		names = haFilter(deps, haOnly, role)
	default:
		n, err := listDeployments(tfDir)
		if err != nil {
			return err
		}
		names = n
	}
	if jsonOut {
		// A bare JSON array drops straight into `fromJSON(...)` →
		// strategy.matrix.region with no wrapper to unpick. names is never nil
		// (listDeployments seeds it to []), so this prints `[]`, never `null`.
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

func envListCmd() *cobra.Command {
	var jsonOut, haOnly, ordered bool
	var role string
	c := &cobra.Command{
		Use:   "list",
		Short: "list the scaffolded deployments (the CI matrix source of truth)",
		Long: "Lists every deployment scaffolded by `llz env add` — one per\n" +
			"terraform-iac-bootstrap/cluster/<name>.tfvars. The CI workflows' `discover`\n" +
			"job runs `llz env list --json` and feeds it into each per-deployment\n" +
			"matrix, so Terraform (the tfvars) is the single source of truth and a new\n" +
			"deployment is covered everywhere the moment it is added. --ha narrows to the\n" +
			"OpenBao HA members (ha_role != standalone); --role filters by exact role.\n" +
			"--ordered emits only the deployments that declare a promotion_rank, in\n" +
			"ascending promotion order (dev → staging → prod) — the sequence a\n" +
			"promote-on-green pipeline walks (see `llz env next`).\n" +
			"Layout-aware (instance root or a template-repo checkout).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runEnvList(jsonOut, haOnly, ordered, role) },
	}
	f := c.Flags()
	f.BoolVar(&jsonOut, "json", false, "emit a JSON array of deployment names (for `fromJSON` in a workflow matrix)")
	f.BoolVar(&haOnly, "ha", false, "only deployments in an HA pair (ha_role active|standby)")
	f.BoolVar(&ordered, "ordered", false, "only ranked deployments, in promotion order (ascending promotion_rank)")
	f.StringVar(&role, "role", "", "only deployments with this exact ha_role (active|standby|standalone)")
	return c
}
