package health

import (
	"encoding/json"
	"testing"
)

func TestNodeHealthyAndTaints(t *testing.T) {
	const raw = `{
      "metadata": {"name": "lke-node-1"},
      "spec": {"taints": [
        {"key": "node-role.kubernetes.io/control-plane", "effect": "NoSchedule"},
        {"key": "node.kubernetes.io/unreachable", "effect": "NoExecute"},
        {"key": "dedicated", "value": "gpu", "effect": "NoSchedule"},
        {"key": "informational", "effect": "PreferNoSchedule"}
      ]},
      "status": {"conditions": [
        {"type": "Ready", "status": "True"},
        {"type": "MemoryPressure", "status": "False"},
        {"type": "DiskPressure", "status": "False"},
        {"type": "PIDPressure", "status": "False"}
      ]}
    }`
	var n Node
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatal(err)
	}
	if ok, ready, _, _, _ := NodeHealthy(n); !ok || ready != "True" {
		t.Errorf("node should be healthy (ok=%v ready=%q)", ok, ready)
	}
	// Only the operator taint (dedicated=gpu:NoSchedule) is unexpected — the
	// k8s-managed ones and the PreferNoSchedule one are excluded.
	tt := UnexpectedTaints(n)
	if len(tt) != 1 || tt[0].Key != "dedicated" || tt[0].Value != "gpu" {
		t.Errorf("UnexpectedTaints = %+v, want [dedicated=gpu]", tt)
	}

	// A node under memory pressure (or missing the Ready condition) is unhealthy.
	pressured := Node{}
	pressured.Status.Conditions = []NodeCondition{{Type: "Ready", Status: "True"}, {Type: "MemoryPressure", Status: "True"}}
	if ok, _, mem, _, _ := NodeHealthy(pressured); ok || mem != "True" {
		t.Errorf("memory-pressured node should be unhealthy")
	}
	missing := Node{}
	if ok, ready, _, _, _ := NodeHealthy(missing); ok || ready != "?" {
		t.Errorf("node with no conditions: ok=%v ready=%q, want false/?", ok, ready)
	}
}

func TestNamespaceTerminating(t *testing.T) {
	if !NamespaceTerminating("Terminating") {
		t.Error("Terminating phase should be flagged")
	}
	if NamespaceTerminating("Active") {
		t.Error("Active phase must not be flagged")
	}
}

func TestAPIServiceUnavailable(t *testing.T) {
	good := APIService{}
	good.Status.Conditions = []APIServiceCondition{{Type: "Available", Status: "True"}}
	if bad, _ := APIServiceUnavailable(good); bad {
		t.Error("Available=True should not be flagged")
	}
	down := APIService{}
	down.Status.Conditions = []APIServiceCondition{{Type: "Available", Status: "False", Reason: "MissingEndpoints", Message: "no backing pods"}}
	bad, msg := APIServiceUnavailable(down)
	if !bad || msg != "MissingEndpoints: no backing pods" {
		t.Errorf("APIServiceUnavailable = (%v, %q)", bad, msg)
	}
}

func TestDefaultStorageClasses(t *testing.T) {
	const raw = `[
      {"metadata": {"name": "block-storage-retain", "annotations": {"storageclass.kubernetes.io/is-default-class": "true"}}},
      {"metadata": {"name": "linode-block-storage", "annotations": {}}},
      {"metadata": {"name": "other-default", "annotations": {"storageclass.kubernetes.io/is-default-class": "true"}}}
    ]`
	var classes []StorageClass
	if err := json.Unmarshal([]byte(raw), &classes); err != nil {
		t.Fatal(err)
	}
	def := DefaultStorageClasses(classes)
	if len(def) != 2 {
		t.Fatalf("DefaultStorageClasses = %v, want 2 (the misconfig case)", def)
	}
	// Single-default is the healthy case.
	one := DefaultStorageClasses(classes[:1])
	if len(one) != 1 || one[0] != "block-storage-retain" {
		t.Errorf("single default = %v", one)
	}
}

func TestRequiredSetsNonEmpty(t *testing.T) {
	if len(RequiredCRDs()) == 0 || len(RequiredStorageClasses()) == 0 {
		t.Error("required CRD / StorageClass sets must be non-empty")
	}
}
