package linode

// objendpoint_test.go — the cluster is the LAST label before the domain.
//
// Two callers read this out of TF_STATE_ENDPOINT and each had its own
// implementation: the rotation took the FIRST label (so a virtual-host endpoint
// yielded the BUCKET name) and onboarding took everything before
// `.linodeobjects.com` (so the same input yielded "<bucket>.us-ord-1"). Both
// answers scope a minted OBJ key to a cluster that is not where the state bucket
// lives, and a bucket is reachable only at the endpoint it was created against —
// so the key cannot read the state whose credentials it just replaced.

import "testing"

func TestObjClusterFromEndpoint(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		// path-style — the form `llz tokens` writes
		{"path style", "https://us-sea-1.linodeobjects.com", "us-sea-1"},
		{"newer generation ordinal", "https://us-ord-10.linodeobjects.com", "us-ord-10"},
		{"trailing slash", "https://us-ord-10.linodeobjects.com/", "us-ord-10"},
		{"bare host", "us-sea-1.linodeobjects.com", "us-sea-1"},
		{"whitespace", "  https://de-fra-2.linodeobjects.com \n", "de-fra-2"},
		{"uppercase", "https://US-ORD-1.LINODEOBJECTS.COM", "us-ord-1"},
		{"explicit port", "https://us-ord-1.linodeobjects.com:443", "us-ord-1"},
		{"path and query", "https://us-ord-1.linodeobjects.com/bucket?x=1", "us-ord-1"},

		// virtual-host style — THE ONE BOTH COPIES GOT WRONG
		{"virtual host", "https://acme-tfstate.us-ord-1.linodeobjects.com", "us-ord-1"},
		{"virtual host, dotted bucket", "https://my.acme.state.us-ord-1.linodeobjects.com", "us-ord-1"},

		// refusals — a guessed cluster is the bug this exists to remove
		{"empty", "", ""},
		{"apex carries no cluster", "https://linodeobjects.com", ""},
		{"apex with scheme-less spelling", "linodeobjects.com", ""},
		{"another provider", "https://s3.us-east-1.amazonaws.com", ""},
		{"single label", "https://localhost", ""},
		{"not a linode endpoint at all", "https://example.com/us-ord-1.linodeobjects.com", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ObjClusterFromEndpoint(tc.in); got != tc.want {
				t.Errorf("ObjClusterFromEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
