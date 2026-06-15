package terraform

import (
	"reflect"
	"testing"
)

func TestClusterBackedAddrs(t *testing.T) {
	stateList := `helm_release.apl
kubectl_manifest.apl_operator_namespace
kubernetes_namespace.foo
null_resource.cleanup
random_password.bootstrap
local_file.kubeconfig
kubectl_manifest.platform_app_storage_class
`
	got := ClusterBackedAddrs(stateList)
	want := []string{
		"helm_release.apl",
		"kubectl_manifest.apl_operator_namespace",
		"kubernetes_namespace.foo",
		"kubectl_manifest.platform_app_storage_class",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ClusterBackedAddrs = %v, want %v", got, want)
	}
	if a := ClusterBackedAddrs("null_resource.x\nlocal_file.y\n"); len(a) != 0 {
		t.Errorf("no cluster-backed addrs expected, got %v", a)
	}
}

func TestStateHas(t *testing.T) {
	sl := "helm_release.apl\nnull_resource.cleanup\n"
	if !StateHas(sl, "helm_release.apl") {
		t.Error("should find helm_release.apl")
	}
	if StateHas(sl, "helm_release.ap") {
		t.Error("must be an exact-line match, not a prefix")
	}
	if StateHas(sl, "kubectl_manifest.absent") {
		t.Error("absent address should not be found")
	}
}

func TestNoUsableKubeconfig(t *testing.T) {
	for _, raw := range []string{`""`, "null"} {
		if !NoUsableKubeconfig(raw) {
			t.Errorf("%q should be no-usable-kubeconfig (CASE B)", raw)
		}
	}
	for _, raw := range []string{`"apiVersion: v1..."`, "__CONSOLE_FAILED__", ""} {
		if NoUsableKubeconfig(raw) {
			t.Errorf("%q should NOT be treated as no-usable-kubeconfig", raw)
		}
	}
}

func TestClassifyKubeHost(t *testing.T) {
	if ClassifyKubeHost(KubeHostSentinel) != KubeHostGone {
		t.Error("sentinel host should classify as gone")
	}
	if ClassifyKubeHost("https://abc123.us-ord.linodelke.net:443") != KubeHostProbe {
		t.Error("a real https host should be probed")
	}
	for _, h := range []string{"", "garbage", "http://insecure"} {
		if ClassifyKubeHost(h) != KubeHostUnknown {
			t.Errorf("%q should classify as unknown (conservative)", h)
		}
	}
}

func TestAplCoreChain(t *testing.T) {
	if got := AplCoreChain(); len(got) != 4 || got[0] != "helm_release.apl" {
		t.Errorf("AplCoreChain = %v", got)
	}
}
