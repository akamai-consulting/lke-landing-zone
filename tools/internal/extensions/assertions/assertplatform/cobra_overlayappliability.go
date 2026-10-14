package assertplatform

// cobra_overlayappliability.go — the CLI surface for overlayappliability.
//
// Split from overlayappliability.go for the reason every cobra_*.go here is:
// an extension directory should show its commands at a glance, and flag wiring
// living beside decision logic is how the two come to be edited as one thing.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func OverlayAppliabilityCmd() *cobra.Command {
	var emit bool
	var out string
	var printNS bool
	cmd := &cobra.Command{
		Use:   "assert-overlay-appliability",
		Short: "fail unless each overlay field's declared CreateOnly matches what an apiserver actually does",
		Long: "clusterspec.OverlayField.CreateOnly is a hand-set boolean, and the guards around\n" +
			"it only check that it is USED consistently — that a CreateOnly field names a\n" +
			"migration and a mutable one does not. Nothing asks an API server whether the\n" +
			"classification is true. This lane does.\n\n" +
			"Against a cluster holding each mapped object in its PRE-OVERLAY shape, it\n" +
			"server-dry-runs every declared change and compares the apiserver's answer to what\n" +
			"the field map claims. A field declared mutable that the apiserver fixes at create\n" +
			"time is the 16-day Loki outage arriving again: Argo dry-run-applies, is refused,\n" +
			"produces no diff, and reports Synced while the value — and every other change to\n" +
			"that object — is discarded. A field declared CreateOnly that the apiserver accepts\n" +
			"is the opposite error: a destructive recreate registered for a problem that does\n" +
			"not exist, which `llz ci converge` applies unattended at bootstrap or on a\n" +
			"dispatched platform-scope health run.\n\n" +
			"FOR A THROWAWAY CLUSTER. --emit-fixtures WRITES OBJECTS. They are named apart\n" +
			"from the real ones (a -llz-appliability-fixture suffix) and carry no replicas, but\n" +
			"they land in the app's own namespace on whatever cluster your kubeconfig points\n" +
			"at. Point it at a kind cluster you are about to delete, not at a live one.\n\n" +
			"  for ns in $(llz ci assert-overlay-appliability --print-namespaces); do\n" +
			"    kubectl create namespace \"$ns\" --dry-run=client -o yaml | kubectl apply -f -\n" +
			"  done\n" +
			"  llz ci assert-overlay-appliability --emit-fixtures --out /tmp/fixtures.json\n" +
			"  kubectl apply -f /tmp/fixtures.json\n" +
			"  llz ci assert-overlay-appliability\n\n" +
			"THE NAMESPACE LOOP IS NOT OPTIONAL. The fixtures carry no Namespace objects — the\n" +
			"kind lane creates those in a separate step — so without it the apply fails with\n" +
			"`namespaces \"monitoring\" not found`, and the gate then blames the fixture step\n" +
			"for not having run when it did. --print-namespaces exists so this recipe cannot\n" +
			"go stale against a row in a namespace nobody thought of.\n\n" +
			"--out RATHER THAN A PIPE, and the difference is not style: `llz … | kubectl apply\n" +
			"-f -` takes kubectl's exit status without pipefail, and kubectl given empty stdin\n" +
			"exits 0 — so a refusal to emit would leave the gate probing an absent object and\n" +
			"surface two steps later as something else.\n\n" +
			"The probe itself is a server dry run, which the capability model classifies as a\n" +
			"cluster read; the fixtures are applied by the caller, not by this verb.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			// --out ALONE IS A TYPO, NOT A DEFAULT. Ignoring it ran the gate, wrote no
			// file, and left the next workflow step to fail on a missing path — the
			// delayed, misattributed red this flag was added to eliminate.
			if out != "" && !emit {
				return fmt.Errorf("--out only applies with --emit-fixtures; on its own it would write " +
					"nothing and leave the next step to fail on a missing file")
			}
			if printNS {
				for _, ns := range FixtureNamespaces() {
					fmt.Println(ns)
				}
				return nil
			}
			if emit {
				doc, err := EmitFixtures()
				if err != nil {
					return err
				}
				// A FILE RATHER THAN A PIPE, when asked for one. `llz … | kubectl apply -f -`
				// reads fine and fails open: without pipefail the pipeline's status is
				// kubectl's, and kubectl given an empty stdin exits 0 — so a verb that
				// refused to emit would leave the lane probing an absent object, which is a
				// state this lane treats as fatal but which would arrive as a confusing red
				// two steps later. Writing the file here keeps the failure at its source and
				// keeps the workflow to one command per step.
				if out != "" {
					return os.WriteFile(out, []byte(doc+"\n"), 0o644)
				}
				fmt.Println(doc)
				return nil
			}
			return assertOverlayAppliability()
		},
	}
	cmd.Flags().BoolVar(&emit, "emit-fixtures", false,
		"emit the pre-overlay objects this lane probes, as a JSON List for `kubectl apply`")
	cmd.Flags().BoolVar(&printNS, "print-namespaces", false,
		"list the namespaces the fixtures need (they carry no Namespace objects; the caller owns those)")
	cmd.Flags().StringVar(&out, "out", "",
		"with --emit-fixtures, write to this path instead of stdout (avoids a pipeline that fails open)")
	return cmd
}
