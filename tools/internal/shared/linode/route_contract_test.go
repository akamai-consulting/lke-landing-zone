package linode

// route_contract_test.go — the PRODUCER half of a split contract that did not
// exist until issue #449.
//
// `llz ci validate-tokens` now probes the LKE-Enterprise version catalog route to
// prove the pipeline's PAT is authorized for it, because `llz ci
// assert-k8s-version` warns and PASSES when that read is refused and was therefore
// able to be permanently inert while looking green. That probe is only worth
// anything if it knocks on the door the gate actually uses — a probe reporting
// "authorized" about a route nothing reads is a green check with less behind it
// than no check at all.
//
// So both sides take the route from ONE exported definition, and this asserts the
// producer really does: the REAL client, against a server that records what it
// asked for. Restating the string here would test nothing; the assertion is that
// the request and the constant agree.

import (
	"context"
	"net/http"
	"testing"
)

func TestTheReadsUseTheExportedRouteDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		call func(*Client) error
	}{
		{
			name: "the version catalog",
			want: LKEVersionsPath(LKETierEnterprise),
			call: func(c *Client) error {
				_, err := c.ListLKEVersions(context.Background(), LKETierEnterprise)
				return err
			},
		},
		{
			// The SECOND OPINION route. Its whole job is to need the same
			// `lke:read_only` grant as the catalog, so that a token refused at one and
			// accepted at the other proves the refusal is about the route rather than
			// the grant. A probe pointed somewhere else answers a different question and
			// would acquit an under-scoped PAT.
			name: "the account's cluster list",
			want: LKEClustersPath,
			call: func(c *Client) error {
				_, err := c.ListClusters(context.Background())
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Path
				writeJSON(w, http.StatusOK, map[string]any{"data": []map[string]any{}, "pages": 1})
			})
			if err := tc.call(c); err != nil {
				t.Fatalf("read failed: %v", err)
			}
			if got != tc.want {
				t.Errorf("the real read requested %q while the exported definition says %q — "+
					"`llz ci validate-tokens` probes the definition, so they may not diverge: a probe "+
					"on the old route would keep reporting the credential authorized for a door "+
					"nothing opens any more", got, tc.want)
			}
		})
	}
}
