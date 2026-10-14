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

// A container that RUNS but never becomes Ready is the commonest broken-workload
// signal there is — a readiness probe that never passes. Reading it as "starting"
// would stop converge fast-failing on it and trade a precise verdict for a
// generic budget timeout.
func TestRunningButNeverReadyIsNotStarting(t *testing.T) {
	s := PodStatus{Phase: "Running", ContainerStatuses: []ContainerStatus{
		{Name: "main", Ready: false, State: ContainerState{Running: &struct{}{}}},
	}}
	if PodIsStarting(s) {
		t.Error("a Running-but-not-Ready container was softened to pending — that is a failing readiness " +
			"probe, and the gate must keep failing on it")
	}
}

// An UNSCHEDULABLE pod publishes no container statuses, so without reading
// conditions it is indistinguishable from a pod the kubelet has not reported on
// yet — and would read as starting forever. Waiting does not schedule a pod that
// nothing can schedule (node pool full, unsatisfiable taint/selector, unbound PVC).
func TestUnschedulableIsNotStarting(t *testing.T) {
	s := PodStatus{Phase: "Pending", Conditions: []Condition{
		{Type: "PodScheduled", Status: "False", Reason: "Unschedulable",
			Message: "0/5 nodes are available: insufficient cpu"},
	}}
	if PodIsStarting(s) {
		t.Error("an Unschedulable pod read as starting — the node pool being full would never fail the gate")
	}
	// ...while a pod merely awaiting its first status still does.
	if !PodIsStarting(PodStatus{Phase: "Pending", Conditions: []Condition{{Type: "PodScheduled", Status: "True"}}}) {
		t.Error("a scheduled pod with no statuses yet must still count as starting")
	}
}

// RUNNING MEANS OPPOSITE THINGS FOR AN INIT AND A MAIN CONTAINER, and flattening
// the two lists reintroduced the incident this file exists to prevent: a
// non-sidecar init container reports ready=false for its whole run, so a pod
// sitting in a perfectly normal `wait-for-db` init was recorded as a hard failure
// and aborted converge.
func TestARunningInitContainerIsANormalStartup(t *testing.T) {
	s := PodStatus{
		Phase: "Pending",
		InitContainerStatuses: []ContainerStatus{
			{Name: "wait-for-db", Ready: false, State: ContainerState{Running: &struct{}{}}},
		},
		ContainerStatuses: []ContainerStatus{waiting("main", "PodInitializing")},
	}
	if !PodIsStarting(s) {
		t.Error("a pod running its init container was judged broken — that is the incident this file " +
			"was written to fix, reintroduced by judging init and main containers alike")
	}
	// The MAIN container's Running-but-not-Ready must still be a failure, or the
	// separation has softened both instead of one.
	m := PodStatus{Phase: "Running", ContainerStatuses: []ContainerStatus{
		{Name: "main", Ready: false, State: ContainerState{Running: &struct{}{}}},
	}}
	if PodIsStarting(m) {
		t.Error("a main container Running without ever becoming Ready must still fail")
	}
}

// An init container that FAILED is still a failure, whatever it is doing now.
func TestAFailedInitContainerIsNotStarting(t *testing.T) {
	s := PodStatus{Phase: "Pending", InitContainerStatuses: []ContainerStatus{
		{Name: "migrate", State: ContainerState{Terminated: &StateDetail{Reason: "Error"}}},
	}}
	if PodIsStarting(s) {
		t.Error("an init container that terminated with an error read as starting")
	}
	s.InitContainerStatuses = []ContainerStatus{waiting("migrate", "CrashLoopBackOff")}
	if PodIsStarting(s) {
		t.Error("a CrashLoopBackOff init container read as starting")
	}
}

// A container status published with no waiting reason is no evidence either way —
// reasonOr() exists because that is observed in practice — and a blank field must
// not read as a broken workload.
func TestAnEmptyWaitingReasonIsNotAFailure(t *testing.T) {
	if !PodIsStarting(PodStatus{Phase: "Pending", ContainerStatuses: []ContainerStatus{waiting("main", "")}}) {
		t.Error("a container waiting with no reason was judged broken; the budget bounds it, a blank field should not condemn it")
	}
}

