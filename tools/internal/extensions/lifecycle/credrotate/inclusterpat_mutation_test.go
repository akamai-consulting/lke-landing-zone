package credrotate

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// The drain is non-fatal by design, so its outcome is reported ONLY through the
// warning + summary line. Emitting those on a drain that actually succeeded
// tells an operator that old privileged tokens are still live when they are
// not — the one thing the monthly cadence relies on being trustworthy.
func TestRotateInclusterPATDoesNotReportASuccessfulDrainAsFailed(t *testing.T) {
	sum := inclusterPATEnv(t, "broad-pat")
	oidcServer(t)
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.Contains(a, "get pod "+baoread.RootPod) {
			return nil, nil
		}
		return nil, errors.New("unexpected: " + a)
	})
	// THE OLD SIBLING HERE IS THE ONE THAT WAS JUST SUPERSEDED, so nothing may
	// revoke it yet — see below. Id 6 is a genuinely drainable one: superseded 30
	// days ago, when id 7 was minted.
	now := time.Now()
	s := &patMintStub{stubLinode: stubLinode{pats: []map[string]any{
		{"label": "llz-incluster-acme-primary", "id": jn(6), "created": linode.FmtLinodeTS(now.Unix() - 60*linode.DaySecs)},
		{"label": "llz-incluster-acme-primary", "id": jn(7), "created": linode.FmtLinodeTS(now.Unix() - 30*linode.DaySecs)},
		{"label": "llz-incluster-acme-primary", "id": jn(101), "created": linode.FmtLinodeTS(now.Unix())},
	}}}
	withInclusterPATStubs(t, s, now)
	stubInclusterBaoExec(t, "", "propagator-token")

	var err error
	_, stderr := captureFirewallOutput(t, func() { err = RunRotateInClusterPAT() })
	if err != nil {
		t.Fatalf("rotate-incluster-pat: %v", err)
	}
	// THIS ASSERTION USED TO READ `s.deleted == [7]` AND PINNED THE DEFECT. Id 7
	// is the token that was live until this run's KV write seconds ago; the
	// grace window exists precisely so ESO (5m refresh) and kubelet (~1m
	// projection) can catch up before it dies. The old clock measured id 7's own
	// age — ~30 days, the monthly cadence — so it failed a 7-day test and was
	// revoked immediately, leaving the volume-labeler, cidr-firewall, the DNS-01
	// solver webhook and ExternalDNS holding a dead token for minutes.
	//
	// Id 6 drains because it was superseded 30 days ago, which is the window
	// doing what it is for. That both ids exist in one fixture is deliberate:
	// asserting only "6 was revoked" would pass again if 7 were revoked too.
	if len(s.deleted) != 1 || s.deleted[0] != 6 {
		t.Fatalf("drain must revoke the LONG-superseded sibling (6) and only that one, got %v", s.deleted)
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
