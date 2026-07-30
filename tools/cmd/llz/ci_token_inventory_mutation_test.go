package main

import (
	"context"
	"testing"
)

// Two entries that tie on BOTH sort keys (same provider, same name — e.g. one
// logical PAT probed at two API hosts) must keep their input order. sort.Slice's
// comparator has to be a strict less: a `<=` on the name tie-breaker reports
// equal elements as ordered and the sort swaps them, silently reordering the
// inventory (and with it any diff a reviewer takes against the previous run).
func TestBuildTokenInventoryTiedEntriesKeepInputOrder(t *testing.T) {
	orig := ghPATProbe
	t.Cleanup(func() { ghPATProbe = orig })
	ghPATProbe = func(_, token string) (int, string, error) {
		if token == "first" {
			return 200, "2026-09-01 00:00:00 UTC", nil
		}
		return 200, "2026-10-01 00:00:00 UTC", nil
	}

	inv := buildTokenInventory(context.Background(), tokenInvDeps{
		ghTargets: []patTarget{
			{"dup", "https://api.github.com", "first"},
			{"dup", "https://ghe.example.com/api/v3", "second"},
		},
		region:   "primary",
		now:      tiNow,
		maxDays:  90,
		warnDays: 14,
	})
	if len(inv.Tokens) != 2 {
		t.Fatalf("tokens = %+v, want 2", inv.Tokens)
	}
	if inv.Tokens[0].Expiry >= inv.Tokens[1].Expiry {
		t.Errorf("tied entries reordered: expiries = [%d %d], want the first-appended (earlier expiry) first",
			inv.Tokens[0].Expiry, inv.Tokens[1].Expiry)
	}
}
