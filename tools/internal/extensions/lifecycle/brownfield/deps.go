package brownfield

// deps.go — what adopting a brownfield cluster is handed.
//
// THE CATALOG PREDICTED THIS LIST AND WAS RIGHT ABOUT IT. It says import's "four
// non-trivial core calls become `llz render` / `llz new` argv", and the closure
// census found exactly those four — env-add, render, and the two spec editors —
// plus a kubectl seam and two constants. That is the most specific prediction the
// catalog made about any candidate, and it survived contact.
//
// WHAT IT WAS WRONG ABOUT is the conclusion it drew: `ext? ✔`, externalisable. See
// the extension declaration.

import yamlv3 "gopkg.in/yaml.v3"

// EnvSpec is what an adopted cluster tells `llz env add` about itself — the fields
// import can actually derive from a live cluster, and no others.
//
// A NARROW VALUE TYPE rather than package main's envAddOpts, which carries 16
// fields covering HA roles, promotion ranks and runner CIDRs that no amount of
// looking at a cluster reveals. Declaring only what import fills keeps the
// boundary honest: a field appearing here is a claim that adoption can discover it.
type EnvSpec struct {
	Region          string
	ClusterDomain   string
	ObjCluster      string
	AplChartVersion string
	NodeType        string
	NodeCount       string
	SubnetCIDR      string
}

// Deps carries what this package cannot reach for itself.
type Deps struct {
	// ── the scaffold pipeline: import's output is an instance repo ──

	// New scaffolds a fresh instance repo — `llz new`. The fifth of the four calls
	// the catalog named, and the one it folded into "llz new argv" without saying
	// so; import cannot adopt anything without first having a repo to adopt into.
	New func(org, ref, dir string) error

	ValidateEnvName func(env string) error
	EnvAdd          func(env string, spec EnvSpec) error
	EnvSpecFile     func(env string) (string, error)
	EditSpec        func(path string, mutate func(*yamlv3.Node) error, parse func([]byte) error) error
	SetSpecPath     func(doc *yamlv3.Node, dotted, value string) error
	Render          func(env string) error

	// ── reading the cluster being adopted ──

	KubectlOut func(args ...string) (string, error)

	// Confirm is `--yes`. THE THIRD OCCURRENCE of the authorisation-not-capability
	// seam, after teardown and template-sustain, and the one that settles it as a
	// pattern rather than a quirk of destroy: import writes a repo the operator
	// owns, so being able to and being asked to are different questions here too.
	Confirm func() bool

	// DryRun is `--dry-run`, and it is NOT the negation of Confirm. `--yes` is
	// AUTHORISATION for something destructive; `--dry-run` is a request to describe
	// rather than act. Two of this package's steps had the read-only one gated on
	// the destructive one — `llz import scan`, whose own help says "Purely
	// read-only; no --yes needed", printed "(dry-run) … would write" and exited 0
	// on every documented invocation, and `llz import init` scaffolded a whole
	// instance while silently skipping the component toggles the scan had found.
	DryRun func() bool

	// ── defaults that live in core because other verbs read them too ──

	DefaultAplChartVersion string
	DefaultTemplateOrg     string
}
