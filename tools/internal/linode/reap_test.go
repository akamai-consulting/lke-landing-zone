package linode

import (
	"encoding/json"
	"testing"
)

func TestLKEIDFromLabel(t *testing.T) {
	cases := map[string]string{
		"lke613260": "613260",
		"lke0":      "0",
		"lke":       "",
		"lke12a":    "",
		"lke-613":   "", // hyphen is not part of the VPC-label form
		"foo":       "",
		"":          "",
	}
	for in, want := range cases {
		if got := LKEIDFromLabel(in); got != want {
			t.Errorf("LKEIDFromLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLKEIDFromTags(t *testing.T) {
	cases := []struct {
		tags []string
		want string
	}{
		{[]string{"lke613260"}, "613260"},
		{[]string{"lke-613260"}, "613260"},
		{[]string{"kubernetes", "lke-42"}, "42"},
		{[]string{"kubernetes"}, ""},
		{nil, ""},
		{[]string{"lkex"}, ""},
	}
	for _, c := range cases {
		if got := LKEIDFromTags(c.tags); got != c.want {
			t.Errorf("LKEIDFromTags(%v) = %q, want %q", c.tags, got, c.want)
		}
	}
}

func TestClassifyNodeBalancer(t *testing.T) {
	live := map[string]bool{"100": true}
	cases := []struct {
		name         string
		lkeClusterID string
		tags         []string
		label        string
		want         NBDecision
	}{
		// lke_cluster.id is authoritative and wins over the tag fallbacks.
		{"lke-cluster-live", "100", []string{"kubernetes"}, "ccm-x", NBKeep},
		{"lke-cluster-gone", "200", []string{"kubernetes"}, "ccm-x", NBOrphan},
		// The LKE-E case that used to leak: kubernetes-tagged, but lke_cluster.id
		// points to a gone cluster -> orphan (was NBCheckBackends before).
		{"lke-e-ccm-gone", "616722", []string{"kubernetes"}, "ccm-a3c603ec7667", NBOrphan},
		// Tag fallbacks when lke_cluster.id is absent.
		{"tag-live", "", []string{"lke100"}, "ccm-x", NBKeep},
		{"tag-gone", "", []string{"lke-200"}, "ccm-x", NBOrphan},
		{"ccm-label-no-tag", "", nil, "ccm-abc", NBCheckBackends},
		{"kubernetes-tag-no-owner", "", []string{"kubernetes"}, "whatever", NBCheckBackends},
		{"unrelated", "", []string{"prod"}, "my-lb", NBKeep},
	}
	for _, c := range cases {
		if got := ClassifyNodeBalancer(c.lkeClusterID, c.tags, c.label, live); got != c.want {
			t.Errorf("%s: ClassifyNodeBalancer = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLKEClusterIDFromNB(t *testing.T) {
	nb := map[string]any{"lke_cluster": map[string]any{"id": json.Number("616722")}}
	if got := LKEClusterIDFromNB(nb); got != "616722" {
		t.Errorf("LKEClusterIDFromNB = %q, want 616722", got)
	}
	if got := LKEClusterIDFromNB(map[string]any{}); got != "" {
		t.Errorf("LKEClusterIDFromNB(no field) = %q, want empty", got)
	}
}

func TestVPCIsOrphan(t *testing.T) {
	live := map[string]bool{"100": true}
	if !VPCIsOrphan("lke200", live) {
		t.Error("lke200 (cluster gone) should be orphan")
	}
	if VPCIsOrphan("lke100", live) {
		t.Error("lke100 (cluster live) should not be orphan")
	}
	if VPCIsOrphan("platform-vpc", live) {
		t.Error("a BYO label carries no lke id — not an lke-orphan")
	}
}

func TestClassifyVolume(t *testing.T) {
	live := map[string]bool{"100": true}
	cases := []struct {
		name string
		tags []string
		want VolDecision
	}{
		// The block-storage SC stamps `lke<id>`; a live cluster's detached PVC is kept.
		{"live-cluster", []string{"block-storage", "lke100"}, VolKeep},
		{"gone-cluster", []string{"block-storage", "lke200"}, VolOrphan},
		{"hyphen-form-live", []string{"lke-100"}, VolKeep},
		// No cluster tag (legacy / other tooling) — no ownership signal.
		{"no-cluster-tag", []string{"block-storage", "platform-support-services"}, VolUntagged},
		{"no-tags", nil, VolUntagged},
	}
	for _, c := range cases {
		if got := ClassifyVolume(c.tags, live); got != c.want {
			t.Errorf("%s: ClassifyVolume(%v) = %v, want %v", c.name, c.tags, got, c.want)
		}
	}
}

func TestVolumeIsCandidate(t *testing.T) {
	// attached → never
	if VolumeIsCandidate(false, "pvc-1", "us-ord", nil, "", nil, "1", "") {
		t.Error("attached volume must not be a candidate")
	}
	// non-pvc label → never
	if VolumeIsCandidate(true, "my-disk", "us-ord", nil, "", nil, "2", "") {
		t.Error("non-pvc label must not be a candidate")
	}
	// unattached pvc-, no constraints → yes
	if !VolumeIsCandidate(true, "pvc-abc", "us-ord", nil, "", nil, "3", "") {
		t.Error("unattached pvc- with no constraints should match")
	}
	// region filter mismatch → no
	if VolumeIsCandidate(true, "pvc-abc", "us-ord", nil, "us-sea", nil, "4", "") {
		t.Error("region mismatch must exclude")
	}
	// id allowlist excludes → no; includes → yes
	allow := map[string]bool{"5": true}
	if VolumeIsCandidate(true, "pvc-abc", "us-ord", nil, "", allow, "6", "") {
		t.Error("id not in allowlist must exclude")
	}
	if !VolumeIsCandidate(true, "pvc-abc", "us-ord", nil, "", allow, "5", "") {
		t.Error("id in allowlist should match")
	}
	// tag-must-include
	if VolumeIsCandidate(true, "pvc-abc", "us-ord", []string{"other"}, "", nil, "7", "block-storage") {
		t.Error("missing required tag must exclude")
	}
	if !VolumeIsCandidate(true, "pvc-abc", "us-ord", []string{"block-storage"}, "", nil, "8", "block-storage") {
		t.Error("present required tag should match")
	}
}