// The remaining shapes, so every arm of both per-container judgements is driven:
// an init container waiting on a startup reason, a main container that completed
// (a Job-shaped pod), and a status carrying no state at all.
func TestPodIsStartingCoversTheRemainingContainerShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    PodStatus
		want bool
	}{
		{"init container pulling its image", PodStatus{Phase: "Pending",
			InitContainerStatuses: []ContainerStatus{waiting("init", "ContainerCreating")}}, true},
		{"init container stuck on a bad image", PodStatus{Phase: "Pending",
			InitContainerStatuses: []ContainerStatus{waiting("init", "ImagePullBackOff")}}, false},
		{"init container already Ready (a sidecar)", PodStatus{Phase: "Pending",
			InitContainerStatuses: []ContainerStatus{{Name: "sidecar", Ready: true}},
			ContainerStatuses:     []ContainerStatus{waiting("main", "PodInitializing")}}, true},
		{"main container completed", PodStatus{Phase: "Running",
			ContainerStatuses: []ContainerStatus{{Name: "main",
				State: ContainerState{Terminated: &StateDetail{Reason: "Completed"}}}}}, true},
		{"status with no state reported yet", PodStatus{Phase: "Pending",
			ContainerStatuses: []ContainerStatus{{Name: "main"}}}, true},
	} {
		if got := PodIsStarting(tc.s); got != tc.want {
			t.Errorf("%s: PodIsStarting = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The last arms of the two per-container judgements: an init container waiting on
// a reason that waiting cannot fix, and a main container that terminated cleanly
// while its pod is still Pending.
func TestPerContainerJudgementsCoverTheirRemainingArms(t *testing.T) {
	if initContainerIsStarting(ContainerStatus{Name: "i", Ready: true}) != true {
		t.Error("a Ready init container (a sidecar) is fine")
	}
	if initContainerIsStarting(waiting("i", "ImagePullBackOff")) {
		t.Error("an init container that cannot pull its image is not starting")
	}
	if !initContainerIsStarting(ContainerStatus{Name: "i"}) {
		t.Error("an init container with no state reported yet is starting")
	}
	if mainContainerIsStarting(ContainerStatus{Name: "m",
		State: ContainerState{Terminated: &StateDetail{Reason: "Error"}}}) {
		t.Error("a main container that terminated with an error is not starting")
	}
	if !mainContainerIsStarting(ContainerStatus{Name: "m"}) {
		t.Error("a main container with no state reported yet is starting")
	}
}

// The pod and Service classifiers must agree about the SAME physical state.
// ClassifyServiceEndpoints reads "endpoints exist, none Ready" as a rollout;
// checkPods read those same pods as a hard failure, so a workload whose readiness
// probe legitimately takes longer than a poll apart (keycloak, harbor-core) still
// tripped the hard-failed-twice abort this change set exists to prevent.
func TestPodIsWarmingUpMatchesTheServiceClassifier(t *testing.T) {
	running := func(ready bool, restarts int) ContainerStatus {
		return ContainerStatus{Name: "main", Ready: ready, RestartCount: restarts,
			State: ContainerState{Running: &struct{}{}}}
	}
	if !PodIsWarmingUp(PodStatus{Phase: "Running", ContainerStatuses: []ContainerStatus{running(false, 0)}}) {
		t.Error("a Running, never-restarted container that has not answered its probe yet is warming up")
	}
	// A container that has RESTARTED is cycling — a verdict, not a wait.
	if PodIsWarmingUp(PodStatus{Phase: "Running", ContainerStatuses: []ContainerStatus{running(false, 3)}}) {
		t.Error("a restarting container was called warming up; restarts are what separate the two")
	}
	// All Ready is not warming up (it is simply fine), and neither is a pod that
	// has not reached Running.
	if PodIsWarmingUp(PodStatus{Phase: "Running", ContainerStatuses: []ContainerStatus{running(true, 0)}}) {
		t.Error("a fully Ready pod is not warming up")
	}
	if PodIsWarmingUp(PodStatus{Phase: "Pending", ContainerStatuses: []ContainerStatus{waiting("m", "ContainerCreating")}}) {
		t.Error("a Pending pod is PodIsStarting's case, not this one")
	}
	// And warming up must NOT swallow a real failure signal on a sibling.
	if PodIsWarmingUp(PodStatus{Phase: "Running", ContainerStatuses: []ContainerStatus{
		running(false, 0), waiting("side", "CrashLoopBackOff")}}) {
		t.Error("a CrashLoopBackOff sibling was masked by a warming-up container")
	}
}

// TestPodBlockedReason_PartitionsWaitingReasons pins PodBlockedReason as the
// exact complement of startingWaitReasons: every startup reason is NOT blocked,
// and every "Kubernetes tried and cannot proceed" reason IS. The two share one
// set, so a reason added to one and not the other changes the meaning of both.
func TestPodBlockedReason_PartitionsWaitingReasons(t *testing.T) {
	waiting := func(reason string) PodStatus {
		return PodStatus{Phase: "Running", ContainerStatuses: []ContainerStatus{
			{Name: "openbao", State: ContainerState{Waiting: &StateDetail{Reason: reason}}}}}
	}
	for reason := range startingWaitReasons {
		if got := PodBlockedReason(waiting(reason)); got != "" {
			t.Errorf("PodBlockedReason(%s) = %q, want \"\" — it is a startup reason", reason, got)
		}
	}
	for _, reason := range []string{
		"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull",
		"CreateContainerConfigError", "CreateContainerError", "InvalidImageName",
	} {
		if got := PodBlockedReason(waiting(reason)); got != "openbao:"+reason {
			t.Errorf("PodBlockedReason(%s) = %q, want %q", reason, got, "openbao:"+reason)
		}
	}
}

// TestPodBlockedReason_IgnoresReadiness is the property that lets `wait-pods` use
// this on OpenBao pods that CANNOT be Ready before `bao operator init` runs.
func TestPodBlockedReason_IgnoresReadiness(t *testing.T) {
	running := PodStatus{Phase: "Running", ContainerStatuses: []ContainerStatus{
		{Name: "openbao", Ready: false, State: ContainerState{Running: &struct{}{}}}}}
	if got := PodBlockedReason(running); got != "" {
		t.Errorf("PodBlockedReason(running, ready=false) = %q, want \"\" — not-Ready is not wedged", got)
	}
	// An init container that has given up is still the pod's verdict.
	initBlocked := PodStatus{Phase: "Pending", InitContainerStatuses: []ContainerStatus{
		{Name: "init", State: ContainerState{Waiting: &StateDetail{Reason: "ImagePullBackOff"}}}}}
	if got := PodBlockedReason(initBlocked); got != "init:ImagePullBackOff" {
		t.Errorf("PodBlockedReason(init blocked) = %q, want init:ImagePullBackOff", got)
	}
}
