package providerlock

// providerlock.go — the provider lockfiles an instance CARRIES must satisfy the
// provider constraints the template SHIPS.
//
// ── THE FAILURE THIS EXISTS FOR ────────────────────────────────────────────────
//
// An instance commits zero Terraform code. The roots are generated at every
// terraform op by the `llz` inside vars.TF_IMAGE (render.go, tfroots.Files), and
// `terraform-iac-bootstrap/*/*.tf` is gitignored. What an instance DOES commit is
// `.terraform.lock.hcl`, which .template-manifest classes as `owned`: seeded once
// at scaffold time and never re-touched by an upgrade.
//
// So the provider CONSTRAINT ships in the image and the provider VERSION is
// pinned in the adopter's repo, and nothing has ever compared them. Raise
// `linode` from `~> 3.11` to `~> 4.0` and:
//
//	a NEW adopter   — scaffolds the template's current lock. Fine.
//	release-e2e     — force-pushes a fresh instantiation every run. Fine, green.
//	EVERY EXISTING  — keeps a lock pinning 3.x. `tofu init` refuses outright:
//	INSTANCE          "locked provider does not match configured version
//	                  constraints ... you may need to run tofu init -upgrade".
//
// The composite action runs a plain `tofu init` with no `-upgrade`, so there is
// no recovery inside CI. Every deployment is hard-blocked at the first step of
// every terraform op, and the tag that did it was green on every gate.
//
// That asymmetry — greenfield passes, brownfield breaks — is the whole reason
// this is a gate and not a review item. No e2e lane upgrades an existing
// instance, so no lane can see it.
//
// ── WHAT IS FATAL, AND WHAT IS ONLY REPORTED ──────────────────────────────────
//
// FATAL: a provider recorded in a shipped lock whose version violates a
// constraint the root or one of its modules declares. That, and only that, is
// what makes `tofu init` refuse.
//
// REPORTED, NOT FATAL: a constraint with no lock entry (tofu installs it and
// records it — no error), and a lock entry nothing constrains any more (dead
// weight tofu ignores). Both are drift worth seeing; neither breaks an adopter,
// and failing on them would make the gate cry wolf on states that work.
//
// ── WHAT IT CANNOT SEE ────────────────────────────────────────────────────────
//
// It compares the TEMPLATE's shipped lock against the TEMPLATE's constraints. A
// real adopter's lock is older than that by however many releases they have
// skipped, and this repo cannot read it. The gate therefore catches the bump at
// the moment it is authored — when the fix is editing the lock beside the
// constraint — rather than proving any particular instance is safe.

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// ── Locations ────────────────────────────────────────────────────────────────

// rootsDir holds the TF roots embedded into the llz binary; lockDir holds the
// lockfiles copier delivers. Relative to the repo root the guard is pointed at.
const (
	rootsDir   = "tools/internal/shared/tfroots/roots"
	modulesDir = "terraform-modules"
	lockDir    = "instance-template/terraform-iac-bootstrap"
	lockFile   = ".terraform.lock.hcl"
)

// ── Parsing ──────────────────────────────────────────────────────────────────
//
// Regex rather than an HCL parser, deliberately: tools/AGENTS.md is stdlib-first
// (cobra and sigs.k8s.io/yaml only), and both shapes below are generated or
// hand-written to one house style. The cost of that choice is that a
// reformatting could make a pattern stop matching — which is why every scan here
// fails closed on an empty result instead of reporting a clean tree.

// providerBlock matches one entry of a `required_providers` block:
//
//	linode = {
//	  source  = "linode/linode"
//	  version = "~> 3.11"
//	}
//
// The source and version lines are matched in either order.
var providerBlock = regexp.MustCompile(
	`(?s)([A-Za-z_][A-Za-z0-9_-]*)\s*=\s*\{(.*?)\}`)

