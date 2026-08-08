package identityconfig

import "testing"

// The JWKS URL must share a host with the issuer. They are two independent
// settings on the same OpenBao mount, and when they disagree the failure is
// remote: `bound_issuer` still matches, so tokens validate right up until the key
// fetch, which then fails against a name the certificate does not cover. Deriving
// one from the other makes disagreement unrepresentable.
func TestKeycloakJWKSURLSharesTheIssuerHost(t *testing.T) {
	issuer := "https://keycloak.lke637212.akamai-apl.net/realms/otomi"
	want := "https://keycloak.lke637212.akamai-apl.net/realms/otomi/protocol/openid-connect/certs"
	if got := keycloakJWKSURL(issuer); got != want {
		t.Errorf("keycloakJWKSURL(%q)\n got: %q\nwant: %q", issuer, got, want)
	}
	if h := keycloakHostFromIssuer(issuer); h != "keycloak.lke637212.akamai-apl.net" {
		t.Errorf("host mismatch between issuer and the alias that must resolve it: %q", h)
	}
}

// The hostname goes into /etc/hosts, where a malformed entry does not error —
// it simply never resolves, and the JWKS fetch fails much later with a DNS
// error that points nowhere near this function.
func TestKeycloakHostFromIssuer(t *testing.T) {
	for _, tc := range []struct{ name, issuer, want string }{
		{"normal issuer", "https://keycloak.example.com/realms/otomi", "keycloak.example.com"},
		{"no path", "https://keycloak.example.com", "keycloak.example.com"},
		{"managed domain", "https://keycloak.lke634445.akamai-apl.net/realms/otomi", "keycloak.lke634445.akamai-apl.net"},
		{"http is rejected — this hop must be TLS", "http://keycloak.example.com/realms/otomi", ""},
		{"host:port is rejected — hostAliases takes names, not host:port", "https://keycloak.example.com:8443/realms/otomi", ""},
		{"empty issuer (no teams / no domain)", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keycloakHostFromIssuer(tc.issuer); got != tc.want {
				t.Errorf("keycloakHostFromIssuer(%q) = %q, want %q", tc.issuer, got, tc.want)
			}
		})
	}
}

// Selecting the wrong Service pins /etc/hosts at something that cannot terminate
// TLS for the wildcard cert, which reads as a certificate error rather than a
// wrong-target error.
func TestPickGatewayClusterIP(t *testing.T) {
	for _, tc := range []struct {
		name     string
		svcs     []gatewaySvc
		wantIP   string
		wantName string
	}{
		{
			name: "prefers the Service exposing 443 — that is the TLS listener",
			svcs: []gatewaySvc{
				{Name: "internal-istio", ClusterIP: "10.0.0.1", Ports: []int{80}},
				{Name: "platform-istio", ClusterIP: "10.0.0.2", Ports: []int{80, 443}},
			},
			wantIP: "10.0.0.2", wantName: "platform-istio",
		},
		{
			name: "skips headless Services — \"None\" in /etc/hosts silently never resolves",
			svcs: []gatewaySvc{
				{Name: "headless", ClusterIP: "None", Ports: []int{443}},
				{Name: "platform-istio", ClusterIP: "10.0.0.2", Ports: []int{443}},
			},
			wantIP: "10.0.0.2", wantName: "platform-istio",
		},
		{
			name:   "falls back to any ClusterIP when none advertises 443",
			svcs:   []gatewaySvc{{Name: "only", ClusterIP: "10.0.0.9", Ports: []int{8080}}},
			wantIP: "10.0.0.9", wantName: "only",
		},
		{
			name: "no usable Service yields empty, so the caller errors instead of pinning junk",
			svcs: []gatewaySvc{{Name: "headless", ClusterIP: "None", Ports: []int{443}}},
		},
		{name: "no gateways at all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ip, name := pickGatewayClusterIP(tc.svcs)
			if ip != tc.wantIP || name != tc.wantName {
				t.Errorf("pickGatewayClusterIP = (%q, %q), want (%q, %q)", ip, name, tc.wantIP, tc.wantName)
			}
		})
	}
}

