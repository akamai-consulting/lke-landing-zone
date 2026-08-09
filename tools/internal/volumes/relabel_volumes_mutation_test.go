package volumes

// Mutation-test gap closure for ci_relabel_volumes.go: the PV-list acceptance
// window and the run's own tally. The relabeler mutates real Linode Volumes, so
// "how many did you rename" is the only record of what it touched.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// statusKube serves the PV list with a caller-chosen HTTP status, so the
// accept/reject window around 2xx can be pinned at both edges.
type statusKube struct {
	pvList map[string]any
	status int
}

func (f statusKube) GetJSON(context.Context, string) (map[string]any, int, error) {
	return f.pvList, f.status, nil
}
func (f statusKube) CreateJSON(context.Context, string, any) (int, error) { return 201, nil }
func (f statusKube) MergePatch(context.Context, string, any) error        { return nil }

// Only a 2xx may be read as a PV list. A non-2xx body is an API error object,
// and treating one as "zero PVs" makes the relabeler silently no-op forever;
// rejecting a legitimate 200 makes it fail forever. Pin both edges.
func TestRunRelabelVolumesPVListStatusWindow(t *testing.T) {
	claim := map[string]any{"namespace": "team", "name": "x"}
	list := map[string]any{"items": []any{pv(linodeCSIDriver, "100-x", claim)}}

	for _, tc := range []struct {
		status  int
		wantErr bool
	}{
		{status: 199, wantErr: true},
		{status: 200, wantErr: false}, // the inclusive lower edge
		{status: 299, wantErr: false}, // the inclusive upper edge
		{status: 300, wantErr: true},  // exclusive: a redirect is not a PV list
		{status: 403, wantErr: true},
	} {
		t.Run(fmt.Sprintf("status %d", tc.status), func(t *testing.T) {
			t.Setenv("REGION_SHORT", "pri")
			t.Setenv("LINODE_TOKEN", "tok")
			lc := &fakeLinodeVols{vols: []map[string]any{{"id": jnum("100"), "label": "old"}}}
			d := withRelabelSeams(t, statusKube{pvList: list, status: tc.status}, lc)

			var err error
			captureStdout(t, func() { err = Relabel(context.Background(), d) })
			if tc.wantErr && err == nil {
				t.Errorf("status %d must fail the run, not read as an empty PV list", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("status %d is a valid PV list, got error: %v", tc.status, err)
			}
			if !tc.wantErr && len(lc.renamed) != 1 {
				t.Errorf("status %d must actually relabel, renamed = %v", tc.status, lc.renamed)
			}
		})
	}
}

// The summary line is the CronJob's entire audit trail. Each counter has to move
// in its own direction and only for its own case, or a run that renamed nothing
// is indistinguishable from one that renamed everything.
func TestRunRelabelVolumesSummaryTally(t *testing.T) {
	t.Setenv("REGION_SHORT", "pri")
	t.Setenv("LINODE_TOKEN", "tok")

	claim := func(ns, name string) map[string]any { return map[string]any{"namespace": ns, "name": name} }
	kube := fakeRelabelKube{pvList: map[string]any{"items": []any{
		pv(linodeCSIDriver, "100-x", claim("team", "needs-rename")), // renamed
		pv(linodeCSIDriver, "200-x", claim("team", "already-ok")),   // already-ok
		pv(linodeCSIDriver, "300-x", claim("team", "gone")),         // missing
	}}}
	lc := &fakeLinodeVols{vols: []map[string]any{
		{"id": jnum("100"), "label": "pvc-olduuid"},
		{"id": jnum("200"), "label": "pri-team-already-ok"},
	}}
	d := withRelabelSeams(t, kube, lc)

	var err error
	out := captureStdout(t, func() { err = Relabel(context.Background(), d) })
	if err != nil {
		t.Fatalf("runRelabelVolumes: %v", err)
	}
	if want := "summary: renamed=1 already-ok=1 missing=1 errors=0"; !strings.Contains(out, want) {
		t.Errorf("summary line wrong.\ngot:\n%s\nwant it to contain: %s", out, want)
	}
}

// A volumeHandle whose id segment is unusable is SKIPPED, never guessed at —
// relabelling by a mis-parsed id would rename someone else's volume. This also
// documents why `IndexByte(handle,'-') >= 0` cannot be tightened to `> 0`: at
// offset 0 the handle starts with '-', and both the empty prefix and the whole
// handle fail ParseUint identically.
func TestLinodeCSIVolumesSkipsUnparseableHandles(t *testing.T) {
	claim := map[string]any{"namespace": "team", "name": "x"}
	for _, handle := range []string{"-123", "-", "", "abc-1", "-abc"} {
		got := linodeCSIVolumes(map[string]any{"items": []any{pv(linodeCSIDriver, handle, claim)}})
		if len(got) != 0 {
			t.Errorf("volumeHandle %q yielded %+v, want it skipped (no id may be inferred)", handle, got)
		}
	}
}

// captureStdout is a local copy of package main's helper. The relabel lane prints
// its per-Volume decisions to stdout and that output is the operator's only record
// of what was renamed, so the mutation cases assert on it — which means the
// package needs to be able to read it without depending on the CLI's test helpers.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	b, _ := io.ReadAll(r)
	return string(b)
}
