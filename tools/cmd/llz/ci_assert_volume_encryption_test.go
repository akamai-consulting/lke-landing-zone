package main

import (
	"os"
	"strings"
	"testing"
)

var wantTags = []string{"block-storage", "platform-support-services", "lke637888"}

func encVol(encryption string, tags ...string) map[string]any {
	t := make([]any, 0, len(tags))
	for _, s := range tags {
		t = append(t, s)
	}
	return map[string]any{"label": "e2e-harbor-data-harbor-redis-0", "encryption": encryption, "tags": t}
}

func encPV(ns, claim, id string) pvVolume { return pvVolume{VolumeID: id, Namespace: ns, PVC: claim} }

// TestJudgeVolume_Encryption covers the primary gate. `encryption` absent (a Volume
// predating the feature) must read as NOT encrypted — the safe bias, and the shape
// the Linode API actually returns for older volumes.
func TestJudgeVolume_Encryption(t *testing.T) {
	cases := []struct {
		name       string
		encryption string
		wantOK     bool
	}{
		{"enabled passes", "enabled", true},
		{"disabled fails", "disabled", false},
		{"absent fails", "", false},
		{"unknown value fails", "pending", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := judgeVolume(encPV("harbor", "data-harbor-redis-0", "17094415"), encVol(tc.encryption, wantTags...), wantTags, "")
			if v.ok() != tc.wantOK {
				t.Fatalf("encryption=%q → ok=%v, want %v (problem: %s)", tc.encryption, v.ok(), tc.wantOK, v.problem())
			}
			if !tc.wantOK && !strings.Contains(v.problem(), "NOT ENCRYPTED") {
				t.Errorf("an unencrypted Volume must report NOT ENCRYPTED, got %q", v.problem())
			}
		})
	}
}

// TestJudgeVolume_Tagging is the tagging half of the invariant, and it is a
// SEPARATE failure from encryption on purpose: an encrypted-but-untagged Volume is
// safe at rest yet un-attributable, so `llz reap`'s cluster-liveness gate cannot
// sweep it and its fail-safe KEEPS it — an unbounded cost leak. The gate must catch
// that even though nothing is wrong with the data.
func TestJudgeVolume_Tagging(t *testing.T) {
	// The ownership tag is the one reap keys on; losing it is the expensive case.
	v := judgeVolume(encPV("harbor", "data-harbor-redis-0", "17094415"),
		encVol("enabled", "block-storage", "platform-support-services"), wantTags, "")
	if v.ok() {
		t.Fatal("an encrypted Volume MISSING its lke<id> ownership tag must still fail — reap cannot attribute it")
	}
	if !strings.Contains(v.problem(), "lke637888") {
		t.Errorf("the verdict must name the missing tag, got %q", v.problem())
	}
	if strings.Contains(v.problem(), "NOT ENCRYPTED") {
		t.Errorf("a tagging failure must not be reported as an encryption failure: %q", v.problem())
	}

	// Every missing tag is named, not just the first.
	v = judgeVolume(encPV("x", "y", "1"), encVol("enabled"), wantTags, "")
	for _, want := range wantTags {
		if !strings.Contains(v.problem(), want) {
			t.Errorf("verdict should name every missing tag; %q omits %q", v.problem(), want)
		}
	}

	// Extra tags an operator added by hand are fine — the invariant is "carries at
	// least the required set", not "carries exactly it".
	v = judgeVolume(encPV("x", "y", "1"), encVol("enabled", append(wantTags, "cluster-abcd-test")...), wantTags, "")
	if !v.ok() {
		t.Errorf("extra operator tags must not fail the gate: %s", v.problem())
	}
}

// TestJudgeVolume_UnreadableFailsClosed: a Volume the API will not return is a
// FAILURE. Treating it as a pass would let an API outage launder an unencrypted
// fleet into a green check.
func TestJudgeVolume_UnreadableFailsClosed(t *testing.T) {
	v := judgeVolume(encPV("x", "y", "1"), nil, wantTags, "")
	if v.ok() {
		t.Fatal("a Volume that could not be read must fail, never pass")
	}
	if !strings.Contains(v.problem(), "UNREADABLE") {
		t.Errorf("problem should say the volume was unreadable, got %q", v.problem())
	}
}

