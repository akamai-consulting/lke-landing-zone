package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// jnum mirrors how the Linode client decodes numbers (UseNumber → json.Number),
// which is what MapUint reads.
func jnum(n string) json.Number { return json.Number(n) }

func TestDesiredVolumeLabel(t *testing.T) {
	cases := []struct {
		region, ns, pvc, want string
	}{
		{"pri", "team-gsap", "data-web-0", "pri-team-gsap-data-web-0"},
		{"sec", "kube_system", "vol_1", "sec-kube_system-vol_1"},
		// '/' and '.' are outside Linode's charset → '-'.
		{"pri", "team/foo", "data.web", "pri-team-foo-data-web"},
		// Truncated to 32, then trailing '-' stripped.
		{"lab", "a-very-long-namespace-here", "and-pvc", "lab-a-very-long-namespace-here-a"},
		// Truncation landing on a '-' strips it.
		{"pri", "abcdefghijklmnopqrstuvwxyz012", "z", "pri-abcdefghijklmnopqrstuvwxyz01"},
	}
	for _, c := range cases {
		got := desiredVolumeLabel(c.region, c.ns, c.pvc)
		if got != c.want {
			t.Errorf("desiredVolumeLabel(%q,%q,%q) = %q, want %q", c.region, c.ns, c.pvc, got, c.want)
		}
		if len(got) > maxLinodeLabel {
			t.Errorf("label %q exceeds %d chars", got, maxLinodeLabel)
		}
		if strings.HasSuffix(got, "-") {
			t.Errorf("label %q has a trailing dash", got)
		}
	}
}

func pv(driver, handle string, claim map[string]any) map[string]any {
	spec := map[string]any{"csi": map[string]any{"driver": driver, "volumeHandle": handle}}
	if claim != nil {
		spec["claimRef"] = claim
	}
	return map[string]any{"spec": spec}
}

func TestLinodeCSIVolumes(t *testing.T) {
	claim := func(ns, name string) map[string]any { return map[string]any{"namespace": ns, "name": name} }
	list := map[string]any{"items": []any{
		pv(linodeCSIDriver, "123456-pvcabc", claim("team", "data-0")),
		pv(linodeCSIDriver, "789", claim("ns2", "d")),        // handle with no dash
		pv("ebs.csi.aws.com", "999-x", claim("a", "b")),      // not Linode CSI → skip
		pv(linodeCSIDriver, "111-x", nil),                    // no claimRef (orphan) → skip
		pv(linodeCSIDriver, "notanumber-x", claim("a", "b")), // unparseable id → skip
	}}
	got := linodeCSIVolumes(list)
	if len(got) != 2 {
		t.Fatalf("got %d volumes, want 2: %+v", len(got), got)
	}
	if got[0].id != 123456 || got[0].namespace != "team" || got[0].pvcName != "data-0" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].id != 789 {
		t.Errorf("second id = %d, want 789", got[1].id)
	}
}

// fakeKube serves a fixed PV list for /api/v1/persistentvolumes.
type fakeRelabelKube struct{ pvList map[string]any }

func (f fakeRelabelKube) GetJSON(_ context.Context, path string) (map[string]any, int, error) {
	if path == "/api/v1/persistentvolumes" {
		return f.pvList, 200, nil
	}
	return nil, 404, nil
}
func (f fakeRelabelKube) CreateJSON(context.Context, string, any) (int, error) { return 201, nil }
func (f fakeRelabelKube) MergePatch(context.Context, string, any) error        { return nil }

// fakeLinodeVols records UpdateVolumeLabel calls.
type fakeLinodeVols struct {
	vols      []map[string]any
	updateErr error
	renamed   map[uint64]string
}

func (f *fakeLinodeVols) ListVolumes(context.Context) ([]map[string]any, error) { return f.vols, nil }
func (f *fakeLinodeVols) UpdateVolumeLabel(_ context.Context, id uint64, label string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	if f.renamed == nil {
		f.renamed = map[uint64]string{}
	}
	f.renamed[id] = label
	return nil
}

func withRelabelSeams(t *testing.T, kube kubeAPI, lc volumeLabeler) {
	t.Helper()
	origK, origL := discoverKubeFn, relabelLinodeFn
	discoverKubeFn = func() (kubeAPI, error) { return kube, nil }
	relabelLinodeFn = func(string) volumeLabeler { return lc }
	t.Cleanup(func() { discoverKubeFn = origK; relabelLinodeFn = origL })
}

func TestRunRelabelVolumes(t *testing.T) {
	t.Setenv("REGION_SHORT", "pri")
	t.Setenv("LINODE_TOKEN", "tok")

	claim := func(ns, name string) map[string]any { return map[string]any{"namespace": ns, "name": name} }
	kube := fakeRelabelKube{pvList: map[string]any{"items": []any{
		pv(linodeCSIDriver, "100-x", claim("team", "needs-rename")), // current != desired
		pv(linodeCSIDriver, "200-x", claim("team", "already-ok")),   // current == desired
		pv(linodeCSIDriver, "300-x", claim("team", "gone")),         // absent from account list
	}}}
	lc := &fakeLinodeVols{vols: []map[string]any{
		{"id": jnum("100"), "label": "pvc-olduuid"},
		{"id": jnum("200"), "label": "pri-team-already-ok"},
		// 300 intentionally absent
	}}
	withRelabelSeams(t, kube, lc)

	if err := runRelabelVolumes(context.Background()); err != nil {
		t.Fatalf("runRelabelVolumes: %v", err)
	}
	if len(lc.renamed) != 1 || lc.renamed[100] != "pri-team-needs-rename" {
		t.Fatalf("renamed = %v, want {100: pri-team-needs-rename}", lc.renamed)
	}
}

func TestRunRelabelVolumesRequiresEnv(t *testing.T) {
	t.Setenv("REGION_SHORT", "")
	t.Setenv("LINODE_TOKEN", "tok")
	if err := runRelabelVolumes(context.Background()); err == nil {
		t.Error("missing REGION_SHORT should error")
	}
}

func TestRunRelabelVolumesUpdateErrorSurfaces(t *testing.T) {
	t.Setenv("REGION_SHORT", "pri")
	t.Setenv("LINODE_TOKEN", "tok")
	claim := map[string]any{"namespace": "team", "name": "x"}
	kube := fakeRelabelKube{pvList: map[string]any{"items": []any{pv(linodeCSIDriver, "100-x", claim)}}}
	lc := &fakeLinodeVols{
		vols:      []map[string]any{{"id": jnum("100"), "label": "old"}},
		updateErr: errors.New("linode 500"),
	}
	withRelabelSeams(t, kube, lc)
	if err := runRelabelVolumes(context.Background()); err == nil {
		t.Error("a rename error should surface a non-nil error (so the CronJob/alert fires)")
	}
}
