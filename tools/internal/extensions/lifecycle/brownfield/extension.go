package brownfield

// extension.go — `import-brownfield` declares itself, and is the first extension
// that does NOT ship enabled.
//
// EIGHTH EXTENSION, AND THE CATALOG'S OWN WORKED EXAMPLE. Its declaration is
// transcribed by hand in extension_test.go's TestCatalogSampleIsExpressible, where
// it is the case used to explain multi-binding transitions. Declaring it against
// the real code is therefore also a test of that transcription — the check on
// whether the catalog quietly disagreed with itself.
//
// It did not. The transcription is correct, bindings and grants both.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `import-brownfield` declaration.
//
//	transition:scaffolded  [read-repo, cloud-read, own-paths]  write an instance repo
//	transition:provisioned [cloud-mutate]                      adopt the cloud substrate
//
// FIRST `alwaysEnabled: false`, AFTER SEVEN THAT ALL SHIP `always`. Adoption is
// the one capability an instance either needs once or never — a greenfield
// instance has nothing to import — so this is the case the enablement machinery
// has to work for, and the first evidence that `Always` is a field with two
// possible values rather than a constant nobody sets.
//
// AND `own-paths` IS REACHABLE AFTER ALL. Extraction #7 concluded it might be the
// one grant no extension can hold, because `template-sustain`'s restore pass reads
// `.template-manifest`'s class table and ADR 0014 pins that as core. This is the
// correction: import WRITES files the manifest classes `owned` without ever
// reading the class table, because writing an owned file and knowing which class a
// file is are different operations. The grant is a FENCE — "copier must not render
// these bytes" — and declaring the fence needs no access to the thing that
// enforces it. What is unreachable is the RESTORE pass, not the grant.
//
// TWO TRANSITIONS, NOT ONE, and the catalog's note explains why: an earlier draft
// declared a single transition to `provisioned` holding `own-paths`, which the
// validator rejects — own-paths is only meaningful where files are written, and
// `provisioned` is not that moment. Import does two things at two moments, and
// saying so is what makes each half checkable.
func Extension() extension.Extension {
	return extension.Extension{
		Name:  "import-brownfield",
		Short: "adopt an existing APL cluster into an LLZ instance",
		// NOT always. See above — this is the first opt-in extension.
		Always: false,
		Bindings: []extension.Binding{
			{
				// `llz import scan` + `init`: read the old cluster, author a spec,
				// scaffold the repo. own-paths because the rendered instance files
				// are `owned` and copier must not re-render them.
				Kind:  extension.Transition,
				State: extension.Scaffolded,
				Grants: []extension.Grant{
					extension.ReadRepo, extension.CloudRead, extension.OwnPaths,
				},
			},
			{
				// Adopting the substrate itself — the Linode resources the old site
				// already has become the new instance's.
				Kind:   extension.Transition,
				State:  extension.Provisioned,
				Grants: []extension.Grant{extension.CloudMutate},
			},
		},
		Incomplete: []string{
			"the `sync` phase — `llz import plan` emits the migration to-do list, and the " +
				"apply half was never built; the catalog counts it in import's 3,133 lines",
		},
	}
}
