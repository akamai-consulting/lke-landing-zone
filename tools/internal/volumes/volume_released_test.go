package volumes

import (
	"encoding/json"
	"strings"
	"testing"
)

// pvJSON builds a PV list with the given (phase, namespace, pvc) triples, all on
// the Linode CSI driver.
func pvJSON(t *testing.T, rows [][3]string) map[string]any {
	t.Helper()
	var items []string
	for i, r := range rows {
		items = append(items, `{"spec":{"csi":{"driver":"`+linodeCSIDriver+`","volumeHandle":"`+
			string(rune('1'+i))+`0000-pvc-x"},"claimRef":{"namespace":"`+r[1]+`","name":"`+r[2]+
			`"}},"status":{"phase":"`+r[0]+`"}}`)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(`{"items":[`+strings.Join(items, ",")+`]}`), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// THE bug this closes. A Released PV keeps its claimRef, so a claimRef-only test
// admits every leaked PV from every previous incarnation of a StatefulSet pod.
// They all derive the SAME desired label, and Linode Volume labels are
// account-unique — so the first rename wins and every other one fails
// duplicate-label forever. The relabeler must not attempt them at all.
func TestRelabelerSkipsNonBoundPVs(t *testing.T) {
	got := linodeCSIVolumes(pvJSON(t, [][3]string{
		{"Bound", "monitoring", "storage-loki-0"},
		{"Released", "monitoring", "storage-loki-0"},
		{"Released", "monitoring", "storage-loki-0"},
		{"Available", "monitoring", "storage-loki-0"},
	}))
	if len(got) != 1 {
		t.Fatalf("only the BOUND PV may be relabeled, got %d — the others share its derived label "+
			"and would fail duplicate-label forever", len(got))
	}
	if got[0].pvcName != "storage-loki-0" {
		t.Errorf("wrong PV selected: %+v", got[0])
	}
}

// A PV list with no phase (older shapes / fixtures) must still be relabeled rather
// than silently skipped — absence of evidence is not Released.
func TestRelabelerTreatsMissingPhaseAsEligible(t *testing.T) {
	var out map[string]any
	if err := json.Unmarshal([]byte(`{"items":[{"spec":{"csi":{"driver":"`+linodeCSIDriver+
		`","volumeHandle":"90000-pvc-x"},"claimRef":{"namespace":"ns","name":"p"}}}]}`), &out); err != nil {
		t.Fatal(err)
	}
	if got := linodeCSIVolumes(out); len(got) != 1 {
		t.Errorf("a PV with no status.phase must remain eligible, got %d", len(got))
	}
}

// The assert must agree with the relabeler: it may not demand a readable label on
// a PV the relabeler will never touch.
func TestAssertDoesNotDemandLabelsOnReleasedPVs(t *testing.T) {
	pvs := parsePVVolumes(pvJSON(t, [][3]string{
		{"Bound", "monitoring", "storage-loki-0"},
		{"Released", "monitoring", "storage-loki-0"},
	}))
	if len(pvs) != 2 {
		t.Fatalf("the assert still sees both PVs (tags + encryption apply to any phase), got %d", len(pvs))
	}
	var bound, released *pvVolume
	for i := range pvs {
		if pvs[i].Phase == "Bound" {
			bound = &pvs[i]
		} else {
			released = &pvs[i]
		}
	}
	if bound == nil || released == nil {
		t.Fatal("phase was not parsed off the PV")
	}
	if released.Phase != "Released" {
		t.Errorf("phase = %q, want Released", released.Phase)
	}
}
