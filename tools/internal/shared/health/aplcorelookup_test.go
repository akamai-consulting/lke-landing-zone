package health

import (
	"strings"
	"testing"
)

// The realm as apl-core sees it in the two states that matter: healthy (otomi is
// the only nameless client) and captured (something nameless sorts ahead of it).
func TestAplCoreOtomiLookup(t *testing.T) {
	healthy := []RealmClient{
		{ClientID: "account", Name: "${client_account}"},
		{ClientID: "admin-cli", Name: "${client_admin-cli}"},
		{ClientID: "llz", Name: "llz"},
		{ClientID: "otomi"},
		{ClientID: "security-admin-console", Name: "${client_security-admin-console}"},
	}

	cases := []struct {
		name    string
		clients []RealmClient
		failed  bool
		want    string // substring the operator must be able to act on
	}{
		{"otomi is the only nameless client", healthy, false, "resolves to `otomi`"},
		{
			// The live incident: `llz` created without a name, sorting ahead of otomi.
			name: "a nameless client sorts ahead of otomi",
			clients: []RealmClient{
				{ClientID: "account", Name: "${client_account}"},
				{ClientID: "llz"},
				{ClientID: "otomi"},
			},
			failed: true,
			want:   "`llz` has no name and sorts before `otomi`",
		},
		{
			// Order must be reconstructed from clientId, not taken from the caller —
			// a probe that returned the list shuffled must reach the same verdict.
			name: "verdict does not depend on the order the caller supplies",
			clients: []RealmClient{
				{ClientID: "otomi"},
				{ClientID: "llz"},
				{ClientID: "account", Name: "${client_account}"},
			},
			failed: true,
			want:   "`llz` has no name and sorts before `otomi`",
		},
		{
			name: "a nameless client sorting AFTER otomi is a caveat, not a failure",
			clients: []RealmClient{
				{ClientID: "otomi"},
				{ClientID: "zz-custom"},
			},
			failed: false,
			want:   "caveat: `zz-custom` also have no name",
		},
		{
			name:    "every client named — apl-core's create branch will 409",
			clients: []RealmClient{{ClientID: "otomi", Name: "otomi"}, {ClientID: "llz", Name: "llz"}},
			failed:  true,
			want:    "clear the name on `otomi`",
		},
		{
			name:    "no otomi client at all",
			clients: []RealmClient{{ClientID: "llz", Name: "llz"}},
			failed:  true,
			want:    "has no `otomi` client",
		},
		{
			// Fail-closed: a gate that passes having examined nothing looks exactly
			// like the outage it exists to catch.
			name:    "empty client list",
			clients: nil,
			failed:  true,
			want:    "read 0 clients",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs, failed := AplCoreOtomiLookup(tc.clients)
			if failed != tc.failed {
				t.Errorf("failed = %v, want %v (msgs: %v)", failed, tc.failed, msgs)
			}
			joined := strings.Join(msgs, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("messages did not name the actionable detail %q; got:\n%s", tc.want, joined)
			}
		})
	}
}

// The failure message has to carry the client to fix. An operator reading it is
// three hops from the symptom they were sent to investigate ("users can't log in"),
// so a bare "lookup broken" would leave them exactly where they started.
func TestAplCoreOtomiLookup_NamesTheClientToFix(t *testing.T) {
	msgs, failed := AplCoreOtomiLookup([]RealmClient{{ClientID: "llz-smoke-171"}, {ClientID: "otomi"}})
	if !failed {
		t.Fatal("a nameless client ahead of otomi must fail")
	}
	joined := strings.Join(msgs, "\n")
	for _, want := range []string{"llz-smoke-171", "authorization settings", "APL console users"} {
		if !strings.Contains(joined, want) {
			t.Errorf("message must mention %q so the operator can connect it to the symptom; got:\n%s", want, joined)
		}
	}
}
