package bootstrapcluster

// brownfield_kubeconfig_test.go — the read and the write must reach the same
// cluster.
//
// The hazard is specific and was live: bootstrapDeps.kubectl sets cmd.Env from
// the resolved kubeconfig, while capability's Writer execs through
// kubectlprobe.Exec, which inherits the process environment. Read from the named
// cluster, delete on the ambient one. ResolveKubeconfig refuses to fall back to
// the ambient config precisely because this command class "deletes and recreates"
// — and that refusal was being undone one call later.

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/spf13/cobra"
)

func TestPinKubeconfigPointsTheProcessAtTheNamedCluster(t *testing.T) {
	t.Setenv("KUBECONFIG", "/ambient/config")
	restore := PinKubeconfig("/named/config")
	if got := os.Getenv("KUBECONFIG"); got != "/named/config" {
		t.Errorf("KUBECONFIG = %q, want the named path — the Writer execs through the process env", got)
	}
	restore()
	if got := os.Getenv("KUBECONFIG"); got != "/ambient/config" {
		t.Errorf("KUBECONFIG after restore = %q, want the ambient value back", got)
	}
}

// An unset variable must come back UNSET, not set to empty: an empty KUBECONFIG
// is not the same as no KUBECONFIG to kubectl's resolution order.
func TestPinKubeconfigRestoresAnUnsetVariableToUnset(t *testing.T) {
	t.Setenv("KUBECONFIG", "placeholder")
	if err := os.Unsetenv("KUBECONFIG"); err != nil {
		t.Fatal(err)
	}
	restore := PinKubeconfig("/named/config")
	restore()
	if v, had := os.LookupEnv("KUBECONFIG"); had {
		t.Errorf("KUBECONFIG is set to %q after restore; it was unset before", v)
	}
}

// No path resolved (the ambient config IS the target) must leave the environment
// exactly as it was.
func TestPinKubeconfigWithNoPathChangesNothing(t *testing.T) {
	t.Setenv("KUBECONFIG", "/ambient/config")
	PinKubeconfig("")()
	if got := os.Getenv("KUBECONFIG"); got != "/ambient/config" {
		t.Errorf("KUBECONFIG = %q, want it untouched", got)
	}
}

