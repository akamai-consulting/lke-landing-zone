package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintRecordAndLogRoundTripsAndRedacts(t *testing.T) {
	var buf bytes.Buffer
	orig := RecordLog
	RecordLog = &buf
	t.Cleanup(func() { RecordLog = orig })

	rec := map[string]any{"event": "e", "dry_run": false, "new_token": "SECRET", "new_secret_key": "S2", "label": "l"}
	if err := PrintRecordAndLog(rec); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "SECRET") || strings.Contains(buf.String(), "S2") {
		t.Fatalf("a live credential reached the log copy: %s", buf.String())
	}
	if rec["new_token"] != "SECRET" {
		t.Fatal("redaction must not mutate the caller's record (stdout carries the real one)")
	}
	// What GitHub serves back: a timestamp per line, the step's script echoed
	// first (marker without a record), then the record.
	log := "2026-10-15T01:02:03.0000000Z ##[group]Run echo \"" + LogRecordMarker + "$X\"\n" +
		"2026-10-15T01:02:04.0000000Z " + buf.String()
	got := ParseLogRecords(log)
	if len(got) != 1 || got[0]["event"] != "e" || got[0]["label"] != "l" || got[0]["dry_run"] != false {
		t.Fatalf("parsed %v", got)
	}
	if got := ParseLogRecords("no records here\n"); len(got) != 0 {
		t.Fatalf("parsed records out of nothing: %v", got)
	}
}
