package assertobs

// The judgement is unit-tested in shared/health. What is tested here is the part
// health cannot see: that the probe reads the right fields off a real pod JSON,
// and that every way of learning nothing fails closed rather than passing
// vacuously — which is this lane's own recorded failure mode.

import (
	"errors"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
)

// ingesterPodJSON is a `kubectl get pods -o json` list holding one ingester with
// the given memory limit and volume stanza. Built from a real pod shape rather
// than from the struct this file decodes into, so a field the decoder gets wrong
// shows up as a wrong answer instead of agreeing with itself.
func ingesterPodJSON(limit, volume string) string {
	return `{"items":[{"metadata":{"namespace":"monitoring","name":"loki-ingester-0",
	  "labels":{"app.kubernetes.io/name":"loki"}},
	  "spec":{"containers":[
	    {"name":"ingester","resources":{"limits":{"cpu":"500m","memory":"` + limit + `"}}}],
	   "volumes":[{"name":"data",` + volume + `},{"name":"config","configMap":{"name":"loki"}}]},
	  "status":{"phase":"Running"}}]}`
}

const (
	volEmptyDir = `"emptyDir":{}`
	volPVC      = `"persistentVolumeClaim":{"claimName":"data-loki-ingester-0"}`
)

// walClass is the class the overlay asserts; the fake apiserver answers the PVC
// lookup with it unless a test says otherwise.
const walClass = "block-storage-retain"

func findings(t *testing.T, listJSON string) (string, bool) {
	t.Helper()
	return findingsWithClass(t, listJSON, walClass)
}

// findingsWithClass fakes BOTH reads the probe makes: the pod list, and the
// jsonpath lookup of the WAL PVC's storageClassName. Dispatching on the args (not
// answering everything with one blob) is what keeps the PVC arm honest — a stub
// that returned the pod list to every call would have the class read as empty and
// every PVC-backed case would fail for the wrong reason.
func findingsWithClass(t *testing.T, listJSON, class string) (string, bool) {
	t.Helper()
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if strings.Contains(a, "storageClassName") {
				return []byte(class), nil
			}
		}
		return []byte(listJSON), nil
	})
	var b strings.Builder
	failed := false
	for _, m := range lokiDurabilityFindings("loki") {
		b.WriteString(m.text + "\n")
		failed = failed || m.fatal
	}
	return b.String(), failed
}

// THE PRODUCTION POD, decoded end to end. This is the coupling that matters: the
// judgement is only as good as the fields the probe pulls out, and a limit read
// from the wrong container or a volume read from the wrong key would produce a
// confident, wrong verdict.
func TestTheProbeReadsTheLimitAndVolumeOffARealPod(t *testing.T) {
	got, failed := findings(t, ingesterPodJSON("1Gi", volEmptyDir))
	if !failed {
		t.Fatalf("a 1Gi emptyDir ingester passed the probe — the LIMIT half gates:\n%s", got)
	}
	if !strings.Contains(got, "1Gi") || !strings.Contains(got, "emptyDir") {
		t.Errorf("the probe did not surface the observed limit and volume:\n%s", got)
	}
	if !strings.Contains(got, "monitoring/loki-ingester-0") {
		t.Errorf("the finding does not name which ingester it is about:\n%s", got)
	}
}

func TestTheAssertedPodSpecPasses(t *testing.T) {
	got, failed := findings(t, ingesterPodJSON("3Gi", volPVC))
	if failed {
		t.Errorf("a 3Gi PVC-backed ingester was failed:\n%s", got)
	}
}

// AN EMPTY FLEET IS A FAILURE, NOT A PASS. A gate that examined nothing and
// reported success is indistinguishable from a healthy one, and that is what a
// renamed workload looks like. The message must also say the gate may be blind,
// because "no ingesters" and "the chart moved" are different remedies.
func TestNoIngestersFailsRatherThanPassingVacuously(t *testing.T) {
	got, failed := findings(t, `{"items":[]}`)
	if !failed {
		t.Fatalf("zero ingesters passed:\n%s", got)
	}
	if !strings.Contains(got, "nothing was examined") {
		t.Errorf("the message does not say nothing was examined:\n%s", got)
	}
}

