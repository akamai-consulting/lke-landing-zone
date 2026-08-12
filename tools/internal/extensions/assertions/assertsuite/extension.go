package assertsuite

// extension.go — `assert-suite` declares itself: the runner that decides whether
// the assertion battery passed.
//
// FIFTIETH EXTENSION, AND THE ONE THAT RUNS THE OTHER EXTENSIONS. Every
// `assert-*` extension in this catalog is a lane in the table this package owns,
// and until now the thing that scheduled them and decided the exit status was the
// last piece of the battery still living in internal/cli.
//
// ITS OWN FILE HEADER ARGUES THE CASE BETTER THAN A DECLARATION CAN: the suite
// replaced a bash job runner whose lane list was written TWICE, so a lane present
// in the `lane <name>` calls but absent from the collection loop would run and
// could never fail the step. That is the vacuous pass every individual gate
// refuses, one level up. Extracting it puts the runner under the same coverage
// floor as the lanes it runs.
//
// CLOSURE 2, BOTH NOISE. 442 lines that never needed anything from package main
// except `main` itself — it has been separable this whole time and was simply
// never measured.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `assert-suite` declaration.
//
//	assertion:verified[read-repo, cloud-read, cluster-read, secret-read]
//
// `verified` IS THE POINT. Every other assertion in this catalog binds the state
// it contributes evidence about — storage at `converged`, identity at `seeded`.
// This one binds `verified` itself, because it is not evidence about a state: it
// is the thing that AGGREGATES the evidence and decides whether the platform has
// been verified at all. It is the only member of the catalog for which `verified`
// is the honest state rather than a tempting one.
//
// THE GRANTS ARE THE UNION OF ITS LANES, and here that is correct rather than the
// over-granting the model usually punishes. A runner that schedules lanes holding
// {read-repo, cloud-read, cluster-read, secret-read} must be able to be all of
// them; it has no capability of its own beyond scheduling.
//
// NOTE WHAT IS ABSENT: no mutating grant. Its own header records that the lane
// table is collision-free only because the MUTATING lanes are separated — the
// mutations belong to the lanes, declared by the lanes, and the runner does none.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "assert-suite",
		Short:  "run the assertion lanes in parallel and decide whether the battery passed",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:  extension.Assertion,
			State: extension.Verified,
			Grants: []extension.Grant{
				extension.ReadRepo, extension.CloudRead,
				extension.ClusterRead, extension.SecretRead,
			},
		}},
	}
}
