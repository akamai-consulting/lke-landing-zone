package linode

// httpstatus_mutation_test.go pins the SUCCESS WINDOW of every response-status
// guard in the client. Fourteen wrappers spell the same predicate by hand —
// `resp.StatusCode < 200 || resp.StatusCode >= 300` (and DeleteResourcePath's
// inverted twin) — and each per-call test only ever exercised one 2xx and one
// obviously-broken status (401/403/500), so the EDGES of the window were free:
// nothing observed 200 or 299 as success, and nothing observed 199 or 300 as
// failure. A guard that had drifted to `> 300` (300 = success, i.e. a redirect
// silently parsed as a payload) passed every existing test.
//
// One table drives every wrapper rather than fourteen near-duplicate tests: the
// predicate is copy-pasted in the source, so the test that constrains it should
// be written once and applied to each copy. The file is named for the shared
// invariant instead of a single source file for the same reason.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// statusClient returns a Client whose transport synthesises one response with
// the given status and body. No listener, no dial, no network.
//
// It exists rather than reusing clientFor because the table has to cover 199:
// Go's HTTP server treats a WriteHeader below 200 as an *informational* response
// and keeps waiting to write a final one, so httptest cannot deliver a 1xx as
// the status a client sees. Synthesising the response is the only way to
// exercise the `< 200` side of the guard at all.
func statusClient(code int, body string) *Client {
	c := NewClient("tok", 5*time.Second)
	c.base = "http://linode.invalid" // never resolved: the transport answers everything
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: code,
			Status:     fmt.Sprintf("%d %s", code, http.StatusText(code)),
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})
	return c
}

// statusProbeBody is one JSON object that every decoder in the package accepts:
// each wrapper picks out the field it wants and ignores the rest. Being
// decodable at EVERY status is the point — a guard that wrongly admits 199 or
// 300 then parses this happily and reports success, which is exactly the
// difference the table measures.
const statusProbeBody = `{"vcpus":7,"kubeconfig":"KUBE","k8s_version":"v1.2.3",` +
	`"username":"u","password":"p","pages":1,"data":[{"id":1}],` +
	`"acl":{"enabled":true,"addresses":{"ipv4":["1.1.1.1/32"],"ipv6":[]}}}`

func is2xx(code int) bool { return code >= 200 && code < 300 }

// is2xxOr404 is the widened window of the two wrappers that treat "already
// gone" as success.
func is2xxOr404(code int) bool { return is2xx(code) || code == 404 }

// statusProbe describes one guarded wrapper. ok reports whether the wrapper took
// its SUCCESS path — for most that is a nil error, but LinodeTypeVCPUs and
// GetKubeconfig swallow a bad status into a zero value, so there the payload
// itself is the only evidence of which branch ran.
type statusProbe struct {
	name    string
	source  string // file:line of the guard this probe constrains
	ok      func(context.Context, *Client) bool
	success func(code int) bool
}

func statusProbes() []statusProbe {
	return []statusProbe{{
		name: "GetControlPlaneACL", source: "acl.go:38",
		ok: func(ctx context.Context, c *Client) bool {
			_, err := c.GetControlPlaneACL(ctx, 7)
			return err == nil
		},
	}, {
		name: "PutControlPlaneACL", source: "acl.go:76",
		ok: func(ctx context.Context, c *Client) bool {
			_, err := c.PutControlPlaneACL(ctx, 7, ControlPlaneACL{})
			return err == nil
		},
	}, {
		name: "PostgresInstance", source: "database.go:44",
		ok: func(ctx context.Context, c *Client) bool {
			_, err := c.PostgresInstance(ctx, 3)
			return err == nil
		},
	}, {
		name: "PostgresCredentials", source: "database.go:67",
		ok: func(ctx context.Context, c *Client) bool {
			creds, err := c.PostgresCredentials(ctx, 3)
			return err == nil && creds.Username == "u"
		},
	}, {
		name: "ResetPostgresCredentials", source: "database.go:95",
		ok: func(ctx context.Context, c *Client) bool {
			return c.ResetPostgresCredentials(ctx, 3) == nil
		},
	}, {
		name: "InstanceLKEClusterID", source: "discover.go:37",
		ok: func(ctx context.Context, c *Client) bool {
			_, err := c.InstanceLKEClusterID(ctx, 11)
			return err == nil
		},
	}, {
		// Non-2xx is reported as (0, nil), so only the value distinguishes the
		// branches.
		name: "LinodeTypeVCPUs", source: "lookup.go:52",
		ok: func(ctx context.Context, c *Client) bool {
			n, err := c.LinodeTypeVCPUs(ctx, "g6-standard-4")
			return err == nil && n == 7
		},
	}, {
		// Non-2xx is reported as ("", nil) — same story.
		name: "GetKubeconfig", source: "lookup.go:74",
		ok: func(ctx context.Context, c *Client) bool {
			kc, err := c.GetKubeconfig(ctx, 7)
			return err == nil && kc == "KUBE"
		},
	}, {
		name: "UpdateVolumeLabel", source: "reap.go:224",
		ok: func(ctx context.Context, c *Client) bool {
			return c.UpdateVolumeLabel(ctx, 5, "lbl") == nil
		},
		success: is2xxOr404,
	}, {
		name: "DeleteResourcePath", source: "reap.go:238",
		ok: func(ctx context.Context, c *Client) bool {
			return c.DeleteResourcePath(ctx, "/v4/nodebalancers/1") == nil
		},
		success: is2xxOr404,
	}, {
		name: "listAllPages", source: "rotate.go:46",
		ok: func(ctx context.Context, c *Client) bool {
			toks, err := c.ListProfileTokens(ctx)
			return err == nil && len(toks) == 1
		},
	}, {
		name: "postJSON", source: "rotate.go:83",
		ok: func(ctx context.Context, c *Client) bool {
			out, err := c.CreateProfileToken(ctx, "ci", "*", "2030-01-01T00:00:00")
			return err == nil && out["kubeconfig"] == "KUBE"
		},
	}, {
		name: "deleteExpect2xx", source: "rotate.go:101",
		ok: func(ctx context.Context, c *Client) bool {
			return c.DeleteProfileToken(ctx, 9) == nil
		},
	}, {
		name: "ClusterK8sVersion", source: "rotate.go:233",
		ok: func(ctx context.Context, c *Client) bool {
			v, err := c.ClusterK8sVersion(ctx, 42)
			return err == nil && v == "v1.2.3"
		},
	}}
}

func TestResponseStatusSuccessWindow(t *testing.T) {
	ctx := context.Background()
	// 199/200 straddle the lower edge, 299/300 the upper one; 404 separates the
	// two 404-tolerant wrappers from the rest, and 500 is the plain failure.
	codes := []int{199, 200, 299, 300, 404, 500}
	for _, p := range statusProbes() {
		t.Run(p.name, func(t *testing.T) {
			want := p.success
			if want == nil {
				want = is2xx
			}
			for _, code := range codes {
				got := p.ok(ctx, statusClient(code, statusProbeBody))
				if got != want(code) {
					t.Errorf("%s (%s) on HTTP %d: success = %v, want %v",
						p.name, p.source, code, got, want(code))
				}
			}
		})
	}
}
