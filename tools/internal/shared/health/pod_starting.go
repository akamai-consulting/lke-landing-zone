package health

// pod_starting.go — telling a pod that is STARTING apart from one that has
// FAILED.
//
// THE INCIDENT. A release-e2e run aborted 60 seconds into a 1200-second
// convergence budget with "cluster hard-failed twice in a row — operator
// intervention required". What the two polls actually saw was
//
//	Pod otel/platform-logs-collector-… phase=Pending ready=0/1
//	  state=main/otc-container:waiting:ContainerCreating
//
// — a pod the kubelet was in the middle of creating, on a cluster that had
// existed for a few minutes. Every Application was still flipping OutOfSync ->
// Synced as the deadline hit. PodIsFailing is a liveness predicate ("is this pod
// serving?"), and the health gate was reading it as a verdict ("is this pod
// broken?"). Those are the same answer for a CrashLoopBackOff and opposite
// answers for a ContainerCreating.
//
// The budget is what bounds a pod that never starts: if ContainerCreating is
// still true twenty minutes in, converge fails on budget exhaustion and says so
// accurately. What it must not do is call the first sixty seconds of a cluster's
// life an emergency.

// startingWaitReasons are container waiting reasons that mean "Kubernetes is
// working on it": image pull in flight, volumes mounting, init containers still
// running. None of them is a verdict about the workload.
//
// DELIBERATELY NOT HERE: ImagePullBackOff, ErrImagePull, CrashLoopBackOff,
// CreateContainerConfigError, CreateContainerError, InvalidImageName. Those are
// Kubernetes reporting that it has TRIED and cannot proceed — a real failure the
// gate should keep catching at full speed, because waiting does not fix any of
// them.
var startingWaitReasons = map[string]bool{
	"ContainerCreating": true,
	"PodInitializing":   true,
}

// PodIsStarting reports whether a not-yet-ready pod is merely coming up: it has
// not been scheduled long enough to have container statuses, or every container
// that is not already running is waiting on one of the startup reasons above.
//
// A pod with a single CrashLoopBackOff container is NOT starting, even while its
// siblings are ContainerCreating — one container that cannot start is the whole
// pod's verdict.
func PodIsStarting(s PodStatus) bool {
	if s.Phase == "Succeeded" || s.Phase == "Failed" {
		return false
	}
	// UNSCHEDULABLE IS NOT STARTING. A pod no node will accept — node pool full,
	// an unsatisfiable taint or selector, an unbound PVC — never publishes
	// container statuses, so without this it would look identical to a pod the
	// kubelet has merely not reported on yet, and read as "starting" forever.
	// Waiting does not schedule a pod nothing can schedule.
	for _, c := range s.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" {
			return false
		}
	}
	all := append(append([]ContainerStatus{}, s.InitContainerStatuses...), s.ContainerStatuses...)
	// Scheduled but no statuses published yet — the kubelet has not reported in.
	// That is the earliest moment of a pod's life, not a failure.
	if len(all) == 0 {
		return s.Phase == "Pending"
	}
	for _, c := range all {
		switch {
		case c.Ready:
			continue // already up
		case c.State.Running != nil:
			// RUNNING BUT NOT READY IS NOT "STARTING", and calling it that would
			// have removed the commonest broken-workload signal there is: a
			// container whose readiness probe never passes runs forever without
			// ever being Ready. Softening it to pending means converge stops
			// fast-failing on it and instead burns the whole budget to report a
			// generic timeout. The gate judged this case before and still does —
			// only the CREATION states were ever the bug.
			return false
		case c.State.Waiting != nil:
			if !startingWaitReasons[c.State.Waiting.Reason] {
				return false // a waiting reason that waiting will not fix
			}
		case c.State.Terminated != nil:
			// An init container that completed is normal; a main container that
			// terminated while the pod is not Succeeded is not "starting".
			if c.State.Terminated.Reason != "Completed" {
				return false
			}
		}
	}
	return true
}
