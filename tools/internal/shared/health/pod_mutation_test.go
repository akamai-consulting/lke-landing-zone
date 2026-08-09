package health

import "testing"

// pod_mutation_test.go pins the two pod verdict boundaries: the ready-count
// comparison that decides whether a Running pod is failing, and the restart-count
// threshold that decides whether a container is flapping.

// TestPodIsFailing_ReadyCountBoundary walks the ready/total boundary. All-ready is
// the load-bearing case: treating ready == total as failing flunks every healthy
// cluster.
func TestPodIsFailing_ReadyCountBoundary(t *testing.T) {
	running := func(ready ...bool) PodStatus {
		s := PodStatus{Phase: "Running"}
		for _, r := range ready {
			s.ContainerStatuses = append(s.ContainerStatuses, ContainerStatus{Ready: r})
		}
		return s
	}
	cases := []struct {
		name string
		s    PodStatus
		want bool
	}{
		{"1/1 ready", running(true), false},
		{"0/1 ready", running(false), true},
		{"2/2 ready", running(true, true), false},
		{"1/2 ready", running(true, false), true},
		{"0/2 ready", running(false, false), true},
		{"0 containers (status not populated yet)", running(), false},
	}
	for _, c := range cases {
		if got := PodIsFailing(c.s); got != c.want {
			t.Errorf("%s: PodIsFailing = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestFlappingContainers_ThresholdIsExclusive pins the restart threshold: a
// container AT the threshold is not yet flapping, one above it is. An off-by-one
// here either pages on every steady-state restart budget or misses the first real
// crash-loop.
func TestFlappingContainers_ThresholdIsExclusive(t *testing.T) {
	pod := func(restarts int) PodStatus {
		return PodStatus{ContainerStatuses: []ContainerStatus{{Name: "app", RestartCount: restarts}}}
	}
	cases := []struct {
		name      string
		restarts  int
		threshold int
		want      string
	}{
		{"below threshold", 4, 5, ""},
		{"exactly at threshold", 5, 5, ""},
		{"one above threshold", 6, 5, "app=6"},
		{"zero restarts, zero threshold", 0, 0, ""},
		{"one restart, zero threshold", 1, 0, "app=1"},
	}
	for _, c := range cases {
		if got := FlappingContainers(pod(c.restarts), c.threshold); got != c.want {
			t.Errorf("%s: FlappingContainers(restarts=%d, threshold=%d) = %q, want %q",
				c.name, c.restarts, c.threshold, got, c.want)
		}
	}

	// Init containers are swept on the same boundary, after the main containers.
	both := PodStatus{
		ContainerStatuses:     []ContainerStatus{{Name: "app", RestartCount: 3}},
		InitContainerStatuses: []ContainerStatus{{Name: "init", RestartCount: 4}},
	}
	if got := FlappingContainers(both, 3); got != "init=4" {
		t.Errorf("FlappingContainers(threshold 3) = %q, want only the init container over the threshold", got)
	}
}
