package extension_test

// mustbinding_test.go — selecting a binding by NAME, and failing loudly when there
// is none.
//
// THE PANIC PATHS ARE THE POINT, so they are what this exercises hardest. A miss
// that returned a zero Binding is how two assert lanes shipped unable to mutate: a
// zero Binding declares no grants, capability.For hands back refusing handles, and
// the lane fails at its first write with a permission message naming a grant
// nobody forgot. The whole value of these two functions is that the miss is loud.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func multi() extension.Extension {
	return extension.Extension{
		Name: "assert-thing", Short: "x",
		Bindings: []extension.Binding{
			{Kind: extension.Assertion, Name: "certificates", State: extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead}},
			{Kind: extension.Transition, Name: "login-smoke", State: extension.Converged,
				Grants: []extension.Grant{extension.ClusterRead, extension.ClusterWrite}},
		},
	}
}

func TestMustBindingSelectsByName(t *testing.T) {
	b := multi().MustBinding("login-smoke")
	if b.Kind != extension.Transition || b.Name != "login-smoke" {
		t.Fatalf("MustBinding returned %s, want the login-smoke transition", b)
	}
	// THE REGRESSION, STATED AS AN ASSERTION: the mutating binding is not index 0,
	// and selecting by name must not care where it sits.
	if multi().Bindings[0].Name == "login-smoke" {
		t.Fatal("the fixture no longer models the defect — the mutating binding must NOT be " +
			"first, or this proves nothing about position-independence")
	}
	if !bindingHasGrant(b, extension.ClusterWrite) {
		t.Error("the selected binding does not declare cluster-write — which is the grant the " +
			"handle built from it needs, and the whole reason position was wrong")
	}
}

func TestMustBindingPanicsOnAMiss(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("MustBinding returned instead of panicking on an unknown name — a zero " +
				"Binding declares no grants, so the caller would get a refusing handle and " +
				"report a permission error rather than a wiring bug")
		}
		msg, _ := r.(string)
		for _, want := range []string{"assert-thing", "no-such-binding", "wiring bug"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the panic does not mention %q: %s", want, msg)
			}
		}
	}()
	_ = multi().MustBinding("no-such-binding")
}

func TestMustBindingOfSelectsTheSingleUnnamedBinding(t *testing.T) {
	e := extension.Extension{
		Name: "kyverno-policy", Short: "x",
		Bindings: []extension.Binding{{Kind: extension.Transition, State: extension.Converged,
			Grants: []extension.Grant{extension.ClusterWrite}}},
	}
	if b := e.MustBindingOf(extension.Transition, extension.Converged); b.Kind != extension.Transition {
		t.Fatalf("MustBindingOf returned %s", b)
	}
}

func TestMustBindingOfPanicsOnAMissAndOnAmbiguity(t *testing.T) {
	// A miss.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("MustBindingOf returned on a miss")
			}
		}()
		_ = multi().MustBindingOf(extension.Invariant, extension.Operating)
	}()

	// AMBIGUITY MUST PANIC TOO, and that is not caution. Two bindings of one kind at
	// one state is exactly when Name becomes required; returning the first would be
	// `Bindings[0]` again with more steps, which is the defect this replaced.
	amb := extension.Extension{
		Name: "two-lanes", Short: "x",
		Bindings: []extension.Binding{
			{Kind: extension.Invariant, Name: "a", State: extension.Operating,
				Grants: []extension.Grant{extension.ClusterWrite}},
			{Kind: extension.Invariant, Name: "b", State: extension.Operating,
				Grants: []extension.Grant{extension.SecretCustody}},
		},
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("MustBindingOf picked one of two matching bindings — choosing among them by " +
				"position is the mistake this function exists to make impossible")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "ambiguous") || !strings.Contains(msg, "2") {
			t.Errorf("the panic does not say it is ambiguous, nor how many matched: %v", r)
		}
	}()
	_ = amb.MustBindingOf(extension.Invariant, extension.Operating)
}

// The count in the ambiguity message is rendered without strconv, because this
// package is pinned to import nothing but strings, fmt and sort. A wrong number
// there would misreport how many bindings collided.
func TestTheAmbiguityCountRendersCorrectly(t *testing.T) {
	for _, n := range []int{2, 3, 10, 42} {
		e := extension.Extension{Name: "n", Short: "x"}
		for i := 0; i < n; i++ {
			e.Bindings = append(e.Bindings, extension.Binding{
				Kind: extension.Invariant, Name: string(rune('a' + i)), State: extension.Operating,
				Grants: []extension.Grant{extension.ClusterWrite}})
		}
		func() {
			defer func() {
				msg, _ := recover().(string)
				if !strings.Contains(msg, "("+itoaLocal(n)+" bindings)") {
					t.Errorf("ambiguity message for %d bindings reads: %s", n, msg)
				}
			}()
			_ = e.MustBindingOf(extension.Invariant, extension.Operating)
		}()
	}
}

// itoaLocal is the test's own rendering, deliberately NOT the package's — comparing
// a function against itself would pass however wrong both were.
func itoaLocal(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{digits[n%10]}, out...)
		n /= 10
	}
	return string(out)
}

func bindingHasGrant(b extension.Binding, g extension.Grant) bool {
	for _, have := range b.Grants {
		if have == g {
			return true
		}
	}
	return false
}
