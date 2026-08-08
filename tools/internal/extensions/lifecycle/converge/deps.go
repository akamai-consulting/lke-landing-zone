package converge

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
)

// Deps carries what this package cannot reach for itself.
//
// AND IT IS INSTALLED, NOT THREADED — which is this extraction's finding, not a
// shortcut taken quietly.
//
// Every extension before this one takes Deps as a PARAMETER: `RunACL(d, …)`,
// `RunOpenbao(d)`. That works when the entry point is close to the seam. Here it
// is not. `converge` is 2,476 lines whose call tree runs six or seven frames deep
// from `RunConverge` to the leaf that shells out, and the leaves are the classic
// health sections — each one a small pure-ish function that reads one thing and
// classifies it. Threading a Deps parameter to all of them would put a capability
// argument on forty functions whose entire content is a predicate, which is the
// opposite of what the split is for.
//
// So the seams are package-level and main installs them once, at startup, via
// Install. That is what they already were in package main (execOutput,
// lokiConfigText and the firewall names were all package-level there); the change
// is that they are now NAMED as a capability set instead of being ambient.
//
// THE COST IS REAL AND WORTH STATING: an installed seam is global mutable state,
// so tests must restore it (see helpers_test.go) and two callers cannot hold
// different Deps at once. Nothing in this repo needs that today — there is one
// binary and one cluster per invocation — but "nothing needs it today" is exactly
// the sentence that precedes an action ABI. **This is the strongest evidence yet
// for one**: an action ABI would hand each binding its own capability handle at
// dispatch time, which is precisely the thing package-level installation cannot
// do. See docs/designs/internal-extension-model.md.
type Deps struct {
	// Writer is the six named cluster mutations, scoped by this extension's
	// declared grants. The mutating calls here used to be assembled as an argv on
	// the general exec seam, which could equally have run `delete namespace` or
	// `exec ... -- sh -c`.
	Writer capability.Writer
	// Exec captures a command's stdout. The classified cluster reads go through
	// internal/kubectlprobe, which owns its own seam; this is for the calls that
	// need raw output or a non-probe verb (rollout restart, annotate, patch, wait).
	Exec func(name string, args ...string) ([]byte, error)

	// DryRun suppresses every mutation and prints what would have run. This is the
	// only half of package main's globalOpts this package uses; taking the whole
	// struct would drag the command tree in behind it.
	DryRun bool

	// Summary appends to a GitHub Actions file (GITHUB_STEP_SUMMARY). Must do real
	// work in fixtures: the convergence report IS the summary, so a no-op stub
	// makes every assertion about what converge reported run against nothing.
	Summary func(envVar string, lines ...string) error

	// StripOversizedCRDLastApplied clears the kubectl.kubernetes.io/last-applied-
	// configuration annotation when a CRD exceeds the 256KB limit, returning the
	// CRDs it stripped. A CLUSTER WRITE performed from inside what otherwise reads
	// — which is why it lands on the transition binding and not the assertion. See
	// the declaration.
	StripOversizedCRDLastApplied func() []string

	// FirewallDeploymentName / FirewallConfigMapName are the single source of truth
	// for the firewall controller's object names, owned by the firewall verbs.
	// Installed rather than redeclared so this package cannot drift from them.
	FirewallDeploymentName string
	FirewallConfigMapName  string

	// ExecCombined runs a command and returns stdout+stderr as one string,
	// ignoring exit status. The wait lanes use it for diagnostics: on a failure
	// path the tool's own error text is the whole value, and Exec (stdout-only,
	// error-gated) discards it.
	ExecCombined func(name string, args ...string) string

	// BaoLoopbackEnv is the in-pod BAO_ADDR/BAO_CACERT set. Owned by the openbao
	// verbs, which is where the loopback address and pod CA path are defined.
	BaoLoopbackEnv func() []string

	// ConvergenceReport is the RECONCILER's Argo-CD convergence classifier, reached
	// through the pod ServiceAccount rather than kubectl. The kubectl-free
	// `health-incluster` sibling runs on it.
	//
	// Injected as an opaque func because its real signature takes the reconciler's
	// nodeGetter, and a converge package that knew that interface would be a
	// converge package that depends on the reconciler's client model. What it needs
	// is a verdict, so that is what the seam returns.
	ConvergenceReport func(ctx context.Context) (health.Report, bool, error)
}

// deps is the installed capability set. Defaults do the real thing, so a caller
// that forgets Install gets working behaviour rather than a nil-func panic —
// the action-ABI lesson from the earlier extractions: hand zero values that work.
var deps = Deps{
	// Refuses until installed: an un-installed Deps must not mutate a cluster.
	Writer:                       capability.For(extension.Binding{}).Writer,
	Exec:                         func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).Output() },
	Summary:                      realGHAAppend,
	StripOversizedCRDLastApplied: func() []string { return nil },
	ExecCombined:                 func(string, ...string) string { return "" },
	BaoLoopbackEnv:               func() []string { return nil },
	ConvergenceReport: func(context.Context) (health.Report, bool, error) {
		return health.Report{}, false, errors.New("converge: ConvergenceReport not installed")
	},
}

// Install wires the capabilities main owns. Call once, before any command runs.
func Install(d Deps) { deps = d }

// realGHAAppend is the DEFAULT Summary, and it does the real thing.
//
// It began as `func(string, ...string) error { return nil }` and that broke a
// test immediately: TestReportConvergeLongPoleWarnsOnlyOnWriteFailure asserts
// that an unwritable summary path WARNS, and a no-op default can never fail, so
// the assertion ran against nothing and the test reported a swallowed warning.
//
// That is the same vacuous-fixture bug two earlier extractions shipped (teardown's
// Summary, objenc's SecretField). The lesson generalises past fixtures: an
// INSTALLED default is a fixture too, and defaulting it to a no-op quietly makes
// every un-installed caller untestable in exactly the same way.
func realGHAAppend(envVar string, lines ...string) error {
	path := os.Getenv(envVar)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(lines, "\n") + "\n")
	return err
}

// W returns the Writer, or a refusing one if the field was never populated. A
// Deps built as a struct literal has a nil interface there, and a nil interface
// method call is a panic rather than the permission fault the denied handle
// exists to produce.
func (d Deps) W() capability.Writer {
	if d.Writer == nil {
		return capability.Denied()
	}
	return d.Writer
}
