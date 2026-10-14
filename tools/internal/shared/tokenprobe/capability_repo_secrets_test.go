package tokenprobe

// capability_repo_secrets_test.go — the repo-level Secrets check, and the gate
// that keeps it pointed at the route the cluster really calls.
//
// THE FAILURE THIS CHECK EXISTS FOR ran for weeks in a live deployment. The PAT
// probed "✓ valid, expires in 10d" and passed the environment-secret scope check,
// while every five minutes the in-cluster harbor-robot-provisioner logged
//
//	llz: check repo secret HARBOR_ROBOT_NAME: HTTP 403:
//	     {"message":"Resource not accessible by personal access token"}
//
// and `llz ci converge` hard-failed the bootstrap on the three failed Jobs it
// left behind — 35 minutes into a run, after the cluster, apl-core and Argo were
// already up. Nothing was wrong with the token's validity or its Environments
// grant. The permission nobody had asked about was repo-level Secrets.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/forge"
)

// THE PROBE AND THE REAL CALL MUST HIT THE SAME COLLECTION, asserted by driving
// forge's REAL SetRepoSecret against a test server and comparing the path it
// requests with the path the capability check builds. Restating the route as a
// literal in this test would let both copies drift together — the exact vacuity
// docs/e2e-gates.md forbids: assert at the CONSUMER, on what the producer really
// emitted.
func TestRepoSecretProbeHitsTheRouteTheProvisionerCalls(t *testing.T) {
	var realPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		realPaths = append(realPaths, r.URL.Path)
		// The sealed-box write fetches the collection's public key first; answering
		// it with a well-formed 200 lets the real code proceed to the PUT.
		if strings.HasSuffix(r.URL.Path, "/public-key") {
			w.Header().Set("Content-Type", "application/json")
			// 32 zero bytes, base64 — a valid X25519 key for sealing.
			_, _ = w.Write([]byte(`{"key_id":"1","key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	w, err := forge.NewGitHubSecretWriter(srv.URL, "tok", "acme/platform")
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	if err := w.SetRepoSecret("HARBOR_ROBOT_NAME", "robot$ci"); err != nil {
		t.Fatalf("SetRepoSecret: %v", err)
	}
	if len(realPaths) == 0 {
		t.Fatal("the real writer made no request — this gate examined nothing")
	}

	orig := GHCapabilityProbe
	t.Cleanup(func() { GHCapabilityProbe = orig })
	var probed string
	GHCapabilityProbe = func(_, _, path string) (int, error) { probed = path; return 200, nil }

	cr := probeCapability(CapContext{Repo: "acme/platform"}, capCheckFor(t, "OPENBAO_SECRETS_WRITE_TOKEN", opRepoSecrets), "tok")
	if cr.Status != CapOK {
		t.Fatalf("200 → status %v, want CapOK", cr.Status)
	}
	if probed == "" {
		t.Fatal("the capability check made no request")
	}
	// Every path the real write touched must live under the collection the probe
	// asked about — otherwise the probe is clearing a door the caller never uses.
	base := strings.TrimSuffix(probed, "/public-key")
	for _, p := range realPaths {
		if !strings.HasPrefix(p, base+"/") && p != base {
			t.Errorf("the writer called %q, which is not under the probed collection %q", p, base)
		}
	}
}

// A 403 HERE IS THE PROVISIONER'S 403. The verdict must be DENIED (blocking,
// re-scope) rather than the ambiguous warn a 404 gets: 403 means the credential
// authenticated and was refused, which is measured, not inferred.
func TestRepoSecretDenialIsBlockingAndSaysReScope(t *testing.T) {
	orig := GHCapabilityProbe
	t.Cleanup(func() { GHCapabilityProbe = orig })
	GHCapabilityProbe = func(_, _, _ string) (int, error) { return 403, nil }

	cr := probeCapability(CapContext{Repo: "acme/platform"}, capCheckFor(t, "OPENBAO_SECRETS_WRITE_TOKEN", opRepoSecrets), "tok")
	if cr.Status != CapDenied {
		t.Fatalf("403 → status %v, want CapDenied", cr.Status)
	}
	if !strings.Contains(cr.Detail, "under-scoped") {
		t.Errorf("the verdict must say under-scoped, not expired; got %q", cr.Detail)
	}
	h := CapabilityHint("OPENBAO_SECRETS_WRITE_TOKEN", opRepoSecrets)
	// The remediation must name the SEPARATE permission, and say why holding the
	// other one is not enough — the guidance we ship elsewhere says "Environments:
	// write, NOT Secrets", which is right about environment secrets and is exactly
	// what leaves this grant off the PAT.
	for _, want := range []string{"Secrets: read and write", "SEPARATE", "harbor-robot-provisioner"} {
		if !strings.Contains(h, want) {
			t.Errorf("hint must name %q — without it the operator re-checks the permission they already have; got %q", want, h)
		}
	}
}

// THE TWO CHECKS ON THIS ONE PAT MUST BOTH BE REPORTED. The lookup used to
// return the first match and a bool, a shape in which registering the second
// check compiles and does nothing. Denying only the repo-level grant must still
// produce a denial.
func TestBothGrantsOnTheSamePATAreProbedIndependently(t *testing.T) {
	orig := GHCapabilityProbe
	t.Cleanup(func() { GHCapabilityProbe = orig })
	// Environments: write is granted; repo-level Secrets is not — the live
	// configuration that produced the outage.
	GHCapabilityProbe = func(_, _, path string) (int, error) {
		if strings.Contains(path, "/environments/") {
			return 200, nil
		}
		return 403, nil
	}

	rs := CheckCapabilities(CapContext{Repo: "acme/platform", Region: "prod"}, "OPENBAO_SECRETS_WRITE_TOKEN", "tok")
	if len(rs) != 2 {
		t.Fatalf("results = %d, want 2", len(rs))
	}
	byOp := map[string]CapabilityResult{}
	for _, r := range rs {
		byOp[r.Op] = r
	}
	if got := byOp[opEnvSecrets].Status; got != CapOK {
		t.Errorf("environment-secret check = %v, want CapOK", got)
	}
	if got := byOp[opRepoSecrets].Status; got != CapDenied {
		t.Errorf("repo-secret check = %v, want CapDenied — this is the grant the outage was missing", got)
	}
	// And the reduction a narrow column uses must surface the denial, not the pass.
	if worst, ok := WorstCapability(rs); !ok || worst != CapDenied {
		t.Errorf("WorstCapability = %v (ok=%v), want CapDenied — a column that shows the passing half hides the fault", worst, ok)
	}
}

// The probe must be READ-ONLY. A capability check that mutates would publish a
// secret from a preflight, which is a different program.
func TestRepoSecretProbeIsReadOnly(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("GITHUB_API", srv.URL)

	cr := probeCapability(CapContext{Repo: "acme/platform"}, capCheckFor(t, "OPENBAO_SECRETS_WRITE_TOKEN", opRepoSecrets), "tok")
	if cr.Status != CapOK {
		t.Fatalf("status = %v, want CapOK", cr.Status)
	}
	if len(methods) == 0 {
		t.Fatal("no request reached the server — this gate examined nothing")
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Errorf("capability probe used %s — every check must be a read-only GET", m)
		}
	}
}
