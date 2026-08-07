package envtopology

// envlist.go implements `llz env list` — the Deployment inventory the CI
// workflows fan their per-Deployment matrices out over. The set of deployments
// is whatever `llz env add` has scaffolded: one `<name>.tfvars` per Deployment
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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/promote"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
)

// ListDeployments returns the sorted Deployment names from BOTH sources: the
// committed <tfDir>/cluster/*.tfvars (one per Deployment that owns a Linode
// cluster) AND the LandingZone spec's environments (the environments/<env>.yaml set)
// when a landingzone.yaml is present. The union (dedup by name) means a
// spec-driven Deployment whose transient tfvars are rendered at build time still
// shows up in the CI matrix. The template's own
// terraform.tfvars[.example] and any non-conforming basename are skipped — the
// latter with a stderr warning, so a stray file can never inject a poisoned value
// into a CI matrix. Pure (takes tfDir; the spec is read from the sibling
// instance root) so it is unit-testable against a temp dir.
func ListDeployments(tfDir string) ([]string, error) {
	set := map[string]struct{}{}

	matches, err := filepath.Glob(filepath.Join(tfDir, "cluster", "*.tfvars"))
	if err != nil {
		return nil, err
	}
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
		set[name] = struct{}{}
	}

	// Union the LandingZone spec's environments. The spec lives at the instance
	// root (the parent of terraform-iac-bootstrap), so it is found in both the
	// instance and template-checkout layouts.
	specRoot := filepath.Dir(tfDir)
	if clusterspec.InstancePresent(specRoot) {
		if lz, lerr := clusterspec.LoadInstance(specRoot); lerr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not read LandingZone spec at %s — %v\n", specRoot, lerr)
		} else {
			for _, name := range lz.EnvNames() {
				if err := validateEnvName(name); err != nil {
					fmt.Fprintf(os.Stderr, "warning: skipping spec env %q — %v\n", name, err)
					continue
				}
				set[name] = struct{}{}
			}
		}
	}

	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

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
		deps, err := ReadTopology(tfDir)
		if err != nil {
			return err
		}
		names = haFilter(deps, haOnly, role)
	default:
		n, err := ListDeployments(tfDir)
		if err != nil {
			return err
		}
		names = n
	}
	if jsonOut {
		// A bare JSON array drops straight into `fromJSON(...)` →
		// strategy.matrix.region with no wrapper to unpick. names is never nil
		// (ListDeployments seeds it to []), so this prints `[]`, never `null`.
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
