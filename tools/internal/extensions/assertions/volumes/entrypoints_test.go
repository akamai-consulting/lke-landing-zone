package volumes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The two top-level entry points were 0% covered before this package existed, and
// not because nobody tried: as `runCIAssertVolumeEncryption` and
// `runCIReconcileVolumeTags` in package main they reached for a Linode token, an
// in-cluster client and kubectl themselves, so a test could only have run them
// against a real cluster. Taking Deps as a parameter is what made them reachable —
// which is the concrete argument for the action ABI delivering capabilities rather
// than letting an extension acquire them.

func scJSON(tags ...string) map[string]any {
	return map[string]any{
		"metadata":   map[string]any{"name": DefaultTagsSC},
		"parameters": map[string]any{volumeTagsSCParam: strings.Join(tags, ",")},
	}
}

func boundPV(ns, claim, volID string) map[string]any {
	return map[string]any{
		"spec": map[string]any{
			"csi":       map[string]any{"driver": linodeCSIDriver, "volumeHandle": volID + "-" + claim},
			"claimRef":  map[string]any{"namespace": ns, "name": claim},
			"nodeAffin": nil,
		},
	}
}

func pvListJSON(items ...map[string]any) map[string]any {
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = it
	}
	return map[string]any{"items": out}
}

// fakeKube serves canned GetJSON responses by path.
type fakeKube struct {
	byPath map[string]map[string]any
	status int
	err    error
}

func (f fakeKube) GetJSON(_ context.Context, path string) (map[string]any, int, error) {
	if f.err != nil {
		return nil, 0, f.err
	}
	st := f.status
	if st == 0 {
		st = 200
	}
	return f.byPath[path], st, nil
}

func stubTagClient(t *testing.T, c tagReconcileClient) {
	t.Helper()
	orig := tagReconcileLinodeFn
	tagReconcileLinodeFn = func(string) tagReconcileClient { return c }
	t.Cleanup(func() { tagReconcileLinodeFn = orig })
}

func TestAssertEncryptionPassesAndFails(t *testing.T) {
	kubectl := func(args ...string) ([]byte, error) {
		switch {
		case len(args) > 1 && args[1] == "storageclass":
			return json.Marshal(scJSON(wantTags...))
		case len(args) > 1 && args[1] == "persistentvolumes":
			return json.Marshal(pvListJSON(boundPV("llz-openbao", "data-platform-openbao-0", "1")))
		}
		return nil, fmt.Errorf("unexpected kubectl %v", args)
	}

	// "renamed" USED to be the compliant shape here. It is now the broken one: the
	// compliant volume keeps the label its volumeHandle names, which is what the
	// CSI will look the device up by on the next attach.
	t.Run("an encrypted, tagged, un-renamed volume passes", func(t *testing.T) {
		stubTagClient(t, &fakeTagClient{vols: map[string]map[string]any{
			"1": {"id": jnum("1"), "label": "data-platform-openbao-0", "encryption": "enabled", "tags": anySlice(wantTags)},
		}})
		d, _ := capturingDeps()
		d.Token, d.Kubectl = "tok", kubectl
		if err := AssertEncryption(context.Background(), d, DefaultTagsSC); err != nil {
			t.Fatalf("a compliant fleet must pass: %v", err)
		}
	})

	t.Run("an unencrypted volume fails and writes the summary", func(t *testing.T) {
		stubTagClient(t, &fakeTagClient{vols: map[string]map[string]any{
			"1": {"id": jnum("1"), "label": "pri-llz-openbao-data", "encryption": "disabled", "tags": anySlice(wantTags)},
		}})
		d, sum := capturingDeps()
		d.Token, d.Kubectl = "tok", kubectl
		err := AssertEncryption(context.Background(), d, DefaultTagsSC)
		if err == nil {
			t.Fatal("an unencrypted Volume must fail the gate")
		}
		if len(*sum) == 0 {
			t.Error("a failing run must write the step summary — it is the operator's only record of which volume and why")
		}
	})
}

// An empty token is a caller error, and the message has to say so rather than the
// lane quietly examining nothing. This is the vacuous-pass failure mode the whole
// storage gate family exists to refuse.
func TestEntryPointsRefuseAnEmptyToken(t *testing.T) {
	if err := AssertEncryption(context.Background(), Deps{}, DefaultTagsSC); err == nil {
		t.Error("AssertEncryption must refuse an empty token")
	}
	if err := ReconcileTags(context.Background(), Deps{}, DefaultTagsSC); err == nil {
		t.Error("ReconcileTags must refuse an empty token")
	}
}

func TestReconcileTagsHealsMissingTags(t *testing.T) {
	kube := fakeKube{byPath: map[string]map[string]any{
		scStorageClassesPath + "/" + DefaultTagsSC: scJSON(wantTags...),
		"/api/v1/persistentvolumes":                pvListJSON(boundPV("harbor", "data-harbor-redis-0", "17")),
	}}
	c := &fakeTagClient{vols: map[string]map[string]any{
		"17": {"id": jnum("17"), "label": "pri-harbor-data", "tags": anySlice(wantTags[:1])},
	}}
	stubTagClient(t, c)
	if err := ReconcileTags(context.Background(), Deps{Token: "tok", Kube: kube}, DefaultTagsSC); err != nil {
		t.Fatalf("ReconcileTags: %v", err)
	}
	got := c.puts["17"]
	if len(got) != len(wantTags) {
		t.Fatalf("PUT tags = %v, want the full desired set %v", got, wantTags)
	}
}

// A cluster read that fails must surface, not be treated as "no volumes" — which
// would report a clean run over nothing.
func TestReconcileTagsSurfacesAClusterReadFailure(t *testing.T) {
	kube := fakeKube{err: errors.New("apiserver down")}
	stubTagClient(t, &fakeTagClient{})
	if err := ReconcileTags(context.Background(), Deps{Token: "tok", Kube: kube}, DefaultTagsSC); err == nil {
		t.Error("a failed StorageClass read must surface as an error")
	}
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
