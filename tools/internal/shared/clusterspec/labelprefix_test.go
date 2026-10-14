package clusterspec

// LabelPrefixFor arrived here from internal/extensions/objenc at 0% covered, which
// is part of why it was there in the first place: it was a convenience nobody
// tested, sitting in the package that happened to need it first, and three other
// extensions imported that whole capability to reach it.
//
// The behaviour worth pinning is the ABSENCE of a fallback. Every instance used to
// collide on the same global bucket names because this value was the constant
// `platform` in a handful of places, so a "just use platform" default hiding in
// here would not merely be untidy — it would point rotation and teardown at
// ANOTHER instance's buckets and keys, silently and successfully.

import (
	"strings"
	"testing"
)

func TestLabelPrefixForResolvesFromTheSpec(t *testing.T) {
	t.Chdir(writeInstance(t, splitFiles()))

	got, err := LabelPrefixFor("llz ci teardown")
	if err != nil {
		t.Fatalf("LabelPrefixFor: %v", err)
	}
	if got == "" {
		t.Fatal("empty prefix returned without an error — the one outcome this must never have")
	}
	lz, err := LoadInstance(".")
	if err != nil {
		t.Fatalf("LoadInstance: %v", err)
	}
	if want := lz.ObjLabelPrefix(); got != want {
		t.Errorf("LabelPrefixFor = %q, want %q — it must be the spec's answer, not a derived one", got, want)
	}
}

func TestLabelPrefixForRefusesRatherThanGuessing(t *testing.T) {
	// No spec at all: run from outside an instance checkout. The error has to name
	// the spec, because that is the only place the answer can come from.
	t.Chdir(t.TempDir())

	got, err := LabelPrefixFor("llz ci rotate-obj-keys")
	if err == nil {
		t.Fatalf("no spec present and LabelPrefixFor returned %q — a fallback here points "+
			"rotation and teardown at another instance's buckets", got)
	}
	for _, want := range []string{"llz ci rotate-obj-keys", "landingzone.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q — the message is the whole remediation", err, want)
		}
	}
}