// A POD THAT IS NOT AN INGESTER MUST NOT COUNT AS ONE. The querier carries no
// WAL; grading it here would let a healthy querier vouch for a broken ingester —
// and with the name fallback in play, every Loki pod reaches this filter.
func TestNonIngesterPodsAreNotGraded(t *testing.T) {
	querier := `{"items":[{"metadata":{"namespace":"monitoring","name":"loki-querier-0"},
	  "spec":{"containers":[{"name":"querier","resources":{"limits":{"memory":"256Mi"}}}],
	   "volumes":[{"name":"data","emptyDir":{}}]},"status":{"phase":"Running"}}]}`
	got, failed := findings(t, querier)
	if !failed {
		t.Errorf("a fleet of only queriers passed — it contains no ingester, so nothing "+
			"was examined and the check has no evidence to offer:\n%s", got)
	}
	if strings.Contains(got, "loki-querier-0") {
		t.Errorf("the querier was graded as an ingester:\n%s", got)
	}
}

// AN UNREADABLE CLUSTER IS NOT AN EMPTY ONE. Both fail, but they are different
// facts and the operator needs to know which — one is a broken deployment, the
// other a broken kubeconfig.
func TestAnUnreadableAPIFailsAndSaysSo(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("no cluster") })
	var lines []string
	failed := false
	for _, m := range lokiDurabilityFindings("loki") {
		lines = append(lines, m.text)
		failed = failed || m.fatal
	}
	got := strings.Join(lines, "\n")
	if !failed {
		t.Fatalf("an unreachable API passed:\n%s", got)
	}
	if !strings.Contains(got, "could not read") {
		t.Errorf("an unreadable cluster was reported as an empty one:\n%s", got)
	}
}

// A MIXED FLEET CANNOT AVERAGE OUT. The live incident had one healthy replica of
// three; a verdict that let it speak for the fleet would have reported green.
func TestOneHealthyReplicaDoesNotVouchForABrokenOne(t *testing.T) {
	two := `{"items":[
	  {"metadata":{"namespace":"monitoring","name":"loki-ingester-2"},
	   "spec":{"containers":[{"name":"ingester","resources":{"limits":{"memory":"3Gi"}}}],
	    "volumes":[{"name":"data",` + volPVC + `}]},"status":{"phase":"Running"}},
	  {"metadata":{"namespace":"monitoring","name":"loki-ingester-0"},
	   "spec":{"containers":[{"name":"ingester","resources":{"limits":{"memory":"1Gi"}}}],
	    "volumes":[{"name":"data",` + volEmptyDir + `}]},"status":{"phase":"Running"}}]}`
	got, failed := findings(t, two)
	if !failed {
		t.Fatalf("a fleet with one broken ingester passed:\n%s", got)
	}
	if !strings.Contains(got, "loki-ingester-0") {
		t.Errorf("the broken replica is not named:\n%s", got)
	}
}

// THE CLASS IS READ FROM THE PVC, not assumed from the volume type. This is the
// arm that catches an override written one level above the key the chart reads:
// the PVC exists, so a volume-TYPE check passes, and the class is empty.
func TestAPVCBackedWALOnNoClassIsCaught(t *testing.T) {
	// REPORTED, not gating — the WAL PVC cannot be delivered by the overlay at
	// all (health.WALFindingsGate). What must hold is that the finding is made.
	got, _ := findingsWithClass(t, ingesterPodJSON("3Gi", volPVC), "")
	if !strings.Contains(got, "NO storageClassName") {
		t.Fatalf("a PVC with no storageClassName produced no finding:\n%s", got)
	}
}

func TestAPVCBackedWALOnTheAssertedClassPasses(t *testing.T) {
	got, failed := findingsWithClass(t, ingesterPodJSON("3Gi", volPVC), walClass)
	if failed {
		t.Errorf("the asserted class was failed:\n%s", got)
	}
}

