package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// The drain is non-fatal by design, so its outcome is reported ONLY through the
// warning + summary line. Emitting those on a drain that actually succeeded
// tells an operator that old privileged tokens are still live when they are
// not — the one thing the monthly cadence relies on being trustworthy.
func TestRotateInclusterPATDoesNotReportASuccessfulDrainAsFailed(t *testing.T) {
	sum := inclusterPATEnv(t, "broad-pat")
	oidcServer(t)
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.Contains(a, "get pod "+rootOpenbaoPod) {
			return nil, nil
		}
		return nil, errors.New("unexpected: " + a)
	})
	now := time.Now()
	s := &patMintStub{stubLinode: stubLinode{pats: []map[string]any{
		{"label": "llz-incluster-primary", "id": jn(7), "created": linode.FmtLinodeTS(now.Unix() - 30*linode.DaySecs)},
		{"label": "llz-incluster-primary", "id": jn(101), "created": linode.FmtLinodeTS(now.Unix())},
	}}}
	withInclusterPATStubs(t, s, now)
	stubInclusterBaoExec(t, "", "propagator-token")

	var err error
	_, stderr := captureFirewallOutput(t, func() { err = runCIRotateInclusterPAT() })
	if err != nil {
		t.Fatalf("rotate-incluster-pat: %v", err)
	}
	if len(s.deleted) != 1 || s.deleted[0] != 7 {
		t.Fatalf("drain must have succeeded and revoked the old sibling, got %v", s.deleted)
	}
	if strings.Contains(stderr, "drain of old") {
		t.Errorf("a successful drain must not warn:\n%s", stderr)
	}
	summary, _ := os.ReadFile(sum)
	if strings.Contains(string(summary), "Drain of older") {
		t.Errorf("a successful drain must not be recorded as a failure:\n%s", summary)
	}
	if !strings.Contains(string(summary), "new_pat_id=`101`") {
		t.Errorf("summary missing the audit line:\n%s", summary)
	}
}
