package volumes

import "testing"

// THE PARSE THE WHOLE GATE RESTS ON. The CSI volumeHandle is `<id>-<label>` and
// the label half is itself `pvc-<uuid>` — so it CONTAINS a dash, and the split
// has to take the FIRST one only. Splitting on the last (or on every) dash would
// yield a truncated label that never equals the live one, turning the drift check
// into a permanent false alarm on every healthy volume — the same shape of bug as
// the gate this replaced, just failing in the opposite direction.
func TestParsePVVolumesSplitsHandleOnTheFirstDashOnly(t *testing.T) {
	pvJSON := func(handle string) map[string]any {
		return map[string]any{"spec": map[string]any{
			"csi":      map[string]any{"driver": linodeCSIDriver, "volumeHandle": handle},
			"claimRef": map[string]any{"namespace": "monitoring", "name": "data-loki-ingester-0"},
		}}
	}

	for _, tc := range []struct {
		name, handle, wantID, wantLabel string
	}{
		{
			// The real shape, taken from a live PV.
			name:      "CSI default label contains its own dash",
			handle:    "17656487-pvc-5e9cdc9a98684924",
			wantID:    "17656487",
			wantLabel: "pvc-5e9cdc9a98684924",
		},
		{
			name:      "a label renamed by an older build keeps every dash",
			handle:    "17656487-e2e-monitoring-data-lok-gester-0",
			wantID:    "17656487",
			wantLabel: "e2e-monitoring-data-lok-gester-0",
		},
		{
			// No dash at all: the id is still usable, but there is no label to
			// compare against, so the drift check must stay silent rather than
			// invent one. TestJudgeVolumeCannotDriftWithoutAHandleLabel pins the
			// other half of that contract.
			name:      "a handle with no dash yields no label",
			handle:    "17656487",
			wantID:    "17656487",
			wantLabel: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePVVolumes(map[string]any{"items": []any{pvJSON(tc.handle)}})
			if len(got) != 1 {
				t.Fatalf("parsePVVolumes returned %d volumes, want 1", len(got))
			}
			if got[0].VolumeID != tc.wantID {
				t.Errorf("VolumeID = %q, want %q", got[0].VolumeID, tc.wantID)
			}
			if got[0].HandleLabel != tc.wantLabel {
				t.Errorf("HandleLabel = %q, want %q", got[0].HandleLabel, tc.wantLabel)
			}
		})
	}
}

// A PV whose handle carried no label half cannot be judged for drift — there is
// nothing to compare the live label against. Reporting one anyway would flag
// healthy volumes as unmountable, which is the expensive direction to be wrong in:
// the remedy it implies is recreating the PVC.
func TestJudgeVolumeCannotDriftWithoutAHandleLabel(t *testing.T) {
	noHandle := encPVHandle("harbor", "data-harbor-redis-0", "17094415", "")
	v := judgeVolume(noHandle, labelledVol("anything-at-all"), wantTags, "")
	if v.BadLabel != "" {
		t.Errorf("no handle label means no drift verdict, got %q", v.BadLabel)
	}
}

// An empty LIVE label is still a violation in its own right, and separately from
// drift: a Volume with no label has no by-id path at all.
func TestJudgeVolumeFlagsAnEmptyLiveLabel(t *testing.T) {
	born := encPVHandle("harbor", "data-harbor-redis-0", "17094415", "pvc-e2e495ebe1504dff")
	v := judgeVolume(born, labelledVol(""), wantTags, "")
	if v.BadLabel == "" {
		t.Fatal("a Volume with no label must be flagged")
	}
	if v.ok() {
		t.Error("an unlabelled Volume cannot be ok()")
	}
}
