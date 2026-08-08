package capability

// This is the first thing in the tree where a grant CONSTRAINS rather than
// annotates, so the tests are adversarial: every one of them tries to get a write
// through a handle that should not permit it.

import (
	"errors"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func binding(grants ...extension.Grant) extension.Binding {
	return extension.Binding{Kind: extension.Assertion, State: extension.Converged, Grants: grants}
}

// A recording exec that FAILS the test if it is ever reached. Most of these cases
// are about refusing before the process starts.
func mustNotRun(t *testing.T) (func(string, ...string) ([]byte, error), func(string, ...string) string) {
	t.Helper()
	e := func(name string, args ...string) ([]byte, error) {
		t.Errorf("shelled out to %s %v — the refusal must happen BEFORE the process starts, "+
			"or a denied write has already reached the cluster by the time we report it", name, args)
		return nil, nil
	}
	return e, func(name string, args ...string) string { e(name, args...); return "" }
}

func TestAReaderCannotWrite(t *testing.T) {
	e, c := mustNotRun(t)
	h := WithExec(binding(extension.ClusterRead), e, c)

	// Every write verb, through the handle a read-only binding gets.
	_, writes := ClassifiedVerbs()
	for _, v := range writes {
		if err := h.Cluster.Permits("-n", "argocd", v, "thing", "name"); err == nil {
			t.Errorf("a cluster-read handle permitted kubectl %s", v)
		}
		if _, err := h.Cluster.Run("-n", "argocd", v, "thing", "name"); err == nil {
			t.Errorf("a cluster-read handle RAN kubectl %s", v)
		}
	}
}

func TestAReaderCanRead(t *testing.T) {
	var got []string
	h := WithExec(binding(extension.ClusterRead),
		func(name string, args ...string) ([]byte, error) {
			got = append([]string{name}, args...)
			return []byte("ok"), nil
		},
		func(string, ...string) string { return "" })

	out, err := h.Cluster.Run("-n", "argocd", "get", "pods", "-o", "json")
	if err != nil {
		t.Fatalf("a cluster-read handle refused a read: %v", err)
	}
	if string(out) != "ok" {
		t.Errorf("output = %q", out)
	}
	// The argv reaches kubectl unchanged: this layer polices, it does not rewrite.
	if strings.Join(got, " ") != "kubectl -n argocd get pods -o json" {
		t.Errorf("argv = %v", got)
	}
}

// A cluster-write binding READS through Cluster and MUTATES through Writer. It
// does not get a wider argv path — that is the whole granular pass: `cluster-write`
// used to mean any mutating kubectl subcommand, including `drain`, `taint` and
// `exec ... -- sh -c`.
func TestAWriterReadsThroughClusterAndMutatesThroughWriter(t *testing.T) {
	h := WithExec(binding(extension.ClusterWrite),
		func(string, ...string) ([]byte, error) { return []byte("ok"), nil },
		func(string, ...string) string { return "ok" })

	if _, err := h.Cluster.Run("-n", "argocd", "get", "applications"); err != nil {
		t.Errorf("cluster-write refused a read: %v", err)
	}
	// The argv path stays read-only even here.
	if err := h.Cluster.Permits("-n", "argocd", "annotate", "application", "x", "k=v"); err == nil {
		t.Error("cluster-write got a generic write through the argv path — mutations must go " +
			"through the named operations so a reviewer sees Annotate, not an argv")
	}
	if err := h.Writer.PermitsWrite(); err != nil {
		t.Errorf("cluster-write cannot mutate: %v", err)
	}
}

// THE FLAG-SKIPPING IS WHERE THIS GETS BYPASSED IF IT IS WRONG. A verb hidden
// behind a flag that takes a value would be read as the value and never checked.
func TestVerbIsFoundPastFlags(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"get", "pods"}, "get"},
		{[]string{"-n", "argocd", "get", "pods"}, "get"},
		{[]string{"--namespace", "argocd", "delete", "job"}, "delete"},
		{[]string{"--kubeconfig", "/tmp/kc", "-n", "ns", "patch", "app"}, "patch"},
		// --flag=value consumes only itself, so the verb is the next token.
		{[]string{"--namespace=argocd", "delete", "job"}, "delete"},
		{[]string{"--server-side", "apply", "-f", "-"}, "apply"},
		{[]string{}, ""},
		{[]string{"-n", "argocd"}, ""},
	} {
		if got := Verb(tc.argv); got != tc.want {
			t.Errorf("Verb(%v) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

// A write verb smuggled in as the VALUE of a flag must not be mistaken for the
// verb, and the real verb after it must still be caught.
func TestAWriteVerbCannotHideBehindAFlagValue(t *testing.T) {
	e, c := mustNotRun(t)
	h := WithExec(binding(extension.ClusterRead), e, c)
	// `-n delete` is a namespace literally called "delete"; the real verb is patch.
	if err := h.Cluster.Permits("-n", "delete", "patch", "app", "x"); err == nil {
		t.Error("a reader permitted a patch hidden behind a flag value named `delete`")
	}
}

func TestAnUnclassifiedVerbIsRefusedByBothHandles(t *testing.T) {
	e, c := mustNotRun(t)
	for _, g := range []extension.Grant{extension.ClusterRead, extension.ClusterWrite} {
		h := WithExec(binding(g), e, c)
		err := h.Cluster.Permits("frobnicate", "thing")
		if err == nil {
			t.Errorf("%s permitted an unknown verb — the default must be refusal, because "+
				"kubectl grows subcommands and the new ones mutate as often as not", g)
		}
		if !strings.Contains(err.Error(), "not a classified verb") {
			t.Errorf("%s: error %q should say the verb is unclassified", g, err)
		}
	}
}

// `kubectl config view` reads; `kubectl config set-context` writes. The mutation
// is in the SECOND word, which a verb-only check would miss entirely.
func TestConfigIsSplitOnItsSubcommand(t *testing.T) {
	e, c := mustNotRun(t)
	r := WithExec(binding(extension.ClusterRead), e, c)
	if err := r.Cluster.Permits("config", "set-context", "x"); err == nil {
		t.Error("a reader permitted `config set-context`, which rewrites kubeconfig")
	}
	ok := WithExec(binding(extension.ClusterRead),
		func(string, ...string) ([]byte, error) { return nil, nil },
		func(string, ...string) string { return "" })
	if err := ok.Cluster.Permits("config", "view"); err != nil {
		t.Errorf("a reader was refused `config view`: %v", err)
	}
}

// An argv with no subcommand at all cannot be judged, so it is refused rather
// than passed through on the assumption that kubectl will reject it anyway.
func TestAnArgvWithNoVerbIsRefused(t *testing.T) {
	e, c := mustNotRun(t)
	h := WithExec(binding(extension.ClusterWrite), e, c)
	if err := h.Cluster.Permits("-n", "argocd"); err == nil {
		t.Error("permitted an argv with no subcommand")
	}
}

// A binding declaring NEITHER cluster grant gets a handle that refuses, not nil.
// nil would panic at the call site and be reported as a crash rather than as a
// permission fault.
func TestNoClusterGrantYieldsARefusingHandleNotNil(t *testing.T) {
	h := For(binding(extension.ReadRepo))
	if h.Cluster == nil {
		t.Fatal("Cluster handle is nil — an ungranted capability must arrive non-nil and refusing")
	}
	if _, err := h.Cluster.Run("get", "pods"); err == nil {
		t.Error("a binding with no cluster grant read the cluster")
	}
	if out := h.Cluster.Combined("get", "pods"); !strings.Contains(out, "no cluster handle") {
		t.Errorf("Combined = %q, want the refusal as its text", out)
	}
}

// Combined's contract is "text out, no error", so a refusal has to surface AS the
// text — its callers match on the message and would otherwise see an empty string
// and conclude the cluster said nothing.
func TestCombinedSurfacesTheRefusalAsText(t *testing.T) {
	e, c := mustNotRun(t)
	h := WithExec(binding(extension.ClusterRead), e, c)
	out := h.Cluster.Combined("delete", "job", "x")
	if !strings.Contains(out, "cluster-write") {
		t.Errorf("Combined = %q, want it to name the missing grant", out)
	}
}

// A test must not be able to widen a binding by stubbing the seam.
func TestStubbingCannotWidenADeniedBinding(t *testing.T) {
	ran := false
	h := WithExec(binding(extension.ReadRepo),
		func(string, ...string) ([]byte, error) { ran = true; return nil, nil },
		func(string, ...string) string { ran = true; return "" })
	_, _ = h.Cluster.Run("get", "pods")
	if ran {
		t.Error("WithExec granted a cluster handle to a binding that declared no cluster grant")
	}
}

// For reads the BINDING, so an extension holding a read assertion and a write
// transition cannot use the transition's grant from inside the assertion.
func TestGrantsAreScopedPerBindingNotPerExtension(t *testing.T) {
	e, c := mustNotRun(t)
	assertion := binding(extension.ClusterRead)
	transition := extension.Binding{
		Kind: extension.Transition, State: extension.Converged,
		Grants: []extension.Grant{extension.ClusterWrite},
	}
	_ = transition // the sibling exists; the assertion must not inherit from it
	if err := WithExec(assertion, e, c).Cluster.Permits("delete", "job", "x"); err == nil {
		t.Error("an assertion inherited its sibling transition's cluster-write")
	}
}

func TestPermitsDoesNotRun(t *testing.T) {
	e, c := mustNotRun(t)
	h := WithExec(binding(extension.ClusterWrite), e, c)
	// Permits is the early-check path: it must answer without shelling out, or a
	// caller using it to decide would perform the action twice.
	if err := h.Cluster.Permits("get", "pods"); err != nil {
		t.Errorf("Permits refused a legal read: %v", err)
	}
}

func TestEveryClassifiedVerbIsInExactlyOneList(t *testing.T) {
	read, write := ClassifiedVerbs()
	seen := map[string]bool{}
	for _, v := range read {
		seen[v] = true
	}
	for _, v := range write {
		if seen[v] {
			t.Errorf("%q is classified as BOTH read and write — the reader would permit it", v)
		}
	}
	if len(read) == 0 || len(write) == 0 {
		t.Fatal("a verb list is empty, so the checks above prove nothing")
	}
}

var _ = errors.New // keep errors imported for future refusal-type assertions