// An UNREADABLE PVC is not a PVC with no class. Both fail, but the message must
// say which — one is a wrong override, the other a broken kubeconfig.
func TestAnUnreadablePVCRefusesToVouch(t *testing.T) {
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if strings.Contains(a, "storageClassName") {
				return nil, errors.New("pvc unreadable")
			}
		}
		return []byte(ingesterPodJSON("3Gi", volPVC)), nil
	})
	var b strings.Builder
	failed := false
	for _, m := range lokiDurabilityFindings("loki") {
		b.WriteString(m.text + "\n")
		failed = failed || m.fatal
	}
	_ = failed // the volume half reports without gating; the finding is the property
	if !strings.Contains(b.String(), "could not be read") {
		t.Errorf("an unreadable PVC was reported as a classless one:\n%s", b.String())
	}
}

// THE ONE PLACE BOTH SIDES ARE VISIBLE, which is why the coupling lives here.
//
// clusterspec writes the WAL claim's NAME into the overlay; health looks for a
// volume by that name on the running pod. They are two constants because
// clusterspec may not import health — that would pull the APL layer onto a
// concrete cloud (clusterspec → health → linode), which
// TestAPLLayerDoesNotDependOnAConcreteCloud forbids. assertobs imports both, so
// this is where the duplication is held honest.
//
// If they ever diverge, the overlay asks for a claim the probe cannot find, and
// the durability check reports "no data volume" on a correctly-configured
// cluster — a confident, wrong verdict, which is worse than no check.
func TestTheAssertedClaimNameIsTheOneTheProbeLooksFor(t *testing.T) {
	if clusterspec.LokiWALClaimName != health.LokiWALVolumeName {
		t.Errorf("the overlay asserts a claim named %q but the probe looks for a volume named %q — "+
			"the WAL check would report 'no data volume' on a correct cluster",
			clusterspec.LokiWALClaimName, health.LokiWALVolumeName)
	}
}

// THE CONTAINER NAME IS THE ONE PIECE OF TOPOLOGY THIS REPO HAS NOT CONFIRMED
// against a live cluster, and "no ingester pods" is fatal — so a rename would
// turn the gating lane red with a message blaming the probe. Three signals are
// accepted; any one is enough.
func TestAnIngesterIsRecognisedByName_PodName_OrTargetArg(t *testing.T) {
	for name, tc := range map[string]struct {
		pod, container string
		args           []string
		want           bool
	}{
		"container name":           {"loki-write-0", "ingester", nil, true},
		"pod name":                 {"loki-ingester-0", "loki", nil, true},
		"target arg":               {"loki-write-0", "loki", []string{"-target=ingester"}, true},
		"target among other args":  {"x-0", "c", []string{"-config.file=/etc/loki/config.yaml", "-target=ingester"}, true},
		"a querier is not one":     {"loki-querier-0", "querier", []string{"-target=querier"}, false},
		"a distributor is not one": {"loki-distributor-0", "distributor", nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isLokiIngester(tc.pod, tc.container, tc.args); got != tc.want {
				t.Errorf("isLokiIngester(%q, %q, %v) = %v, want %v", tc.pod, tc.container, tc.args, got, tc.want)
			}
		})
	}
}

// And end to end: a pod whose container is renamed but still runs -target=ingester
// is graded, not skipped. Skipping it would empty the fleet and fail the lane with
// "no ingester pods" — the probe blaming the cluster for its own assumption.
func TestARenamedIngesterContainerIsStillGraded(t *testing.T) {
	renamed := `{"items":[{"metadata":{"namespace":"monitoring","name":"loki-write-0"},
	  "spec":{"containers":[{"name":"loki","args":["-target=ingester"],
	    "resources":{"limits":{"memory":"1Gi"}}}],
	   "volumes":[{"name":"data",` + volEmptyDir + `}]},"status":{"phase":"Running"}}]}`
	got, failed := findings(t, renamed)
	if !failed {
		t.Fatalf("a renamed-container ingester below the floor was not failed:\n%s", got)
	}
	if strings.Contains(got, "nothing was examined") {
		t.Errorf("the renamed ingester was skipped, so the fleet read empty:\n%s", got)
	}
}
