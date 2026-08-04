package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeRegions struct {
	ids []string
	err error
}

func (f fakeRegions) ListRegions(context.Context) ([]map[string]any, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []map[string]any
	for _, id := range f.ids {
		out = append(out, map[string]any{"id": id})
	}
	return out, nil
}

func stubRegions(t *testing.T, l regionLister) {
	t.Helper()
	orig := regionClient
	t.Cleanup(func() { regionClient = orig })
	regionClient = func() regionLister { return l }
}

func TestCheckRegionAcceptsARealRegion(t *testing.T) {
	// Including a numeric-suffixed one: de-fra-2 is a REGION, and no shape rule
	// could tell it from the object-storage cluster id us-sea-1.
	stubRegions(t, fakeRegions{ids: []string{"us-sea", "us-ord", "de-fra-2"}})
	for _, r := range []string{"us-sea", "de-fra-2"} {
		if err := checkRegion(r); err != nil {
			t.Errorf("checkRegion(%q) = %v, want nil", r, err)
		}
	}
}

func TestCheckRegionNamesTheObjClusterSwap(t *testing.T) {
	// The quickstart puts --region and --obj-cluster next to each other, so this
	// is the transposition to expect. "us-sea-1" is an OBJ cluster in region
	// us-sea; say so rather than just "not a region".
	stubRegions(t, fakeRegions{ids: []string{"us-sea", "us-ord"}})
	err := checkRegion("us-sea-1")
	if err == nil {
		t.Fatal("expected an error for an OBJ cluster id passed as --region")
	}
	for _, want := range []string{"shaped like an object-storage CLUSTER id", "--region us-sea --obj-cluster us-sea-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestCheckRegionSuggestsNearMisses(t *testing.T) {
	stubRegions(t, fakeRegions{ids: []string{"us-sea", "us-ord", "us-iad", "eu-west"}})
	err := checkRegion("us-seat")
	if err == nil {
		t.Fatal("expected an error for a typo'd region")
	}
	if !strings.Contains(err.Error(), "Did you mean") || !strings.Contains(err.Error(), "us-sea") {
		t.Errorf("error %q should suggest the near matches", err)
	}
	// Only the same-prefix ones, and at most three — not the global region list.
	if strings.Contains(err.Error(), "eu-west") {
		t.Errorf("error %q suggested an unrelated region", err)
	}
}

func TestCheckRegionSilentWhenTheAnswerIsUnknown(t *testing.T) {
	// No token, or the API is unreachable: `llz env add` has never required a
	// Linode token and must not start. Unknown is not "wrong".
	stubRegions(t, nil)
	if err := checkRegion("nonsense"); err != nil {
		t.Errorf("no client ⇒ no opinion, got %v", err)
	}
	stubRegions(t, fakeRegions{err: errors.New("500 Internal Server Error")})
	if err := checkRegion("nonsense"); err != nil {
		t.Errorf("API error ⇒ no opinion, got %v", err)
	}
}