var (
	sourceLine  = regexp.MustCompile(`source\s*=\s*"([^"]+)"`)
	versionLine = regexp.MustCompile(`version\s*=\s*"([^"]+)"`)
	// lockProvider matches a lock stanza's address and the version inside it.
	lockProvider = regexp.MustCompile(`(?s)provider\s+"([^"]+)"\s*\{(.*?)\n\}`)
	// moduleSource pulls the module name out of a root's git:: source.
	moduleSource = regexp.MustCompile(`//terraform-modules/([A-Za-z0-9._-]+)\?ref=`)
	// requiredProviders isolates the block so a `module "x" = {` elsewhere in the
	// file cannot be mistaken for a provider entry.
	requiredProviders = regexp.MustCompile(`(?s)required_providers\s*\{(.*)`)
)

// Constraint is one provider requirement and where it was declared, so a failure
// can name the file to edit rather than just the provider.
type Constraint struct {
	Provider string // normalised address, e.g. "linode/linode"
	Spec     string // e.g. "~> 3.11"
	From     string // repo-relative file that declares it
}

// Locked is one provider pin read out of a lockfile.
type Locked struct {
	Provider string // normalised address
	Version  string
}

// normalizeProvider reduces a provider address to `<namespace>/<name>`, which is
// the one spelling both sides share: a lock records the full
// `registry.opentofu.org/linode/linode` while required_providers declares
// `linode/linode`. Comparing the raw strings would find no overlap at all and the
// gate would pass having matched nothing.
func normalizeProvider(addr string) string {
	parts := strings.Split(strings.TrimSpace(addr), "/")
	if len(parts) < 2 {
		return strings.TrimSpace(addr)
	}
	return strings.Join(parts[len(parts)-2:], "/")
}

// ParseConstraints reads every `required_providers` entry in one .tf file.
// Entries with no `version =` are skipped: an unconstrained provider cannot be
// violated.
func ParseConstraints(body, from string) []Constraint {
	block := requiredProviders.FindStringSubmatch(body)
	if block == nil {
		return nil
	}
	var out []Constraint
	for _, m := range providerBlock.FindAllStringSubmatch(block[1], -1) {
		src := sourceLine.FindStringSubmatch(m[2])
		ver := versionLine.FindStringSubmatch(m[2])
		if src == nil || ver == nil {
			continue
		}
		out = append(out, Constraint{Provider: normalizeProvider(src[1]), Spec: ver[1], From: from})
	}
	return out
}

// ParseLock reads the provider pins out of a .terraform.lock.hcl.
func ParseLock(body string) []Locked {
	var out []Locked
	for _, m := range lockProvider.FindAllStringSubmatch(body, -1) {
		ver := versionLine.FindStringSubmatch(m[2])
		if ver == nil {
			continue
		}
		out = append(out, Locked{Provider: normalizeProvider(m[1]), Version: ver[1]})
	}
	return out
}

// ModulesOf lists the terraform-modules a root composes, read from its git::
// source lines. The roots carry a commented-out relative `source` beside each
// git:: one; both spell the module name the same way, and a duplicate is
// harmless because the result is used as a set.
func ModulesOf(mainTF string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range moduleSource.FindAllStringSubmatch(mainTF, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// ── Version comparison ───────────────────────────────────────────────────────

// version is a dotted numeric version. Prerelease/build metadata is dropped:
// lockfiles record plain releases, and a constraint that needs prerelease
// ordering is outside what this gate claims to judge.
type version []int

func parseVersion(s string) (version, error) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return nil, fmt.Errorf("empty version")
	}
	var v version
	for _, part := range strings.Split(s, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not a dotted numeric version", s)
		}
		v = append(v, n)
	}
	return v, nil
}

// compare returns -1, 0 or 1, padding the shorter version with zeros so 3.12 and
// 3.12.0 compare equal.
func compare(a, b version) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

