package main

import "testing"

// commonDomainSuffix walks reversed label lists in lockstep and leaves the scan by
// one of two `goto done`s: a label MISMATCH, or a host RUNNING OUT of labels. Only
// the mismatch exit was covered — every multi-host case in the existing table
// diverges before the shortest host is exhausted.
//
// The run-out exit is the one that matters on a real import: an apex host beside
// its own subdomains ("example.com" with "www.example.com") is the ordinary shape
// of a cluster's Ingress set, and it is also the input that walks furthest into
// the scan before stopping. Nothing bounded that walk.
func TestCommonDomainSuffixStopsWhenAHostRunsOutOfLabels(t *testing.T) {
	cases := []struct {
		name  string
		hosts []string
		want  string
	}{
		{"apex host is itself the common suffix", []string{"example.com", "www.example.com"}, "example.com"},
		{"apex first or last makes no difference", []string{"www.example.com", "example.com"}, "example.com"},
		{"the shortest host bounds the scan", []string{"a.b.demo.example.com", "b.demo.example.com", "demo.example.com"}, "demo.example.com"},
		{"a shared apex among deeper hosts", []string{"api.example.com", "app.demo.example.com", "example.com"}, "example.com"},
		{"a bare TLD shared by an apex host is not a domain", []string{"com", "example.com"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := commonDomainSuffix(c.hosts); got != c.want {
				t.Fatalf("commonDomainSuffix(%v) = %q, want %q", c.hosts, got, c.want)
			}
		})
	}
}
