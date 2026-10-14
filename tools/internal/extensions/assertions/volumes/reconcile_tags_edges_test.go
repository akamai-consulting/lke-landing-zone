package volumes

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The tag lane's refusal paths. Every one of these used to be unexercised, and
// they matter for the same reason the retired relabeler's did: this lane runs
// unattended in-pod, so an exit that returns nil on a cluster it never read is
// indistinguishable from a clean pass. A silent no-op is exactly how the volume
// labels sat unwritten for an hour before anyone noticed (e2eb26fb), and it is
// the failure mode a reconciler lane relapses into most easily.
func TestReconcileTagsRefusalPaths(t *testing.T) {
	scPath := scStorageClassesPath + "/" + DefaultTagsSC
	goodSC := map[string]map[string]any{scPath: scJSON(wantTags...)}

	for _, tc := range []struct {
		name    string
		deps    Deps
		wantErr string
	}{
		{
			// Without a token the lane cannot reach Linode at all. Returning nil
			// here would report a healed fleet it never looked at.
			name:    "no token",
			deps:    Deps{Kube: fakeKube{byPath: goodSC}},
			wantErr: "LINODE_TOKEN",
		},
		{
			name:    "storageclass read fails",
			deps:    Deps{Token: "tok", Kube: fakeKube{err: errors.New("connection refused")}},
			wantErr: "connection refused",
		},
		{
			// A non-2xx is an ANSWERED request that is not a StorageClass. Reading
			// it as an empty desired-tag set would let the lane "heal" every Volume
			// to no tags at all.
			name:    "storageclass returns non-2xx",
			deps:    Deps{Token: "tok", Kube: fakeKube{byPath: goodSC, status: 403}},
			wantErr: "status 403",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubTagClient(t, &fakeTagClient{})
			err := ReconcileTags(context.Background(), tc.deps, DefaultTagsSC)
			if err == nil {
				t.Fatalf("%s must error rather than report a clean pass", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q should name %q", err, tc.wantErr)
			}
		})
	}
}

// A StorageClass carrying no volumeTags parameter cannot define a desired set.
// The lane must say so rather than proceed with an empty one — "heal every Volume
// to zero tags" is a destructive reading of a missing config.
func TestReconcileTagsRefusesAStorageClassWithNoTags(t *testing.T) {
	scPath := scStorageClassesPath + "/" + DefaultTagsSC
	stubTagClient(t, &fakeTagClient{})
	d := Deps{Token: "tok", Kube: fakeKube{byPath: map[string]map[string]any{
		scPath: {"metadata": map[string]any{"name": DefaultTagsSC}, "parameters": map[string]any{}},
	}}}
	if err := ReconcileTags(context.Background(), d, DefaultTagsSC); err == nil {
		t.Error("a StorageClass with no volumeTags must error, not heal every Volume to none")
	}
}

// The PV list is the lane's other input, and it fails independently of the
// StorageClass — the class can read fine while the apiserver refuses the PV list.
func TestReconcileTagsSurfacesAPVListFailure(t *testing.T) {
	scPath := scStorageClassesPath + "/" + DefaultTagsSC
	stubTagClient(t, &fakeTagClient{})
	// 200 for the class, 404 for everything else: byPath returns nil for the PV
	// path, which the lane must treat as unreadable rather than "no volumes".
	d := Deps{Token: "tok", Kube: fakeKube{byPath: map[string]map[string]any{scPath: scJSON(wantTags...)}}}
	if err := ReconcileTags(context.Background(), d, DefaultTagsSC); err == nil {
		t.Error("an unreadable PV list must error, not read as a cluster with no volumes")
	}
}

// AssertEncryption's own refusals. This one is a GATE, so a silent pass is the
// worst possible failure: it reports a compliant fleet it never measured. Both
// exits below existed unexercised.
func TestAssertEncryptionRefusesRatherThanPassingSilently(t *testing.T) {
	t.Run("no token", func(t *testing.T) {
		d, _ := capturingDeps()
		d.Kubectl = func(...string) ([]byte, error) { return nil, nil }
		err := AssertEncryption(context.Background(), d, DefaultTagsSC)
		if err == nil {
			t.Fatal("without a token the gate cannot read encryption state and must refuse, not pass")
		}
		if !strings.Contains(err.Error(), "silent pass") {
			t.Errorf("the refusal should say why skipping is unacceptable, got %q", err)
		}
	})

	t.Run("cluster unreadable", func(t *testing.T) {
		d, _ := capturingDeps()
		d.Token = "tok"
		d.Kubectl = func(...string) ([]byte, error) { return nil, errors.New("no kubeconfig") }
		if err := AssertEncryption(context.Background(), d, DefaultTagsSC); err == nil {
			t.Error("an unreadable cluster must fail the gate, not report an empty compliant fleet")
		}
	})
}
