package main

// Tests for `llz credentials` (credentials.go / credentials_pat.go /
// credentials_objkey.go) — the rotation logic folded in from the former
// linode-pat-rotator / linode-obj-key-rotator binaries. Fake clients stand in
// for the Linode API via the newPATRotatorClient / newObjKeyRotatorClient
// seams; stdout JSON + ::add-mask:: stderr behavior are the action-facing
// contract under test.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// fakeRotatorClient implements both patAPI and objKeyAPI.
type fakeRotatorClient struct {
	createResp map[string]any
	createErr  error
	listResp   []map[string]any
	listErr    error
	deleteErr  error

	createdLabel, createdScopes, createdExpiry        string
	createdCluster, createdBucket, createdPermissions string
	deletedIDs                                        []uint64
}

func (f *fakeRotatorClient) CreateProfileToken(_ context.Context, label, scopes, expiry string) (map[string]any, error) {
	f.createdLabel, f.createdScopes, f.createdExpiry = label, scopes, expiry
	return f.createResp, f.createErr
}

func (f *fakeRotatorClient) ListProfileTokens(context.Context) ([]map[string]any, error) {
	return f.listResp, f.listErr
}

func (f *fakeRotatorClient) DeleteProfileToken(_ context.Context, id uint64) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletedIDs = append(f.deletedIDs, id)
	return nil
}

func (f *fakeRotatorClient) CreateObjectStorageKey(_ context.Context, label, cluster, bucket, permissions string) (map[string]any, error) {
	f.createdLabel, f.createdCluster, f.createdBucket, f.createdPermissions = label, cluster, bucket, permissions
	return f.createResp, f.createErr
}

func (f *fakeRotatorClient) ListObjectStorageKeys(context.Context) ([]map[string]any, error) {
	return f.listResp, f.listErr
}

func (f *fakeRotatorClient) DeleteObjectStorageKey(_ context.Context, id uint64) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletedIDs = append(f.deletedIDs, id)
	return nil
}

// decodeRecord parses the single JSON record a subcommand printed on stdout.
func decodeRecord(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rec); err != nil {
		t.Fatalf("stdout is not one JSON record: %v\n%s", err, stdout)
	}
	return rec
}

// ── rotatorOpts ───────────────────────────────────────────────────────────────

func TestRotatorOptsResolve(t *testing.T) {
	t.Setenv("LINODE_TOKEN", "")
	t.Setenv("ROTATION_APPLY", "")

	if _, _, err := (&rotatorOpts{}).resolve(); err == nil || !strings.Contains(err.Error(), "Linode PAT is required") {
		t.Fatalf("want missing-token error, got %v", err)
	}

	t.Setenv("LINODE_TOKEN", "env-token")
	tok, apply, err := (&rotatorOpts{}).resolve()
	if err != nil || tok != "env-token" || apply {
		t.Fatalf("env defaults: got (%q, %v, %v)", tok, apply, err)
	}

	t.Setenv("ROTATION_APPLY", "true")
	if _, apply, _ = (&rotatorOpts{}).resolve(); !apply {
		t.Fatal("ROTATION_APPLY=true should arm")
	}

	// Flags override env.
	tok, apply, err = (&rotatorOpts{token: "flag-token", apply: true}).resolve()
	if err != nil || tok != "flag-token" || !apply {
		t.Fatalf("flag overrides: got (%q, %v, %v)", tok, apply, err)
	}
}

// ── pat create ────────────────────────────────────────────────────────────────

func TestCredentialsPATCreateValidation(t *testing.T) {
	for _, days := range []int64{91, 0, -3} {
		if err := runCredentialsPATCreate(context.Background(), &fakeRotatorClient{}, true, "l", "s", days, "", nil); err == nil {
			t.Errorf("validity-days=%d: want error, got nil", days)
		}
	}
}

