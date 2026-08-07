package main

// credentials_cobra_test.go — the credentials tests that assert against the LIVE
// cobra tree, and therefore stayed.
//
// Same call as internal/docsguard's six and internal/manifestguard's one: only
// package main can build the command tree, so a test that walks `llz credentials`
// and its subcommands has to live here even though every rotator it reaches now
// lives in internal/credrotate.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/credrotate"
)

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

	origPAT, origObj := credrotate.NewPATClient, credrotate.NewObjKeyClient
	defer func() { credrotate.NewPATClient, credrotate.NewObjKeyClient = origPAT, origObj }()
	fake := &fakeRotatorClient{createResp: map[string]any{
		"id": json.Number("1"), "token": "t", "access_key": "a", "secret_key": "s",
	}}
	var gotToken string
	credrotate.NewPATClient = func(token string) credrotate.PATAPI { gotToken = token; return fake }
	credrotate.NewObjKeyClient = func(token string) credrotate.ObjKeyAPI { gotToken = token; return fake }

	run := func(args ...string) string {
		t.Helper()
		c := credrotate.CredentialsCmd()
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

// fakeRotatorClient is a COPY of internal/credrotate's test fake.
//
// A test fixture that exists to be reachable from two packages would have to be
// EXPORTED from a production package, which puts a symbol in an API for no runtime
// reason. Fixtures travel by copy — the same call made for withBaoReadSeam,
// withKubectl and every localised printer in this campaign.
// fakeRotatorClient implements both PATAPI and ObjKeyAPI.
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

func jn(i int) json.Number { return json.Number(strconv.Itoa(i)) }

func TestIsDue(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	day := int64(86400)
	for _, tc := range []struct {
		name      string
		rotatedAt string
		after     int
		want      bool
	}{
		{"never rotated (empty) is due", "", 80, true},
		{"unparseable is due", "not-a-ts", 80, true},
		{"recently rotated is not due", strconvI(now.Unix() - 10*day), 80, false},
		{"exactly at threshold is due", strconvI(now.Unix() - 80*day), 80, true},
		{"past threshold is due", strconvI(now.Unix() - 365*day), 80, true},
	} {
		if got := credrotate.IsDue(tc.rotatedAt, now, tc.after); got != tc.want {
			t.Errorf("%s: credrotate.IsDue=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// stubLinode is a COPY of internal/credrotate's test fake — fixtures travel by
// copy rather than being exported from a production package.
type stubLinode struct {
	pats, objkeys []map[string]any
	deleted       []uint64
	verifyErr     error
	patCreates    int
	objCreates    int
}

func (s *stubLinode) ListProfileTokens(context.Context) ([]map[string]any, error) { return s.pats, nil }
func (s *stubLinode) CreateProfileToken(context.Context, string, string, string) (map[string]any, error) {
	s.patCreates++
	return map[string]any{"id": 100 + s.patCreates, "token": "new-pat"}, nil
}
func (s *stubLinode) DeleteProfileToken(_ context.Context, id uint64) error {
	s.deleted = append(s.deleted, id)
	return nil
}
func (s *stubLinode) ListObjectStorageKeys(context.Context) ([]map[string]any, error) {
	return s.objkeys, nil
}
func (s *stubLinode) CreateObjectStorageKeyBuckets(context.Context, string, string, []string, string) (map[string]any, error) {
	s.objCreates++
	// id as json.Number — the only numeric type cli.AsUint64 accepts, mirroring
	// how the real client decodes API responses.
	return map[string]any{"id": jn(200 + s.objCreates), "access_key": "AK", "secret_key": "SK"}, nil
}
func (s *stubLinode) DeleteObjectStorageKey(_ context.Context, id uint64) error {
	s.deleted = append(s.deleted, id)
	return nil
}
func (s *stubLinode) Verify(context.Context) error { return s.verifyErr }

// stubBao is a COPY, same reasoning as stubLinode above.
type stubBao struct{ data map[string]map[string]string }

func (b *stubBao) Get(_ context.Context, path, key string) (string, bool, error) {
	v, ok := b.data[path][key]
	return v, ok, nil
}
func (b *stubBao) Write(_ context.Context, path string, d map[string]string) error {
	b.data[path] = d
	return nil
}
func withRotatorStubs(t *testing.T, lc credrotate.LinodeAPI, bao credrotate.BaoStore, now time.Time) {
	t.Helper()
	ol, ob, on := credrotate.NewLinodeClient, credrotate.NewBaoStore, credrotate.Now
	credrotate.NewLinodeClient = func(string) credrotate.LinodeAPI { return lc }
	credrotate.NewBaoStore = func(context.Context) (credrotate.BaoStore, error) { return bao, nil }
	credrotate.Now = func() time.Time { return now }
	t.Cleanup(func() { credrotate.NewLinodeClient, credrotate.NewBaoStore, credrotate.Now = ol, ob, on })
}
func strconvI(n int64) string { return strconv.FormatInt(n, 10) }

// jn mirrors how the Linode client decodes ids — json.Number, the only type
// cli.AsUint64 accepts.
