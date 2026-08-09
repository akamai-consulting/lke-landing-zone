package pincoherence

// cobra_pincoherence.go — the CLI surface for Assert.
//
// IT EXISTED ONLY AS A FUNCTION, WHICH IS WHY THE REGISTRY COULD NOT DRIVE IT.
// The gate driver's unit is a *cobra.Command — an entry point that already takes
// its own flags and already works — so a gate whose only entry point is
// `Assert(dir string) error`, reachable from `verbs/lint` and from
// assert-image-fresh, was undriveable for want of thirty lines. That is a
// different thing from the two gates that genuinely cannot be driven, and lumping
// them together is what a prose list of "the other twelve" encouraged.
//
// Split from pincoherence.go so the directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

// Cmd is `llz ci pin-coherence`.
func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "pin-coherence",
		Short: "an instance's two template pins must name the same release",
		Long: "Fails when .copier-answers.yml records `_commit` and `llz_version` as two\n" +
			"different exact release tags. They record the same fact, and everything that\n" +
			"resolves the pin prefers llz_version while copier's own record says the tree\n" +
			"only ever received _commit — so the instance deploys one release's manifests\n" +
			"from another release's scaffold.\n\n" +
			"Silent when there is no instance at --root, when either pin is absent, or when\n" +
			"either is not an exact release tag: those are not skew, and a template checkout\n" +
			"legitimately has no answers file at all.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return Assert(root) },
	}
	c.Flags().StringVar(&root, "root", ".", "instance root containing .copier-answers.yml")
	return c
}
