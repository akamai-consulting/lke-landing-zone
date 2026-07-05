package metrics

import (
	"math"
	"strings"
	"testing"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	n, err := r.WriteTo(&b)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if int(n) != b.Len() {
		t.Fatalf("WriteTo reported %d bytes, wrote %d", n, b.Len())
	}
	return b.String()
}

func TestSingleGauge(t *testing.T) {
	r := NewRegistry()
	r.SetGauge("llz_reconcile_up", "1 if the last sample succeeded", nil, 1)
	got := render(t, r)
	want := "# HELP llz_reconcile_up 1 if the last sample succeeded\n" +
		"# TYPE llz_reconcile_up gauge\n" +
		"llz_reconcile_up 1\n"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestFamiliesSortedSamplesSorted(t *testing.T) {
	r := NewRegistry()
	// Insert out of order across two families with multiple label sets.
	r.SetGauge("z_metric", "z help", map[string]string{"k": "b"}, 2)
	r.SetGauge("z_metric", "z help", map[string]string{"k": "a"}, 1)
	r.SetGauge("a_metric", "a help", nil, 5)
	got := render(t, r)
	want := "# HELP a_metric a help\n" +
		"# TYPE a_metric gauge\n" +
		"a_metric 5\n" +
		"# HELP z_metric z help\n" +
		"# TYPE z_metric gauge\n" +
		`z_metric{k="a"} 1` + "\n" +
		`z_metric{k="b"} 2` + "\n"
	if got != want {
		t.Fatalf("families/samples not deterministically ordered.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestLabelsSortedByKey(t *testing.T) {
	r := NewRegistry()
	r.SetGauge("m", "h", map[string]string{"b": "2", "a": "1", "c": "3"}, 0)
	got := render(t, r)
	if !strings.Contains(got, `m{a="1",b="2",c="3"} 0`) {
		t.Fatalf("label keys not sorted: %s", got)
	}
}

func TestSetGaugeUpsert(t *testing.T) {
	r := NewRegistry()
	r.SetGauge("m", "h", map[string]string{"k": "v"}, 1)
	r.SetGauge("m", "h", map[string]string{"k": "v"}, 9) // same label set → overwrite
	got := render(t, r)
	if strings.Contains(got, "m{k=\"v\"} 1\n") {
		t.Fatalf("stale value not overwritten:\n%s", got)
	}
	if !strings.Contains(got, `m{k="v"} 9`) {
		t.Fatalf("upsert value missing:\n%s", got)
	}
	// One family, one sample line — no duplicate.
	if c := strings.Count(got, "# TYPE m gauge"); c != 1 {
		t.Fatalf("expected 1 TYPE line, got %d:\n%s", c, got)
	}
}

func TestLabelValueEscaping(t *testing.T) {
	r := NewRegistry()
	r.SetGauge("m", "h", map[string]string{"path": `a\b"c` + "\n" + "d"}, 0)
	got := render(t, r)
	if !strings.Contains(got, `m{path="a\\b\"c\nd"} 0`) {
		t.Fatalf("label value not escaped per spec: %s", got)
	}
}

func TestHelpNewlineEscaped(t *testing.T) {
	r := NewRegistry()
	r.SetGauge("m", "line1\nline2", nil, 0)
	got := render(t, r)
	if !strings.Contains(got, "# HELP m line1\\nline2\n") {
		t.Fatalf("HELP newline not escaped: %q", got)
	}
}

func TestNoHelpStillEmitsType(t *testing.T) {
	r := NewRegistry()
	r.SetGauge("m", "", nil, 3) // no help text
	got := render(t, r)
	if strings.Contains(got, "# HELP") {
		t.Fatalf("unexpected HELP line for empty help: %s", got)
	}
	if !strings.Contains(got, "# TYPE m gauge\nm 3\n") {
		t.Fatalf("TYPE + sample missing: %s", got)
	}
}

func TestValueFormatting(t *testing.T) {
	cases := map[float64]string{
		0:            "0",
		1:            "1",
		3.5:          "3.5",
		1234567:      "1.234567e+06",
		math.Inf(1):  "+Inf",
		math.Inf(-1): "-Inf",
		1751644800:   "1.7516448e+09", // a unix timestamp round-trips
	}
	for v, want := range cases {
		if got := formatValue(v); got != want {
			t.Errorf("formatValue(%v) = %q, want %q", v, got, want)
		}
	}
	if got := formatValue(math.NaN()); got != "NaN" {
		t.Errorf("formatValue(NaN) = %q, want NaN", got)
	}
}

func TestEmptyRegistry(t *testing.T) {
	if got := render(t, NewRegistry()); got != "" {
		t.Fatalf("empty registry should render nothing, got %q", got)
	}
}