func TestReportVolumeEncryption(t *testing.T) {
	good := judgeVolume(encPV("llz-openbao", "data-platform-openbao-0", "1"), encVol("enabled", wantTags...), wantTags, "")

	t.Run("all compliant passes and writes no summary", func(t *testing.T) {
		sum := withGHASummaryFile(t)
		if err := reportVolumeEncryption([]volumeVerdict{good}, wantTags, "block-storage-retain"); err != nil {
			t.Fatalf("a compliant fleet must pass: %v", err)
		}
		if b, _ := os.ReadFile(sum); len(b) != 0 {
			t.Errorf("clean run must write no step summary, got %q", b)
		}
	})

	t.Run("any violation fails and names the remedy", func(t *testing.T) {
		bad := judgeVolume(encPV("harbor", "data-harbor-redis-0", "17094415"), encVol("disabled", wantTags...), wantTags, "")
		sum := withGHASummaryFile(t)
		err := reportVolumeEncryption([]volumeVerdict{good, bad}, wantTags, "block-storage-retain")
		if err == nil {
			t.Fatal("an unencrypted Volume must fail the gate")
		}
		if !strings.Contains(err.Error(), "1 of 2") {
			t.Errorf("error should count violations against the total, got %q", err)
		}
		b, _ := os.ReadFile(sum)
		body := string(b)
		for _, want := range []string{
			"data-harbor-redis-0",
			"17094415",
			// Re-running cannot fix this; the summary must not imply otherwise.
			"is immutable once bound",
			"destroys that volume's data",
			// And it should point at the actual upstream cause on managed.
			"cluster.defaultStorageClass",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("summary missing %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, "data-platform-openbao-0") {
			t.Error("compliant Volumes must not be listed as violations")
		}
	})
}

// labelledVol is an otherwise-compliant Volume carrying a specific Linode label.
func labelledVol(label string) map[string]any {
	v := encVol("enabled", wantTags...)
	v["label"] = label
	return v
}

// TestJudgeVolume_Label is the third leg of the invariant. `pvc-<uuid>` is the CSI
// default on LKE-E (the managed controller ships no --volume-label-prefix), and a
// Volume still wearing it is anonymous everywhere it matters — the Linode UI, the
// billing export, the quota census. You cannot tell which workload owned it, and
// once the cluster is deleted you can never work it out.
func TestJudgeVolume_Label(t *testing.T) {
	pvc := encPV("harbor", "data-harbor-redis-0", "17094415")

	t.Run("CSI default is rejected", func(t *testing.T) {
		v := judgeVolume(pvc, labelledVol("pvc-e2e495ebe1504dff"), wantTags, "")
		if v.ok() {
			t.Fatal("a Volume still labelled pvc-<uuid> must fail — it is unattributable in the Linode UI and the billing export")
		}
		if !strings.Contains(v.problem(), "CSI default") {
			t.Errorf("problem should name the CSI default, got %q", v.problem())
		}
		// It IS healable — the volume-labels lane renames it — so the gate should be
		// willing to wait rather than failing instantly like it does for encryption.
		if !v.healable() {
			t.Error("a label violation must be healable; only encryption is final")
		}
	})

	t.Run("readable label passes", func(t *testing.T) {
		if v := judgeVolume(pvc, labelledVol("e2e-harbor-data-harbor-redis-0"), wantTags, ""); !v.ok() {
			t.Errorf("a relabelled Volume must pass: %s", v.problem())
		}
	})

	t.Run("with REGION_SHORT the exact label is required", func(t *testing.T) {
		want := desiredVolumeLabel("e2e", "harbor", "data-harbor-redis-0")
		if v := judgeVolume(pvc, labelledVol(want), wantTags, "e2e"); !v.ok() {
			t.Errorf("the relabeler's own output must satisfy the gate: %s", v.problem())
		}
		// A readable but WRONG label (e.g. left over from another cluster) is caught
		// only when REGION_SHORT is known — that is the point of the stronger check.
		v := judgeVolume(pvc, labelledVol("sta-harbor-data-harbor-redis-0"), wantTags, "e2e")
		if v.ok() {
			t.Fatal("a label from a different environment must fail when REGION_SHORT is known")
		}
	})

	t.Run("released PV is exempt", func(t *testing.T) {
		// The relabeler derives the label from namespace/pvc and skips PVs with no
		// claimRef, so demanding one here would assert something nothing ever sets.
		orphan := pvVolume{VolumeID: "1"}
		if v := judgeVolume(orphan, labelledVol("pvc-e2e495ebe1504dff"), wantTags, "e2e"); !v.ok() {
			t.Errorf("a released PV must not be failed for its label: %s", v.problem())
		}
	})
}

// TestVolumeVerdict_HealableSplit pins WHICH violations the gate is allowed to wait
// on. Waiting on encryption would be pure latency — it is decided inside
// CreateVolume and storageClassName is immutable once bound, so it can never
// improve. Tags and labels are applied by reconciler lanes after the fact.
func TestVolumeVerdict_HealableSplit(t *testing.T) {
	pvc := encPV("harbor", "data-harbor-redis-0", "1")

	unencrypted := judgeVolume(pvc, encVol("disabled", wantTags...), wantTags, "")
	if unencrypted.healable() {
		t.Error("encryption must NOT be healable — waiting on it only wastes the budget")
	}

	untagged := judgeVolume(pvc, encVol("enabled", "block-storage"), wantTags, "")
	if !untagged.healable() {
		t.Error("a tag violation is healable by the volume-tags lane")
	}

	unreadable := judgeVolume(pvc, nil, wantTags, "")
	if unreadable.healable() {
		t.Error("an unreadable Volume is not healable — it is a hard failure")
	}
}
