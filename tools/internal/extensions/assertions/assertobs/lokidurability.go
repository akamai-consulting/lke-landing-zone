package assertobs

// lokidurability.go is the transport half of assert-loki's WAL-survivability
// check: read each ingester's delivered resource limits and WAL volume off the
// running pod, and hand them to health.LokiIngesterDurability, which holds the
// judgement.
//
// WHY IT READS THE POD AND NOT THE VALUES. The setting this checks was carried,
// for months, on a chart key the running topology does not read — correct in
// intent, applied to nothing, invisible to every review. So the only trustworthy
// question is what the ingester actually GOT. This file therefore never mentions
// a values key except in the remedy text, and it would keep working if apl-core
// changed how the value is delivered.
//
// SCOPE: ingesters only. Queriers and distributors hold no WAL, so a low limit
// there is a capacity question, not the closed OOM loop this exists to catch.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	yaml "gopkg.in/yaml.v3"
)

// lokiIngesterContainer is the container within an ingester pod that holds the
// WAL. The chart names it after the target it runs.
//
// MATCHED THREE WAYS, because this name is the one piece of the topology this
// repo has NOT confirmed against a live cluster, and it is load-bearing: "no
// ingester pods" is fatal (correctly — a check that examined nothing must not
// pass), so a wrong name turns the gating lane red with a diagnostic blaming the
// probe rather than the cluster. isLokiIngester therefore accepts the container
// name, the pod name, or the `-target=ingester` argument the process is actually
// started with — the last being the one thing that cannot be true of a
// non-ingester.
const lokiIngesterContainer = "ingester"

// lokiIngesterTargetArg is how an ingester process is started, whatever the chart
// calls its container or workload.
const lokiIngesterTargetArg = "-target=ingester"

// isLokiIngester reports whether a container in a pod is the WAL-holding
// ingester. Any of the three signals is enough: they are alternative spellings of
// one fact, and requiring agreement would make a rename fatal again.
func isLokiIngester(podName, containerName string, args []string) bool {
	if containerName == lokiIngesterContainer || strings.Contains(podName, "ingester") {
		return true
	}
	for _, a := range args {
		if strings.Contains(a, lokiIngesterTargetArg) {
			return true
		}
	}
	return false
}

// lokiDurabilityFindings returns one line per property per ingester, plus whether
// any of them is fatal.
//
// FAIL-CLOSED ON AN EMPTY FLEET. Zero ingester pods is a failure, not a pass: a
// gate that examined nothing and reported success is exactly what a broken
// deployment looks like, and this lane's own history is of vacuous greens. The
// one legitimate "no ingesters" state — Loki not deployed at all — is already
// separated out upstream by lokiBootstrapped's own no-pods failure, so reaching
// here with none means the pods exist under a name this probe cannot see.
func lokiDurabilityFindings(nameMatch string) []lokiWriteMsg {
	specs, answered := lokiIngesterSpecs(nameMatch)
	if !answered {
		return []lokiWriteMsg{{"FAIL: could not read Loki pods to check WAL survivability — " +
			"this is 'could not tell', not 'nothing wrong', and the lane is not vouching for it", true}}
	}
	if len(specs) == 0 {
		return []lokiWriteMsg{{fmt.Sprintf(
			"FAIL: no Loki ingester pods found (matched name~=%q, container %q) — nothing was examined, "+
				"so this check is not evidence of WAL survivability. If the chart renamed the ingester "+
				"workload, this gate is blind until the name here is corrected",
			nameMatch, lokiIngesterContainer), true}}
	}
	var out []lokiWriteMsg
	for _, s := range specs {
		msgs, failed := health.LokiIngesterDurability(s,
			clusterspec.LokiWALReplayMemoryLimit, clusterspec.LokiIngesterStorageClass)
		for _, m := range msgs {
			// Every line from one ingester carries that ingester's verdict, so a
			// mixed fleet — the shape actually observed live, where one replica of
			// three was healthy — cannot have its healthy replica vouch for the
			// other two.
			out = append(out, lokiWriteMsg{m, failed})
		}
	}
	return out
}

