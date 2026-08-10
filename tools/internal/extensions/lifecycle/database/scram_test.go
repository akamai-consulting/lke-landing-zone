package database

// scram_test.go — the DB half of what was ci_assert_obj_certs_db_test.go.
//
// SECOND TIME THIS ONE FILE HAS SPLIT. It is named for THREE unrelated subjects —
// obj, certs, db — so each extraction takes a third and leaves the rest. The certs
// tests went with assert-identity, the obj tests with assert-objstore, and these
// are the database ones. Nothing in the name pointed at a subject, which is why it
// had to be read three times to find out what was in it.

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

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
// A declared-but-half-seeded cluster must fail WITHOUT being probed: dialing with
// an empty password would return "rejected" and blame the database.
func TestProbeDBClustersHalfSeededFailsWithoutProbing(t *testing.T) {
	oCreds, oProbe := ReadCreds, pgProbeCredential
	t.Cleanup(func() { ReadCreds, pgProbeCredential = oCreds, oProbe })
	ReadCreds = func(context.Context, string) (dbAdminCreds, error) {
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
// No declared database is a clean SKIP: most deployments declare none, and a gate
// that failed on their absence would be switched off everywhere.
func TestRunAssertDatabaseSkipsWhenNoneDeclared(t *testing.T) {
	orig := ListClusters
	t.Cleanup(func() { ListClusters = orig })
	ListClusters = func(context.Context) ([]string, error) { return nil, nil }
	if err := RunAssertDatabase(time.Second, 0, time.Millisecond); err != nil {
		t.Errorf("no declared database must skip cleanly, got %v", err)
	}
}

func TestRunAssertDatabaseFailsOnRejectedCredential(t *testing.T) {
	oList, oCreds, oProbe := ListClusters, ReadCreds, pgProbeCredential
	t.Cleanup(func() { ListClusters, ReadCreds, pgProbeCredential = oList, oCreds, oProbe })
	ListClusters = func(context.Context) ([]string, error) { return []string{"pg1"}, nil }
	ReadCreds = func(context.Context, string) (dbAdminCreds, error) {
		return dbAdminCreds{Endpoint: "e", Port: "5432", Username: "u", Password: "p"}, nil
	}
	pgProbeCredential = func(_, _, _, _, _ string, _ time.Duration) (pgVerdict, string) {
		return pgRejected, "28P01"
	}
	err := RunAssertDatabase(time.Second, 0, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "pg1") {
		t.Errorf("a rejected credential must fail naming the cluster, got %v", err)
	}
}
