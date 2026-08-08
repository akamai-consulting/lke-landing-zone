package promwire

// The decoders' own tests, moved with them out of internal/assertreconciler.
//
// They were never about the reconciler — they assert the property this package
// exists to protect: a query FAILURE and an EMPTY RESULT are different answers.
// Leaving them behind would have left the shared decoder untested and credited its
// coverage to whichever caller happened to exercise it.

import "testing"

func TestPromScalar(t *testing.T) {
	one := []byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1720000000,"1"]}]}}`)
	if v, ok, err := Scalar(one); !ok || v != 1 || err != nil {
		t.Errorf("expected (1,true,nil), got (%v,%v,%v)", v, ok, err)
	}
	zero := []byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1720000000,"0"]}]}}`)
	if v, ok, err := Scalar(zero); !ok || v != 0 || err != nil {
		t.Errorf("expected (0,true,nil), got (%v,%v,%v)", v, ok, err)
	}

	// An empty result is a real ANSWER: Prometheus was asked and the series is
	// genuinely absent. No query error.
	empty := []byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`)
	if _, ok, err := Scalar(empty); ok || err != nil {
		t.Errorf("empty result must be (no series, no error), got (ok=%v, err=%v)", ok, err)
	}

	// These are NOT answers about the series — the query itself failed. Folding
	// them into "no series" is what made a Prometheus hiccup report as "the
	// reconciler isn't reporting (pod down / not scraped)".
	for _, tt := range []struct{ name, body string }{
		{"prometheus returned an error", `{"status":"error","error":"bad"}`},
		{"unparseable body", `not json`},
		{"malformed sample tuple", `{"status":"success","data":{"result":[{"value":[1720000000]}]}}`},
		{"non-string value", `{"status":"success","data":{"result":[{"value":[1720000000,7]}]}}`},
		{"non-numeric value", `{"status":"success","data":{"result":[{"value":[1720000000,"NaNsense"]}]}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok, err := Scalar([]byte(tt.body)); ok || err == nil {
				t.Errorf("must report a QUERY error, not an absent series; got (ok=%v, err=%v)", ok, err)
			}
		})
	}
}

func TestPromVectorByLabel(t *testing.T) {
	raw := []byte(`{"status":"success","data":{"resultType":"vector","result":[
	  {"metric":{"reconciler":"observe"},"value":[1,"1720000000"]},
	  {"metric":{"reconciler":"apl-overlay"},"value":[1,"1720000300"]},
	  {"metric":{},"value":[1,"5"]}
	]}}`)
	got, err := VectorByLabel(raw, "reconciler")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got["observe"] != 1720000000 || got["apl-overlay"] != 1720000300 {
		t.Errorf("unexpected vector: %v", got)
	}
	// A query failure must be an ERROR, not an empty map — an empty map would read
	// as "every lane is dead" and fail the gate blaming the reconciler.
	if _, err := VectorByLabel([]byte(`{"status":"error","error":"boom"}`), "reconciler"); err == nil {
		t.Error("a Prometheus error must not decode as an empty vector")
	}
	if _, err := VectorByLabel([]byte(`nope`), "reconciler"); err == nil {
		t.Error("an unparseable body must be an error")
	}
}
