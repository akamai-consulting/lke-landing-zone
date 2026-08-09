package clusteraccess

import (
	"path/filepath"
	"strings"
	"testing"
)

// A non-empty but undecodable kubeconfig is a DIFFERENT failure from a cluster
// that has none yet: the first means the API answered with garbage, the second
// that the cluster is not provisioned. Only the empty case was covered, so the
// two messages were interchangeable as far as the tests were concerned.
func TestFetchKubeconfigDistinguishesBadBase64FromMissing(t *testing.T) {
	fake := &fakeKubeconfigClient{kubeconfig: "!! not base64 !!"}
	withFakeKubeconfig(t, fake)
	out := filepath.Join(t.TempDir(), "kubeconfig")

	err := RunFetch(FetchOpts{Ref: ClusterRef{ClusterID: "5"}, Output: out})
	if err == nil {
		t.Fatal("undecodable kubeconfig must error")
	}
	if !strings.Contains(err.Error(), "not valid base64") {
		t.Errorf("err = %v, want the base64 decode error", err)
	}
	if strings.Contains(err.Error(), "empty kubeconfig") {
		t.Errorf("err = %v, must not be reported as a not-yet-provisioned cluster", err)
	}
}
