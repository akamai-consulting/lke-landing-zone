package main

// Tests for the three coverage gates found in the post-PR functional pass:
// assert-obj-roundtrip, assert-certificates and assert-database.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ── assert-obj-roundtrip ─────────────────────────────────────────────────────

func objSecretJSON(access, secret string) []byte {
	b, _ := json.Marshal(map[string]any{"data": map[string]string{
		"AWS_ACCESS_KEY_ID":     base64.StdEncoding.EncodeToString([]byte(access)),
		"AWS_SECRET_ACCESS_KEY": base64.StdEncoding.EncodeToString([]byte(secret)),
	}})
	return b
}

func TestDecodeSecretField(t *testing.T) {
	raw := objSecretJSON("AK", "SK")
	if v, err := decodeSecretField(raw, "AWS_ACCESS_KEY_ID"); err != nil || v != "AK" {
		t.Errorf("unexpected (%q,%v)", v, err)
	}
	if _, err := decodeSecretField(raw, "NOPE"); err == nil {
		t.Error("a missing key must be an error — ESO not having materialized it is a finding")
	}
	empty, _ := json.Marshal(map[string]any{"data": map[string]string{
		"AWS_ACCESS_KEY_ID": base64.StdEncoding.EncodeToString([]byte("  ")),
	}})
	if _, err := decodeSecretField(empty, "AWS_ACCESS_KEY_ID"); err == nil {
		t.Error("an empty credential must be an error, not an empty string handed to the signer")
	}
	if _, err := decodeSecretField([]byte(`nope`), "x"); err == nil {
		t.Error("an unparseable Secret must be an error")
	}
}

func TestParseObjConfig(t *testing.T) {
	loki := `
      storage:
        bucketnames: llz-loki-chunks
        s3:
          endpoint: us-ord-10.linodeobjects.com
          region: us-ord-10
`
	ep, bucket, err := parseObjConfig(loki)
	if err != nil || ep != "us-ord-10.linodeobjects.com" || bucket != "llz-loki-chunks" {
		t.Fatalf("unexpected (%q,%q,%v)", ep, bucket, err)
	}

	// The distribution registry spells them differently; both must be read as-is.
	harbor := `
storage:
  s3:
    regionendpoint: https://us-ord-10.linodeobjects.com
    bucket: llz-harbor-registry
`
	ep, bucket, err = parseObjConfig(harbor)
	if err != nil || ep != "us-ord-10.linodeobjects.com" || bucket != "llz-harbor-registry" {
		t.Fatalf("unexpected harbor parse (%q,%q,%v)", ep, bucket, err)
	}

	// THE refusal that matters. A config with a bucket but no endpoint must ERROR,
	// never fall back to a derived one — the derived endpoint is the value that was
	// already wrong when Loki and Harbor 404'd against buckets that existed.
	_, _, err = parseObjConfig("bucket: only-a-bucket\n")
	if err == nil {
		t.Fatal("a config with no endpoint must fail rather than derive one")
	}
	if !strings.Contains(err.Error(), "refusing to derive") {
		t.Errorf("the failure must say it refuses to derive an endpoint, got %q", err)
	}
	if _, _, err := parseObjConfig("nothing useful here\n"); err == nil {
		t.Error("a config with neither must fail")
	}
}

// Loki renders bucketNames as a NESTED mapping:
//
//	bucketnames:
//	  chunks: llz-loki-chunks
//
// A regex using \s* after the colon spans the newline and captures the next
// key ("chunks") as the bucket name.
func TestParseObjConfigNestedBucketNames(t *testing.T) {
	cfg := "storage:\n  bucketnames:\n    chunks: llz-loki-chunks\n  s3:\n    endpoint: us-ord-10.linodeobjects.com\n"
	ep, bucket, err := parseObjConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bucket == "chunks" {
		t.Fatalf("captured the KEY as the bucket name — \\s* spanned the newline (endpoint=%q)", ep)
	}
	if bucket != "llz-loki-chunks" {
		t.Errorf("bucket = %q, want llz-loki-chunks", bucket)
	}
}