// The command re-runs on every bootstrap. Reading the CURRENT alias is what makes
// it idempotent: a needless patch rolls the StatefulSet, restarting a healthy
// raft cluster for no reason.
func TestHostAliasIP(t *testing.T) {
	const host = "keycloak.example.com"
	for _, tc := range []struct{ name, raw, want string }{
		{"pinned", `[{"ip":"10.0.0.2","hostnames":["keycloak.example.com"]}]`, "10.0.0.2"},
		{"pinned alongside other aliases", `[{"ip":"10.0.0.5","hostnames":["other"]},{"ip":"10.0.0.2","hostnames":["a","keycloak.example.com"]}]`, "10.0.0.2"},
		{"different host only", `[{"ip":"10.0.0.5","hostnames":["other.example.com"]}]`, ""},
		{"no hostAliases yet — jsonpath returns empty", "", ""},
		{"garbled input reads as unpinned, so the caller writes rather than skips", "not-json", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostAliasIP([]byte(tc.raw), host); got != tc.want {
				t.Errorf("hostAliasIP(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// This command runs in the bootstrap critical path, and the Gateway-API label is
// Istio's convention rather than a guarantee. If a rename made discovery return
// an error, a team-login-only feature would take down every cluster build — so a
// label miss must degrade to the port-based sweep, not fail.
func TestParseGatewayServices(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		wantIP    string
		wantOK    bool
	}{
		{
			name:   "labelled query returns the 443 Service",
			raw:    `{"items":[{"metadata":{"name":"platform-istio"},"spec":{"clusterIP":"10.0.0.2","ports":[{"port":80},{"port":443}]}}]}`,
			wantIP: "10.0.0.2", wantOK: true,
		},
		{
			name: "label matched nothing — caller must fall back, not error",
			raw:  `{"items":[]}`,
		},
		{
			name: "only headless Services — nothing dialable, caller errors",
			raw:  `{"items":[{"metadata":{"name":"h"},"spec":{"clusterIP":"None","ports":[{"port":443}]}}]}`,
		},
		{name: "garbled output", raw: "not json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ip, _, ok := parseGatewayServices([]byte(tc.raw))
			if ok != tc.wantOK || ip != tc.wantIP {
				t.Errorf("parseGatewayServices = (%q, %v), want (%q, %v)", ip, ok, tc.wantIP, tc.wantOK)
			}
		})
	}
}

// THE RUN THIS BROKE. The pin ran before Argo had created the StatefulSet, and a
// missing StatefulSet was treated as fatal, so the whole bootstrap died at:
//
//	llz: read llz-openbao/platform-openbao hostAliases: exit status 1:
//
// The code justified it with "this command runs after the OpenBao Application has
// synced" — but the preceding gate only asserts the Application EXISTS, not that
// its resources are reconciled. So absence is NORMAL here, and the next step
// (wait-pods, 600s) is what legitimately owns it.
//
// Blast radius is the point: losing the pin costs team login, failing here costs
// the entire cluster build.
func TestStatefulSetHostAliasIPDoesNotFailTheBuildWhenAbsent(t *testing.T) {
	prevWait, prevPoll, prevExec := statefulSetWait, statefulSetPoll, execCombined
	statefulSetWait, statefulSetPoll = 0, 0 // deadline already passed
	execCombined = func(string, ...string) string {
		return `Error from server (NotFound): statefulsets.apps "platform-openbao" not found`
	}
	t.Cleanup(func() { statefulSetWait, statefulSetPoll, execCombined = prevWait, prevPoll, prevExec })

	ip, ok := statefulSetHostAliasIP("llz-openbao", "platform-openbao", "keycloak.example.com")
	if ok {
		t.Error("a NotFound StatefulSet must report ok=false, not a bogus empty pin")
	}
	if ip != "" {
		t.Errorf("no IP should be returned when the StatefulSet is absent, got %q", ip)
	}
}

// Once it exists, the alias must be read normally — the wait must not swallow the
// real answer and skip a pin that was needed.
func TestStatefulSetHostAliasIPReadsExistingAlias(t *testing.T) {
	prevExec := execCombined
	execCombined = func(string, ...string) string {
		return `[{"ip":"10.0.0.2","hostnames":["keycloak.example.com"]}]`
	}
	t.Cleanup(func() { execCombined = prevExec })

	ip, ok := statefulSetHostAliasIP("llz-openbao", "platform-openbao", "keycloak.example.com")
	if !ok || ip != "10.0.0.2" {
		t.Errorf("statefulSetHostAliasIP = (%q, %v), want (\"10.0.0.2\", true)", ip, ok)
	}
}
