package bootstrapcluster

// cobra_brownfield_migrations.go — the CLI surface for brownfield_migrations.
//
// Split from brownfield_migrations.go so an extension directory shows its commands
// at a glance: every file named cobra_*.go is flag wiring and help text, and
// nothing else.
//
// TWO VERBS, AND THE SPLIT IS THE SAFETY. One reads and reports; the other
// recreates a live object and takes `--yes`. Folding them into one verb with a
// flag would make the destructive path the same command an operator runs to look.

import (
	"fmt"

	"github.com/spf13/cobra"
)

func BrownfieldMigrationsCmd() *cobra.Command {
	var kubeconfig string
	cmd := &cobra.Command{
		Use:   "brownfield-migrations",
		Short: "report which overlay changes this cluster's existing objects cannot accept in place",
		Long: "Lists every brownfield migration and where it stands HERE. A migration is\n" +
			"PENDING when the overlay declares a field the API server fixes at create time\n" +
			"and the live object does not carry it: Argo computes its diff by\n" +
			"dry-run-applying, so that rejection produces no diff, the Application reads\n" +
			"Synced, and the change never lands — along with every other change to the same\n" +
			"object, because the diff is per object.\n\n" +
			"Read-only, and it never fails: the gate that fails on an undelivered field is\n" +
			"`llz ci assert-overlay-applied`. This exists so a cluster SAYS what it is\n" +
			"carrying. On a greenfield cluster every migration reads DONE — each object was\n" +
			"created in its final shape.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			path, cleanup, err := ResolveKubeconfig(kubeconfig)
			if err != nil {
				return err
			}
			defer cleanup()
			defer PinKubeconfig(path)()
			reportMigrations(NewBootstrapDeps(path))
			return nil
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig to use (defaults to the ambient one)")
	return cmd
}

func BrownfieldMigrateCmd() *cobra.Command {
	var (
		id         string
		kubeconfig string
		yes        bool
		force      bool
	)
	cmd := &cobra.Command{
		Use:   "brownfield-migrate",
		Short: "land one overlay change that a live object cannot accept in place (deletes and lets Argo recreate)",
		Long: "Applies one brownfield migration. It re-reads the precondition first and does\n" +
			"nothing when the field is already delivered, when the object does not exist\n" +
			"here, or when the cluster did not answer — acting on a status computed a minute\n" +
			"ago is how a migration recreates an object somebody already fixed.\n\n" +
			"The orphan-recreate strategy deletes the object with `--cascade=orphan`: the\n" +
			"pods keep running, Argo recreates the object in the declared shape and adopts\n" +
			"them by selector. The adopted pods still carry the OLD spec — rolling them is\n" +
			"left to the operator, one at a time, because rolling an ingest path is not\n" +
			"something a migration should do while nobody is watching.\n\n" +
			"Writes to a live cluster, so it requires --yes — and the global --dry-run\n" +
			"overrides it, printing the plan and writing nothing (exit 0). Without --yes and\n" +
			"without --dry-run it prints the plan and exits NON-ZERO: it was asked to do\n" +
			"something and declined.\n\n" +
			"--force overrides one refusal and one only: that this object has already been\n" +
			"recreated once and the value still did not arrive. Use it when you have fixed\n" +
			"what made that attempt fail. It cannot override a read that failed, or the\n" +
			"checks on whether Argo would put the object back.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			// THE SAME LATE READ converge makes, on the same flag, for the same write.
			// This verb is the OTHER way an orphan delete happens, and it was missed:
			// `llz --dry-run ci brownfield-migrate --id … --yes` really deleted a live
			// StatefulSet. The error is not discardable for the reason cobra_nudge.go
			// gives — a flag that cannot be read must not be assumed to be false on a
			// path that mutates a cluster.
			dryRun, err := cmd.Flags().GetBool("dry-run")
			if err != nil {
				return fmt.Errorf("cannot read the global --dry-run flag (%w) — refusing to run, because "+
					"the alternative is treating an unreadable flag as \"not a dry run\" and recreating a "+
					"live StatefulSet", err)
			}
			path, cleanup, err := ResolveKubeconfig(kubeconfig)
			if err != nil {
				return err
			}
			defer cleanup()
			// BEFORE the Writer is built, and before anything reads: the handle execs
			// through the ambient environment, so the process has to be pointed at the
			// cluster the operator named or the read and the write can disagree.
			defer PinKubeconfig(path)()
			return runMigration(NewBootstrapDeps(path), MustMigrationWriter(), id, yes, dryRun, force)
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "the migration to apply (required)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig to use (defaults to the ambient one)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the recreate — without it the plan is printed, nothing is written, and the command exits non-zero (use --dry-run for a plan-only run that exits 0)")
	cmd.Flags().BoolVar(&force, "force", false,
		"proceed despite an ADVISORY refusal — today, that this object was already recreated once and the "+
			"value still did not arrive. It never overrides a failed read or a precondition about whether "+
			"the object can come back")
	return cmd
}
