package credrotate

import (
	"strings"
	"testing"
)

func TestObjClusterFromEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"the form llz tokens writes", "https://us-sea-1.linodeobjects.com", "us-sea-1"},
		{"newer generation ordinal", "https://us-ord-10.linodeobjects.com", "us-ord-10"},
		{"trailing slash", "https://us-ord-10.linodeobjects.com/", "us-ord-10"},
		{"hand-set bare host", "us-sea-1.linodeobjects.com", "us-sea-1"},
		{"surrounding whitespace", "  https://de-fra-2.linodeobjects.com \n", "de-fra-2"},
		{"empty", "", ""},
		// A host with no cluster label must not yield "linodeobjects": guessing a
		// cluster is precisely the failure this replaces.
		{"apex host carries no cluster", "https://linodeobjects.com", ""},
		{"single label", "https://localhost", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := objClusterFromEndpoint(tc.in); got != tc.want {
				t.Fatalf("objClusterFromEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestResolveObjBucketCluster(t *testing.T) {
	t.Run("explicit wins over the endpoint", func(t *testing.T) {
		got, err := resolveObjBucketCluster("us-ord-1", "https://us-sea-1.linodeobjects.com")
		if err != nil || got != "us-ord-1" {
			t.Fatalf("got (%q, %v), want (us-ord-1, nil)", got, err)
		}
	})
	t.Run("derives from the endpoint", func(t *testing.T) {
		got, err := resolveObjBucketCluster("", "https://us-sea-1.linodeobjects.com")
		if err != nil || got != "us-sea-1" {
			t.Fatalf("got (%q, %v), want (us-sea-1, nil)", got, err)
		}
	})
	// The whole point: never fall back to a literal. An unresolvable cluster has
	// to stop the rotation, because a wrong one mints a key against a bucket
	// namespace the state does not live in and the failure is silent.
	t.Run("refuses to guess", func(t *testing.T) {
		_, err := resolveObjBucketCluster("", "")
		if err == nil {
			t.Fatal("want an error when neither the flag nor TF_STATE_ENDPOINT resolves")
		}
		for _, want := range []string{"TF_STATE_ENDPOINT", "--bucket-cluster"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should name %q so the operator can act; got:\n%s", want, err)
			}
		}
	})
}

func TestRotationLabelIsInstanceScoped(t *testing.T) {
	a := rotationLabel("acme-lz", rotationKindPAT)
	b := rotationLabel("other-lz", rotationKindPAT)
	if a == b {
		t.Fatal("two instances must not share a rotation label — that is the cross-instance revocation bug")
	}
	if !strings.Contains(a, "acme-lz") {
		t.Errorf("label %q should carry the instance prefix", a)
	}
	// The legacy literals must not be reachable from the derivation, or the
	// collision survives the fix.
	for kind, legacy := range legacyRotationLabels {
		if rotationLabel("acme-lz", kind) == legacy {
			t.Errorf("derived label for %s collides with the legacy literal %q", kind, legacy)
		}
	}
}

func TestResolveRotationLabelExplicitWins(t *testing.T) {
	// An explicit --label is how an operator drains a legacy label by hand, so it
	// must bypass the spec lookup entirely (this test runs outside an instance).
	got, err := resolveRotationLabel("  gha-platform-platform_TF_STATE_KEY  ", rotationKindTFStateKey, "test")
	if err != nil {
		t.Fatalf("explicit label must not need a spec: %v", err)
	}
	if got != "gha-platform-platform_TF_STATE_KEY" {
		t.Fatalf("got %q, want the trimmed explicit value", got)
	}
}

func TestResolveRotationLabelNeedsSpec(t *testing.T) {
	// Outside an instance root there is no prefix to derive from. Failing loudly
	// beats minting under a shared default.
	t.Chdir(t.TempDir())
	if _, err := resolveRotationLabel("", rotationKindPAT, "`llz credentials pat create`"); err == nil {
		t.Fatal("want an error when the spec cannot supply an instance prefix")
	}
}
