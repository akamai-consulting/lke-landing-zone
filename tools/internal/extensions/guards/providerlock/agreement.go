package providerlock

// agreement.go — the SECOND half of provider coherence: the constraints must
// agree with EACH OTHER, not just with the delivered locks.
//
// ── THE FAILURE THIS EXISTS FOR ────────────────────────────────────────────────
//
// One provider constraint is restated at seven sites: the four generated roots
// under tools/internal/shared/tfroots/roots/*/versions.tf and the three reusable
// modules under terraform-modules/*/versions.tf. Terraform INTERSECTS a root's
// constraint with those of every module it sources, so the restatements are not
// decoration — they all have to hold at once.
//
// Dependabot scans `terraform-modules/*` and NOT the roots (the roots carry
// copier tokens in main.tf, so they are not parseable HCL until rendered — see
// the `terraform` entry in .github/dependabot.yml). So a major bump moves three
// of the seven sites and
// leaves four behind, and the intersection of `~> 3.11` and `~> 4.3` is EMPTY:
//
//	Error: Failed to resolve provider packages
//	Could not resolve provider linode/linode: no available releases match the
//	given constraints ~> 3.11, ~> 4.3
//
// That is PR #504 verbatim. Nothing named the real problem: provider-lock-guard
// reported it as a lock violation ("locked at 3.12.0 ... does not satisfy
// ~> 3.11, ~> 4.3"), which reads as "regenerate the lock" when no lock could ever
// have satisfied it, and `terraform validate` reported it per-root as a registry
// resolution failure with no hint that a second declaration site existed.
//
// ── WHY EQUALITY, NOT MERELY A NON-EMPTY INTERSECTION ─────────────────────────
//
// Terraform would accept a root at `>= 3.0` beside a module at `~> 4.3`. This
// gate is stricter on purpose: in THIS repo the restatements are one constraint
// written down seven times — llz-databases/versions.tf says so in prose ("the
// same CONSTRAINT the cluster and object-storage roots carry"). A rule of "these
// strings are equal" needs no version solver to be right, and it gives a bump the
// one remediation that is always correct: make them equal. A deliberate
// divergence is then a deliberate edit to this gate, with a comment saying why —
// which is the point.
//
// ── WHAT IT CANNOT SEE ────────────────────────────────────────────────────────
//
// It compares declaration sites inside this repo. It says nothing about which
// version an instance resolves (that is the lock, and provider-lock-guard's job),
// and nothing about whether the new major is API-compatible with the resources
// the modules use — `terraform validate` against the real provider is what shows
// that, and it only runs once the constraints agree.

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// versionsFile is the one filename in both trees that carries a
// required_providers block.
const versionsFile = "versions.tf"

// Conflict is one provider declared with more than one version spec. Every site
// is carried so the fix does not need a second search.
type Conflict struct {
	Provider string
	// Sites maps a spec to the repo-relative files declaring it, sorted.
	Sites map[string][]string
}

// Specs returns the distinct specs, sorted, so output is deterministic.
func (c Conflict) Specs() []string {
	out := make([]string, 0, len(c.Sites))
	for s := range c.Sites {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func (c Conflict) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is declared with %d different specs:", c.Provider, len(c.Sites))
	for _, s := range c.Specs() {
		fmt.Fprintf(&b, "\n        %-12s %s", s, strings.Join(c.Sites[s], ", "))
	}
	return b.String()
}

// CheckAgreement reports every provider constrained with more than one distinct
// spec. A provider declared once, or declared many times with the same string,
// is not a conflict.
func CheckAgreement(constraints []Constraint) []Conflict {
	byProvider := map[string]map[string]map[string]bool{}
	for _, c := range constraints {
		if byProvider[c.Provider] == nil {
			byProvider[c.Provider] = map[string]map[string]bool{}
		}
		if byProvider[c.Provider][c.Spec] == nil {
			byProvider[c.Provider][c.Spec] = map[string]bool{}
		}
		// A file is recorded once per spec even if it declares the provider twice;
		// the set is what the report names.
		byProvider[c.Provider][c.Spec][c.From] = true
	}
	var out []Conflict
	for _, p := range sortedKeys(byProvider) {
		specs := byProvider[p]
		if len(specs) < 2 {
			continue
		}
		sites := map[string][]string{}
		for spec, files := range specs {
			sites[spec] = sortedKeys(files)
		}
		out = append(out, Conflict{Provider: p, Sites: sites})
	}
	return out
}

// sortedKeys returns a map's keys in sorted order. Generic so it serves both the
// nested provider map and the plain file set.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AllConstraints reads every required_providers declaration in the template:
// each generated root and each reusable module. Unlike Scan it does NOT skip a
// root that ships no lockfile — databases and vpc ship none, and a constraint
// conflict there breaks `tofu init` just as hard.
//
// Both trees are required to yield at least one declaration. FAIL CLOSED: an
// empty corpus is what a moved directory or a changed file layout looks like,
// and reporting "everything agrees" over nothing read is the failure mode this
// package's regex parsing is most exposed to.
func AllConstraints(repo capability.Repo) ([]Constraint, error) {
	var out []Constraint
	for _, tree := range []string{rootsDir, modulesDir} {
		entries, err := repo.ReadDir(tree)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w (this gate compares the provider constraints "+
				"declared there; with the tree unreadable it would compare nothing)", tree, err)
		}
		found := 0
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			rel := path.Join(tree, e.Name(), versionsFile)
			body, err := repo.ReadFile(rel)
			if err != nil {
				// A directory with no versions.tf constrains nothing — not every module
				// or root has to declare providers.
				continue
			}
			cs := ParseConstraints(string(body), rel)
			found += len(cs)
			out = append(out, cs...)
		}
		if found == 0 {
			return nil, fmt.Errorf("no provider constraint found anywhere under %s — either the "+
				"required_providers blocks moved or the parser stopped matching them, and this gate "+
				"would report agreement having read nothing", tree)
		}
	}
	return out, nil
}
