package doctor

// extension.go — `doctor-probes` declares itself, and closes the fifth-kind
// question by splitting the family a third way.
//
// THIRTY-SIXTH EXTENSION, AND CASE THREE OF THREE. `argocd-diagnostics` opened the
// question — a check that always exits 0 and fits no binding kind — and named this
// row and `phase-timing` as the cases that would settle it. `phase-timing` showed
// the first two disagreed about SHAPE. This one splits the family again, and the
// three together answer the question in the negative:
//
//	argocd-diagnostics  attaches to the FAILURE of `converged`
//	phase-timing        attaches to NO state — it records the boundaries between them
//	doctor-probes       attaches to `scaffolded` AND `configured`, and one half of it
//	                    RETURNS AN ERROR
//
// "DIAGNOSTIC" WAS NEVER A KIND. It is a description of a TONE OF VOICE — prints
// for a human, phrased as advice — and this row proves it by containing three
// different things wearing that tone:
//
//   - cross-org reuse is a GATE. It reads .github/workflows for a `secrets:
//     inherit` job crossing an org boundary, touches no network, and returns an
//     error that fails `llz doctor`. That is a gate by every part of the
//     definition: fast, local, files only, findings out.
//   - the Linode account probe and the LKE-version comparison are ASSERTIONS at
//     `configured`. They ask a cloud whether the inputs the spec declares actually
//     resolve — which is what `configured` means — and report without a verdict.
//   - the token table probes credentials and counts what is valid.
//
// So no fifth kind is invented, and the reason is now positive rather than
// cautious: the three cases have three different positions in the lifecycle, and
// position in the lifecycle is the entire content of a binding. A word covering all
// three would describe their output rather than their attachment, and the model
// already has a field for "how does this read to a human" — it is called Short.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `doctor-probes` declaration.
//
//	gate:scaffolded       "cross-org-reuse"  [read-repo]
//	assertion:configured  "credential-reach" [read-repo, cloud-read, secret-read]
//
// SPLIT BY CAPABILITY, which is the rule `guard-charts` settled and
// `guard-manifests` had to be corrected into by its own test. The two halves differ in
// exactly the way that matters: one touches only files, the other reaches a cloud
// and reads credential metadata. Collapsing them would put `cloud-read` and
// `secret-read` on a binding that is otherwise a pure file scan, and a gate may
// hold `read-repo` and nothing else — so the collapse is not merely imprecise, it
// does not validate.
//
// `secret-read` rather than `secret-custody`: the token table READS credentials and
// asks each issuer whether they still work. It places none. That distinction is the
// whole content of the split `token-inventory` forced into the vocabulary.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "doctor-probes",
		Short:  "before anything is built: do the workflows cross an org, and do the declared inputs actually resolve",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Gate,
				Name:   "cross-org-reuse",
				State:  extension.Scaffolded,
				Grants: []extension.Grant{extension.ReadRepo},
			},
			{
				Kind:   extension.Assertion,
				Name:   "credential-reach",
				State:  extension.Configured,
				Grants: []extension.Grant{extension.ReadRepo, extension.CloudRead, extension.SecretRead},
			},
		},
	}
}
