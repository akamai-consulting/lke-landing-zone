package capability_test

// cloud_test.go — the cloud handles, tested against a REAL http server.
//
// The lesson forge.go and secrets.go taught this package the expensive way: every
// test asserted Permits() and none asserted that the permitted call reaches the
// wire or that the refused one never does. Permits is the cheap half. These
// tests run an httptest server and count what actually arrived, because the
// property that matters for `cloud-mutate` is that a DELETE the binding may not
// make never leaves the process.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// recorder is a Linode-shaped endpoint that records every method it is asked for.
type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) methods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func cloudServer(t *testing.T) (*recorder, *httptest.Server) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec.mu.Lock()
		rec.seen = append(rec.seen, req.Method)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

// A REFUSED MUTATION MUST NOT REACH THE WIRE. This is the whole point of the
// handle: `cloud-mutate` holders delete clusters, Volumes, NodeBalancers and VPCs,
// and without it a `cloud-read` binding can do all of it. Asserting the error is
// not enough — the request must never have been sent.
func TestAReadOnlyBindingCannotMutateTheAccount(t *testing.T) {
	rec, srv := cloudServer(t)
	c := capability.CloudFor(binding(extension.CloudRead))

	if err := c.Permits(http.MethodDelete); !errors.Is(err, capability.ErrNoCloudMutate) {
		t.Errorf("Permits(DELETE) = %v, want ErrNoCloudMutate", err)
	}
	// Through the real client, against a real server.
	cl := c.Client("tok", 5*time.Second).WithBase(srv.URL)
	if err := cl.DeleteResourcePath(t.Context(), "/v4/lke/clusters/1"); err == nil {
		t.Fatal("DELETE succeeded through a cloud-read handle")
	}
	if got := rec.methods(); len(got) != 0 {
		t.Errorf("the refused request reached the server: %v — a refusal that still sends is not a fence", got)
	}

	// The read half still works, and reaches the wire.
	if _, _, err := cl.ListRaw(t.Context(), "v4", "lke/clusters"); err != nil {
		t.Fatalf("a cloud-read binding was refused a GET: %v", err)
	}
	if got := rec.methods(); len(got) != 1 || got[0] != http.MethodGet {
		t.Errorf("server saw %v, want exactly one GET", got)
	}
}

// cloud-mutate IMPLIES cloud-read, as cluster-write implies cluster-read: every
// mutating path reads back what it changed — the reaper lists before it deletes.
func TestCloudMutateImpliesCloudRead(t *testing.T) {
	rec, srv := cloudServer(t)
	cl := capability.CloudFor(binding(extension.CloudMutate)).Client("tok", 5*time.Second).WithBase(srv.URL)

	if _, _, err := cl.ListRaw(t.Context(), "v4", "lke/clusters"); err != nil {
		t.Fatalf("cloud-mutate was refused a GET: %v", err)
	}
	if err := cl.DeleteResourcePath(t.Context(), "/v4/lke/clusters/1"); err != nil {
		t.Fatalf("cloud-mutate was refused a DELETE: %v", err)
	}
	if got := rec.methods(); len(got) != 2 {
		t.Errorf("server saw %v, want both requests through", got)
	}
}

// NEITHER GRANT MEANS NO LINODE AT ALL, and the handle is non-nil and refusing
// rather than nil — a nil handle panics at the call site and gets reported as a
// crash rather than as a permission fault.
func TestNoCloudGrantYieldsARefusingHandleNotNil(t *testing.T) {
	rec, srv := cloudServer(t)
	c := capability.CloudFor(binding(extension.ReadRepo))
	if c == nil {
		t.Fatal("CloudFor returned nil")
	}
	if err := c.Permits(http.MethodGet); !errors.Is(err, capability.ErrNoCloudRead) {
		t.Errorf("Permits(GET) = %v, want ErrNoCloudRead", err)
	}

	cl := c.Client("tok", 5*time.Second).WithBase(srv.URL)
	if _, _, err := cl.ListRaw(t.Context(), "v4", "lke/clusters"); err == nil {
		t.Error("GET succeeded with neither cloud grant")
	}
	if got := rec.methods(); len(got) != 0 {
		t.Errorf("a request reached the server with no cloud grant: %v", got)
	}
	if capability.DeniedCloud() == nil {
		t.Error("DeniedCloud() is nil")
	}
	if err := capability.DeniedCloud().Permits(http.MethodGet); err == nil {
		t.Error("DeniedCloud permits a GET")
	}
}

