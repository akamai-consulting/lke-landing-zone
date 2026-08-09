package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

func sampleExtensions() []extension.Extension {
	return []extension.Extension{
		{
			Name: "guard-budgets", Short: "cap the budgets", Always: true,
			Bindings: []extension.Binding{{
				Kind: extension.Gate, State: extension.Scaffolded,
				Grants: []extension.Grant{extension.ReadRepo},
			}},
		},
		{
			Name: "harbor-provisioner", Short: "provision harbor", Always: false,
			Bindings: []extension.Binding{
				{Kind: extension.Transition, State: extension.Seeded,
					Grants: []extension.Grant{extension.SecretCustody}},
				{Kind: extension.Assertion, State: extension.Verified,
					Grants: []extension.Grant{extension.ClusterRead}},
			},
		},
	}
}

func extensionListing(t *testing.T, verbose bool) string {
	t.Helper()
	var buf bytes.Buffer
	if err := listExtensions(&buf, sampleExtensions(), verbose); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestExtensionListShowsEnablementBindingsAndGrants(t *testing.T) {
	out := extensionListing(t, false)
	for _, want := range []string{
		"guard-budgets", "always", "gate:scaffolded", "read-repo",
		"harbor-provisioner", "opt-in", "transition:seeded", "assertion:verified",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("listing is missing %q:\n%s", want, out)
		}
	}
}

// The summary line prints the DERIVED union (Extension.Grants), and a
// multi-binding extension is the only place that can be told apart from a
// per-binding list. This is the one misreading the model was corrected to prevent,
// so pin that --verbose is where a binding's own grants appear and that the
// summary never implies they are shared.
func TestExtensionListVerboseAttributesGrantsToTheirOwnBinding(t *testing.T) {
	terse, verbose := extensionListing(t, false), extensionListing(t, true)
	if strings.Contains(terse, "transition:seeded[") {
		t.Error("the terse listing should summarise bindings, not print per-binding grants")
	}
	for _, want := range []string{"transition:seeded[secret-custody]", "assertion:verified[cluster-read]"} {
		if !strings.Contains(verbose, want) {
			t.Errorf("--verbose is missing %q:\n%s", want, verbose)
		}
	}
	if strings.Contains(verbose, "assertion:verified[secret-custody]") {
		t.Error("a binding must not be shown holding a sibling binding's grant")
	}
}

// An empty registry is a real state (nothing compiled in) and must not render as a
// header with no rows, which reads as a broken command rather than an empty set.
func TestExtensionListEmptyRegistrySaysSo(t *testing.T) {
	var buf bytes.Buffer
	if err := listExtensions(&buf, nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no extensions") {
		t.Errorf("empty listing should say so, got %q", buf.String())
	}
	if strings.Contains(buf.String(), "NAME") {
		t.Error("empty listing should not print a header with no rows")
	}
}

// bindingSummary dedupes kind:state, which is what makes an extension holding
// several named invariants (reconcile-actions is seven) readable rather than a
// line of repeats.
func TestBindingSummaryDedupesRepeatedAttachments(t *testing.T) {
	e := extension.Extension{
		Name: "reconcile-actions", Short: "x",
		Bindings: []extension.Binding{
			{Kind: extension.Invariant, Name: "tokens", State: extension.Operating,
				Grants: []extension.Grant{extension.SecretCustody}},
			{Kind: extension.Invariant, Name: "sc-demote", State: extension.Operating,
				Grants: []extension.Grant{extension.ClusterWrite}},
		},
	}
	if got := bindingSummary(e); got != "invariant:operating" {
		t.Errorf("bindingSummary = %q, want a single deduped invariant:operating", got)
	}
	// The union is still both grants — dedup is about display, not about widening
	// or narrowing what the extension touches.
	if got := grantSummary(e); got != "cluster-write,secret-custody" {
		t.Errorf("grantSummary = %q, want both grants in vocabulary order", got)
	}
}
