package tokeninv

// extension.go — `token-inventory` declares itself.
//
// THIRTEENTH EXTENSION, and the first to bind `configured` — the last state in
// the vocabulary that no extension had ever claimed.
//
// WHAT THE CATALOG GOT WRONG, AGAIN, AND DIFFERENTLY. The row named six files
// totalling 1,473 lines and predicted a split across three states. The prediction
// was right; the file list was not. `tokens.go` (437 lines) is `llz tokens` — the
// credential PROVISIONING wizard that creates OBJ buckets and gathers PATs — and
// it alone tripled the measured closure, from 13 to 42, by dragging in the
// wizard, the state model and the command tree. It is a `transition` to
// `configured` holding cloud-mutate; these are the checks that READ what it
// wrote. Measuring before trusting the catalog is what separated them.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `token-inventory` declaration.
//
// THREE BINDINGS ACROSS TWO STATES. The catalog predicted three states
// (configured, seeded, operating); the `seeded` half was `tokens.go`, the
// provisioning wizard that stayed behind. What is left binds two:
//
//	assertion:configured "validate-tokens"  [read-repo, secret-read]
//	gate:configured      "rotation-plan"    [read-repo]
//	invariant:operating  "expiry-inventory" [cloud-read, secret-read]
//
// WHY `configured` FOR THE FIRST TWO. `validate-tokens` runs as an early CI
// preflight, before anything provisions, and answers one question: are the
// credentials this instance was CONFIGURED with actually usable? Its motivating
// scar is a GHCR_READ_TOKEN 403 that surfaced mid-bootstrap — 45 minutes after
// the run began — so its whole value is being early. `rotation-plan` routes a
// rotation dispatch and refuses on a confirm mismatch or a blank reason; nothing
// downstream runs unless it passed, which is a gate by definition.
//
// WHY THE THIRD IS AN INVARIANT. The expiry inventory is scheduled, runs against
// a working cluster forever, and emits a ConfigMap the in-cluster reconciler
// re-exposes as `llz_token_expiry_timestamp_seconds`. Failure means a credential
// DRIFTED toward expiry, not that a step failed. That is `operating`.
//
// THE MODEL REFUSED `validate-tokens`, AND NOT FOR THE REASON EXPECTED. The guess
// was that `grantStates` would reject secret-custody at `configured`, the way it
// rejected it at `provisioned` for cluster-access. It never got that far: a GATE
// permits `read-repo` and nothing else, because a gate is defined here as the fast
// pre-commit path over files alone. This check makes network calls to GitHub,
// Linode and S3, so it is not a gate — it is an ASSERTION, which may bind at any
// state and permits read grants.
//
// And an assertion could not hold it either, because `secret-custody` was not a
// read grant. It was one word meaning "read OR WRITE credential material" (its own
// doc comment said so), and a check that only READS credentials was therefore
// unrepresentable — not mis-described, impossible. The grant was split:
// `secret-read` for reading credential material or its metadata, `secret-custody`
// for placing it. See internal/extension/extension.go for the argument.
//
// So no `grantStates` row was widened after all. That is the better outcome: the
// ceiling was not too tight, the vocabulary was too coarse, and widening the row
// would have "fixed" this by letting every credential-reading check claim a
// mutating grant.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "token-inventory",
		Short:  "probe pipeline credentials before anything provisions, and measure their expiry forever after",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Assertion,
				Name:   "validate-tokens",
				State:  extension.Configured,
				Grants: []extension.Grant{extension.ReadRepo, extension.SecretRead},
			},
			{
				Kind:   extension.Gate,
				Name:   "rotation-plan",
				State:  extension.Configured,
				Grants: []extension.Grant{extension.ReadRepo},
			},
			{
				Kind:   extension.Invariant,
				Name:   "expiry-inventory",
				State:  extension.Operating,
				Grants: []extension.Grant{extension.CloudRead, extension.SecretRead},
			},
		},
	}
}

// cloudBinding is the binding this package reaches Linode through — the one
// carrying a cloud grant. Looked up rather than reconstructed, following objenc's
// seedBinding: handles belong to a BINDING, and an extension with several must
// not hand back the union.
func cloudBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		for _, g := range b.Grants {
			if g == extension.CloudRead || g == extension.CloudMutate {
				return b
			}
		}
	}
	panic("token-inventory: no binding carries a cloud grant — the Linode client is built from " +
		"one, so its absence is a wiring bug")
}
