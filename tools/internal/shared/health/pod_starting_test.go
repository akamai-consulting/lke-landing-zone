package health

// pod_starting_test.go — the line between a pod coming up and a pod that broke.
//
// It was drawn in the wrong place and cost a release-e2e round: a pod
// mid-ContainerCreating on a four-minute-old cluster was recorded as a hard
// failure, twice sixty seconds apart, and convergence aborted with "operator
// intervention required" while every Application was still flipping OutOfSync ->
// Synced. Both directions matter — the reasons that mean "waiting helps" must
// pend, and the reasons that mean "waiting will not help" must still fail fast.

import "testing"

func waiting(name, reason string) ContainerStatus {
	return ContainerStatus{Name: name, State: ContainerState{Waiting: &StateDetail{Reason: reason}}}
}

func TestPodIsStartingAcceptsTheStartupReasons(t *testing.T) {
	for _, reason := range []string{"ContainerCreating", "PodInitializing"} {
		s := PodStatus{Phase: "Pending", ContainerStatuses: []ContainerStatus{waiting("main", reason)}}
		if !PodIsStarting(s) {
			t.Errorf("%s is Kubernetes saying it is working on it — treating it as a failure aborts a "+
				"cluster that is merely young", reason)
		}
		// And it must still read as not-serving, or the gate stops waiting for it.
		if !PodIsFailing(s) {
			t.Errorf("%s should still count as not-ready for the liveness predicate", reason)
		}
	}
}

// The reasons waiting cannot fix must keep failing at full speed.
func TestPodIsStartingRejectsRealFailures(t *testing.T) {
	for _, reason := range []string{
		"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull",
		"CreateContainerConfigError", "CreateContainerError", "InvalidImageName",
	} {
		s := PodStatus{Phase: "Pending", ContainerStatuses: []ContainerStatus{waiting("main", reason)}}
		if PodIsStarting(s) {
			t.Errorf("%s means Kubernetes TRIED and cannot proceed — waiting does not fix it, so the "+
				"gate must not soften to pending", reason)
		}
	}
}

// One container that cannot start is the whole pod's verdict, even while a
// sibling is still being created.
func TestOneBrokenContainerOutweighsAStartingSibling(t *testing.T) {
	s := PodStatus{Phase: "Pending", ContainerStatuses: []ContainerStatus{
		waiting("sidecar", "ContainerCreating"),
		waiting("main", "CrashLoopBackOff"),
	}}
	if PodIsStarting(s) {
		t.Error("a CrashLoopBackOff container was masked by a ContainerCreating sibling")
	}
}

// A pod the kubelet has not reported on yet is the earliest moment of its life.
func TestAPodWithNoStatusesYetIsStarting(t *testing.T) {
	if !PodIsStarting(PodStatus{Phase: "Pending"}) {
		t.Error("a scheduled pod with no container statuses yet is starting, not broken")
	}
	// ...but a Failed pod is not, whatever its statuses say.
	if PodIsStarting(PodStatus{Phase: "Failed"}) {
		t.Error("a Failed pod must never read as starting")
	}
}

// A completed init container is normal; a main container that died is not.
func TestTerminatedContainersAreJudgedByReason(t *testing.T) {
	done := ContainerStatus{Name: "init", State: ContainerState{Terminated: &StateDetail{Reason: "Completed"}}}
	if !PodIsStarting(PodStatus{Phase: "Pending", InitContainerStatuses: []ContainerStatus{done},
		ContainerStatuses: []ContainerStatus{waiting("main", "PodInitializing")}}) {
		t.Error("a completed init container plus a PodInitializing main container is a normal startup")
	}
	died := ContainerStatus{Name: "main", State: ContainerState{Terminated: &StateDetail{Reason: "Error"}}}
	if PodIsStarting(PodStatus{Phase: "Pending", ContainerStatuses: []ContainerStatus{died}}) {
		t.Error("a container that terminated with an error is not starting")
	}
}
