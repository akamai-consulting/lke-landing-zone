package chartpublish

// extension.go — `chart-publish` declares itself, and widens a row that has been
// waiting for a second case since the twenty-first extraction.
//
// THIRTY-FIRST EXTENSION. SEVENTH CATALOG CORRECTION, AND THE SECOND FILE PULLED
// OUT OF THE SAME ROW. `chart-publish-check` is filed under `release-publish`,
// annotated "template-repo-side, not instance-side". It is neither: it scans a
// RENDERED INSTANCE's apl-values Argo Application manifests for first-party chart
// pins and fails before bootstrap if the registry never received one.
// e2e-instantiate.yml runs it against `.e2e-instance`. `deliver-docs` came out of
// that same row for the same reason two extractions ago — one row, two files, both
// mis-filed the same way, which says the row was grouped by "things that mention
// publishing" rather than by what the code touches.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `chart-publish` declaration.
//
//	assertion:configured  "pins-published" [read-repo, cloud-read]
//	transition:configured "publish-missing" [read-repo, cloud-mutate]
//
// THE SPLIT IS THE SAME ONE `converge` AND `token-inventory` FOUND, and it is
// forced rather than stylistic: the default run READS the OCI registry and reports,
// while `--publish-if-missing` DISPATCHES publish-charts.yml and waits. One verb,
// two capabilities, and an assertion may hold read grants only — so a single
// binding would have to widen to the union and claim the write on every run.
//
// `cloud-mutate` IS THE GRANT FOR A GITHUB WRITE, which `env-topology` established:
// a PUT against GitHub's deployment-branch-policy API is a mutation of
// infrastructure this repo does not contain, and cloud-mutate is what says so. The
// dispatch here is the same shape — `gh workflow run` against the template repo.
//
// AND THIS IS THE SECOND CASE FOR cloud-mutate AT `configured`, which is the point.
// env-topology wrote exactly this binding, ran Validate(), was refused —
//
//	"cloud-mutate" may only be asked for at provisioned, seeded, converged,
//	operating, destroyed
//
// — and moved branchpolicy.go back to package main rather than widen a row on one
// case. It predicted the second would be `llz tokens`; it is this instead, which
// does not matter. Two independent shipping cases is the bar, and the argument the
// row was missing is now clear: those five states are where a LINODE cloud exists
// to mutate, and `configured` was read as a purely local moment. It is not. GitHub
// is configured before Linode is provisioned, and "its inputs resolve" — the
// definition of `configured` — can require CREATING an input, not merely reading
// it. A pinned chart that was never published and a branch policy that was never
// locked are both inputs that do not resolve until something writes.
//
// So the row is widened, and `env-topology`'s third binding is now expressible.
// Moving branchpolicy.go back is a separate extraction and deliberately not done
// here.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "chart-publish",
		Short:  "every pinned first-party chart is published before anything tries to sync it",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Assertion,
				Name:   "pins-published",
				State:  extension.Configured,
				Grants: []extension.Grant{extension.ReadRepo, extension.CloudRead},
			},
			{
				Kind:   extension.Transition,
				Name:   "publish-missing",
				State:  extension.Configured,
				Grants: []extension.Grant{extension.ReadRepo, extension.CloudMutate},
			},
		},
	}
}
