package cli

// logrecord.go — the copy of a credential verb's JSON record that reaches the
// job LOG, and the reader that finds it there again.
//
// WHY A SECOND COPY. PrintRecord writes the record to stdout, and every composite
// action that wraps these verbs redirects stdout into a temp file, parses it with
// jq, and deletes it. So the one structured statement of what the verb DID —
// dry_run, skipped, what it probed — never reached anything a later reader can
// fetch: the step summary has no API, and the temp file is gone. A job that
// succeeded was the only evidence left, and success is exactly what a dry run
// that called nothing, or a stand-down, also reports.
//
// The release-e2e lane (`llz ci assert-instance-workflow`) reads the record back
// out of the job log and judges THAT. Producer and reader share this file, so the
// marker cannot drift between them, and the coupling test in releasepublish runs
// the real credential verbs into the real reader.
//
// SECRETS NEVER REACH THE LOG COPY. The armed create paths put a fresh token or
// key pair in the record; ::add-mask:: would redact them from the log, but a copy
// that never carried them is the property worth having, not one that depends on
// masking having run first.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// BroadPATStandDown is the `skipped` value a broad-PAT verb's record carries when
// spec.components.broadPatRotator owns the credential and the CI path stands
// down (credrotate.reportBroadPATStandDown). The composite action keys on the
// field and release-e2e asserts the value, so it is written once, here.
const BroadPATStandDown = "broad-pat-owned-in-cluster"

// LogRecordMarker prefixes the log copy of a record. One line: marker + JSON.
const LogRecordMarker = "llz-record: "

// RecordLog is where the log copy goes; nil means os.Stderr AT CALL TIME, which
// the composite actions leave attached to the job log. Resolved late so a test
// that swaps os.Stderr captures it too. A package var so tests can redirect it.
var RecordLog io.Writer

// redactedRecordKeys are the record fields that carry a live credential.
var redactedRecordKeys = []string{"new_token", "new_access_key", "new_secret_key", "token", "secret_key", "access_key"}

// PrintRecordAndLog is PrintRecord plus a redacted, marker-prefixed copy on
// RecordLog, for the verbs whose record is the evidence a later reader judges.
func PrintRecordAndLog(record map[string]any) error {
	if err := PrintRecord(record); err != nil {
		return err
	}
	logged := make(map[string]any, len(record))
	for k, v := range record {
		logged[k] = v
	}
	for _, k := range redactedRecordKeys {
		if _, ok := logged[k]; ok {
			logged[k] = "(redacted)"
		}
	}
	out, err := json.Marshal(logged)
	if err != nil {
		return err
	}
	w := RecordLog
	if w == nil {
		w = os.Stderr
	}
	_, err = fmt.Fprintf(w, "%s%s\n", LogRecordMarker, out)
	return err
}

// ParseLogRecords returns every record PrintRecordAndLog wrote into log, in
// order. It tolerates the per-line timestamp GitHub prefixes a job log with, and
// skips a marker line that is not followed by a JSON object — which is what the
// script text a job log echoes before running a step looks like, since it names
// the marker without carrying a record.
func ParseLogRecords(log string) []map[string]any {
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(log))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		i := strings.Index(line, LogRecordMarker)
		if i < 0 {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(line[i+len(LogRecordMarker):]))
		dec.UseNumber()
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil || rec == nil {
			continue
		}
		out = append(out, rec)
	}
	return out
}
