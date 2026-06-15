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

func TestHarborWorkloadSets(t *testing.T) {
	if len(HarborDeployments()) != 4 || HarborDeployments()[0] != "harbor-core" {
		t.Errorf("HarborDeployments = %v", HarborDeployments())
	}
	if len(HarborStatefulSets()) != 2 {
		t.Errorf("HarborStatefulSets = %v", HarborStatefulSets())
	}
}