// seamObjS3 replaces the signed HTTP layer with a fake object store.
type fakeObjStore struct {
	objects   map[string][]byte
	putStatus int
	putCode   string
	getStatus int
	// corrupt makes GET return different bytes than were written — the
	// wrong-endpoint signature.
	corrupt bool
	deletes int
}

func seamObjS3(t *testing.T, f *fakeObjStore) {
	orig := s3ObjectRequest
	t.Cleanup(func() { s3ObjectRequest = orig })
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	s3ObjectRequest = func(method, _, _, _, bucket, key string, payload []byte) (int, string, []byte, error) {
		full := bucket + "/" + key
		switch method {
		case http.MethodPut:
			if f.putStatus != 0 && f.putStatus/100 != 2 {
				return f.putStatus, f.putCode, nil, nil
			}
			f.objects[full] = payload
			return 200, "", nil, nil
		case http.MethodGet:
			if f.getStatus != 0 && f.getStatus/100 != 2 {
				return f.getStatus, "", nil, nil
			}
			b, ok := f.objects[full]
			if !ok {
				return 404, "NoSuchKey", nil, nil
			}
			if f.corrupt {
				return 200, "", []byte("different bytes entirely"), nil
			}
			return 200, "", b, nil
		case http.MethodDelete:
			f.deletes++
			delete(f.objects, full)
			return 204, "", nil, nil
		}
		return 400, "", nil, nil
	}
}

func TestS3ObjectRoundTrip(t *testing.T) {
	f := &fakeObjStore{}
	seamObjS3(t, f)
	r := s3ObjectRoundTrip("AK", "SK", "us-ord-10.linodeobjects.com", "b", "k", []byte("payload"))
	if !r.OK() || !r.Cleaned {
		t.Fatalf("a healthy bucket must round-trip and clean up: %+v", r)
	}
	if f.deletes != 1 {
		t.Errorf("the probe object must be deleted, got %d deletes", f.deletes)
	}
}

// A write that cannot be read back at the SAME endpoint is the disjoint-namespace
// failure, and it is the reason the gate reads back at all.
func TestS3ObjectRoundTripFailsWhenReadBackDiffers(t *testing.T) {
	f := &fakeObjStore{corrupt: true}
	seamObjS3(t, f)
	r := s3ObjectRoundTrip("AK", "SK", "ep", "b", "k", []byte("payload"))
	if r.OK() {
		t.Fatal("bytes that come back different must fail — a PUT can succeed against the wrong endpoint")
	}
	if !r.Wrote || r.ReadBack {
		t.Errorf("the verdict should record the write succeeded and the read did not: %+v", r)
	}
}

func TestS3ObjectRoundTripNoSuchBucketExplainsTheSplit(t *testing.T) {
	seamObjS3(t, &fakeObjStore{putStatus: 404, putCode: "NoSuchBucket"})
	r := s3ObjectRoundTrip("AK", "SK", "us-ord.linodeobjects.com", "llz-loki-chunks", "k", []byte("x"))
	if r.OK() {
		t.Fatal("NoSuchBucket must fail")
	}
	// The failure has to name the obj-cluster split, or the operator goes looking
	// at the Linode console, sees the bucket, and concludes the gate is wrong.
	if !strings.Contains(r.FailWhy, "gen-1") || !strings.Contains(r.FailWhy, "by label") {
		t.Errorf("the failure must explain that the bucket may exist at a different endpoint, got %q", r.FailWhy)
	}
}

func TestExplainS3Write(t *testing.T) {
	for _, tc := range []struct {
		code string
		want string
	}{
		{"AccessDenied", "not permitted to write"},
		{"InvalidAccessKeyId", "revoked or rotated"},
		{"SignatureDoesNotMatch", "revoked or rotated"},
	} {
		if got := explainS3Write(403, tc.code, "b", "e"); !strings.Contains(got, tc.want) {
			t.Errorf("%s: expected %q in %q", tc.code, tc.want, got)
		}
	}
}

