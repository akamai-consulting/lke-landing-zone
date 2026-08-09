package capability

// ceiling_test.go — the three holes an audit of this package found, each pinned by
// the call that got through.
//
// EVERY CASE HERE IS A TRANSCRIBED PROBE, not a hypothetical. Each one was run
// against the shipping code and came back permitted; the test is the same argv,
// asserting the refusal. That is the difference between a regression test and a
// test written from the fix — it fails if the fix is reverted, and it names what
// the fix was for.

import (
	"errors"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// readHandle is a cluster-read assertion — the narrowest binding that has a
// cluster handle at all, and the one every case below tries to get a write out of.
func readHandle() Cluster { return For(binding(extension.ClusterRead)).Cluster }

// `kubectl auth reconcile` CREATES AND UPDATES RBAC. It reached the cluster through
// a read handle because `auth` was classified on its first word, justified by a
// comment about `auth can-i` — the verb table and the argument for it disagreed,
// and the code followed the table.
func TestAuthReconcileIsRefusedThroughAReadHandle(t *testing.T) {
	c := readHandle()

	if err := c.Permits("auth", "reconcile", "-f", "rbac.yaml"); err == nil {
		t.Error("`kubectl auth reconcile` is permitted through a cluster-read handle — it " +
			"creates and updates ClusterRoles and ClusterRoleBindings, so a read grant that " +
			"allows it is an RBAC escalation with a permission check in front of it")
	}
	// The subcommand the read classification was actually justified by must still
	// work, or the fix has traded a hole for an outage.
	if err := c.Permits("auth", "can-i", "get", "pods"); err != nil {
		t.Errorf("`kubectl auth can-i` refused through a read handle: %v", err)
	}
	// An unlisted subcommand of a split verb is refused, which is what makes the
	// table safe to extend one line at a time.
	if err := c.Permits("auth", "whoami"); err == nil {
		t.Error("an unlisted `auth` subcommand is permitted — subverbReads must fail closed, " +
			"or the next subcommand kubectl adds is allowed before anyone classifies it")
	}
}

// THE DRY-RUN EXEMPTION SCANNED THE WHOLE ARGV, BEFORE CLASSIFYING ANYTHING.
func TestTheDryRunExemptionIsScopedToKubectlsOwnFlags(t *testing.T) {
	c := readHandle()

	for _, tc := range []struct {
		what string
		argv []string
	}{
		// The live one. kubectl stops parsing at `--`, so this `--dry-run` is the
		// CONTAINER's flag and the exec runs for real: arbitrary in-pod execution
		// out of a read grant.
		{"a flag past the `--` separator", []string{"exec", "pod", "--", "helm", "upgrade", "--dry-run"}},
		// kubectl implements no --dry-run for `cp`, so one in this argv can only be
		// an accident or an attack.
		{"a verb with no dry-run flag", []string{"cp", "/etc/passwd", "pod:/tmp/x", "--dry-run=server"}},
		// The exemption ran ahead of the classification it was an exception to, so
		// it contradicted this package's own "unclassified is refused by BOTH
		// handles" rule.
		{"an unclassified verb", []string{"frobnicate", "--dry-run=server"}},
	} {
		if err := c.Permits(tc.argv...); err == nil {
			t.Errorf("%s: `kubectl %s` is permitted through a cluster-read handle — the "+
				"dry-run exemption belongs to kubectl's own flags on a verb that implements "+
				"it, not to any argv containing the string", tc.what, strings.Join(tc.argv, " "))
		}
	}

	// The shipping caller the exemption exists for. assert-network's wave-health
	// gate is built entirely on this argv, so a fix that refused it would have
	// forced a read-only gate to declare cluster-write.
	if err := c.Permits("apply", "--dry-run=server", "-f", "-"); err != nil {
		t.Errorf("`kubectl apply --dry-run=server` refused through a read handle: %v — this "+
			"is the call the exemption was written for", err)
	}
	if err := c.Permits("apply", "--dry-run=none", "-f", "-"); err == nil {
		t.Error("`--dry-run=none` is an explicit request to really apply, and it was permitted")
	}
}

// A DECLARATION THAT CONTRADICTS ITSELF MUST NARROW, NOT WIDEN.
//
// Validate() refuses an assertion holding cluster-write and a gate holding
// anything but read-repo. Nothing enforced either over what RUNS: For() read
// b.Grants and never looked at b.Kind, so a binding that never went through the
// lint — or one minted at a call site — was handed the wider capability.
func TestAKindsBlanketRuleIsAppliedWhenTheHandleIsBuilt(t *testing.T) {
	// Deliberately illegal: Validate() rejects both of these, which is the point.
	contradictoryAssertion := extension.Binding{
		Kind: extension.Assertion, State: extension.Converged,
		Grants: []extension.Grant{extension.ClusterRead, extension.ClusterWrite, extension.SecretCustody},
	}
	if errs := (extension.Extension{Name: "x", Short: "x",
		Bindings: []extension.Binding{contradictoryAssertion}}).Validate(); len(errs) == 0 {
		t.Fatal("the fixture validates — it is supposed to be a declaration the model refuses, " +
			"so this test would be asserting nothing about the contradictory case")
	}

	h := For(contradictoryAssertion)
	if _, err := h.Writer.Annotate("ns", "pod", "p", "k=v"); err == nil {
		t.Error("an assertion holding cluster-write was handed the Writer — the validator " +
			"refuses that declaration, and the handle layer must not disagree with it")
	}
	if err := h.Custodian.Put("secret/x", map[string]string{"k": "v"}); err == nil {
		t.Error("an assertion holding secret-custody was handed the Custodian")
	}
	// Narrowed, not refused: the read half of a mis-declared assertion still works,
	// so a wrong declaration is a smaller capability rather than an outage.
	if err := h.Cluster.Permits("get", "pods"); err != nil {
		t.Errorf("the read half of a contradictory assertion was refused too: %v — narrowing "+
			"is the rule, not refusing", err)
	}

	// A gate reaches files and nothing else, whatever it declares.
	gate := extension.Binding{
		Kind: extension.Gate, State: extension.Scaffolded,
		Grants: []extension.Grant{extension.ReadRepo, extension.ClusterRead},
	}
	g := For(gate)
	if err := g.Cluster.Permits("get", "pods"); err == nil {
		t.Error("a gate was handed a cluster handle — a gate runs in the fast pre-commit " +
			"path over files alone, and the one that reached a cluster would do it against " +
			"live infrastructure")
	}
	if err := g.Cloud.Permits("GET"); err == nil {
		t.Error("a gate was handed a cloud handle")
	}
}

// ReadOnlyCloud IS A FENCE AND NOT A LABEL. The static guard next door proves the
// verbs tree TAKES it; this proves taking it means something — a mutating request
// is refused before it leaves, which is the whole difference between this and the
// `linode.NewClient(tok, …)` it replaced.
func TestReadOnlyCloudRefusesEveryMutation(t *testing.T) {
	c := ReadOnlyCloud()

	// The reads the three converted call sites actually make.
	if err := c.Permits("GET"); err != nil {
		t.Errorf("ReadOnlyCloud refused a GET: %v — doctor's version check, onboard's bucket "+
			"preflight and the token wizard all read, so a fence that refuses reads is an outage", err)
	}

	// The methods the client it replaced could reach. PutControlPlaneACL and
	// ResetPostgresCredentials are the two that make this worth fencing.
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if err := c.Permits(m); err == nil {
			t.Errorf("ReadOnlyCloud permits %s — a command that only looks must only look, and "+
				"this is the tree with no declaration to check it against", m)
		} else if !errors.Is(err, ErrNoCloudMutate) {
			t.Errorf("ReadOnlyCloud refused %s with %v, want ErrNoCloudMutate — the two cloud "+
				"errors are separate so a caller can tell 'may not look' from 'may not change'", m, err)
		}
	}

	// It must be the SAME value a cloud-read binding gets, or the classification
	// has quietly forked into a second copy — the thing the helper exists to avoid.
	declared := CloudFor(binding(extension.CloudRead))
	for _, m := range []string{"GET", "POST", "PUT", "DELETE"} {
		a, b := c.Permits(m) == nil, declared.Permits(m) == nil
		if a != b {
			t.Errorf("ReadOnlyCloud and a cloud-read binding disagree about %s (%v vs %v) — "+
				"they must be one classification, not two", m, a, b)
		}
	}
}

