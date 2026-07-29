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

func TestClassifyBlockStorageParams(t *testing.T) {
	// tally counts findings by category.
	tally := func(fs []StorageClassParamFinding) (fails, warns int) {
		for _, f := range fs {
			switch f.Cat {
			case CatFail:
				fails++
			case CatWarn:
				warns++
			}
		}
		return
	}
	mk := func(prov string, params map[string]string) StorageClass {
		var sc StorageClass
		sc.Provisioner = prov
		sc.Parameters = params
		return sc
	}
	good := "linodebs.csi.linode.com"

	// Healthy: right provisioner, encrypted=true, volumeTags with sweep + lke tags.
	if fails, warns := tally(ClassifyBlockStorageParams(mk(good, map[string]string{
		"linodebs.csi.linode.com/encrypted":  "true",
		"linodebs.csi.linode.com/volumeTags": "block-storage,platform-support-services,lke12345",
	}))); fails != 0 || warns != 0 {
		t.Errorf("healthy class: fails=%d warns=%d, want 0/0", fails, warns)
	}

	// The silent-ignore footgun: misspelled keys are present, honored keys absent.
	// Both the encryption and volumeTags misspellings must hard-fail, plus the
	// honored keys being unset.
	if fails, _ := tally(ClassifyBlockStorageParams(mk(good, map[string]string{
		"linodebs.csi.linode.com/encryption":  "enabled",
		"linodebs.csi.linode.com/volume-tags": "block-storage,lke12345",
	}))); fails < 3 {
		t.Errorf("misspelled keys: fails=%d, want >=3 (ignored-encryption + ignored-volume-tags + unset honored keys)", fails)
	}

	// Wrong provisioner is a hard fail.
	if fails, _ := tally(ClassifyBlockStorageParams(mk("csi.other.io", map[string]string{
		"linodebs.csi.linode.com/encrypted":  "true",
		"linodebs.csi.linode.com/volumeTags": "block-storage,lke1",
	}))); fails < 1 {
		t.Error("wrong provisioner should fail")
	}

	// Canonical class with the sweep tag but no lke<id> ownership tag → FAIL: with
	// the labeler gone, a missing provision-time tag has no backfill, so attribution
	// is broken (params are immutable — the class must be recreated).
	if fails, _ := tally(ClassifyBlockStorageParams(mk(good, map[string]string{
		"linodebs.csi.linode.com/encrypted":  "true",
		"linodebs.csi.linode.com/volumeTags": "block-storage,platform-support-services",
	}))); fails < 1 {
		t.Error("canonical class with no lke<id> ownership tag should fail")
	}

	// A MALFORMED lke-prefixed tag (not `lke<digits>`) must FAIL, not pass: reap's
	// attribution parser (`^lke-?[0-9]+$`) rejects it, so the audit must agree —
	// a loose "lke" prefix match would wrongly green-light an unattributable class.
	if fails, _ := tally(ClassifyBlockStorageParams(mk(good, map[string]string{
		"linodebs.csi.linode.com/encrypted":  "true",
		"linodebs.csi.linode.com/volumeTags": "block-storage,platform-support-services,lke-foo",
	}))); fails < 1 {
		t.Error("canonical class with a malformed lke tag (lke-foo) should fail — reap can't attribute it")
	}
}

func TestAuditLinodeStorageClasses(t *testing.T) {
	tally := func(fs []StorageClassParamFinding) (fails, warns int) {
		for _, f := range fs {
			switch f.Cat {
			case CatFail:
				fails++
			case CatWarn:
				warns++
			}
		}
		return
	}
	sc := func(name, prov string, params map[string]string) StorageClass {
		var c StorageClass
		c.Metadata.Name = name
		c.Provisioner = prov
		c.Parameters = params
		return c
	}
	lb := "linodebs.csi.linode.com"

	classes := []StorageClass{
		// Canonical — healthy, no findings.
		sc("block-storage-retain", lb, map[string]string{
			"linodebs.csi.linode.com/encrypted":  "true",
			"linodebs.csi.linode.com/volumeTags": "block-storage,platform-support-services,lke12345",
		}),
		// LKE stock — untagged by design, acknowledged (no fail/warn).
		sc("linode-block-storage", lb, nil),
		sc("linode-block-storage-retain", lb, nil),
		// A non-linodebs class — ignored entirely.
		sc("efs", "efs.csi.aws.com", nil),
		// A rogue custom linodebs class with no lke<id> tag — the bleed we broadened
		// the check to catch: WARN.
		sc("team-fast-ssd", lb, map[string]string{
			"linodebs.csi.linode.com/encrypted": "true",
		}),
	}
	fails, warns := tally(AuditLinodeStorageClasses(classes))
	if fails != 0 {
		t.Errorf("unexpected fails=%d (healthy canonical + acknowledged stock + warned custom)", fails)
	}
	if warns != 1 {
		t.Errorf("warns=%d, want 1 (the untagged custom linodebs class)", warns)
	}

	// A custom linodebs class that DOES carry an lke tag is fine (no warn).
	if _, warns := tally(AuditLinodeStorageClasses([]StorageClass{
		sc("team-tagged", lb, map[string]string{"linodebs.csi.linode.com/volumeTags": "lke999"}),
	})); warns != 0 {
		t.Errorf("tagged custom class: warns=%d, want 0", warns)
	}

	// A custom class whose lke tag is MALFORMED (`lkexyz`, not `lke<digits>`) is
	// unattributable by reap, so it must WARN — the loose-prefix bug would have
	// silently OK'd it.
	if _, warns := tally(AuditLinodeStorageClasses([]StorageClass{
		sc("team-badtag", lb, map[string]string{"linodebs.csi.linode.com/volumeTags": "lkexyz"}),
	})); warns != 1 {
		t.Errorf("custom class with malformed lke tag: warns=%d, want 1", warns)
	}

	// A broken canonical class (misspelled keys) still hard-fails through the audit.
	if fails, _ := tally(AuditLinodeStorageClasses([]StorageClass{
		sc("block-storage-retain", lb, map[string]string{"linodebs.csi.linode.com/volume-tags": "block-storage"}),
	})); fails < 1 {
		t.Error("broken canonical class should fail through AuditLinodeStorageClasses")
	}
}