func TestProbeObjConsumerFailsClosedOnMissingConfig(t *testing.T) {
	oSec, oCfg := readObjSecret, readObjConfig
	t.Cleanup(func() { readObjSecret, readObjConfig = oSec, oCfg })
	readObjSecret = func(string) ([]byte, error) { return objSecretJSON("AK", "SK"), nil }
	readObjConfig = func(string) (string, error) { return "", errors.New("NotFound") }

	v := probeObjConsumer(objConsumers[0], "p/", time.Now())
	if v.FailWhy == "" {
		t.Fatal("an unreadable consumer config must fail — deriving the endpoint is the bug")
	}
	if !strings.Contains(v.FailWhy, "derived endpoint is the value that was already wrong") {
		t.Errorf("the failure must say why it will not derive one, got %q", v.FailWhy)
	}

	// A missing credential Secret is equally fatal: the consumer cannot be writing.
	readObjSecret = func(string) ([]byte, error) { return nil, errors.New("NotFound") }
	if v := probeObjConsumer(objConsumers[0], "p/", time.Now()); v.FailWhy == "" {
		t.Error("a missing credential Secret must fail")
	}
}

// Both load-bearing writers must be covered, and each must read ITS OWN key
// names — normalizing them would mean re-deriving the consumer's contract.
func TestObjConsumersCoverLokiAndHarbor(t *testing.T) {
	byName := map[string]objConsumer{}
	for _, c := range objConsumers {
		byName[c.Name] = c
	}
	for _, want := range []string{"loki", "harbor"} {
		c, ok := byName[want]
		if !ok {
			t.Fatalf("%s is a load-bearing object-storage writer and must be gated", want)
		}
		if c.SecretRef == "" || c.AccessKeyField == "" || c.SecretKeyField == "" || len(c.ConfigRefs) == 0 {
			t.Errorf("%s consumer is under-specified: %+v", want, c)
		}
	}
	if byName["loki"].AccessKeyField == byName["harbor"].AccessKeyField {
		t.Error("Loki and Harbor spell their credential keys differently; sharing one field name means one of them is wrong")
	}
}

// ── assert-certificates ──────────────────────────────────────────────────────

func certListJSON(items ...string) []byte {
	return []byte(`{"items":[` + strings.Join(items, ",") + `]}`)
}

func certItemJSON(ns, name, ready, notAfter, reason string) string {
	return `{"metadata":{"name":"` + name + `","namespace":"` + ns + `"},
	  "status":{"notAfter":"` + notAfter + `","conditions":[{"type":"Ready","status":"` + ready + `","reason":"` + reason + `","message":"m"}]}}`
}

func TestEvalCertificates(t *testing.T) {
	now := time.Unix(1_720_000_000, 0).UTC()
	minLeft := 14 * 24 * time.Hour

	healthy := certItemJSON("llz-openbao", "openbao-tls", "True", now.Add(60*24*time.Hour).Format(time.RFC3339), "Ready")
	vs, err := evalCertificates(certListJSON(healthy), now, minLeft)
	if err != nil || vs[0].FailWhy != "" {
		t.Fatalf("a healthy cert must pass: %+v (%v)", vs, err)
	}

	// Broken ISSUANCE keeps cert-manager's own reason — that is the diagnosis.
	notReady := certItemJSON("ns", "bad", "False", "", "IssuerNotFound")
	vs, _ = evalCertificates(certListJSON(notReady), now, minLeft)
	if vs[0].FailWhy == "" || !strings.Contains(vs[0].FailWhy, "IssuerNotFound") {
		t.Errorf("a not-Ready cert must fail carrying the issuer's reason, got %q", vs[0].FailWhy)
	}

	// Broken RENEWAL is a DIFFERENT failure and must say so — the remedies differ.
	expiring := certItemJSON("ns", "soon", "True", now.Add(3*24*time.Hour).Format(time.RFC3339), "Ready")
	vs, _ = evalCertificates(certListJSON(expiring), now, minLeft)
	if vs[0].FailWhy == "" || !strings.Contains(vs[0].FailWhy, "renewal has stopped") {
		t.Errorf("an expiring cert must fail as a renewal problem, got %q", vs[0].FailWhy)
	}

	// Ready with no expiry is as blind as an expired one.
	noExpiry := certItemJSON("ns", "noexp", "True", "", "Ready")
	vs, _ = evalCertificates(certListJSON(noExpiry), now, minLeft)
	if vs[0].FailWhy == "" {
		t.Error("Ready with no notAfter must fail closed")
	}
	if _, err := evalCertificates([]byte(`nope`), now, minLeft); err == nil {
		t.Error("an unparseable list must be an error")
	}
}

