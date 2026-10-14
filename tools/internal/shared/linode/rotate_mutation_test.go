package linode

// rotate_mutation_test.go covers the transport-failure arm of deleteExpect2xx.
// Every DELETE test reaches a live httptest server, so the request always
// succeeds at the transport level and `if err != nil { return err }` never runs
// — leaving the guard free to be inverted, which would swallow every failed
// revocation as success (and dereference a nil response on a real network
// error). Revocation reporting a false success is the worst failure mode this
// client has: the caller drops the old credential believing it is gone.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// errClient returns a Client whose transport always fails, without touching the
// network.
func errClient(err error) *Client {
	c := NewClient("tok", 5*time.Second)
	c.base = "http://linode.invalid"
	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, err })
	return c
}

func TestDeleteExpect2xxTransportError(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("dial refused")
	for name, call := range map[string]func() error{
		"DeleteProfileToken":      func() error { return errClient(boom).DeleteProfileToken(ctx, 9) },
		"DeleteObjectStorageKey":  func() error { return errClient(boom).DeleteObjectStorageKey(ctx, 3) },
		"DeleteKubeconfig":        func() error { return errClient(boom).DeleteKubeconfig(ctx, 7) },
		"DeleteResourcePath":      func() error { return errClient(boom).DeleteResourcePath(ctx, "/v4/nodebalancers/1") },
		"UpdateVolumeLabel":       func() error { return errClient(boom).UpdateVolumeLabel(ctx, 5, "lbl") },
		"ResetPostgresCredential": func() error { return errClient(boom).ResetPostgresCredentials(ctx, 3) },
	} {
		err := call()
		if err == nil {
			t.Errorf("%s on a transport failure = nil, want an error", name)
			continue
		}
		if !errors.Is(err, boom) {
			t.Errorf("%s error = %v, want it to wrap %v", name, err, boom)
		}
	}
}
