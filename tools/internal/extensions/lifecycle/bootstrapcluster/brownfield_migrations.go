package bootstrapcluster

// brownfield_migrations.go — this extension's half of the brownfield migrations:
// the capability handle, the kubectl seam, and the bootstrap call site.
//
// THE ENGINE MOVED TO internal/shared/brownfield when `llz ci converge` became
// the thing that applies pending migrations. Two callers now drive the same
// recreate, and a second copy of "is this migration pending" — on a step that
// DELETES a live object — is the split-contract shape docs/e2e-gates.md warns
// about. What stays here is what only this extension can supply: a Writer built
// from ITS declaration, and a kubectl runner pointed at the resolved kubeconfig.

import (
	"fmt"
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/brownfield"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// migrationDeps hands the engine the shared runner rather than this package's.
//
// IT USED TO PASS bootstrapDeps.kubectl, WHICH IS cigate.RunCombined — stdout and
// stderr in one buffer. The engine feeds a successful read to json.Unmarshal, so
// one line of kubectl stderr (an apiserver deprecation warning, a klog notice)
// made the decode fail, which reads as MigrationUnknown, which makes
// `brownfield-migrate --yes` refuse to act. A migration that cannot be landed by
// hand because the cluster printed a warning is worse than the fault it repairs.
//
// The kubeconfig still reaches it: PinKubeconfig points the PROCESS at the
// resolved path before this is called, which is also what the capability Writer
// reads. One route for the read and the write is the property that matters.
func migrationDeps(d bootstrapDeps) brownfield.Deps {
	deps := brownfield.DefaultDeps()
	if d.kubectlOut != nil {
		deps.Kubectl = d.kubectlOut
	} else if d.kubectl != nil {
		// A test's stub, or any caller that only wired the combined runner. Better a
		// seam the caller controls than a real kubectl against the ambient cluster —
		// which is what this package's own unit tests were doing once the bootstrap
		// began reporting migrations.
		deps.Kubectl = d.kubectl
	}
	return deps
}

// PinKubeconfig points the whole PROCESS at path, and returns a restore func.
//
// IT EXISTS BECAUSE THE READ AND THE WRITE TOOK DIFFERENT ROUTES TO THE CLUSTER.
// bootstrapDeps.kubectl sets `cmd.Env` from the resolved kubeconfig, but
// capability's Writer execs through kubectlprobe.Exec, which inherits the ambient
// environment and knows nothing about `--kubeconfig` or `$KUBECONFIG_RAW`. So
// `llz ci brownfield-migrate --kubeconfig A --yes` would read PENDING from
// cluster A and orphan-delete the StatefulSet on whatever cluster the machine was
// already pointed at.
//
// ResolveKubeconfig's own header refuses to fall back to the ambient config for
// exactly this reason — "doing that against a cluster you did not name is worse
// than stopping" — and that refusal was being undone one call later, on the
// operation with the largest blast radius in this package. Pinning the process
// env makes every route agree, including the ones that have not been written yet.
//
// The Writer stays the capability handle: what changes is which cluster it
// reaches, not what it is allowed to do.
func PinKubeconfig(path string) func() {
	if path == "" {
		return func() {}
	}
	prev, had := os.LookupEnv("KUBECONFIG")
	_ = os.Setenv("KUBECONFIG", path)
	return func() {
		if had {
			_ = os.Setenv("KUBECONFIG", prev)
			return
		}
		_ = os.Unsetenv("KUBECONFIG")
	}
}

// ReportMigrationsBestEffort is the bootstrap call site. It warns and never
// aborts: a pending migration is a cluster carrying an undelivered change, which
// is a state to surface, not a reason to fail the apply that is placing the
// bridge. The APPLYING is converge's, which runs later in the same pipeline —
// this is the earlier, cheaper "say what you are carrying".
func ReportMigrationsBestEffort(d bootstrapDeps) {
	brownfield.ReportMigrationsBestEffort(migrationDeps(d))
}

// MustMigrationWriter builds the mutating handle from this extension's own
// declaration, rather than from an argv. Named for the reason assert-platform's
// MutatingBinding is: adding a binding must not silently shift which grants a
// destructive handle is built from.
func MustMigrationWriter() capability.Writer {
	return capability.MustWriter(Extension().MustBindingOf(extension.Transition, extension.Provisioned))
}

// runMigration is the verb's body: the operator's single-migration path, which
// waits for the recreate because whoever typed it is owed an answer.
func runMigration(d bootstrapDeps, w capability.Writer, id string, confirmed, dryRun, force bool) error {
	if id == "" {
		return fmt.Errorf("--id is required — `llz ci brownfield-migrations` lists them")
	}
	return brownfield.RunMigration(migrationDeps(d), w, id, confirmed, dryRun, force)
}

// reportMigrations is the report verb's body.
func reportMigrations(d bootstrapDeps) { brownfield.ReportMigrations(migrationDeps(d)) }
