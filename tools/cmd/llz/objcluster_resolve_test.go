package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeOBJLister struct {
	clusters []map[string]any
	err      error
}

func (f fakeOBJLister) ListObjectStorageClusters(context.Context) ([]map[string]any, error) {
	return f.clusters, f.err
}

// withOBJLister installs a fake account. Passing nil models "no LINODE_TOKEN".
func withOBJLister(t *testing.T, l objClusterLister) {
	t.Helper()
	prev := objClusterClient
	t.Cleanup(func() { objClusterClient = prev })
	objClusterClient = func() objClusterLister {
		if l == nil {
			return nil // nil interface, the no-token path
		}
		return l
	}
}

func objAccount(entries ...[2]string) fakeOBJLister {
	var out []map[string]any
	for _, e := range entries {
		out = append(out, map[string]any{"id": e[0], "region": e[1]})
	}
	return fakeOBJLister{clusters: out}
}

// `llz env add` has never required a Linode token or network. The single most
// important property here is that it still doesn't: every offline behaviour must
// be byte-for-byte what it was before.
func TestResolveOBJClusterDegradesWithoutAnAccount(t *testing.T) {
	t.Run("supplied value passes straight through", func(t *testing.T) {
		withOBJLister(t, nil)
		got, note, err := resolveOBJCluster("us-sea-1", "us-sea")
		if err != nil || got != "us-sea-1" {
			t.Fatalf("got (%q, %v), want us-sea-1 with no error", got, err)
		}
		if note != "" {
			t.Errorf("must not claim a check it could not run, got %q", note)
		}
	})

	t.Run("empty value still errors, and says how to get the derivation", func(t *testing.T) {
		withOBJLister(t, nil)
		_, _, err := resolveOBJCluster("", "us-sea")
		if err == nil {
			t.Fatal("expected --obj-cluster to remain required")
		}
		if !strings.Contains(err.Error(), "LINODE_TOKEN") {
			t.Errorf("error should point at the token that unlocks derivation, got: %v", err)
		}
	})

	t.Run("an API error is unknown, not empty", func(t *testing.T) {
		withOBJLister(t, fakeOBJLister{err: errors.New("503")})
		got, _, err := resolveOBJCluster("us-sea-1", "us-sea")
		if err != nil || got != "us-sea-1" {
			t.Fatalf("an unreachable API must not reject a valid-looking value: (%q, %v)", got, err)
		}
	})

	// Shape validation runs first and is unchanged.
	t.Run("malformed value is rejected offline", func(t *testing.T) {
		withOBJLister(t, nil)
		if _, _, err := resolveOBJCluster("us-sea", "us-sea"); err == nil {
			t.Fatal("a bare region is not a cluster id and must still be rejected")
		}
	})
}

func TestResolveOBJClusterDerivesWhenUnambiguous(t *testing.T) {
	withOBJLister(t, objAccount([2]string{"us-sea-1", "us-sea"}, [2]string{"us-ord-1", "us-ord"}))

	got, note, err := resolveOBJCluster("", "us-sea")
	if err != nil {
		t.Fatalf("derivation failed: %v", err)
	}
	if got != "us-sea-1" {
		t.Errorf("got %q, want us-sea-1", got)
	}
	if !strings.Contains(note, "us-sea-1") || !strings.Contains(note, "derived") {
		t.Errorf("note should say what was chosen and why, got %q", note)
	}
}

// The choice between endpoint generations IS the hazard, so llz must not make it.
// Guessing here would just relocate the silent failure it exists to prevent.
func TestResolveOBJClusterRefusesToGuessBetweenGenerations(t *testing.T) {
	withOBJLister(t, objAccount([2]string{"us-ord-1", "us-ord"}, [2]string{"us-ord-10", "us-ord"}))

	_, _, err := resolveOBJCluster("", "us-ord")
	if err == nil {
		t.Fatal("must not pick between two endpoint generations")
	}
	for _, want := range []string{"us-ord-1", "us-ord-10", "will not choose"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list the real options and refuse; missing %q in: %v", want, err)
		}
	}
}

func TestResolveOBJClusterRejectsAValueNotInTheRegion(t *testing.T) {
	withOBJLister(t, objAccount([2]string{"us-sea-1", "us-sea"}, [2]string{"us-ord-1", "us-ord"}))

	// Well-formed, plausible, and wrong — the exact mistake nothing downstream
	// catches, because the apply succeeds.
	_, _, err := resolveOBJCluster("us-ord-1", "us-sea")
	if err == nil {
		t.Fatal("a cluster from another region must be rejected")
	}
	if !strings.Contains(err.Error(), "us-sea-1") {
		t.Errorf("error must name the region's real options, got: %v", err)
	}
	if !strings.Contains(err.Error(), "NoSuchBucket") {
		t.Errorf("error should state the consequence, got: %v", err)
	}
}

func TestResolveOBJClusterConfirmsAGoodValue(t *testing.T) {
	withOBJLister(t, objAccount([2]string{"us-sea-1", "us-sea"}))
	got, note, err := resolveOBJCluster("us-sea-1", "us-sea")
	if err != nil || got != "us-sea-1" {
		t.Fatalf("got (%q, %v)", got, err)
	}
	if !strings.Contains(note, "confirmed") {
		t.Errorf("a checked value should say so, got %q", note)
	}
}

func TestResolveOBJClusterEmptyRegionListIsNamed(t *testing.T) {
	withOBJLister(t, objAccount([2]string{"us-ord-1", "us-ord"}))
	_, _, err := resolveOBJCluster("", "ap-south")
	if err == nil {
		t.Fatal("expected an error when the region has no OBJ cluster")
	}
	if !strings.Contains(err.Error(), "ap-south") {
		t.Errorf("error should name the region, got: %v", err)
	}
}