// AN UNCLASSIFIED METHOD IS REFUSED BY BOTH HANDLES, on purpose. A method nobody
// has classified is a decision someone has to make, not a gap to fall through —
// the same rule the kubectl verb table follows.
func TestAnUnclassifiedMethodIsRefusedWithEveryGrant(t *testing.T) {
	for _, b := range []extension.Binding{
		binding(extension.CloudRead),
		binding(extension.CloudMutate),
		binding(extension.CloudRead, extension.CloudMutate),
	} {
		err := capability.CloudFor(b).Permits("TRACE")
		if err == nil {
			t.Fatalf("TRACE permitted for grants %v", b.Grants)
		}
		if !strings.Contains(err.Error(), "not classified") {
			t.Errorf("the refusal should say the method is unclassified, got: %v", err)
		}
	}
	// An empty method cannot be judged and is refused rather than defaulted.
	if err := capability.CloudFor(binding(extension.CloudMutate)).Permits(""); err == nil {
		t.Error("an empty HTTP method was permitted")
	}
}

// The method is judged case-insensitively and whitespace-tolerantly, because it
// arrives from http.Method* constants in production and from callers in tests.
func TestMethodClassificationIsNormalised(t *testing.T) {
	c := capability.CloudFor(binding(extension.CloudMutate))
	for _, m := range []string{"get", " GET ", "Delete"} {
		if err := c.Permits(m); err != nil {
			t.Errorf("Permits(%q) = %v, want permitted", m, err)
		}
	}
}

// The two tables must be disjoint. A method in both would make the read handle
// silently able to mutate, which is the one mistake this file cannot survive.
func TestMethodTablesAreDisjoint(t *testing.T) {
	read, mutate := capability.ClassifiedMethods()
	set := map[string]bool{}
	for _, m := range read {
		set[m] = true
	}
	for _, m := range mutate {
		if set[m] {
			t.Errorf("%s is classified as BOTH read and mutate — a read handle could mutate", m)
		}
	}
	if len(read) == 0 || len(mutate) == 0 {
		t.Fatalf("classification tables look empty: read=%v mutate=%v", read, mutate)
	}
}

// THE REFUSAL NAMES THE RESOURCE. "DELETE was refused" is far less useful than
// which cluster it would have deleted, and this is the message an operator sees
// at 3am.
func TestTheRefusalNamesTheRequest(t *testing.T) {
	_, srv := cloudServer(t)
	cl := capability.CloudFor(binding(extension.CloudRead)).Client("tok", 5*time.Second).WithBase(srv.URL)

	err := cl.DeleteResourcePath(t.Context(), "/v4/lke/clusters/12345")
	if err == nil {
		t.Fatal("want a refusal")
	}
	for _, want := range []string{"DELETE", "12345", string(extension.CloudMutate)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is missing %q: %v", want, err)
		}
	}
}

// FromEnv carries the same fence as Client. It exists for the ambient-credential
// path (LINODE_TOKEN), which is how the teardown and tofu verbs build their
// clients — so the grant must be checked there too, not only on the explicit
// constructor.
func TestFromEnvCarriesTheSameFence(t *testing.T) {
	rec, srv := cloudServer(t)
	t.Setenv("LINODE_TOKEN", "tok")

	// Granted: reads reach the wire, mutations do not.
	cl, ctx, err := capability.CloudFor(binding(extension.CloudRead)).FromEnv()
	if err != nil {
		t.Fatalf("FromEnv with a token set: %v", err)
	}
	cl = cl.WithBase(srv.URL)
	if _, _, err := cl.ListRaw(ctx, "v4", "lke/clusters"); err != nil {
		t.Errorf("cloud-read was refused a GET through FromEnv: %v", err)
	}
	if err := cl.DeleteResourcePath(ctx, "/v4/lke/clusters/1"); !errors.Is(err, capability.ErrNoCloudMutate) {
		t.Errorf("DELETE through a cloud-read FromEnv client = %v, want ErrNoCloudMutate", err)
	}
	if got := rec.methods(); len(got) != 1 || got[0] != http.MethodGet {
		t.Errorf("server saw %v, want exactly the one permitted GET", got)
	}

	// Denied: nothing gets through at all.
	dcl, dctx, err := capability.DeniedCloud().FromEnv()
	if err != nil {
		t.Fatalf("DeniedCloud().FromEnv: %v", err)
	}
	if _, _, err := dcl.WithBase(srv.URL).ListRaw(dctx, "v4", "lke/clusters"); err == nil {
		t.Error("a GET succeeded through the denied handle's FromEnv client")
	}
	if got := rec.methods(); len(got) != 1 {
		t.Errorf("the denied request reached the server: %v", got)
	}

	// NO TOKEN IS AN ERROR, NOT A REFUSAL. The two are different failures and a
	// caller that conflates them will go looking for the wrong thing.
	t.Setenv("LINODE_TOKEN", "")
	t.Setenv("LINODE_API_TOKEN", "")
	if _, _, err := capability.CloudFor(binding(extension.CloudMutate)).FromEnv(); err == nil {
		t.Error("FromEnv with no token in the environment returned no error")
	} else if errors.Is(err, capability.ErrNoCloudRead) || errors.Is(err, capability.ErrNoCloudMutate) {
		t.Errorf("a missing token reported as a grant refusal: %v", err)
	}
}
