package recondiag

// diagnose.go implements `llz ci diagnose-reconciler` — the diagnostics for the
// llz-reconciler, and specifically for the apl-overlay lane.
//
// WHY IT EXISTS: THE GATE NAMED WHAT IT COULD NOT SHOW YOU. When the obj chain
// does not complete, `llz ci converge` reports
//
//	apl-overlay: Loki not yet S3-backed — … (obj chain settling;
//	check llz-reconciler llz_apl_overlay_synced)
//
// and then neither converge's own dump, nor `llz ci diagnose-argocd`, nor
// cluster-health with VERBOSE=1 collects the reconciler's logs or that metric.
// The instruction is correct and unactionable from the artifacts, so answering it
// costs a live cluster and a session of remote poking — which is exactly what it
// cost, on a real stall where the overlay had been pushed with
// `provider.type: disabled` and the lane was skipping obj.yaml every pass.
//
// The general rule this is an instance of: **if a failure message tells the reader
// to go look at something, the failure bundle has to contain that something.**
// Otherwise the message is a pointer into a cluster that, by the time anyone
// reads it, has been torn down.
//
// Best-effort and ALWAYS exits 0, for argodiag's reason: diagnostics must never
// mask the failure that triggered them. Same reachability gate too — on an
// unreachable apiserver each probe would block on its own ~30s dial timeout.

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// Namespace is where the reconciler runs, and OverlayMetric is the gauge the
// converge message names. Constants rather than literals so the message and the
// probe cannot drift apart.
const (
	Namespace     = "llz-reconciler"
	OverlayMetric = "llz_apl_overlay_synced"
)

// diagStream runs a command with output streamed to stdout, best-effort. A
// package var so tests can record the probe sequence without real binaries —
// argodiag's arrangement, for the same reason.
var diagStream = func(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	_ = cmd.Run()
}

// probe is one diagnostic command, with the question it answers.
type probe struct {
	Why  string
	Args []string
}

// Probes is the plan, as a pure function of the namespace so a test can assert
// WHAT gets collected without a cluster. Exported for that test: the value of
// this verb is entirely in its coverage, so the coverage is what gets pinned.
//
// The ordering is the order a reader needs them in, not the order they were
// written: is the lane running at all → what did it SAY → what did it push →
// what is the cluster's effective view of the thing it pushes.
func Probes(ns string) []probe {
	return []probe{
		{
			Why:  "is the reconciler running, and has it restarted?",
			Args: []string{"kubectl", "-n", ns, "get", "pods", "-o", "wide"},
		},
		{
			// THE ONE THAT WAS MISSING. Every skip the apl-overlay lane takes prints
			// a line here ("obj platform credential not seeded yet — skipping
			// obj.yaml this pass"), and that line is the difference between a chain
			// that is settling and one that will never complete.
			Why:  "what is the apl-overlay lane actually doing each pass?",
			Args: []string{"kubectl", "-n", ns, "logs", "deploy/llz-reconciler", "--tail=300", "--all-containers"},
		},
		{
			Why:  "the previous container, when the lane crash-looped",
			Args: []string{"kubectl", "-n", ns, "logs", "deploy/llz-reconciler", "--tail=200", "--all-containers", "--previous"},
		},
		{
			// The gauge the converge message tells the reader to check. Reading it
			// off the scrape endpoint keeps this honest about what Prometheus sees.
			Why:  OverlayMetric + " — the gauge converge's message names",
			Args: []string{"kubectl", "-n", ns, "get", "servicemonitor,service", "-o", "wide"},
		},
		{
			Why:  "leader election — a lane that never acquires never writes",
			Args: []string{"kubectl", "-n", ns, "get", "lease", "-o", "wide"},
		},
		{
			// AplObjectStorage IS NOT A CRD — this probe used to `kubectl get
			// aplobjectstorage` and the cluster answered `the server doesn't have a
			// resource type "aplobjectstorage"`. It is a FILE in the apl-values git
			// tree (env/settings/obj.yaml) that apl-operator reads, so kubectl was
			// never going to see it. Flagged as unverified when it was written and
			// confirmed useless on the first run that reached a cluster.
			//
			// What apl-core's effective view actually looks like from inside the
			// cluster is the Loki config it rendered — which is also what converge
			// checks, so the two agree by construction rather than by restatement.
			Why:  "apl-core's rendered Loki config — does it reference S3 yet?",
			Args: []string{"kubectl", "-n", "monitoring", "get", "cm", "loki", "-o", "yaml"},
		},
		{
			Why:  "did ESO ever build the credential the overlay is supposed to enable?",
			Args: []string{"kubectl", "-n", "monitoring", "get", "secret", "loki-s3-linode-credentials"},
		},
		{
			Why:  "the reconciler's own inputs — token + Linode creds",
			Args: []string{"kubectl", "-n", ns, "get", "externalsecret,secret", "-o", "wide"},
		},
	}
}

// Run streams every probe, grouped for the run log.
func Run(ns string) error {
	if kubectlprobe.EffectiveKubeconfig() == "" {
		fmt.Fprintln(os.Stderr, "::warning::No kubeconfig available — cluster may not exist; nothing to diagnose")
		return nil
	}
	if !kubectlprobe.Reachable() {
		fmt.Fprintln(os.Stderr, "::warning::apiserver unreachable (control-plane ACL not granted, or cluster gone) — skipping reconciler diagnostics to avoid a per-probe timeout pile-up")
		return nil
	}
	for _, p := range Probes(ns) {
		fmt.Printf("::group::llz-reconciler — %s\n", p.Why)
		diagStream(p.Args[0], p.Args[1:]...)
		fmt.Println("::endgroup::")
	}
	return nil
}
