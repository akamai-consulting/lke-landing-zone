package brownfield

import (
	"reflect"
	"testing"
)

// import_cluster_mutation_test.go closes the gaps mutation testing found in the
// pure parsers behind `llz import scan`. These feed the authored spec, so a
// mis-parse is destructive rather than merely wrong.
//
// The bulk of it is container-image reference parsing (imageName / imageTag).
// That grammar is `[registry[:port]/][org/]name[:tag][@digest]`, and the two
// colons in it — the registry PORT colon and the TAG colon — are told apart only
// by `LastIndex(":") > LastIndex("/")`. Nothing exercised the degenerate ends of
// that comparison (no colon AND no slash at all; a leading "/" or "@"), so the
// off-by-ones in it were invisible. The table below walks the grammar rather
// than sampling it.

// TestImageNameTable pins the repo basename for every combination of registry
// host, registry port, org path, tag, and digest. imageName's result is what
// picks the app a version is attributed to (see versionAppByImageSubstring), so
// "grafana/loki:2.9.2" landing under grafana instead of loki silently rewrites
// the source's reported platform versions.
func TestImageNameTable(t *testing.T) {
	cases := []struct{ in, want string }{
		// bare name — the no-slash, no-colon end of the grammar. This is where the
		// slash/colon comparison is -1 vs -1 and the "tag colon must come AFTER the
		// last slash" rule is doing all the work.
		{"nginx", "nginx"},
		{"nginx:1.25", "nginx"},

		// org path: the tag colon is after the slash.
		{"grafana/loki", "loki"},
		{"grafana/loki:2.9.2", "loki"},
		{"quay.io/otomi/core:v4.14.1", "core"},

		// registry PORT colon — before the last slash, so it is NOT a tag.
		{"registry.example.com:5000/app", "app"},
		{"registry.example.com:5000/app:v1", "app"},
		{"registry.example.com:5000/org/app:v1", "app"},

		// digests, alone and combined with a port/org.
		{"app@sha256:abc123", "app"},
		{"goharbor/harbor-core@sha256:abc123", "harbor-core"},
		{"registry.example.com:5000/org/app@sha256:abc123", "app"},

		// degenerate refs. Not things a healthy cluster reports, but this is a
		// string parser fed whatever `kubectl get pods` printed, and each one sits
		// exactly on a boundary: "@" at index 0, "/" at index 0.
		{"@sha256:abc123", ""},
		{"/loki", "loki"},
		{"", ""},
	}
	for _, c := range cases {
		if got := imageName(c.in); got != c.want {
			t.Errorf("imageName(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestImageTagTable is the same grammar walk for the tag half: a registry port
// colon must never be read as a version, and a digest is not a tag.
func TestImageTagTable(t *testing.T) {
	cases := []struct{ in, want string }{
		{"nginx", ""},
		{"nginx:1.25", "1.25"},
		{"grafana/loki", ""},
		{"grafana/loki:2.9.2", "2.9.2"},
		{"registry.example.com:5000/app", ""},
		{"registry.example.com:5000/org/app:v1", "v1"},
		{"app@sha256:abc123", ""},
		{"registry.example.com:5000/org/app@sha256:abc123", ""},
		// "@" at index 0: the whole ref is a digest with nothing before it, so
		// there is no tag — the digest's own colon must not be harvested as one.
		{"@sha256:abc123", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := imageTag(c.in); got != c.want {
			t.Errorf("imageTag(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestNormalizeHostLeadingSlash pins the "/" at index 0 boundary of the Istio
// "ns/host" selector strip. "./host" (the documented "this namespace" selector)
// puts the "/" at index 1; an empty selector puts it at index 0. Whatever the
// selector, everything up to and including the first "/" comes off — a leftover
// "/" would be authored into the report as part of the domain.
func TestNormalizeHostLeadingSlash(t *testing.T) {
	cases := map[string]string{
		"/host.example.com":  "host.example.com",
		"/*.example.com":     "example.com",
		"./host.example.com": "host.example.com",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q)=%q, want %q", in, got, want)
		}
	}
}

// TestParseClusterIssuersSingleSolver pins each ACME solver type INDEPENDENTLY.
// The existing coverage has one dns01 issuer and one http01 issuer in the same
// list, so "both are reported" holds whether the two guards work or are stuck
// on. Reporting a dns01 solver the source doesn't have sends the migration after
// a DNS provider credential that was never in use (and vice versa: a dropped
// http01 loses the only solver the source had).
func TestParseClusterIssuersSingleSolver(t *testing.T) {
	dnsOnly := `{"items":[{"spec":{"acme":{"email":"ops@example.com","solvers":[{"dns01":{"webhook":{"groupName":"acme"}}}]}}}]}`
	email, solvers := parseClusterIssuers(dnsOnly)
	if email != "ops@example.com" {
		t.Errorf("email=%q", email)
	}
	if !reflect.DeepEqual(solvers, []string{"dns01"}) {
		t.Errorf("dns01-only issuer: solvers=%v, want [dns01]", solvers)
	}

	httpOnly := `{"items":[{"spec":{"acme":{"email":"ops@example.com","solvers":[{"http01":{"ingress":{"class":"istio"}}}]}}}]}`
	_, solvers = parseClusterIssuers(httpOnly)
	if !reflect.DeepEqual(solvers, []string{"http01"}) {
		t.Errorf("http01-only issuer: solvers=%v, want [http01]", solvers)
	}

	// A solver entry with neither key set contributes nothing.
	neither := `{"items":[{"spec":{"acme":{"email":"ops@example.com","solvers":[{}]}}}]}`
	if _, solvers = parseClusterIssuers(neither); len(solvers) != 0 {
		t.Errorf("solver with neither key: solvers=%v, want none", solvers)
	}
}
