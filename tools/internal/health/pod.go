package health

import (
	"fmt"
	"strconv"
	"strings"
)

// The pod types mirror the fields of `kubectl get pods -o json` that the BAD_PODS
// / restart-count jq pipelines read — enough to unmarshal the kubectl output and
// run the same classification in Go.

// StateDetail is the reason carried by a waiting/terminated container state.
type StateDetail struct {
	Reason string `json:"reason"`
}

// ContainerState is a container's current state (exactly one of the three is set).
type ContainerState struct {
	Waiting    *StateDetail `json:"waiting"`
	Running    *struct{}    `json:"running"`
	Terminated *StateDetail `json:"terminated"`
}

// ContainerStatus is one entry of .status.containerStatuses / .initContainerStatuses.
type ContainerStatus struct {
	Name         string         `json:"name"`
	Ready        bool           `json:"ready"`
	RestartCount int            `json:"restartCount"`
	State        ContainerState `json:"state"`
}

// PodStatus is the subset of .status the health checks inspect.
type PodStatus struct {
	Phase                 string            `json:"phase"`
	ContainerStatuses     []ContainerStatus `json:"containerStatuses"`
	InitContainerStatuses []ContainerStatus `json:"initContainerStatuses"`
}

// OwnerRef is the subset of metadata.ownerReferences the health checks read.
type OwnerRef struct {
	Kind string `json:"kind"`
}

// IsJobControlled reports whether a pod is owned by a Job — i.e. an ephemeral
// Job/CronJob pod (a CronJob owns the Job, the Job owns the Pod, so the pod's
// immediate owner Kind is "Job" for both). These pods are transient and
// self-completing; their health is the Job section's job (ClassifyJob), not the
// steady-state workload-pod gate. checkPods skips them so a short-lived CronJob
// pod caught mid-ContainerCreating (e.g. argo-resync-nudger firing on its
// schedule) can't be mistaken for a failing workload and flunk the health gate.
func IsJobControlled(refs []OwnerRef) bool {
	for _, r := range refs {
		if r.Kind == "Job" {
			return true
		}
	}
	return false
}

// PodIsFailing mirrors the BAD_PODS jq selector: a pod is NOT okay when it is not
// Succeeded and either not Running, or Running but not all of its (main)
// containers are ready. A pod with no container statuses yet that is non-Running
// counts as failing (still coming up); a Running pod with zero statuses does not.
func PodIsFailing(s PodStatus) bool {
	if s.Phase == "Succeeded" {
		return false
	}
	if s.Phase != "Running" {
		return true
	}
	total := len(s.ContainerStatuses)
	ready := 0
	for _, c := range s.ContainerStatuses {
		if c.Ready {
			ready++
		}
	}
	return total > 0 && ready < total
}

// transientWaitReasons are container `waiting` reasons that are self-healing:
// the container is blocked on a resource that is still being provisioned —
// typically a Secret or ConfigMap an ExternalSecret has not finished syncing —
// not on a terminal fault. A pod stranded on one of these is in-progress, not
// hard-failed: it comes up on its own once the dependency lands (e.g. the
// openbao ClusterSecretStore goes Ready, then the loki-object-store /
// harbor-registry-s3 ExternalSecrets sync on their 1m refresh and the pod
// restarts). The converge loop should poll it against the budget, exactly as it
// does for the phase1 bootstrap window.
var transientWaitReasons = map[string]bool{
	"CreateContainerConfigError": true, // envFrom/volume Secret or ConfigMap (or a referenced key) not present yet
	"CreateContainerError":       true, // runtime couldn't create the container yet — usually the same missing dep, mid-restart
}

// terminalWaitReasons are container `waiting` reasons the reconciler cannot
// resolve on its own. They must fail fast even when a sibling container is only
// transiently config-pending, so a genuinely broken pod is never masked as
// in-progress until the budget expires.
var terminalWaitReasons = map[string]bool{
	"CrashLoopBackOff":  true,
	"ImagePullBackOff":  true,
	"ErrImagePull":      true,
	"ErrImageNeverPull": true,
	"InvalidImageName":  true,
	"RunContainerError": true,
}

// PodConfigPending reports whether a failing pod's failure is (only) a transient
// config-provisioning wait — a container blocked on a Secret/ConfigMap that has
// not synced yet — rather than a terminal fault. Such a pod self-heals once the
// dependency lands, so converge should poll it against the budget instead of
// hard-failing. It returns false the moment any container is in a terminal
// waiting state or has terminated with an error, so a genuinely broken pod
// still fails. Only meaningful for a pod PodIsFailing already flagged.
func PodConfigPending(s PodStatus) bool {
	sawTransient := false
	all := append(append([]ContainerStatus{}, s.InitContainerStatuses...), s.ContainerStatuses...)
	for _, c := range all {
		switch {
		case c.State.Waiting != nil:
			r := c.State.Waiting.Reason
			if terminalWaitReasons[r] {
				return false
			}
			if transientWaitReasons[r] {
				sawTransient = true
			}
		case c.State.Terminated != nil:
			// A container that terminated for any reason other than a clean exit is
			// a real fault, not a config-provisioning wait.
			if r := c.State.Terminated.Reason; r != "" && r != "Completed" {
				return false
			}
		}
	}
	return sawTransient
}

// ReadyRatio is the "ready/total" main-container string the script reports.
func ReadyRatio(s PodStatus) string {
	total := len(s.ContainerStatuses)
	ready := 0
	for _, c := range s.ContainerStatuses {
		if c.Ready {
			ready++
		}
	}
	return strconv.Itoa(ready) + "/" + strconv.Itoa(total)
}

// SummarizeStates renders the per-container state summary (init containers first,
// then main), matching the script's "prefix/name:waiting:reason" form.
func SummarizeStates(s PodStatus) string {
	parts := append(summarizeStates(s.InitContainerStatuses, "init"),
		summarizeStates(s.ContainerStatuses, "main")...)
	return strings.Join(parts, ",")
}

func summarizeStates(cs []ContainerStatus, prefix string) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		switch {
		case c.State.Waiting != nil:
			out = append(out, fmt.Sprintf("%s/%s:waiting:%s", prefix, c.Name, reasonOr(c.State.Waiting.Reason)))
		case c.State.Terminated != nil:
			out = append(out, fmt.Sprintf("%s/%s:terminated:%s", prefix, c.Name, reasonOr(c.State.Terminated.Reason)))
		case c.State.Running != nil:
			out = append(out, fmt.Sprintf("%s/%s:running", prefix, c.Name))
		default:
			out = append(out, fmt.Sprintf("%s/%s:?", prefix, c.Name))
		}
	}
	return out
}

// FlappingContainers returns "name=restartCount" for every container (init or
// main) whose restartCount exceeds threshold — the restart-count warn check.
func FlappingContainers(s PodStatus, threshold int) string {
	var hot []string
	for _, c := range append(append([]ContainerStatus{}, s.ContainerStatuses...), s.InitContainerStatuses...) {
		if c.RestartCount > threshold {
			hot = append(hot, c.Name+"="+strconv.Itoa(c.RestartCount))
		}
	}
	return strings.Join(hot, ",")
}

func reasonOr(r string) string {
	if r == "" {
		return "?"
	}
	return r
}