// THE WIRING, RUN RATHER THAN ASSERTED ABOUT. An earlier version of this test
// checked that each RunE was non-nil and that the flags existed, which left
// `defer PinKubeconfig(path)()` deletable with the test still green — the exact
// read-here/delete-there regression the file exists for.
//
// So the verbs are EXECUTED against a temp kubeconfig, with kubectlprobe.Exec —
// the seam the capability Writer and the engine's reader both go through —
// replaced by a recorder that captures $KUBECONFIG as each call sees it. Nothing
// reaches a cluster; what is measured is which cluster it WOULD have reached.
func TestTheVerbsPointEveryCallAtTheKubeconfigTheOperatorNamed(t *testing.T) {
	named := filepath.Join(t.TempDir(), "named.kubeconfig")
	if err := os.WriteFile(named, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", "/ambient/config")

	var seen []string
	prev := kubectlprobe.Exec
	kubectlprobe.Exec = func(string, ...string) ([]byte, error) {
		seen = append(seen, os.Getenv("KUBECONFIG"))
		// Absent, so the read resolves without a cluster and nothing is written.
		return nil, errors.New(`Error from server (NotFound): statefulsets.apps "loki-ingester" not found`)
	}
	t.Cleanup(func() { kubectlprobe.Exec = prev })

	for _, tc := range []struct {
		verb string
		args []string
	}{
		{"brownfield-migrations", []string{"--kubeconfig", named}},
		// No --yes: the plan is printed and nothing is written, which is enough —
		// the read has already happened by then, through the same seam.
		{"brownfield-migrate", []string{"--kubeconfig", named, "--id", clusterspec.LokiWALPVCMigration}},
	} {
		seen = nil
		cmd := BrownfieldMigrationsCmd()
		if tc.verb == "brownfield-migrate" {
			cmd = BrownfieldMigrateCmd()
		}
		// Under a root that registers the global --dry-run, because the migrate verb
		// reads it and refuses to run when it cannot — the same late read converge
		// makes, and a verb run outside a root is not the shape it ships in.
		root := &cobra.Command{Use: "llz"}
		root.PersistentFlags().Bool("dry-run", false, "")
		root.AddCommand(cmd)
		root.SetArgs(append([]string{tc.verb}, tc.args...))
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		if err := root.Execute(); err != nil {
			t.Fatalf("%s: %v", tc.verb, err)
		}
		if len(seen) == 0 {
			t.Fatalf("%s made no kubectl call — this test is not reaching the code it describes", tc.verb)
		}
		for _, got := range seen {
			if got != named {
				t.Errorf("%s: a call saw KUBECONFIG=%q, want %q. The read and the orphan delete take "+
					"different routes to the cluster, so an unpinned process reads one cluster and "+
					"deletes on another", tc.verb, got, named)
			}
		}
	}
	// …and the ambient value is back afterwards.
	if got := os.Getenv("KUBECONFIG"); got != "/ambient/config" {
		t.Errorf("KUBECONFIG = %q after the verbs ran, want the ambient value restored", got)
	}
}

// `llz --dry-run ci brownfield-migrate --id … --yes` must write nothing. converge
// was wired for this flag deliberately; this verb — the OTHER way an orphan
// delete happens — was missed, and --yes made it the more dangerous of the two.
func TestTheMigrateVerbHonoursTheGlobalDryRun(t *testing.T) {
	named := filepath.Join(t.TempDir(), "named.kubeconfig")
	if err := os.WriteFile(named, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The object reads as PENDING and its owner permits a recreate, so the only
	// thing standing between this invocation and a delete is the flag.
	deleted := false
	prev := kubectlprobe.Exec
	kubectlprobe.Exec = func(_ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "application.argoproj.io" {
				return []byte(dryRunOwner), nil
			}
			if a == "delete" {
				// The failure, recorded — and the object then reads as migrated so a
				// regression fails in milliseconds instead of sleeping out the six-minute
				// recreate wait.
				deleted = true
				return nil, nil
			}
		}
		if deleted {
			return []byte(dryRunMigratedSTS), nil
		}
		return []byte(dryRunPendingSTS), nil
	}
	t.Cleanup(func() { kubectlprobe.Exec = prev })

	root := &cobra.Command{Use: "llz"}
	root.PersistentFlags().Bool("dry-run", false, "")
	root.AddCommand(BrownfieldMigrateCmd())
	root.SetArgs([]string{"--dry-run", "brownfield-migrate", "--kubeconfig", named,
		"--id", clusterspec.LokiWALPVCMigration, "--yes"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Errorf("a dry run that printed the plan is a success, got %v", err)
	}
	if deleted {
		t.Error("--dry-run issued the orphan delete")
	}
}

const dryRunPendingSTS = `{"apiVersion":"apps/v1","kind":"StatefulSet",
  "metadata":{"name":"loki-ingester","namespace":"monitoring"},
  "spec":{"template":{"spec":{"containers":[{"name":"ingester",
    "resources":{"limits":{"cpu":"1","memory":"1Gi"}}}]}}}}`

const dryRunMigratedSTS = `{"apiVersion":"apps/v1","kind":"StatefulSet",
  "metadata":{"name":"loki-ingester","namespace":"monitoring"},
  "spec":{"template":{"spec":{"containers":[{"name":"ingester","resources":{
    "limits":{"cpu":"1","memory":"3Gi"},"requests":{"cpu":"100m","memory":"512Mi"}}}]}},
    "volumeClaimTemplates":[{"metadata":{"name":"data"}}]}}`

const dryRunOwner = `{"metadata":{"name":"monitoring-loki"},
  "spec":{"project":"default","syncPolicy":{"automated":{"selfHeal":true}},
    "source":{"helm":{"values":"ingester:\n  persistence:\n    enabled: true\n"}}},
  "status":{"sync":{"status":"Synced"},"conditions":[]}}`