// Zero Certificates is a failure: this platform issues its own CA chain, so none
// means cert-manager never reconciled — not that TLS is unused.
func TestRunAssertCertificatesFailsOnEmpty(t *testing.T) {
	orig := readCertificates
	t.Cleanup(func() { readCertificates = orig })
	readCertificates = func() ([]byte, error) { return []byte(`{"items":[]}`), nil }
	if err := runCIAssertCertificates(14, 0, time.Millisecond); err == nil {
		t.Error("no Certificates must fail rather than pass having examined nothing")
	}
}

func TestRunAssertCertificatesFailsOnNotReady(t *testing.T) {
	orig := readCertificates
	t.Cleanup(func() { readCertificates = orig })
	now := time.Now().UTC()
	readCertificates = func() ([]byte, error) {
		return certListJSON(
			certItemJSON("a", "good", "True", now.Add(90*24*time.Hour).Format(time.RFC3339), "Ready"),
			certItemJSON("b", "bad", "False", "", "Failed"),
		), nil
	}
	err := runCIAssertCertificates(14, 0, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "b/bad") {
		t.Errorf("the failure must name the unhealthy certificate, got %v", err)
	}
}

// ── assert-database ──────────────────────────────────────────────────────────

// TestScramClientProofMatchesRFC7677 pins the SCRAM chain to the published
// vector rather than to whatever this implementation happens to compute. Without
// it, a subtly wrong proof would make every database look REJECTED and send
// someone rotating a password that was fine.
func TestScramClientProofMatchesRFC7677(t *testing.T) {
	// RFC 7677 §3: user "user", password "pencil",
	// s=W22ZaJ0SNY7soEsUEjb6gQ==, i=4096,
	// client-first-bare n=user,r=rOprNGfwEbeRWgbNEkqO
	// server-first      r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=...,i=4096
	// client-final w/o proof c=biws,r=<combined nonce>
	// expected proof   dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ=
	const (
		salt64       = "W22ZaJ0SNY7soEsUEjb6gQ=="
		iterations   = 4096
		clientFirst  = "n=user,r=rOprNGfwEbeRWgbNEkqO"
		serverFirst  = "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
		finalNoProof = "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
		wantProof    = "dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	)
	salt, err := base64.StdEncoding.DecodeString(salt64)
	if err != nil {
		t.Fatalf("bad test vector: %v", err)
	}
	authMessage := clientFirst + "," + serverFirst + "," + finalNoProof
	if got := scramClientProof("pencil", string(salt), iterations, authMessage); got != wantProof {
		t.Errorf("SCRAM ClientProof does not match RFC 7677:\n got  %s\n want %s", got, wantProof)
	}
}

func TestParseScramServerFirst(t *testing.T) {
	nonce, salt, iters, err := parseScramServerFirst("r=abc123,s=" + base64.StdEncoding.EncodeToString([]byte("saltysalt")) + ",i=4096")
	if err != nil || nonce != "abc123" || salt != "saltysalt" || iters != 4096 {
		t.Fatalf("unexpected (%q,%q,%d,%v)", nonce, salt, iters, err)
	}
	for _, bad := range []string{"r=only-nonce", "s=notbase64!!!,r=a,i=1", "r=a,s=" + base64.StdEncoding.EncodeToString([]byte("s")) + ",i=zero"} {
		if _, _, _, err := parseScramServerFirst(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestPGErrorText(t *testing.T) {
	body := append([]byte{'S'}, "FATAL\x00"...)
	body = append(body, 'C')
	body = append(body, "28P01\x00"...)
	body = append(body, 'M')
	body = append(body, "password authentication failed\x00\x00"...)
	got := pgErrorText(body)
	if !strings.Contains(got, "28P01") || !strings.Contains(got, "password authentication failed") {
		t.Errorf("the server's own reason must survive, got %q", got)
	}
}

func TestDBAdminCredsMissingFields(t *testing.T) {
	full := dbAdminCreds{Endpoint: "e", Port: "5432", Username: "u", Password: "p"}
	if got := full.missingFields(); len(got) != 0 {
		t.Errorf("a complete record should report nothing missing, got %v", got)
	}
	if got := (dbAdminCreds{Endpoint: "e", Port: "5432"}).missingFields(); len(got) != 2 {
		t.Errorf("expected username+password missing, got %v", got)
	}
}

// REJECTED and UNREACHABLE must stay apart: one is a finding about the
// credential, the other sends someone to rotate a password that is fine.
func TestEvalDBProbeSeparatesRejectedFromUnreachable(t *testing.T) {
	if v := evalDBProbe("c", pgAuthenticated, "ok"); v.FailWhy != "" {
		t.Errorf("authenticated must pass: %s", v.FailWhy)
	}
	rej := evalDBProbe("c", pgRejected, "28P01 password authentication failed")
	if rej.FailWhy == "" || !strings.Contains(rej.FailWhy, "rotate-db-admin") {
		t.Errorf("a rejection must name the in-place-rotation cause, got %q", rej.FailWhy)
	}
	un := evalDBProbe("c", pgUnreachable, "i/o timeout")
	if un.FailWhy == "" || !strings.Contains(un.FailWhy, "NOT evidence about the credential") {
		t.Errorf("unreachable must NOT be reported as a credential failure, got %q", un.FailWhy)
	}
}

// A declared-but-half-seeded cluster must fail WITHOUT being probed: dialing with
// an empty password would return "rejected" and blame the database.
func TestProbeDBClustersHalfSeededFailsWithoutProbing(t *testing.T) {
	oCreds, oProbe := readDBCreds, pgProbeCredential
	t.Cleanup(func() { readDBCreds, pgProbeCredential = oCreds, oProbe })
	readDBCreds = func(context.Context, string) (dbAdminCreds, error) {
		return dbAdminCreds{Endpoint: "e", Port: "5432", Username: "u"}, nil // no password
	}
	probed := false
	pgProbeCredential = func(_, _, _, _, _ string, _ time.Duration) (pgVerdict, string) {
		probed = true
		return pgAuthenticated, ""
	}
	vs := probeDBClusters(context.Background(), []string{"pg1"}, time.Second)
	if len(failedDBs(vs)) != 1 {
		t.Fatalf("a half-seeded record must fail, got %+v", vs)
	}
	if probed {
		t.Error("it must not dial with an empty password — that would blame the database for a seeding gap")
	}
	if !strings.Contains(vs[0].FailWhy, "half-seeded") {
		t.Errorf("the failure must name the half-seeded state, got %q", vs[0].FailWhy)
	}
}

// No declared database is a clean SKIP: most deployments declare none, and a gate
// that failed on their absence would be switched off everywhere.
func TestRunAssertDatabaseSkipsWhenNoneDeclared(t *testing.T) {
	orig := listDBClusters
	t.Cleanup(func() { listDBClusters = orig })
	listDBClusters = func(context.Context) ([]string, error) { return nil, nil }
	if err := runCIAssertDatabase(time.Second, 0, time.Millisecond); err != nil {
		t.Errorf("no declared database must skip cleanly, got %v", err)
	}
}

func TestRunAssertDatabaseFailsOnRejectedCredential(t *testing.T) {
	oList, oCreds, oProbe := listDBClusters, readDBCreds, pgProbeCredential
	t.Cleanup(func() { listDBClusters, readDBCreds, pgProbeCredential = oList, oCreds, oProbe })
	listDBClusters = func(context.Context) ([]string, error) { return []string{"pg1"}, nil }
	readDBCreds = func(context.Context, string) (dbAdminCreds, error) {
		return dbAdminCreds{Endpoint: "e", Port: "5432", Username: "u", Password: "p"}, nil
	}
	pgProbeCredential = func(_, _, _, _, _ string, _ time.Duration) (pgVerdict, string) {
		return pgRejected, "28P01"
	}
	err := runCIAssertDatabase(time.Second, 0, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "pg1") {
		t.Errorf("a rejected credential must fail naming the cluster, got %v", err)
	}
}
