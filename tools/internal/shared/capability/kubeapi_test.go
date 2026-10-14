package capability_test

// kubeapi_test.go — the in-cluster REST fence, tested the way every other handle
// here is: prove the REFUSAL reaches no transport, not just that the permit works.

import (
	"context"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// fakeKube records what reached the transport. Reaching it at all is the failure
// the denied paths are looking for.
type fakeKube struct{ got []string }

func (f *fakeKube) GetJSON(_ context.Context, p string) (map[string]any, int, error) {
	f.got = append(f.got, "get "+p)
	return map[string]any{}, 200, nil
}
func (f *fakeKube) CreateJSON(_ context.Context, p string, _ any) (int, error) {
	f.got = append(f.got, "create "+p)
	return 201, nil
}
func (f *fakeKube) MergePatch(_ context.Context, p string, _ any) error {
	f.got = append(f.got, "patch "+p)
	return nil
}

func kubeBinding(gs ...extension.Grant) extension.Binding {
	return extension.Binding{Kind: extension.Invariant, State: extension.Operating, Grants: gs}
}

// A READ-ONLY BINDING MUST NOT REACH THE TRANSPORT TO MUTATE. Refusing at the
// handle rather than at the apiserver is the point: the apiserver would accept it,
// because the daemon's ServiceAccount is permitted far more than any one lane's
// declaration.
func TestAReadOnlyBindingCannotMutateThroughTheAPI(t *testing.T) {
	f := &fakeKube{}
	k := capability.KubeFor(kubeBinding(extension.ClusterRead), f)

	if _, err := k.CreateJSON(context.Background(), "/api/v1/namespaces/x/configmaps", nil); err == nil {
		t.Error("CreateJSON succeeded on a cluster-read binding")
	}
	if err := k.MergePatch(context.Background(), "/apis/apps/v1/deployments/x", nil); err == nil {
		t.Error("MergePatch succeeded on a cluster-read binding")
	}
	if len(f.got) != 0 {
		t.Errorf("the transport was reached anyway: %v — a refusal that still sends the "+
			"request is not a fence", f.got)
	}
	if _, _, err := k.GetJSON(context.Background(), "/api/v1/nodes"); err != nil {
		t.Errorf("GetJSON refused on a cluster-read binding: %v", err)
	}
}

// cluster-write implies read, the same implication Writer makes and for the same
// reason: every mutating lane reads back what it wrote.
func TestAWriteBindingReadsAndMutatesThroughTheAPI(t *testing.T) {
	f := &fakeKube{}
	k := capability.KubeFor(kubeBinding(extension.ClusterWrite), f)
	if _, _, err := k.GetJSON(context.Background(), "/api/v1/nodes"); err != nil {
		t.Errorf("GetJSON refused with cluster-write: %v", err)
	}
	if err := k.MergePatch(context.Background(), "/p", nil); err != nil {
		t.Errorf("MergePatch refused with cluster-write: %v", err)
	}
	if len(f.got) != 2 {
		t.Errorf("transport saw %v, want both calls through", f.got)
	}
}

// NO CLUSTER GRANT YIELDS A REFUSING HANDLE, NOT NIL — the rule every handle here
// follows, so a mistake is a permission error at the call site rather than a panic
// three frames away.
func TestNoClusterGrantYieldsARefusingKubeHandleNotNil(t *testing.T) {
	f := &fakeKube{}
	k := capability.KubeFor(kubeBinding(extension.ReadRepo), f)
	if k == nil {
		t.Fatal("KubeFor returned nil; it must return a refusing handle")
	}
	if _, _, e := k.GetJSON(context.Background(), "/x"); e == nil {
		t.Error("GetJSON worked with no cluster grant")
	}
	if _, e := k.CreateJSON(context.Background(), "/x", nil); e == nil {
		t.Error("CreateJSON worked with no cluster grant")
	}
	if e := k.MergePatch(context.Background(), "/x", nil); e == nil {
		t.Error("MergePatch worked with no cluster grant")
	}
	if len(f.got) != 0 {
		t.Errorf("the transport was reached with no cluster grant: %v", f.got)
	}
}

// THE REFUSAL MUST NAME THE PATH. A lane's failure is about one object, and
// "cannot patch" without saying what sends someone to the wrong file.
func TestTheKubeRefusalNamesWhatWasAttempted(t *testing.T) {
	k := capability.KubeFor(kubeBinding(extension.ClusterRead), &fakeKube{})
	err := k.MergePatch(context.Background(), "/apis/storage.k8s.io/v1/storageclasses/block", nil)
	if err == nil {
		t.Fatal("no refusal")
	}
	for _, want := range []string{"storageclasses/block", string(extension.ClusterWrite)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
}

// DeniedKube is exported so a caller with no binding can be explicit rather than
// passing nil, and it must actually refuse.
func TestDeniedKubeIsExportedAndRefuses(t *testing.T) {
	if err := capability.DeniedKube().MergePatch(context.Background(), "/x", nil); err == nil {
		t.Error("DeniedKube permitted a patch")
	}
}

// A DIFFERENT MUTATING GRANT BUYS NOTHING HERE. cloud-mutate deletes clusters and
// is the most dangerous grant in the vocabulary; it still must not open the
// in-cluster API, or the grants would stop being distinct.
func TestAnotherMutatingGrantDoesNotOpenTheClusterAPI(t *testing.T) {
	f := &fakeKube{}
	k := capability.KubeFor(kubeBinding(extension.CloudMutate), f)
	if _, err := k.CreateJSON(context.Background(), "/x", nil); err == nil {
		t.Error("cloud-mutate bought in-cluster API access")
	}
	if len(f.got) != 0 {
		t.Errorf("transport reached: %v", f.got)
	}
}
