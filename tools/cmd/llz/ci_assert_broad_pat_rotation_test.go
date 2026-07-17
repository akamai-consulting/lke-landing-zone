package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestE2ERotationJobJSON guards the exercise Job builder: it must derive the Job
// from the CronJob's own jobTemplate and upsert ROTATE_AFTER_DAYS=0 so the tick is
// due BY CONSTRUCTION — no OpenBao credential needed. (An earlier iteration reset
// rotated_at via the root token, which the workflow REVOKES before the e2e asserts
// run → 403, observed live.)
func TestE2ERotationJobJSON(t *testing.T) {
	cron := `{
	  "spec": {
	    "schedule": "0 3 * * 1",
	    "jobTemplate": {
	      "spec": {
	        "backoffLimit": 1,
	        "activeDeadlineSeconds": 300,
	        "template": {
	          "spec": {
	            "serviceAccountName": "broad-pat-rotator",
	            "containers": [{
	              "name": "rotate",
	              "image": "ghcr.io/akamai-consulting/llz:latest",
	              "args": ["ci", "rotate-broad-pat", "--apply"],
	              "env": [
	                {"name": "BROAD_PAT_LABEL", "value": "llz-e2e-broad-pat"},
	                {"name": "ROTATE_AFTER_DAYS", "value": "60"}
	              ]
	            }]
	          }
	        }
	      }
	    }
	  }
	}`
	out, err := e2eRotationJobJSON([]byte(cron))
	if err != nil {
		t.Fatalf("e2eRotationJobJSON: %v", err)
	}
	var job map[string]any
	if err := json.Unmarshal(out, &job); err != nil {
		t.Fatalf("unmarshal built job: %v", err)
	}
	if job["kind"] != "Job" || job["apiVersion"] != "batch/v1" {
		t.Errorf("kind/apiVersion = %v/%v", job["kind"], job["apiVersion"])
	}
	md := job["metadata"].(map[string]any)
	if md["name"] != broadPATRotatorE2EJob || md["namespace"] != broadPATRotatorNS {
		t.Errorf("metadata = %v", md)
	}
	spec := job["spec"].(map[string]any)
	if spec["backoffLimit"] != float64(1) || spec["activeDeadlineSeconds"] != float64(300) {
		t.Errorf("jobTemplate.spec not carried over: %v", spec)
	}
	c := spec["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if c["image"] != "ghcr.io/akamai-consulting/llz:latest" {
		t.Errorf("container image not carried over: %v", c["image"])
	}
	// ROTATE_AFTER_DAYS AND GRACE_DAYS must be UPSERTED to 0 (due by construction +
	// self-reap accumulated test PATs), preserving unrelated env.
	days, grace, labels := "", "", ""
	seen := map[string]int{}
	for _, e := range c["env"].([]any) {
		em := e.(map[string]any)
		name, _ := em["name"].(string)
		seen[name]++
		switch name {
		case "ROTATE_AFTER_DAYS":
			days = em["value"].(string)
		case "GRACE_DAYS":
			grace = em["value"].(string)
		case "BROAD_PAT_LABEL":
			labels = em["value"].(string)
		}
	}
	if days != "0" {
		t.Errorf("ROTATE_AFTER_DAYS = %q, want 0 (due by construction)", days)
	}
	if grace != "0" {
		t.Errorf("GRACE_DAYS = %q, want 0 (self-reap accumulated e2e PATs)", grace)
	}
	if seen["ROTATE_AFTER_DAYS"] != 1 || seen["GRACE_DAYS"] != 1 {
		t.Errorf("env upsert must not duplicate: ROTATE_AFTER_DAYS=%d GRACE_DAYS=%d", seen["ROTATE_AFTER_DAYS"], seen["GRACE_DAYS"])
	}
	if labels != "llz-e2e-broad-pat" {
		t.Errorf("other env entries must be preserved, got label %q", labels)
	}

	// A container with NO env list gets the entry appended.
	cronNoEnv := `{"spec":{"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"rotate"}]}}}}}}`
	out2, err := e2eRotationJobJSON([]byte(cronNoEnv))
	if err != nil {
		t.Fatalf("no-env cronjob: %v", err)
	}
	for _, want := range []string{"ROTATE_AFTER_DAYS", "GRACE_DAYS"} {
		if !strings.Contains(string(out2), `"name":"`+want+`"`) {
			t.Errorf("%s not injected when env absent: %s", want, out2)
		}
	}

	// Malformed input fails loud.
	if _, err := e2eRotationJobJSON([]byte(`{"spec":{}}`)); err == nil {
		t.Error("cronjob without jobTemplate must error")
	}
	if _, err := e2eRotationJobJSON([]byte(`not json`)); err == nil {
		t.Error("garbage must error")
	}
}

func TestParseJobStatus(t *testing.T) {
	tests := []struct {
		in               string
		wantSucc, wantFl bool
	}{
		{"1/", true, false},      // succeeded
		{"/1", false, true},      // failed
		{"0/0", false, false},    // still running
		{"/", false, false},      // neither set yet
		{"", false, false},       // empty (job just created)
		{" 1 / 0 ", true, false}, // whitespace tolerant
		{"1/1", true, true},      // both (caller prefers succeeded)
	}
	for _, tc := range tests {
		succ, fl := parseJobStatus(tc.in)
		if succ != tc.wantSucc || fl != tc.wantFl {
			t.Errorf("parseJobStatus(%q) = (%v,%v), want (%v,%v)", tc.in, succ, fl, tc.wantSucc, tc.wantFl)
		}
	}
}

func TestParseRotationAction(t *testing.T) {
	// A real run: masked-token warning on stderr, then the JSON audit record.
	logs := `::add-mask::redacted
{"action":"rotated","event":"broad-pat-rotator","new_pat_id":123,"published_envs":["infra-e2e"]}`
	if a, ok := parseRotationAction(logs); !ok || a != "rotated" {
		t.Errorf("parseRotationAction = (%q,%v), want (rotated,true)", a, ok)
	}

	// A not-due tick still emits a record — action=skip must be reported (caller fails on it).
	if a, ok := parseRotationAction(`{"event":"broad-pat-rotator","action":"skip"}`); !ok || a != "skip" {
		t.Errorf("skip record: got (%q,%v)", a, ok)
	}

	// No audit record at all (pod never ran / crashed before printing).
	if _, ok := parseRotationAction("error: something\nboom\n"); ok {
		t.Error("expected no action from logs without an audit record")
	}

	// A different JSON event line must not be mistaken for the rotation record.
	if _, ok := parseRotationAction(`{"event":"other","action":"rotated"}`); ok {
		t.Error("must only match event=broad-pat-rotator")
	}
}
