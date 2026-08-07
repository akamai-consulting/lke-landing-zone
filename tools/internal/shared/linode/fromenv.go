package linode

// fromenv.go — the ambient-credential entry points, moved from cmd/llz/ci_shared.go
// and cmd/llz/ci.go.
//
// Six files in package main built a client the same way and one of them had the
// token check while another had the constructor. They are one thing: "give me a
// client from whatever the environment is carrying", and it belongs in the package
// that defines the client.

import (
	"context"
	"fmt"
	"os"
	"time"
)

// firstNonEmpty is copied, not shared. Fifteenth package in this campaign to keep
// its own three lines.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ciToken reads the Linode PAT the CI sweeps run under.
func TokenFromEnv() (string, error) {
	t := firstNonEmpty(os.Getenv("LINODE_TOKEN"), os.Getenv("LINODE_API_TOKEN"))
	if t == "" {
		return "", fmt.Errorf("set LINODE_TOKEN (or LINODE_API_TOKEN) to a Linode PAT")
	}
	return t, nil
}

// ciClient builds the Linode API client the ci verbs share, with the 60s
// per-request timeout they all used, plus the background context they all then
// created. Four verbs (tf-import, the two reap gates, teardown) open-coded this
// exact ciToken → err-check → NewClient(token, 60*time.Second) →
// context.Background() sequence.
//
// This is deliberately NOT applied to the narrow per-command interfaces
// (teardownClient, firewallDiscoverer, credLister, …) — those are intentional
// test seams, not duplication.
func ClientFromEnv() (*Client, context.Context, error) {
	token, err := TokenFromEnv()
	if err != nil {
		return nil, nil, err
	}
	return NewClient(token, 60*time.Second), context.Background(), nil
}
