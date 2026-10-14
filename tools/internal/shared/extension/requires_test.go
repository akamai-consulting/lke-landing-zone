package extension_test

// The precondition axis, checked. Requires is the third word this model has
// gained and the first that is a FIELD rather than a value, so the rules worth
// pinning are the ones that keep it from becoming a way around the other two
// ceilings — particularly the grant check, where the natural reading is a
// widening.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func withBinding(b extension.Binding) extension.Extension {
	return extension.Extension{Name: "probe", Short: "x", Bindings: []extension.Binding{b}}
}

func TestRequiresIsOptional(t *testing.T) {
	// The zero value has to mean "makes no claim", or adding the field would have
	// invalidated every declaration in the catalog that does not use it.
	b := extension.Binding{
		Kind: extension.Transition, State: extension.Seeded,
		Grants: []extension.Grant{extension.SecretCustody},
	}
	if errs := withBinding(b).Validate(); len(errs) != 0 {
		t.Errorf("a binding without a precondition must stay valid: %v", errs)
	}
}

func TestOnlyATransitionMayDeclareAPrecondition(t *testing.T) {
	for _, k := range []extension.BindingKind{extension.Assertion, extension.Invariant, extension.Gate} {
		b := extension.Binding{
			Kind: k, State: extension.Scaffolded, Requires: extension.Operating,
			Grants: []extension.Grant{extension.ReadRepo},
		}
		if k == extension.Invariant {
			b.State = extension.Operating
		}
		got := errText(withBinding(b).Validate())
		if !strings.Contains(got, "may not declare a precondition") {
			t.Errorf("%s accepted a precondition — the other kinds express this with State: %s", k, got)
		}
	}
}

func TestPreconditionStateIsRestricted(t *testing.T) {
	// `converged` is the most plausible next row and is deliberately absent: a
	// transition can already TARGET it, so wanting it as a precondition is a
	// declaration worth examining rather than accommodating.
	b := extension.Binding{
		Kind: extension.Transition, State: extension.Seeded, Requires: extension.Converged,
		Grants: []extension.Grant{extension.SecretCustody},
	}
	if got := errText(withBinding(b).Validate()); !strings.Contains(got, "not a declarable precondition") {
		t.Errorf("`converged` was accepted as a precondition: %s", got)
	}

	unknown := extension.Binding{
		Kind: extension.Transition, State: extension.Seeded, Requires: extension.State("nonsense"),
		Grants: []extension.Grant{extension.SecretCustody},
	}
	if got := errText(withBinding(unknown).Validate()); !strings.Contains(got, "unknown precondition state") {
		t.Errorf("an unknown precondition state was accepted: %s", got)
	}
}

func TestPreconditionMayNotRepeatTheState(t *testing.T) {
	// Noise that reads like a check is worse than silence.
	b := extension.Binding{
		Kind: extension.Transition, State: extension.Operating, Requires: extension.Operating,
		Grants: []extension.Grant{extension.ClusterWrite},
	}
	if got := errText(withBinding(b).Validate()); !strings.Contains(got, "Requires repeats State") {
		t.Errorf("Requires == State was accepted: %s", got)
	}
}

// THE ANTI-WIDENING PROPERTY, and the reason this file exists. Checking the
// mutating grant at the PRECONDITION instead of the state is the natural reading
// — the action does run while that state holds — and it would let a binding ask at
// `operating` for a grant its own declared State forbids, which quietly empties
// the State line of meaning. The check runs at BOTH.
func TestAPreconditionCannotLaunderAGrantPastGrantStates(t *testing.T) {
	// secret-custody is legal at `operating` and NOT at `configured`. If the check
	// used Requires alone, this would validate clean.
	b := extension.Binding{
		Kind: extension.Transition, State: extension.Configured, Requires: extension.Operating,
		Grants: []extension.Grant{extension.SecretCustody},
	}
	got := errText(withBinding(b).Validate())
	if !strings.Contains(got, "may only be asked for at") {
		t.Errorf("secret-custody at `configured` was laundered through a precondition of "+
			"`operating` — the grant check must apply at BOTH states: %s", got)
	}
}

func TestPreconditionRendersInTheBindingString(t *testing.T) {
	// The string is what every validation error shows, and the two states are the
	// entire content of the field — an unlabelled second state would be a guess.
	b := extension.Binding{
		Kind: extension.Transition, Name: "rotate-admin", State: extension.Seeded,
		Requires: extension.Operating, Grants: []extension.Grant{extension.SecretCustody},
	}
	const want = "transition:seeded/rotate-admin (requires operating)[secret-custody]"
	if got := b.String(); got != want {
		t.Errorf("Binding.String()\n got: %s\nwant: %s", got, want)
	}
}
