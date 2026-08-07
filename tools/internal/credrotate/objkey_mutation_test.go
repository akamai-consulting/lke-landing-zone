package credrotate

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// withSlogBuffer captures the rotator's structured log for the duration of a
// test (the OBJ-key commands report through slog, not stdout).
func withSlogBuffer(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func objKeyCreateResp() map[string]any {
	return map[string]any{
		"id":         json.Number("77"),
		"access_key": "AKIAFAKE",
		"secret_key": "sekret-half",
	}
}

// "updated GHA OBJ-key secrets" is an audit claim about GitHub state. It must
// be made when (and only when) a half was actually written: claiming it on a
// run that named no secret sends an operator looking for a rotation that never
// touched GitHub.
func TestCredentialsObjKeyCreateClaimsTheGHAWriteOnlyWhenItHappens(t *testing.T) {
	const claim = "updated GHA OBJ-key secrets"

	t.Run("no GHA names → no claim", func(t *testing.T) {
		logs := withSlogBuffer(t)
		calls := withGHSetSecret(t, nil)
		client := &fakeRotatorClient{createResp: objKeyCreateResp()}
		captureFirewallOutput(t, func() {
			if err := RunObjKeyCreate(context.Background(), client, true,
				"lbl", "us-ord-10", "bkt", "read_write", "", "", []string{"primary"}); err != nil {
				t.Fatal(err)
			}
		})
		if len(*calls) != 0 {
			t.Errorf("no secret names were given, yet %v were written", *calls)
		}
		if strings.Contains(logs.String(), claim) {
			t.Errorf("claimed a GHA update that never happened:\n%s", logs.String())
		}
	})

	t.Run("secret half only → claim", func(t *testing.T) {
		logs := withSlogBuffer(t)
		calls := withGHSetSecret(t, nil)
		client := &fakeRotatorClient{createResp: objKeyCreateResp()}
		captureFirewallOutput(t, func() {
			if err := RunObjKeyCreate(context.Background(), client, true,
				"lbl", "us-ord-10", "bkt", "read_write", "", "OBJ_SECRET", []string{"primary"}); err != nil {
				t.Fatal(err)
			}
		})
		if len(*calls) != 1 || (*calls)[0] != "OBJ_SECRET@infra-primary=sekret-half" {
			t.Errorf("gh writes = %v, want the secret half in infra-primary", *calls)
		}
		if !strings.Contains(logs.String(), claim) {
			t.Errorf("a real GHA write must be logged:\n%s", logs.String())
		}
	})

	t.Run("access half only → claim", func(t *testing.T) {
		logs := withSlogBuffer(t)
		calls := withGHSetSecret(t, nil)
		client := &fakeRotatorClient{createResp: objKeyCreateResp()}
		captureFirewallOutput(t, func() {
			if err := RunObjKeyCreate(context.Background(), client, true,
				"lbl", "us-ord-10", "bkt", "read_write", "OBJ_ACCESS", "", []string{"primary"}); err != nil {
				t.Fatal(err)
			}
		})
		if len(*calls) != 1 || (*calls)[0] != "OBJ_ACCESS@infra-primary=AKIAFAKE" {
			t.Errorf("gh writes = %v, want the access half in infra-primary", *calls)
		}
		if !strings.Contains(logs.String(), claim) {
			t.Errorf("a real GHA write must be logged:\n%s", logs.String())
		}
	})
}

// keep_newest=1 is the documented minimum — "keep only the live key" — and the
// daily drain runs with it. Rejecting it would turn the whole rotation lane off.
func TestCredentialsObjKeyRevokeOldAcceptsKeepNewestOne(t *testing.T) {
	client := &fakeRotatorClient{listResp: []map[string]any{
		objKeyListEntry(10, "lbl"),
		objKeyListEntry(30, "lbl"),
		objKeyListEntry(20, "lbl"),
	}}
	var err error
	stdout, _ := captureFirewallOutput(t, func() {
		err = RunObjKeyRevokeOld(context.Background(), client, true, "lbl", 1)
	})
	if err != nil {
		t.Fatalf("keep_newest=1 must be accepted (it keeps the live key): %v", err)
	}
	rec := decodeRecord(t, stdout)
	kept, revoked := rec["kept_ids"].([]any), rec["revoked_ids"].([]any)
	if len(kept) != 1 || kept[0] != float64(30) {
		t.Errorf("kept = %v, want only the newest id 30", kept)
	}
	if len(revoked) != 2 || revoked[0] != float64(20) || revoked[1] != float64(10) {
		t.Errorf("revoked = %v, want 20 then 10 (newest-first order)", revoked)
	}
}
