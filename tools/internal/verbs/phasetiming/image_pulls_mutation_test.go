package phasetiming

import (
	"strings"
	"testing"
)

// The report is sorted slowest-first with a STABLE sort, so images that took
// the same time keep the order the events arrived in. A strict comparator is
// what makes that hold; a non-strict one reverses every tied run and the table
// reshuffles between runs of the same cluster.
func TestParseImagePullsKeepsEventOrderForEqualDurations(t *testing.T) {
	events := `{"items":[
	  {"message":"Successfully pulled image \"ghcr.io/first:1\" in 5s","involvedObject":{"namespace":"a","name":"p1"}},
	  {"message":"Successfully pulled image \"ghcr.io/second:1\" in 5s","involvedObject":{"namespace":"b","name":"p2"}},
	  {"message":"Successfully pulled image \"ghcr.io/slowest:1\" in 30s","involvedObject":{"namespace":"c","name":"p3"}}
	]}`
	pulls := parseImagePulls([]byte(events))
	if len(pulls) != 3 {
		t.Fatalf("parsed %d pulls, want 3", len(pulls))
	}
	got := []string{pulls[0].Image, pulls[1].Image, pulls[2].Image}
	want := []string{"ghcr.io/slowest:1", "ghcr.io/first:1", "ghcr.io/second:1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want slowest first then the tied pair in event order (%v)", got, want)
	}
}

// kubelet's message is not always prefixed — the quoted image ref can start the
// string. Requiring the opening quote to sit past offset 0 loses the image name
// and the table degrades to "(unknown)".
func TestImageFromPullMessageHandlesAQuoteAtOffsetZero(t *testing.T) {
	if got := imageFromPullMessage(`"ghcr.io/x:1" was pulled in 2s`); got != "ghcr.io/x:1" {
		t.Errorf("leading-quote message → %q, want ghcr.io/x:1", got)
	}
	// An empty pair of quotes carries no image; "" is the right answer either way.
	if got := imageFromPullMessage(`pulled image "" in 2s`); got != "" {
		t.Errorf("empty quotes → %q, want empty", got)
	}
	// An unterminated quote is not an image ref.
	if got := imageFromPullMessage(`pulled image "ghcr.io/x:1 in 2s`); got != "" {
		t.Errorf("unterminated quote → %q, want empty", got)
	}
}

// "(unknown)" is the placeholder for an image the parser could NOT name; a
// named image must appear verbatim, or every row of the report reads as
// unknown and the per-image magnitude — the whole signal — is gone.
func TestRenderImagePullTableSubstitutesUnknownOnlyForABlankImage(t *testing.T) {
	table := renderImagePullTable([]imagePull{
		{Image: "ghcr.io/named:1", Namespace: "ns", Pod: "p", DurationS: 3},
		{Image: "", Namespace: "ns", Pod: "q", DurationS: 1},
	})
	if !strings.Contains(table, "| ghcr.io/named:1 | ns/p |") {
		t.Errorf("a named image must be shown verbatim:\n%s", table)
	}
	if !strings.Contains(table, "| (unknown) | ns/q |") {
		t.Errorf("a blank image must fall back to (unknown):\n%s", table)
	}
}
