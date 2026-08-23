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
	// Scheduled but no statuses published yet — the kubelet has not reported in.
	// That is the earliest moment of a pod's life, not a failure.
	if len(s.InitContainerStatuses) == 0 && len(s.ContainerStatuses) == 0 {
		return s.Phase == "Pending"
	}
	// INIT AND MAIN CONTAINERS ARE JUDGED SEPARATELY, because RUNNING means
	// opposite things for the two. Flattening them into one list reintroduces the
	// incident this file exists to prevent: a non-sidecar init container reports
	// ready=false for its
	// whole run, so a pod sitting in a perfectly normal `wait-for-db` init was
	// recorded CatFail and aborted converge.
	for _, c := range s.InitContainerStatuses {
		if !initContainerIsStarting(c) {
			return false
		}
	}
	for _, c := range s.ContainerStatuses {
		if !mainContainerIsStarting(c) {
			return false
		}
	}
	return true
}

// initContainerIsStarting judges an INIT container. Running is its normal working
// state — that is what an init container is for — and Completed is its success.
func initContainerIsStarting(c ContainerStatus) bool {
	switch {
	case c.Ready, c.State.Running != nil:
		return true // doing its job
	case c.State.Waiting != nil:
		return waitReasonIsStartup(c.State.Waiting.Reason)
	case c.State.Terminated != nil:
		return c.State.Terminated.Reason == "Completed"
	}
	return true
}

// mainContainerIsStarting judges a MAIN container.
//
// RUNNING BUT NOT READY IS NOT "STARTING" here, and calling it that would remove
// the commonest broken-workload signal there is: a container whose readiness
// probe never passes runs forever without ever becoming Ready. Softening it means
// converge stops fast-failing on it and burns the whole budget to report a
// generic timeout instead of naming the workload.
func mainContainerIsStarting(c ContainerStatus) bool {
	switch {
	case c.Ready:
		return true
	case c.State.Running != nil:
		return false
	case c.State.Waiting != nil:
		return waitReasonIsStartup(c.State.Waiting.Reason)
	case c.State.Terminated != nil:
		return c.State.Terminated.Reason == "Completed"
	}
	return true
}

// waitReasonIsStartup reports whether a container's waiting reason means
// Kubernetes is still working on it.
//
// AN EMPTY REASON COUNTS AS STARTUP. Container statuses are observed in the wild
// without a reason — reasonOr() in pod.go exists for exactly that — and no
// evidence is not evidence of failure. The convergence budget bounds it either
// way; what must not happen is a blank field reading as a broken workload.
func waitReasonIsStartup(reason string) bool {
	return reason == "" || startingWaitReasons[reason]
}

// PodIsWarmingUp reports whether a pod is RUNNING but has not passed its
// readiness probes yet, with nothing suggesting it never will.
//
// THE TWO CLASSIFIERS HAD TO AGREE. ClassifyServiceEndpoints treats "endpoints
// exist, none Ready" as a rollout in progress; checkPods treated the very same
// pods as a hard failure, because PodIsStarting deliberately excludes
// Running-but-not-Ready (a readiness probe that NEVER passes is the commonest
// broken workload there is). Both readings are right about different pods and
// wrong about each other's, and the disagreement left a workload whose probe
// legitimately takes more than a poll apart — keycloak, harbor-core — still
// tripping converge's hard-failed-twice abort. That is the incident this whole
// change set exists to prevent, surviving in the check that reported it.
//
// A NEVER-RESTARTED container is the discriminator. A container that is Running
// with restartCount 0 and no failure waiting-reason has simply not answered its
// probe yet; one that has restarted is cycling, which is a verdict. Callers gate
// this on health.Budgeted, so it only ever softens a bounded poll — steady-state
// health still fails a workload that is not Ready.
func PodIsWarmingUp(s PodStatus) bool {
	if s.Phase != "Running" {
		return false
	}
	notReady := false
	for _, c := range append(append([]ContainerStatus{}, s.InitContainerStatuses...), s.ContainerStatuses...) {
		if c.Ready {
			continue
		}
		if c.RestartCount > 0 {
			return false // cycling, not warming up
		}
		if c.State.Waiting != nil && !waitReasonIsStartup(c.State.Waiting.Reason) {
			return false
		}
		notReady = true
	}
	return notReady
}
