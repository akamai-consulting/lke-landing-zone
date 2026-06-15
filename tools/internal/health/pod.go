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
