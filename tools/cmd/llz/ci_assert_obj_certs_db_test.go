package main

// Tests for the three coverage gates found in the post-PR functional pass:
// assert-obj-roundtrip, assert-certificates and assert-database.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/objenc"
)

// ── assert-obj-roundtrip ─────────────────────────────────────────────────────

func objSecretJSON(access, secret string) []byte {
	b, _ := json.Marshal(map[string]any{"data": map[string]string{
		"AWS_ACCESS_KEY_ID":     base64.StdEncoding.EncodeToString([]byte(access)),
		"AWS_SECRET_ACCESS_KEY": base64.StdEncoding.EncodeToString([]byte(secret)),
	}})
	return b
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
		if got := objenc.ExplainS3Write(403, tc.code, "b", "e"); !strings.Contains(got, tc.want) {
			t.Errorf("%s: expected %q in %q", tc.code, tc.want, got)
		}
	}
}

func certListJSON(items ...string) []byte {
	return []byte(`{"items":[` + strings.Join(items, ",") + `]}`)
}

func certItemJSON(ns, name, ready, notAfter, reason string) string {
	return `{"metadata":{"name":"` + name + `","namespace":"` + ns + `"},
	  "status":{"notAfter":"` + notAfter + `","conditions":[{"type":"Ready","status":"` + ready + `","reason":"` + reason + `","message":"m"}]}}`
}

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
