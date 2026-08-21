package registry

// enablement_coverage_test.go — WHICH extensions an instance can actually turn off.
//
// ────────────────────────────────────────────────────────────────────────────
// THE MECHANISM SHIPPED AND THE DATA DID NOT.
//
// The model's headline promise is on Extension.Always: it "IS A DEFAULT, NOT A
// CONSTANT", and an instance with no object storage "has to be able to turn
// assert-objstore off in its own configuration rather than by taking a different
// build". EnabledFor delivers that — but only for an extension that NAMES a
// component, because Component is the only input it has. Rule 2 is `Always`, and
// `Always` is not configuration.
//
// Ten of the sixty-two name one. For the other fifty-two the promise is
// undeliverable, and nothing said so: an extension with no Component looks exactly
// like one whose author decided it should follow nothing.
//
// THAT AMBIGUITY IS THE WHOLE DEFECT, and it is why this is a ratchet rather than
// a fix. `Component: ""` is a LEGITIMATE answer for several — the declaration
// model names the four opt-in extensions that should not be given one
// (import-brownfield is a one-time adoption path, wedge-gameday is not about the
// platform, release-publish runs template-repo-side, and database-provisioner
// follows spec.databases rather than a component), and inventing a component so a
// checkbox exists would be worse than the gap. So the entries below are NOT a
// claim of fifty-two omissions. They are the population nobody has ruled on.
//
// THAT SENTENCE NAMED `dev-mutation-testing` FOR AS LONG AS THE ONE IT WAS COPIED
// FROM DID, and no such extension is registered. Two files asserting the same
// unregistered name is not two witnesses; it is one claim written twice.
// TestComponentlessExtensionsAreRatcheted below measures the population, which is
// why the COUNT stayed right while the membership was wrong — a ratchet over names
// does not read the prose above it.
//
// WHAT THE RATCHET BUYS, precisely: a NEW extension cannot join the list. Its
// author has to either name the component it follows or add a line here, and
// adding the line is the moment the question gets asked. That is the same standing
// unbackedGrants has, and it was worth having for the same reason — an unmeasured
// surface grows quietly, and this one grows every time an extension lands.
//
// AND THE LIST MAY ONLY SHRINK. Linking an extension deletes its line in the same
// commit, so the paydown is banked instead of left as room to regrow. Both
// directions fail, exactly like allowedEdges and undrivenGates.
//
// WHAT THIS DOES NOT TOUCH, deliberately. Enablement is load-bearing for gates and
// for assert lanes and for nothing else; transitions and invariants ignore it, and
// enablement.go's header records that as the remaining slice. That sequencing is
// argued rather than accidental — a skipped gate runs fewer checks and says so,
// while a skipped INVARIANT is an enforcement hole hiding inside a config value —
// so it is not something to widen from a test file.
// ────────────────────────────────────────────────────────────────────────────