func TestCredentialsPATCreateDryRun(t *testing.T) {
	client := &fakeRotatorClient{}
	var err error
	stdout, stderr := captureFirewallOutput(t, func() {
		err = runCredentialsPATCreate(context.Background(), client, false, "lbl", "scopes:read", 90, "", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := decodeRecord(t, stdout)
	if rec["event"] != "linode-pat-rotator.create" || rec["dry_run"] != true {
		t.Errorf("unexpected record: %v", rec)
	}
	if rec["expiry_planned"] == "" || rec["label"] != "lbl" {
		t.Errorf("unexpected record fields: %v", rec)
	}
	if client.createdLabel != "" {
		t.Error("dry-run must not call CreateProfileToken")
	}
	if strings.Contains(stderr, "::add-mask::") {
		t.Error("dry-run must not emit ::add-mask::")
	}
}

func TestCredentialsPATCreateApply(t *testing.T) {
	client := &fakeRotatorClient{createResp: map[string]any{
		"id":     json.Number("4242"),
		"token":  "sekret-token",
		"expiry": "2026-09-01T00:00:00",
	}}
	var err error
	stdout, stderr := captureFirewallOutput(t, func() {
		err = runCredentialsPATCreate(context.Background(), client, true, "lbl", "scopes:read", 30, "", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "::add-mask::sekret-token") {
		t.Errorf("stderr must ::add-mask:: the new token, got:\n%s", stderr)
	}
	rec := decodeRecord(t, stdout)
	if rec["new_pat_id"] != float64(4242) || rec["new_token"] != "sekret-token" || rec["dry_run"] != false {
		t.Errorf("unexpected record: %v", rec)
	}
	if client.createdLabel != "lbl" || client.createdScopes != "scopes:read" {
		t.Errorf("create called with (%q, %q)", client.createdLabel, client.createdScopes)
	}
	if _, ok := linode.ParseTS(client.createdExpiry); !ok {
		t.Errorf("expiry %q is not a Linode timestamp", client.createdExpiry)
	}
}

func TestCredentialsPATCreateBadResponses(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]any
		err  error
		want string
	}{
		{"api error", nil, fmt.Errorf("boom"), "boom"},
		{"missing id", map[string]any{"token": "t"}, nil, "missing .id"},
		{"missing token", map[string]any{"id": json.Number("1")}, nil, "missing .token"},
	}
	for _, tc := range cases {
		client := &fakeRotatorClient{createResp: tc.resp, createErr: tc.err}
		var err error
		captureFirewallOutput(t, func() {
			err = runCredentialsPATCreate(context.Background(), client, true, "l", "s", 30, "", nil)
		})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", tc.name, tc.want, err)
		}
	}
}

// ── pat revoke-old ────────────────────────────────────────────────────────────

func patListEntry(id int, label, created string) map[string]any {
	return map[string]any{"id": json.Number(fmt.Sprint(id)), "label": label, "created": created}
}

func TestCredentialsPATRevokeOldGraceValidation(t *testing.T) {
	if err := runCredentialsPATRevokeOld(context.Background(), &fakeRotatorClient{}, true, "l", -1); err == nil {
		t.Fatal("grace-days=-1: want error")
	}
}

func TestCredentialsPATRevokeOldNoMatches(t *testing.T) {
	client := &fakeRotatorClient{listResp: []map[string]any{patListEntry(1, "other", "2020-01-01T00:00:00")}}
	var err error
	stdout, _ := captureFirewallOutput(t, func() {
		err = runCredentialsPATRevokeOld(context.Background(), client, true, "lbl", 7)
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := decodeRecord(t, stdout)
	if rec["kept_pat_id"] != nil || len(rec["revoked_ids"].([]any)) != 0 {
		t.Errorf("unexpected record: %v", rec)
	}
}

func TestCredentialsPATRevokeOldDrains(t *testing.T) {
	// Newest (id 3, today-ish) is kept; id 2 is in the grace window; id 1 is
	// old enough to drain. Entries are deliberately unsorted.
	client := &fakeRotatorClient{listResp: []map[string]any{
		patListEntry(1, "lbl", "2020-01-01T00:00:00"),
		patListEntry(3, "lbl", "2099-01-10T00:00:00"),
		patListEntry(2, "lbl", "2099-01-09T00:00:00"),
		patListEntry(9, "other", "2099-01-08T00:00:00"),
		{"id": json.Number("8"), "label": "lbl", "created": "not-a-timestamp"},
	}}
	var err error
	stdout, _ := captureFirewallOutput(t, func() {
		err = runCredentialsPATRevokeOld(context.Background(), client, true, "lbl", 7)
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := decodeRecord(t, stdout)
	if rec["kept_pat_id"] != float64(3) {
		t.Errorf("kept_pat_id = %v, want 3", rec["kept_pat_id"])
	}
	if got := fmt.Sprint(rec["revoked_ids"]); got != "[1]" {
		t.Errorf("revoked_ids = %v, want [1]", rec["revoked_ids"])
	}
	if got := fmt.Sprint(rec["skipped_in_grace_ids"]); got != "[2]" {
		t.Errorf("skipped_in_grace_ids = %v, want [2]", rec["skipped_in_grace_ids"])
	}
	if len(client.deletedIDs) != 1 || client.deletedIDs[0] != 1 {
		t.Errorf("deleted %v, want [1]", client.deletedIDs)
	}
}

func TestCredentialsPATRevokeOldDryRunDeletesNothing(t *testing.T) {
	client := &fakeRotatorClient{listResp: []map[string]any{
		patListEntry(1, "lbl", "2020-01-01T00:00:00"),
		patListEntry(2, "lbl", "2099-01-01T00:00:00"),
	}}
	var err error
	stdout, _ := captureFirewallOutput(t, func() {
		err = runCredentialsPATRevokeOld(context.Background(), client, false, "lbl", 7)
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := decodeRecord(t, stdout)
	if rec["dry_run"] != true || fmt.Sprint(rec["revoked_ids"]) != "[1]" {
		t.Errorf("unexpected record: %v", rec)
	}
	if len(client.deletedIDs) != 0 {
		t.Errorf("dry-run deleted %v", client.deletedIDs)
	}
}

func TestCredentialsPATRevokeOldErrors(t *testing.T) {
	client := &fakeRotatorClient{listErr: fmt.Errorf("list boom")}
	var err error
	captureFirewallOutput(t, func() {
		err = runCredentialsPATRevokeOld(context.Background(), client, true, "lbl", 7)
	})
	if err == nil || !strings.Contains(err.Error(), "list boom") {
		t.Fatalf("want list error, got %v", err)
	}

	client = &fakeRotatorClient{
		listResp: []map[string]any{
			patListEntry(1, "lbl", "2020-01-01T00:00:00"),
			patListEntry(2, "lbl", "2099-01-01T00:00:00"),
		},
		deleteErr: fmt.Errorf("delete boom"),
	}
	captureFirewallOutput(t, func() {
		err = runCredentialsPATRevokeOld(context.Background(), client, true, "lbl", 7)
	})
	if err == nil || !strings.Contains(err.Error(), "delete boom") {
		t.Fatalf("want delete error, got %v", err)
	}
}

// ── obj-key create ────────────────────────────────────────────────────────────

func TestCredentialsObjKeyCreateDryRun(t *testing.T) {
	client := &fakeRotatorClient{}
	var err error
	stdout, stderr := captureFirewallOutput(t, func() {
		err = runCredentialsObjKeyCreate(context.Background(), client, false, "lbl", "us-ord-10", "bkt", "read_write", "", "", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := decodeRecord(t, stdout)
	if rec["event"] != "linode-obj-key-rotator.create" || rec["dry_run"] != true || rec["bucket_name"] != "bkt" {
		t.Errorf("unexpected record: %v", rec)
	}
	if client.createdLabel != "" {
		t.Error("dry-run must not call CreateObjectStorageKey")
	}
	if strings.Contains(stderr, "::add-mask::") {
		t.Error("dry-run must not emit ::add-mask::")
	}
}

func TestCredentialsObjKeyCreateApply(t *testing.T) {
	client := &fakeRotatorClient{createResp: map[string]any{
		"id":         json.Number("77"),
		"access_key": "AKIAFAKE",
		"secret_key": "sekret-half",
	}}
	var err error
	stdout, stderr := captureFirewallOutput(t, func() {
		err = runCredentialsObjKeyCreate(context.Background(), client, true, "lbl", "us-ord-10", "bkt", "read_write", "", "", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Both halves must be masked before the record reaches stdout.
	if !strings.Contains(stderr, "::add-mask::AKIAFAKE") || !strings.Contains(stderr, "::add-mask::sekret-half") {
		t.Errorf("stderr must mask both key halves, got:\n%s", stderr)
	}
	rec := decodeRecord(t, stdout)
	if rec["new_obj_key_id"] != float64(77) || rec["new_access_key"] != "AKIAFAKE" || rec["new_secret_key"] != "sekret-half" {
		t.Errorf("unexpected record: %v", rec)
	}
	if client.createdCluster != "us-ord-10" || client.createdBucket != "bkt" || client.createdPermissions != "read_write" {
		t.Errorf("create called with (%q, %q, %q)", client.createdCluster, client.createdBucket, client.createdPermissions)
	}
}

func TestCredentialsObjKeyCreateBadResponses(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]any
		err  error
		want string
	}{
		{"api error", nil, fmt.Errorf("boom"), "boom"},
		{"missing id", map[string]any{"access_key": "a", "secret_key": "s"}, nil, "missing .id"},
		{"missing access", map[string]any{"id": json.Number("1"), "secret_key": "s"}, nil, "missing .access_key"},
		{"missing secret", map[string]any{"id": json.Number("1"), "access_key": "a"}, nil, "missing .secret_key"},
	}
	for _, tc := range cases {
		client := &fakeRotatorClient{createResp: tc.resp, createErr: tc.err}
		var err error
		captureFirewallOutput(t, func() {
			err = runCredentialsObjKeyCreate(context.Background(), client, true, "l", "c", "b", "read_write", "", "", nil)
		})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", tc.name, tc.want, err)
		}
	}
}

// ── obj-key revoke-old ────────────────────────────────────────────────────────

func objKeyListEntry(id int, label string) map[string]any {
	return map[string]any{"id": json.Number(fmt.Sprint(id)), "label": label}
}

func TestCredentialsObjKeyRevokeOldKeepValidation(t *testing.T) {
	for _, keep := range []int64{0, -1} {
		if err := runCredentialsObjKeyRevokeOld(context.Background(), &fakeRotatorClient{}, true, "l", keep); err == nil {
			t.Errorf("keep-newest=%d: want error, got nil", keep)
		}
	}
}

func TestCredentialsObjKeyRevokeOldNoMatches(t *testing.T) {
	client := &fakeRotatorClient{listResp: []map[string]any{objKeyListEntry(5, "other")}}
	var err error
	stdout, _ := captureFirewallOutput(t, func() {
		err = runCredentialsObjKeyRevokeOld(context.Background(), client, true, "lbl", 2)
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := decodeRecord(t, stdout)
	if len(rec["kept_ids"].([]any)) != 0 || len(rec["revoked_ids"].([]any)) != 0 {
		t.Errorf("unexpected record: %v", rec)
	}
}

func TestCredentialsObjKeyRevokeOldKeepsNewestN(t *testing.T) {
	client := &fakeRotatorClient{listResp: []map[string]any{
		objKeyListEntry(10, "lbl"),
		objKeyListEntry(30, "lbl"),
		objKeyListEntry(20, "lbl"),
		objKeyListEntry(99, "other"),
	}}
	var err error
	stdout, _ := captureFirewallOutput(t, func() {
		err = runCredentialsObjKeyRevokeOld(context.Background(), client, true, "lbl", 2)
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := decodeRecord(t, stdout)
	if got := fmt.Sprint(rec["kept_ids"]); got != "[30 20]" {
		t.Errorf("kept_ids = %v, want [30 20]", rec["kept_ids"])
	}
	if got := fmt.Sprint(rec["revoked_ids"]); got != "[10]" {
		t.Errorf("revoked_ids = %v, want [10]", rec["revoked_ids"])
	}
	if len(client.deletedIDs) != 1 || client.deletedIDs[0] != 10 {
		t.Errorf("deleted %v, want [10]", client.deletedIDs)
	}
}

func TestCredentialsObjKeyRevokeOldKeepLargerThanSet(t *testing.T) {
	client := &fakeRotatorClient{listResp: []map[string]any{objKeyListEntry(1, "lbl")}}
	var err error
	stdout, _ := captureFirewallOutput(t, func() {
		err = runCredentialsObjKeyRevokeOld(context.Background(), client, false, "lbl", 5)
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := decodeRecord(t, stdout)
	if got := fmt.Sprint(rec["kept_ids"]); got != "[1]" {
		t.Errorf("kept_ids = %v, want [1]", rec["kept_ids"])
	}
	if len(client.deletedIDs) != 0 {
		t.Errorf("deleted %v, want none", client.deletedIDs)
	}
}

func TestCredentialsObjKeyRevokeOldErrors(t *testing.T) {
	var err error
	captureFirewallOutput(t, func() {
		err = runCredentialsObjKeyRevokeOld(context.Background(), &fakeRotatorClient{listErr: fmt.Errorf("list boom")}, true, "lbl", 2)
	})
	if err == nil || !strings.Contains(err.Error(), "list boom") {
		t.Fatalf("want list error, got %v", err)
	}

	client := &fakeRotatorClient{
		listResp:  []map[string]any{objKeyListEntry(1, "lbl"), objKeyListEntry(2, "lbl"), objKeyListEntry(3, "lbl")},
		deleteErr: fmt.Errorf("delete boom"),
	}
	captureFirewallOutput(t, func() {
		err = runCredentialsObjKeyRevokeOld(context.Background(), client, true, "lbl", 2)
	})
	if err == nil || !strings.Contains(err.Error(), "delete boom") {
		t.Fatalf("want delete error, got %v", err)
	}
}

// ── cobra wiring (flags + env defaults reach the run funcs) ───────────────────

func TestCredentialsCommandWiring(t *testing.T) {
	t.Setenv("LINODE_TOKEN", "tkn")
	t.Setenv("ROTATION_APPLY", "")
	t.Setenv("PAT_LABEL", "")
	t.Setenv("PAT_SCOPES", "")
	t.Setenv("PAT_VALIDITY_DAYS", "")
	t.Setenv("PAT_GRACE_DAYS", "")
	t.Setenv("OBJ_LABEL", "")
	t.Setenv("OBJ_BUCKET_CLUSTER", "")
	t.Setenv("OBJ_BUCKET_NAME", "")
	t.Setenv("OBJ_BUCKET_PERMISSIONS", "")
	t.Setenv("OBJ_KEEP_NEWEST", "")

	origPAT, origObj := newPATRotatorClient, newObjKeyRotatorClient
	defer func() { newPATRotatorClient, newObjKeyRotatorClient = origPAT, origObj }()
	fake := &fakeRotatorClient{createResp: map[string]any{
		"id": json.Number("1"), "token": "t", "access_key": "a", "secret_key": "s",
	}}
	var gotToken string
	newPATRotatorClient = func(token string) patAPI { gotToken = token; return fake }
	newObjKeyRotatorClient = func(token string) objKeyAPI { gotToken = token; return fake }

	run := func(args ...string) string {
		t.Helper()
		c := credentialsCmd()
		c.SetArgs(args)
		var err error
		stdout, _ := captureFirewallOutput(t, func() { err = c.Execute() })
		if err != nil {
			t.Fatalf("llz credentials %s: %v", strings.Join(args, " "), err)
		}
		return stdout
	}

	rec := decodeRecord(t, run("pat", "create", "--label", "L", "--scopes", "S", "--validity-days", "30", "--apply"))
	if rec["dry_run"] != false || fake.createdLabel != "L" || fake.createdScopes != "S" || gotToken != "tkn" {
		t.Errorf("pat create wiring: rec=%v label=%q scopes=%q token=%q", rec, fake.createdLabel, fake.createdScopes, gotToken)
	}

	fake.listResp = nil
	rec = decodeRecord(t, run("pat", "revoke-old", "--label", "L", "--grace-days", "9"))
	if rec["grace_days"] != float64(9) || rec["dry_run"] != true {
		t.Errorf("pat revoke-old wiring: %v", rec)
	}

	rec = decodeRecord(t, run("obj-key", "create", "--label", "L", "--bucket-cluster", "C", "--bucket-name", "B"))
	if rec["bucket_permissions"] != "read_write" || rec["dry_run"] != true {
		t.Errorf("obj-key create wiring: %v", rec)
	}

	rec = decodeRecord(t, run("obj-key", "revoke-old", "--label", "L", "--keep-newest", "3"))
	if rec["keep_newest"] != float64(3) {
		t.Errorf("obj-key revoke-old wiring: %v", rec)
	}

	// Env-var defaults (the composite action sets these instead of flags).
	t.Setenv("OBJ_LABEL", "envL")
	t.Setenv("OBJ_BUCKET_CLUSTER", "envC")
	t.Setenv("OBJ_BUCKET_NAME", "envB")
	t.Setenv("OBJ_BUCKET_PERMISSIONS", "read_only")
	rec = decodeRecord(t, run("obj-key", "create"))
	if rec["label"] != "envL" || rec["bucket_cluster"] != "envC" || rec["bucket_name"] != "envB" || rec["bucket_permissions"] != "read_only" {
		t.Errorf("obj-key env defaults: %v", rec)
	}
}

func TestWriteRotatedSecret(t *testing.T) {
	type call struct{ name, env, val string }
	var got []call
	orig := ghSetSecretFn
	ghSetSecretFn = func(name, ghEnv, value string) error {
		got = append(got, call{name, ghEnv, value})
		return nil
	}
	defer func() { ghSetSecretFn = orig }()

	// Writes the value into infra-<deployment> for EACH deployment.
	got = nil
	if err := writeRotatedSecret("LINODE_API_TOKEN", "tok", []string{"lab", "prod"}); err != nil {
		t.Fatalf("writeRotatedSecret: %v", err)
	}
	want := []call{{"LINODE_API_TOKEN", "infra-lab", "tok"}, {"LINODE_API_TOKEN", "infra-prod", "tok"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("per-env writes = %v, want %v", got, want)
	}

	// No deployments → a single repo-level write (ghEnv "").
	got = nil
	if err := writeRotatedSecret("LINODE_API_TOKEN", "tok", nil); err != nil {
		t.Fatalf("writeRotatedSecret repo: %v", err)
	}
	if len(got) != 1 || got[0].env != "" {
		t.Errorf("repo-level write = %v, want one call with empty env", got)
	}

	// A per-env failure is wrapped with the env for context, and stops the loop.
	ghSetSecretFn = func(_, ghEnv, _ string) error {
		if ghEnv == "infra-prod" {
			return fmt.Errorf("boom")
		}
		return nil
	}
	err := writeRotatedSecret("X", "v", []string{"lab", "prod", "stg"})
	if err == nil || !strings.Contains(err.Error(), "infra-prod") {
		t.Errorf("want error naming infra-prod, got %v", err)
	}
}
