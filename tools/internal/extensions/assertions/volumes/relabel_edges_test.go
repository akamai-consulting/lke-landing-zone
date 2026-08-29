package volumes

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// errKube fails the PV read outright, as opposed to statusKube's non-2xx —
// a refused connection or a dead apiserver rather than an answered request.
type errKube struct{ err error }

func (e errKube) GetJSON(context.Context, string) (map[string]any, int, error) {
	return nil, 0, e.err
}
func (e errKube) CreateJSON(context.Context, string, any) (int, error) { return 0, e.err }
func (e errKube) MergePatch(context.Context, string, any) error        { return e.err }

// A lane that cannot read the PV list must SAY so. Returning nil here would print
// a clean "nothing to relabel" summary over an apiserver it never reached, which
// is the silent-no-op shape this whole area keeps relapsing into — it is what let
// the labels sit unwritten for an hour before e2eb26fb, and it is why the status
// window is pinned separately.
func TestRelabelSurfacesAPVReadFailure(t *testing.T) {
	t.Setenv("REGION_SHORT", "pri")
	d := withRelabelSeams(t, errKube{err: errors.New("connection refused")}, &fakeLinodeVols{})
	if err := Relabel(context.Background(), d); err == nil {
		t.Error("an unreadable PV list must error, not read as an empty cluster")
	}
}

// No CSI PVs is a legitimate, quiet success — a cluster whose workloads are all
// on emptyDir has nothing to report and must not error. It also must not reach
// the Linode API: a client wired to fail proves the early return happens BEFORE
// the account list, which is what keeps this cheap on every PV event.
func TestRelabelWithNoCSIVolumesReturnsBeforeCallingLinode(t *testing.T) {
	t.Setenv("REGION_SHORT", "pri")
	kube := fakeRelabelKube{pvList: map[string]any{"items": []any{}}}
	lc := &fakeLinodeVols{listErr: errors.New("must not be called")}
	d := withRelabelSeams(t, kube, lc)

	out := captureStdout(t, func() {
		if err := Relabel(context.Background(), d); err != nil {
			t.Fatalf("an empty cluster is not an error: %v", err)
		}
	})
	if !strings.Contains(out, "nothing to relabel") {
		t.Errorf("should say why it did nothing, got %q", out)
	}
}

// The account listing is the one remaining Linode call. Its failure has to
// surface: without it the lane cannot tell "already renamed by an older build"
// from "still on its CSI default", and reporting either from an empty list would
// be a fabricated verdict.
func TestRelabelSurfacesAListVolumesFailure(t *testing.T) {
	t.Setenv("REGION_SHORT", "pri")
	claim := map[string]any{"namespace": "team", "name": "x"}
	kube := fakeRelabelKube{pvList: map[string]any{"items": []any{pv(linodeCSIDriver, "100-x", claim)}}}
	lc := &fakeLinodeVols{listErr: errors.New("linode 503")}
	d := withRelabelSeams(t, kube, lc)
	if err := Relabel(context.Background(), d); err == nil {
		t.Error("a failed account listing must error rather than report an empty tally")
	}
}