import (
	"sort"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// componentless is every registered extension whose enablement does NOT follow a
// spec component, and which an instance therefore cannot turn off. MEASURED, not
// chosen.
var componentless = map[string]bool{
	"assert-identity": true,
	// No component: the injection class is a property of the WORKFLOW FILES, which
	// every instance carries regardless of which spec components it turned on.
	"workflow-injection": true,
	// No component: an instance's opt-in to the automated template upgrade is a
	// repo VARIABLE (LLZ_TEMPLATE_UPGRADE), not a spec component — the choice is
	// about who may change this repo, which is not a property of the cluster the
	// spec describes.
	"upstream-updates":            true,
	"assert-network":              true,
	"assert-objstore":             true,
	"assert-platform":             true,
	"assert-secrets":              true,
	"assert-storage":              true,
	"assert-suite":                true,
	"bootstrap-cluster":           true,
	"branch-policy":               true,
	"build-preflight":             true,
	"chart-publish":               true,
	"cluster-access":              true,
	"config-readiness":            true,
	"converge":                    true,
	"credential-objkey":           true,
	"credential-pat":              true,
	"credential-state-passphrase": true,
	"database-provisioner":        true,
	"deliver-docs":                true,
	"environments":                true,
	"guard-budgets":               true,
	"guard-charts":                true,
	"guard-cosign-subject":        true,
	"guard-coverage":              true,
	"guard-docs":                  true,
	"guard-manifests":             true,
	"guard-monitoring-labels":     true,
	"guard-source-refs":           true,
	// Its subject is this repo's own CI wiring, which no instance can turn off.
	"setup-go-sole-site": true,
	// Same again: which tags build-images.yml may publish is a property of THIS
	// repo's publishing, not of any component an instance can turn off.
	"guard-mutable-tags": true,
	// Same: the caller/callee permission contract is a property of the workflow
	// files themselves, not of any component an instance can disable.
	"reusable-workflow-caller-permissions": true,
	"guard-workflow-shells":                true,
	"health-sla":                           true,
	"identity-plane":                       true,
	"import-brownfield":                    true,
	"mesh-egress":                          true,
	"mtls-wiring":                          true,
	"pin-coherence":                        true,
	"posture-at-rest":                      true,
	"posture-credential-coverage":          true,
	"posture-plaintext":                    true,
	"promote-pipeline":                     true,
	"reachability":                         true,
	"release-publish":                      true,
	"render":                               true,
	"seed-special":                         true,
	"teardown":                             true,
	"template-commit":                      true,
	"template-manifest":                    true,
	"template-sustain":                     true,
	"tofu-driver":                          true,
	"token-inventory":                      true,
	"version-pins":                         true,
	"wave-health":                          true,
	"wedge-gameday":                        true,
}

func TestComponentlessExtensionsAreRatcheted(t *testing.T) {
	live := map[string]bool{}
	for _, e := range All() {
		if e.Component == "" {
			live[e.Name] = true
		}
	}
	// The registry must be non-empty for the comparison to mean anything; a walk
	// over nothing agrees with any allowlist.
	if len(All()) == 0 {
		t.Fatal("the registry is empty — this guard would report a clean tree it never read")
	}

	var appeared, banked []string
	for name := range live {
		if !componentless[name] {
			appeared = append(appeared, name)
		}
	}
	for name := range componentless {
		if !live[name] {
			banked = append(banked, name)
		}
	}
	sort.Strings(appeared)
	sort.Strings(banked)

	if len(appeared) > 0 {
		t.Errorf("%d extension(s) name no component and are not on the list:\n\t%s\n"+
			"\tName the spec component this follows, so an instance that turned the feature off "+
			"stops running it — or add it here, which is the record that the question was asked "+
			"and the answer was none.", len(appeared), strings.Join(appeared, "\n\t"))
	}
	if len(banked) > 0 {
		t.Errorf("%d extension(s) now follow a component — DELETE them from componentless:\n\t%s\n"+
			"\tThat is the paydown this ratchet exists to bank.",
			len(banked), strings.Join(banked, "\n\t"))
	}
	t.Logf("%d of %d extensions follow a spec component", len(All())-len(live), len(All()))
}

// EVERY LINKED EXTENSION MUST NAME A COMPONENT THAT EXISTS, checked here rather
// than only inside EnabledFor.
//
// EnabledFor already refuses an unknown component — and refuses it at RUNTIME, in
// front of an operator, for a mistake a developer made. It is also reachable only
// when something calls it with toggles: `llz extension list` does not, and the gate
// driver skips the resolver entirely in a checkout with no spec, which is the
// template repo where this would be caught. So the typo ships.
//
// Same argument registry.Validate makes for being a build-time lint: a compiled-in
// declaration cannot change between builds, so the moment to reject a bad one is
// before it leaves.
func TestEveryComponentLinkResolves(t *testing.T) {
	links := ComponentLinks()
	if len(links) == 0 {
		t.Fatal("no extension follows a component — either the field stopped being read or the " +
			"registry is empty, and both make this guard vacuous")
	}
	var unknown []string
	for name, comp := range links {
		if _, ok := clusterspec.LookupComponent(comp); !ok {
			unknown = append(unknown, name+" -> "+comp)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("%d extension(s) name a component the spec does not define:\n\t%s\n"+
			"\tEnabledFor refuses this at run time, which puts a developer's typo in front of an "+
			"operator. A misspelled component resolves to no link at all, so the extension falls "+
			"back to `Always` and runs on an instance that believes it turned the feature off.",
			len(unknown), strings.Join(unknown, "\n\t"))
	}
}
