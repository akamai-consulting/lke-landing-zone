package volumes

import (
	"strings"
	"testing"
)

// The per-workload tags carry what the retired volume-labels lane used to put in
// the LABEL. They exist in TAGS because nothing resolves a device path from a
// tag, so writing one cannot break a mount the way renaming a Volume does.
func TestWorkloadTagsCarryTheIdentityTheLabelUsedTo(t *testing.T) {
	got := workloadTags(pvVolume{VolumeID: "1", Namespace: "harbor", PVC: "harbor-otomi-db-1"})
	want := []string{"ns-harbor", "harbor-harbor-otomi-db-1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("workloadTags = %v, want %v", got, want)
	}
}

// A released PV keeps no claim, so there is no workload to name. Emitting a
// half-formed tag like "ns-" would be worse than emitting none: it is under
// Linode's 3-char minimum and it would attach the same meaningless tag to every
// orphan on the account.
func TestWorkloadTagsAreEmptyWithoutAClaim(t *testing.T) {
	for _, pv := range []pvVolume{
		{VolumeID: "1"},
		{VolumeID: "2", Namespace: "harbor"},
		{VolumeID: "3", PVC: "data-0"},
	} {
		if got := workloadTags(pv); len(got) != 0 {
			t.Errorf("pv %+v: workloadTags = %v, want none", pv, got)
		}
	}
}

// TRUNCATION KEEPS THE TAIL, and this is the same lesson the Volume LABEL taught
// expensively. A StatefulSet's ordinal lives on the right, so cutting from the
// right collapses every replica onto one string. Labels are account-unique, so
// there that produced `400 {"reason":"Must be unique"}` and 17 of 17 renames
// rejected. Tags tolerate duplicates, so here it is "merely" three replicas you
// cannot tell apart — same mistake, cheaper consequence, equally avoidable.
func TestWorkloadTagsKeepReplicasDistinctWhenTruncated(t *testing.T) {
	ns := "a-very-long-namespace-name-that-eats-the-budget"
	seen := map[string]string{}
	for _, pvc := range []string{
		"data-platform-openbao-0",
		"data-platform-openbao-1",
		"data-platform-openbao-2",
	} {
		tags := workloadTags(pvVolume{VolumeID: "1", Namespace: ns, PVC: pvc})
		wl := tags[1]
		if len(wl) > maxLinodeTag {
			t.Fatalf("tag %q is %d chars — Linode rejects over %d", wl, len(wl), maxLinodeTag)
		}
		if prev, dup := seen[wl]; dup {
			t.Fatalf("%s and %s both truncate to %q — the ordinal was cut off", prev, pvc, wl)
		}
		seen[wl] = pvc
	}
}

// Linode's tag charset is not documented, so this stays inside the subset the API
// demonstrably accepted for Volume LABELS ([A-Za-z0-9_-]). Kubernetes names admit
// '.', which is the one character that has to be mapped.
func TestWorkloadTagsStayInTheSafeCharset(t *testing.T) {
	tags := workloadTags(pvVolume{VolumeID: "1", Namespace: "kube.system", PVC: "data.0"})
	for _, tag := range tags {
		for _, r := range tag {
			ok := r == '-' || r == '_' ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !ok {
				t.Errorf("tag %q contains %q, outside the safe charset", tag, r)
			}
		}
		if len(tag) < 3 {
			t.Errorf("tag %q is under Linode's 3-char minimum", tag)
		}
	}
}