// pessimisticUpper is the exclusive upper bound of `~> c`.
//
// The rule is Terraform's: the LAST specified component may vary and the one
// before it is held. `~> 3.11` allows >=3.11.0 <4.0.0; `~> 3.11.2` allows
// >=3.11.2 <3.12.0. A single-component `~> 3` holds nothing above it and so
// bounds at 4.
func pessimisticUpper(c version) version {
	bump := len(c) - 2
	if bump < 0 {
		bump = 0
	}
	up := make(version, bump+1)
	copy(up, c[:bump+1])
	up[bump]++
	return up
}

// Satisfies reports whether `have` meets `spec`, a comma-separated Terraform
// version constraint. An unparseable spec is an ERROR, never a pass: the gate
// must not launder "I could not tell" into "this is fine".
func Satisfies(have string, spec string) (bool, error) {
	hv, err := parseVersion(have)
	if err != nil {
		return false, fmt.Errorf("locked version: %w", err)
	}
	for _, raw := range strings.Split(spec, ",") {
		clause := strings.TrimSpace(raw)
		if clause == "" {
			continue
		}
		op := "="
		for _, candidate := range []string{"~>", ">=", "<=", "!=", ">", "<", "="} {
			if strings.HasPrefix(clause, candidate) {
				op = candidate
				clause = strings.TrimSpace(strings.TrimPrefix(clause, candidate))
				break
			}
		}
		cv, err := parseVersion(clause)
		if err != nil {
			return false, fmt.Errorf("constraint %q: %w", raw, err)
		}
		cmp := compare(hv, cv)
		ok := false
		switch op {
		case "=":
			ok = cmp == 0
		case "!=":
			ok = cmp != 0
		case ">":
			ok = cmp > 0
		case ">=":
			ok = cmp >= 0
		case "<":
			ok = cmp < 0
		case "<=":
			ok = cmp <= 0
		case "~>":
			ok = cmp >= 0 && compare(hv, pessimisticUpper(cv)) < 0
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// ── The check ────────────────────────────────────────────────────────────────

// Violation is a lock entry that cannot satisfy a declared constraint — the
// state that makes `tofu init` refuse.
type Violation struct {
	Root       string
	Provider   string
	Locked     string
	Spec       string
	DeclaredIn string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s: %s is locked at %s, which does not satisfy %q (declared in %s)",
		v.Root, v.Provider, v.Locked, v.Spec, v.DeclaredIn)
}

// Note is drift that does not break an adopter: a constraint with no pin, or a
// pin nothing constrains.
type Note struct {
	Root, Text string
}

// Result is one root's verdict plus the evidence behind it, so the caller can
// report how much was actually compared.
type Result struct {
	Root       string
	Compared   int // provider pins checked against a constraint
	Violations []Violation
	Notes      []Note
}

// CheckRoot judges one root's lock against the constraints reaching it.
func CheckRoot(root string, constraints []Constraint, locked []Locked) Result {
	res := Result{Root: root}
	byProvider := map[string]Constraint{}
	for _, c := range constraints {
		// A provider constrained in both the root and a module: keep both specs, so
		// the tighter one still has to hold. Joining with a comma is exactly how
		// Terraform combines them.
		if prev, ok := byProvider[c.Provider]; ok && prev.Spec != c.Spec {
			c.Spec = prev.Spec + ", " + c.Spec
			c.From = prev.From + " + " + c.From
		}
		byProvider[c.Provider] = c
	}
	pinned := map[string]bool{}
	for _, l := range locked {
		pinned[l.Provider] = true
		c, ok := byProvider[l.Provider]
		if !ok {
			res.Notes = append(res.Notes, Note{root, fmt.Sprintf(
				"%s is pinned at %s but no root or module constrains it any more — a stale lock entry "+
					"(tofu ignores it; harmless, but it means the lock predates the current module set)",
				l.Provider, l.Version)})
			continue
		}
		res.Compared++
		ok, err := Satisfies(l.Version, c.Spec)
		if err != nil {
			res.Violations = append(res.Violations, Violation{
				Root: root, Provider: l.Provider, Locked: l.Version,
				Spec: c.Spec + " (unparseable: " + err.Error() + ")", DeclaredIn: c.From,
			})
			continue
		}
		if !ok {
			res.Violations = append(res.Violations, Violation{
				Root: root, Provider: l.Provider, Locked: l.Version, Spec: c.Spec, DeclaredIn: c.From,
			})
		}
	}
	for _, p := range sortedProviders(byProvider) {
		if !pinned[p] {
			res.Notes = append(res.Notes, Note{root, fmt.Sprintf(
				"%s is constrained (%s, in %s) but absent from the lock — tofu installs and records it "+
					"on first init, so this does not break an adopter",
				p, byProvider[p].Spec, byProvider[p].From)})
		}
	}
	return res
}

func sortedProviders(m map[string]Constraint) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── Repo traversal ───────────────────────────────────────────────────────────

// Scan walks the repo and returns one Result per root that ships a lockfile.
//
// Roots WITHOUT a lock are skipped rather than failed: vpc and databases ship
// none, so their providers resolve fresh on every init and there is nothing that
// can go stale.
//
// EVERY READ GOES THROUGH capability.Repo. A gate declaring `read-repo` that
// calls os.ReadFile has declared a fence it does not stand behind — it could read
// ~/.aws/credentials while claiming to touch nothing but the repo, and
// TestNoNewRawFilesystemReadsInGuards exists to stop exactly that. Paths here are
// therefore repo-relative and slash-separated, which is what Repo takes.
func Scan(repo capability.Repo) ([]Result, error) {
	rootEntries, err := repo.ReadDir(rootsDir)
	if err != nil {
		return nil, fmt.Errorf("read the TF roots at %s: %w "+
			"(this gate compares them against the delivered lockfiles; with no roots it would "+
			"compare nothing)", rootsDir, err)
	}
	var results []Result
	for _, e := range rootEntries {
		if !e.IsDir() {
			continue
		}
		root := e.Name()
		lockRel := path.Join(lockDir, root, lockFile)
		lockBody, err := repo.ReadFile(lockRel)
		if err != nil {
			if os.IsNotExist(err) {
				continue // no lock shipped for this root — nothing can go stale
			}
			return nil, fmt.Errorf("read %s: %w", lockRel, err)
		}
		locked := ParseLock(string(lockBody))
		if len(locked) == 0 {
			return nil, fmt.Errorf("%s records no provider — either the lock is empty "+
				"(then it should not be committed) or the stanza format changed and this gate is "+
				"reading nothing", lockRel)
		}

		constraints, err := constraintsForRoot(repo, root)
		if err != nil {
			return nil, err
		}
		if len(constraints) == 0 {
			return nil, fmt.Errorf("%s/%s declares no provider constraint — a root that pins providers "+
				"in a committed lock but constrains none cannot be judged, and passing it would be a "+
				"green check over no evidence", rootsDir, root)
		}
		results = append(results, CheckRoot(root, constraints, locked))
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no root with a committed lockfile found under %s — this gate examined "+
			"NOTHING. Either the locks moved, or %s no longer holds the roots", lockDir, rootsDir)
	}
	return results, nil
}

// constraintsForRoot collects the root's own required_providers plus those of
// every module it sources — the effective set `tofu init` resolves against.
func constraintsForRoot(repo capability.Repo, root string) ([]Constraint, error) {
	var out []Constraint
	rootVersions := path.Join(rootsDir, root, "versions.tf")
	body, err := repo.ReadFile(rootVersions)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rootVersions, err)
	}
	out = append(out, ParseConstraints(string(body), rootVersions)...)

	mainBody, err := repo.ReadFile(path.Join(rootsDir, root, "main.tf"))
	if err != nil {
		// A root with no main.tf composes nothing; its own constraints stand alone.
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, mod := range ModulesOf(string(mainBody)) {
		modVersions := path.Join(modulesDir, mod, "versions.tf")
		b, err := repo.ReadFile(modVersions)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", modVersions, err)
		}
		out = append(out, ParseConstraints(string(b), modVersions)...)
	}
	return out, nil
}