// lokiPodSpec is the slice of a pod's spec this check reads. Declared here rather
// than reusing a broader type so the JSON shape and the fields actually consulted
// stay visibly the same thing.
type lokiPodSpec struct {
	Metadata struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Containers []struct {
			Name string   `json:"name"`
			Args []string `json:"args"`
			// The chart passes -target= as an arg on some paths and in the command
			// on others; both are read so a rename of either cannot blind the probe.
			Command   []string `json:"command"`
			Resources struct {
				Limits map[string]string `json:"limits"`
			} `json:"resources"`
		} `json:"containers"`
		Volumes []struct {
			Name     string          `json:"name"`
			EmptyDir json.RawMessage `json:"emptyDir"`
			HostPath json.RawMessage `json:"hostPath"`
			// Decoded rather than left raw: the claimName is what the storage-class
			// lookup needs, and a PVC-backed WAL on the wrong class is the finding
			// a volume-TYPE check cannot see.
			PersistentVolumeClaim *struct {
				ClaimName string `json:"claimName"`
			} `json:"persistentVolumeClaim"`
			// The config volume's backing ConfigMap, so the replay ceiling is read
			// from the object the ingester actually mounts rather than from a name
			// this probe assumes.
			ConfigMap *struct {
				Name string `json:"name"`
			} `json:"configMap"`
		} `json:"volumes"`
	} `json:"spec"`
}

// lokiIngesterSpecs reads the ingester pods' delivered specs. answered=false
// distinguishes an unreadable API from a fleet with no ingesters — the caller
// fails on both, but says which.
func lokiIngesterSpecs(nameMatch string) (specs []health.LokiIngesterSpec, answered bool) {
	// The same two-step lokiPods uses: the chart's label first, then every pod
	// filtered by name. The fallback matters because the label is the chart's to
	// change and this check must not go silently blind when it does — the
	// container-name filter below is what keeps the broad list honest.
	//
	// A FAILED LABEL QUERY FALLS THROUGH RATHER THAN FAILING THE LANE, and the
	// distinction cost a review round to spot. `--name-match` is documented as a
	// substring OR REGEX, and a regex is not a legal label value: `-l
	// app.kubernetes.io/name=loki.*` makes kubectl reject the selector outright.
	// lokiPods survives that because kubectlprobe.Items folds any error into "no
	// items" and it moves on to the name scan; this function used ItemsOK, so the
	// same flag value that merely degrades there hard-failed the whole assert-loki
	// lane here. The fail-closed read is the one that matters — the BROAD list
	// below — and it keeps ItemsOK.
	raws, ok := kubectlprobe.ItemsOK("get", "pods", "-A", "-l", "app.kubernetes.io/name="+nameMatch)
	byName := false
	if !ok || len(raws) == 0 {
		if raws, ok = kubectlprobe.ItemsOK("get", "pods", "-A"); !ok {
			return nil, false
		}
		byName = true
	}
	re, err := regexp.Compile(nameMatch)
	if err != nil {
		return nil, false
	}
	for _, raw := range raws {
		var p lokiPodSpec
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		if byName && !re.MatchString(p.Metadata.Name) {
			continue
		}
		spec := health.LokiIngesterSpec{Namespace: p.Metadata.Namespace, Name: p.Metadata.Name}
		found := false
		for _, ct := range p.Spec.Containers {
			if !isLokiIngester(p.Metadata.Name, ct.Name, append(append([]string{}, ct.Args...), ct.Command...)) {
				continue
			}
			found = true
			spec.MemoryLimit = ct.Resources.Limits["memory"]
		}
		if !found {
			continue // not an ingester pod
		}
		for _, v := range p.Spec.Volumes {
			if v.Name == lokiConfigVolumeName && v.ConfigMap != nil {
				spec.WALReplayCeiling, spec.WALCeilingKnown =
					lokiReplayCeiling(p.Metadata.Namespace, v.ConfigMap.Name)
			}
			if v.Name != health.LokiWALVolumeName {
				continue
			}
			switch {
			case v.PersistentVolumeClaim != nil:
				spec.WALVolumeSource = "persistentVolumeClaim"
				spec.WALStorageClass, spec.WALClassKnown =
					pvcStorageClass(p.Metadata.Namespace, v.PersistentVolumeClaim.ClaimName)
			case len(v.EmptyDir) > 0:
				spec.WALVolumeSource = "emptyDir"
			case len(v.HostPath) > 0:
				spec.WALVolumeSource = "hostPath"
			default:
				spec.WALVolumeSource = "other"
			}
		}
		specs = append(specs, spec)
	}
	return specs, true
}

