package main

// ci_volume_detach_test.go — waitVolumesDetached lives in ci.go and STAYED when
// the destroy verbs were extracted; its test travelled with the teardown file by
// accident, because the two had been neighbours rather than related.
//
// Worth noting rather than silently relocating: an extraction moves the tests it
// can see, and "sits in the same file" is not the same relation as "tests this
// code". This one only surfaced because the compiler could no longer reach a
// package-level var.

import (
	"context"
	"testing"
)

func TestWaitVolumesDetached(t *testing.T) {
	origInterval := volumeDetachPollInterval
	volumeDetachPollInterval = 0
	t.Cleanup(func() { volumeDetachPollInterval = origInterval })

	// Already detached → returns without sleeping.
	fake := &fakeDetachClient{volumes: []map[string]any{
		{"id": float64(1), "label": "pvc-a", "linode_id": nil},
	}}
	waitVolumesDetached(context.Background(), fake, "1", 0)

	// Still attached + zero budget → gives up after the immediate check.
	fake.volumes = []map[string]any{{"id": float64(1), "label": "pvc-a", "linode_id": float64(7)}}
	waitVolumesDetached(context.Background(), fake, "1", 0)
}

// fakeDetachClient is the two-method slice waitVolumesDetached actually drives.
// The test used to reach for ci_teardown's much larger fake because they shared a
// file; extracting teardown surfaced that the dependency was never real — this is
// the whole contract.
type fakeDetachClient struct {
	volumes  []map[string]any
	detached []uint64
}

func (f *fakeDetachClient) ListVolumes(context.Context) ([]map[string]any, error) {
	return f.volumes, nil
}

func (f *fakeDetachClient) DetachVolume(_ context.Context, id uint64) error {
	f.detached = append(f.detached, id)
	return nil
}