// Cloud ARRIVES FROM For() AND NOT ONLY FROM CloudFor(), which is what makes the
// struct's docstring true. A caller that takes For(b) and stops must not silently
// have no cloud handle.
func TestForCarriesTheCloudHandle(t *testing.T) {
	h := For(binding(extension.CloudRead))
	if h.Cloud == nil {
		t.Fatal("Handles.Cloud is nil — every field of Handles is non-nil by contract, so a " +
			"caller would get a panic reported as a crash rather than as a permission fault")
	}
	if err := h.Cloud.Permits("GET"); err != nil {
		t.Errorf("a cloud-read binding was refused a cloud read through For(): %v", err)
	}
	if err := For(binding(extension.ClusterRead)).Cloud.Permits("GET"); err == nil {
		t.Error("a binding declaring no cloud grant was handed a working cloud handle")
	}
}

// AND THE GUARD RAIL ITSELF MUST FIRE. mustWriter passes on a clean tree by
// construction, so prove it rejects the exact shape that shipped: an assertion
// binding asked for a Writer.
func TestTheWiringGuardRailRejectsARefusingHandle(t *testing.T) {
	assertion := extension.Binding{
		Kind: extension.Assertion, State: extension.Verified,
		Grants: []extension.Grant{extension.ClusterRead},
	}
	// This is assert-identity's Bindings[0] in all but name — the binding that was
	// actually selected for a Writer.
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("mustWriter accepted an assertion binding with no cluster-write — the " +
					"guard rail does not fire, and the next mis-wired Deps installs silently")
			}
			if msg, _ := r.(string); !strings.Contains(msg, "cluster-write") ||
				!strings.Contains(msg, "by NAME") {
				t.Errorf("the panic does not name the missing grant and the remedy: %v", r)
			}
		}()
		_ = MustWriter(assertion)
	}()

	// mustCluster is the same rail on the read side.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("mustCluster accepted a binding declaring no cluster grant")
			}
		}()
		_ = MustCluster(extension.Binding{
			Kind: extension.Transition, State: extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		})
	}()

	// A correctly selected binding must pass through untouched, or the rail is an
	// outage rather than a check.
	live := extension.Binding{
		Kind: extension.Transition, State: extension.Converged,
		Grants: []extension.Grant{extension.ClusterRead, extension.ClusterWrite},
	}
	if w := MustWriter(live); IsRefusing(w) {
		t.Error("mustWriter returned a refusing Writer for a binding that declares cluster-write")
	}
	if c := MustCluster(live); IsRefusing(c) {
		t.Error("mustCluster returned a refusing Cluster for a binding that declares cluster-read")
	}
}