// pvcStorageClass reads one PVC's storageClassName. known=false means the PVC
// could not be read at all, which the judgement reports as a refusal to vouch
// rather than as "no class set" — those are different facts with different
// remedies, and collapsing them is how a gate starts diagnosing the wrong thing.
//
// spec.storageClassName, not the status: the spec is what was ASKED for, and a
// PVC still Pending because the class does not exist is precisely a case worth
// failing on rather than skipping.
func pvcStorageClass(ns, name string) (class string, known bool) {
	if ns == "" || name == "" {
		return "", false
	}
	out, ok := kubectlprobe.JSONPathOK("-n", ns, "get", "pvc", name,
		"-o", "jsonpath={.spec.storageClassName}")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// lokiConfigVolumeName is the chart's name for the volume holding Loki's
// rendered config.yaml, which the ingester loads at startup.
//
// A NAME THIS PROBE ASSUMES, said plainly, and the only one left in it. The
// resource limits and the WAL volume are read off the pod, so a chart rename
// cannot make them lie; the ceiling lives in a file this probe has to locate
// first. The mitigation is that a miss reports UNKNOWN rather than ABSENT —
// health.LokiIngesterDurability fails on both, but names which — so a rename
// surfaces as "could not read the config" pointing here, not as a false
// diagnosis of a missing ceiling pointing at the overlay.
const lokiConfigVolumeName = "config"

// lokiReplayCeiling reads ingester.wal.replay_memory_ceiling out of the config
// the ingester loads. known=false means the ConfigMap or its key could not be
// read; known=true with an empty string means the config genuinely sets no
// ceiling, which is the finding.
//
// PARSED AS YAML, not grepped. `replay_memory_ceiling:` can legally appear
// nested under a different top-level section, and a substring match would read a
// neighbouring block's value as the ingester's — a gate confidently quoting a
// number from the wrong place is worse than one that says it cannot tell.
func lokiReplayCeiling(ns, cm string) (ceiling string, known bool) {
	if ns == "" || cm == "" {
		return "", false
	}
	out, ok := kubectlprobe.JSONPathOK("-n", ns, "get", "configmap", cm,
		"-o", "jsonpath={.data.config\\.yaml}")
	if !ok || strings.TrimSpace(out) == "" {
		return "", false
	}
	var doc struct {
		Ingester struct {
			WAL struct {
				ReplayMemoryCeiling any `yaml:"replay_memory_ceiling"`
			} `yaml:"wal"`
		} `yaml:"ingester"`
	}
	if yaml.Unmarshal([]byte(out), &doc) != nil {
		return "", false
	}
	if doc.Ingester.WAL.ReplayMemoryCeiling == nil {
		// The config parsed and simply does not set it: ABSENT, which is the
		// finding, not a refusal to vouch.
		return "", true
	}
	return strings.TrimSpace(fmt.Sprintf("%v", doc.Ingester.WAL.ReplayMemoryCeiling)), true
}
