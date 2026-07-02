package health

import (
	"encoding/json"
	"testing"
)

func TestVerdictAndExitCode(t *testing.T) {
	cases := []struct {
		name string
		r    Report
		want Verdict
		exit int
	}{
		{"empty -> converged", Report{}, Converged, 0},
		{"drift only -> converged", Report{Drift: []string{"x"}}, Converged, 0},
		{"deferred only -> converged", Report{Deferred: []string{"dns token"}}, Converged, 0},
		{"pending -> in-progress", Report{Pending: []string{"cert issuing"}}, InProgress, 2},
		{"failed dominates pending", Report{Failed: []string{"crashloop"}, Pending: []string{"issuing"}}, HardFailed, 1},
		{"failed dominates deferred", Report{Failed: []string{"x"}, Deferred: []string{"y"}}, HardFailed, 1},
		{"pending dominates deferred", Report{Pending: []string{"p"}, Deferred: []string{"d"}}, InProgress, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.r.Verdict(); got != c.want {
				t.Errorf("Verdict = %v, want %v", got, c.want)
			}
			if got := c.r.ExitCode(); got != c.exit {
				t.Errorf("ExitCode = %d, want %d", got, c.exit)
			}
		})
	}
}

func TestReportAccumulates(t *testing.T) {
	var r Report
	r.AddFail("a")
	r.AddPending("b")
	r.AddDeferred("c")
	r.AddDrift("d")
	if len(r.Failed) != 1 || len(r.Pending) != 1 || len(r.Deferred) != 1 || len(r.Drift) != 1 {
		t.Fatalf("buckets not accumulated: %+v", r)
	}
	if r.Verdict() != HardFailed {
		t.Error("a report with a failure must be HardFailed")
	}
}

func TestPodIsFailing(t *testing.T) {
	running2of2 := PodStatus{Phase: "Running", ContainerStatuses: []ContainerStatus{{Ready: true}, {Ready: true}}}
	running1of2 := PodStatus{Phase: "Running", ContainerStatuses: []ContainerStatus{{Ready: true}, {Ready: false}}}
	cases := []struct {
		name string
		s    PodStatus
		want bool
	}{
		{"succeeded job pod -> ok", PodStatus{Phase: "Succeeded"}, false},
		{"running all ready -> ok", running2of2, false},
		{"running not all ready -> failing", running1of2, true},
		{"pending -> failing", PodStatus{Phase: "Pending"}, true},
		{"running zero statuses -> ok", PodStatus{Phase: "Running"}, false},
	}
	for _, c := range cases {
		if got := PodIsFailing(c.s); got != c.want {
			t.Errorf("%s: PodIsFailing = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPodConfigPending(t *testing.T) {
	waiting := func(reason string) ContainerState { return ContainerState{Waiting: &StateDetail{Reason: reason}} }
	term := func(reason string) ContainerState { return ContainerState{Terminated: &StateDetail{Reason: reason}} }
	running := ContainerState{Running: &struct{}{}}
	cases := []struct {
		name string
		s    PodStatus
		want bool
	}{
		// The failure this fixes: loki-0 / harbor-registry stranded on an ESO secret.
		{"registry waiting on secret + sibling running -> pending", PodStatus{
			Phase:                 "Pending",
			InitContainerStatuses: []ContainerStatus{{Name: "istio-init", State: term("Completed")}},
			ContainerStatuses: []ContainerStatus{
				{Name: "registry", State: waiting("CreateContainerConfigError")},
				{Name: "registryctl", Ready: true, State: running},
			},
		}, true},
		{"CreateContainerError -> pending", PodStatus{
			Phase:             "Pending",
			ContainerStatuses: []ContainerStatus{{Name: "c", State: waiting("CreateContainerError")}},
		}, true},
		// Terminal reasons must stay hard-fail even beside a transient sibling.
		{"crashloop -> not pending", PodStatus{
			Phase:             "Running",
			ContainerStatuses: []ContainerStatus{{Name: "c", State: waiting("CrashLoopBackOff")}},
		}, false},
		{"imagepull beside config-error -> not pending", PodStatus{
			Phase: "Pending",
			ContainerStatuses: []ContainerStatus{
				{Name: "a", State: waiting("CreateContainerConfigError")},
				{Name: "b", State: waiting("ImagePullBackOff")},
			},
		}, false},
		{"terminated with Error -> not pending", PodStatus{
			Phase: "Pending",
			ContainerStatuses: []ContainerStatus{
				{Name: "a", State: waiting("CreateContainerConfigError")},
				{Name: "b", State: term("Error")},
			},
		}, false},
		// No transient signal -> leave the verdict to the caller (hard-fail).
		{"plain ContainerCreating -> not pending", PodStatus{
			Phase:             "Pending",
			ContainerStatuses: []ContainerStatus{{Name: "c", State: waiting("ContainerCreating")}},
		}, false},
		{"pending with no statuses -> not pending", PodStatus{Phase: "Pending"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PodConfigPending(c.s); got != c.want {
				t.Errorf("PodConfigPending = %v, want %v", got, c.want)
			}
		})
	}
}

func TestIsJobControlled(t *testing.T) {
	cases := []struct {
		name string
		refs []OwnerRef
		want bool
	}{
		{"no owners", nil, false},
		{"job-owned (CronJob pod)", []OwnerRef{{Kind: "Job"}}, true},
		{"replicaset-owned (Deployment pod)", []OwnerRef{{Kind: "ReplicaSet"}}, false},
		{"statefulset-owned", []OwnerRef{{Kind: "StatefulSet"}}, false},
		{"job among several", []OwnerRef{{Kind: "ReplicaSet"}, {Kind: "Job"}}, true},
	}
	for _, c := range cases {
		if got := IsJobControlled(c.refs); got != c.want {
			t.Errorf("%s: IsJobControlled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSummarizeStatesAndRatio(t *testing.T) {
	// Unmarshal a realistic kubectl pod-status fragment to exercise the json tags.
	const raw = `{
      "phase": "Pending",
      "initContainerStatuses": [
        {"name": "init-vault", "ready": true, "state": {"terminated": {"reason": "Completed"}}}
      ],
      "containerStatuses": [
        {"name": "app", "ready": false, "restartCount": 7, "state": {"waiting": {"reason": "ImagePullBackOff"}}},
        {"name": "sidecar", "ready": true, "state": {"running": {}}}
      ]
    }`
	var s PodStatus
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !PodIsFailing(s) {
		t.Error("pending pod should be failing")
	}
	if got := ReadyRatio(s); got != "1/2" {
		t.Errorf("ReadyRatio = %q, want 1/2", got)
	}
	want := "init/init-vault:terminated:Completed,main/app:waiting:ImagePullBackOff,main/sidecar:running"
	if got := SummarizeStates(s); got != want {
		t.Errorf("SummarizeStates = %q\n             want %q", got, want)
	}
	if got := FlappingContainers(s, 5); got != "app=7" {
		t.Errorf("FlappingContainers = %q, want app=7", got)
	}
	if got := FlappingContainers(s, 10); got != "" {
		t.Errorf("FlappingContainers(threshold 10) = %q, want empty", got)
	}
}

func TestSummarizeStatesMissingReason(t *testing.T) {
	s := PodStatus{ContainerStatuses: []ContainerStatus{{Name: "c", State: ContainerState{Waiting: &StateDetail{}}}}}
	if got := SummarizeStates(s); got != "main/c:waiting:?" {
		t.Errorf("missing reason should render '?', got %q", got)
	}
}
