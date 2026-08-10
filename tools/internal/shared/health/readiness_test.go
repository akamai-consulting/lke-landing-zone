package health

import "testing"

func TestLokiPodReady(t *testing.T) {
	ready := PodStatus{Phase: "Running", ContainerStatuses: []ContainerStatus{{Ready: true}, {Ready: true}}}
	if !LokiPodReady(ready) {
		t.Error("Running + all containers ready should be ready")
	}
	if LokiPodReady(PodStatus{Phase: "Running", ContainerStatuses: []ContainerStatus{{Ready: true}, {Ready: false}}}) {
		t.Error("a not-ready container makes the pod not ready")
	}
	if LokiPodReady(PodStatus{Phase: "Pending"}) {
		t.Error("Pending phase is not ready")
	}
	if !LokiPodReady(PodStatus{Phase: "Succeeded"}) {
		t.Error("Succeeded with no containers is ready")
	}
}

func TestLokiConfigUsesS3(t *testing.T) {
	s3 := `
storage_config:
  aws:
    s3: s3://platform-loki-primary
    bucketnames: platform-loki-primary
    endpoint: us-ord-1.linodeobjects.com
  object_store: s3
`
	if !LokiConfigUsesS3(s3) {
		t.Error("an s3-backed config should be detected")
	}
	// Filesystem default: mentions storage but no s3 marker / no "s3" token.
	fs := `
storage_config:
  filesystem:
    directory: /var/loki/chunks
  object_store: filesystem
`
	if LokiConfigUsesS3(fs) {
		t.Error("a filesystem-default config must NOT be detected as s3")
	}
	// A stray "s3" mention without a storage marker is not enough — needs both.
	if LokiConfigUsesS3("# this config is not about s3 at all\n") {
		t.Error("an s3 mention without a storage marker should not match")
	}
	if LokiConfigUsesS3("") {
		t.Error("empty config is not s3")
	}
}

// THIS TEST WAS AN EMPTY BODY, and the name is why it survived as one. It covered
// HarborDeployments/HarborStatefulSets, the pre-seed control-plane sets f0aa68f
// retired along with the workflow job that was their only consumer. The functions
// went; the test was hollowed out instead of deleted, leaving `func
// TestHarborWorkloadSets(t *testing.T) {}` — which passes unconditionally, counts
// toward the suite, and reads to anyone scanning for coverage as though the Harbor
// workload sets are tested.
//
// An empty test is worse than a missing one: a missing test is visibly missing.
// This is the vacuous-green shape the guards in this tree refuse, sitting in the
// test suite itself. TestNoTestFunctionHasAnEmptyBody now gates the class.
//
// So it tests what actually survived. The set is deliberately ONE deployment and
// the assertion says which, because assertobs/readiness.go derives its probe count
// from this length (ci_readiness_deadline_test.go asserts probes == len(...)), so a
// silent addition here changes a deadline calculation two packages away.
func TestHarborRegistryDeploymentsIsTheSeededSetOnly(t *testing.T) {
	got := HarborRegistryDeployments()
	want := []string{"harbor-registry"}
	if len(got) != len(want) {
		t.Fatalf("HarborRegistryDeployments() = %v, want %v — assertobs derives its "+
			"readiness probe count from this length", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("HarborRegistryDeployments()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// The retired sets must not come back by name without the gate that consumed
	// them: harbor-core and harbor-jobservice were the pre-seed control plane, and
	// nothing waits on them here any more.
	for _, d := range got {
		if d == "harbor-core" || d == "harbor-jobservice" {
			t.Errorf("%q is back in the seeded set — it belonged to the pre-seed gate "+
				"f0aa68f retired, and this set is specifically the workloads that depend "+
				"on the mid-bootstrap object-storage credentials", d)
		}
	}
}
