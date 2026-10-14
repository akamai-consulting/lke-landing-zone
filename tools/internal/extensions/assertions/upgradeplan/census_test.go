package upgradeplan

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestCensusReadsWhatTheLinodeAPIActuallySends is the gate on the join this
// change got wrong.
//
// The count arrived as a float64 assertion against a client that decodes with
// UseNumber(), so it matched NOTHING: every bucket was dropped, the census came
// back empty, every bucket replace blocked, and the exemption was dead on arrival
// — against the live API, with the entire unit suite green. It was green because
// every other test here builds a BucketCensus by hand and none of them decoded a
// response.
//
// So this drives the decode from REAL RESPONSE BYTES, through the same
// json.Decoder setting the Linode client uses. A fixture of Go values would have
// agreed with whatever it was copied from, which is how the bug survived.
func TestCensusReadsWhatTheLinodeAPIActuallySends(t *testing.T) {
	// Trimmed from a real /v4/object-storage/buckets response.
	const body = `[
	  {"label":"platform-loki-chunks-prod","cluster":"us-ord-1","objects":63345,"size":50061153506},
	  {"label":"platform-harbor-registry-prod","cluster":"us-ord-1","objects":46,"size":87450960},
	  {"label":"gsap-apl-loki-ruler-prod","cluster":"us-ord-1","objects":0,"size":0}
	]`
	var buckets []map[string]any
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber() // exactly what linode.Client does
	if err := dec.Decode(&buckets); err != nil {
		t.Fatal(err)
	}

	census := censusFrom(buckets)
	if len(census) != 3 {
		t.Fatalf("the census dropped buckets it should have read: %v", census)
	}
	if got := census["platform-loki-chunks-prod"]; got != 63345 {
		t.Errorf("loki-chunks = %d, want 63345", got)
	}
	if !census.Empty("gsap-apl-loki-ruler-prod") {
		t.Error("a bucket the API reports as 0 objects must read as empty — without this the " +
			"exemption never fires and a correct prefix migration stays unperformable")
	}
	if census.Empty("platform-loki-chunks-prod") {
		t.Error("a bucket holding 63,345 objects must NOT read as empty")
	}
}

// A count that cannot be read is not zero. Every one of these leaves the bucket
// OUT of the census, and an absent bucket blocks — the single guess here that
// could cost data is guessing "empty".
func TestAnUnreadableCountLeavesTheBucketOutOfTheCensus(t *testing.T) {
	for name, b := range map[string]map[string]any{
		"objects absent":     {"label": "acme-loki-ruler-prod"},
		"objects null":       {"label": "acme-loki-ruler-prod", "objects": nil},
		"objects not a num":  {"label": "acme-loki-ruler-prod", "objects": "lots"},
		"objects a bool":     {"label": "acme-loki-ruler-prod", "objects": true},
		"label absent":       {"objects": json.Number("0")},
		"label empty string": {"label": "", "objects": json.Number("0")},
	} {
		census := censusFrom([]map[string]any{b})
		if census.Empty("acme-loki-ruler-prod") {
			t.Errorf("%s: read as EMPTY, which would exempt a bucket nothing verified", name)
		}
	}
}

// The spellings the field could plausibly arrive as. json.Number is what it is
// today; the other two are one decoder setting away, and each costs a case.
func TestObjectCountAcceptsEverySpellingOfANumber(t *testing.T) {
	for name, v := range map[string]any{
		"json.Number": json.Number("42"),
		"float64":     float64(42),
		"string":      "42",
	} {
		n, ok := objectCountOf(v)
		if !ok || n != 42 {
			t.Errorf("%s: got (%d, %v), want (42, true)", name, n, ok)
		}
	}
	for name, v := range map[string]any{
		"json.Number that is not an integer": json.Number("4.2e"),
		"string that is not a number":        "many",
		"nil":                                nil,
	} {
		if _, ok := objectCountOf(v); ok {
			t.Errorf("%s: reported a usable count from something unreadable", name)
		}
	}
}

// Silence, not a guess, when there is no token. The strict answer is the safe one
// and needs no warning: every destructive finding blocks, which is what this gate
// did before the census existed.
func TestNoTokenYieldsNoCensusRatherThanAnEmptyOne(t *testing.T) {
	t.Setenv("LINODE_TOKEN", "")
	t.Setenv("LINODE_API_TOKEN", "")
	if census := LookupBuckets(); census != nil {
		t.Errorf("with no token the census must be nil (unknown), not %v (empty, which exempts)", census)
	}
}

// TestReportOnlyPrintsTheSameVerdictAndExitsZero.
//
// The PLAN lane needs the finding and must not be stopped by it: a plan changes
// nothing, and failing the preview would teach people to skip the preview. The
// arms that matter are that the verdict is still PRINTED (a silent report-only is
// just a disabled check) and that `apply` is unaffected.
func TestReportOnlyPrintsTheSameVerdictAndExitsZero(t *testing.T) {
	prev := LookupBuckets
	LookupBuckets = func() BucketCensus { return BucketCensus{"platform-loki-chunks-prod": 63345} }
	defer func() { LookupBuckets = prev }()

	run := func(t *testing.T, reportOnly bool) (string, error) {
		t.Helper()
		c := Cmd()
		var out, errOut bytes.Buffer
		c.SetOut(&out)
		c.SetErr(&errOut)
		c.SetIn(strings.NewReader(objRenamePlan))
		args := []string{"--plan", "-"}
		if reportOnly {
			args = append(args, "--report-only")
		}
		c.SetArgs(args)
		err := c.Execute()
		return out.String() + errOut.String(), err
	}

	blocking, errBlocking := run(t, false)
	if errBlocking == nil {
		t.Fatal("precondition: this plan destroys a bucket holding 63,345 objects and must fail the apply lane")
	}
	preview, errPreview := run(t, true)
	if errPreview != nil {
		t.Errorf("--report-only must exit 0 — a plan changes nothing: %v", errPreview)
	}
	// The FINDING must survive. A report-only that prints nothing is a check that
	// has been turned off with extra steps.
	for _, want := range []string{"loki_chunks", "destroying or replacing"} {
		if !strings.Contains(preview, want) {
			t.Errorf("--report-only dropped %q from the verdict; got:\n%s", want, preview)
		}
	}
	if !strings.Contains(preview, "FAILS on the apply") {
		t.Errorf("the preview must say the same check blocks the apply; got:\n%s", preview)
	}
	if !strings.Contains(blocking, "loki_chunks") {
		t.Errorf("precondition: the blocking run should name the bucket; got:\n%s", blocking)
	}
}
