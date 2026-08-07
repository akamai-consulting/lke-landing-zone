package ghgitdata

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The ff-retry loop must make EXACTLY maxAttempts attempts. The existing retry
// test scripted a 422 then a 200, which any loop that runs "at least twice"
// satisfies. Here the ref never fast-forwards, so the loop must stop after two
// PATCHes and report ref-kept-moving; the third scripted status is a 500 so an
// off-by-one — or a loop counter that walks the wrong way and never terminates on
// its own — surfaces as a different error rather than as a hang.
func TestGHOverlayCommitNativeStopsAtMaxAttempts(t *testing.T) {
	g := &gitDataServer{
		headCommit:  "head-commit",
		headTree:    "head-tree",
		newTree:     "overlay-tree",
		refStatuses: []int{http.StatusUnprocessableEntity, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
	srv := httptest.NewServer(g.handler(t))
	t.Cleanup(srv.Close)
	setGHAPIBase(t, srv.URL)

	_, changed, err := OverlayCommit(context.Background(), srv.Client(), "tok",
		"acme/platform", "values", map[string]string{"a.yaml": "x"}, "overlay", 2)
	if err == nil {
		t.Fatal("a ref that never fast-forwards must exhaust the attempts and error")
	}
	if !strings.Contains(err.Error(), "non-fast-forward after 2 attempts") {
		t.Errorf("err = %v, want the ref-kept-moving error after exactly 2 attempts", err)
	}
	if changed {
		t.Error("changed must be false when nothing was committed")
	}
	if g.refPatchCount != 2 {
		t.Errorf("ref PATCH count = %d, want exactly 2", g.refPatchCount)
	}
}

// A 2xx carrying an undecodable body is an error, not an empty success: silently
// swallowing it hands the caller a zero-valued struct that later reads as
// "branch has no head" / "tree has no sha".
func TestGHGetJSONSurfacesDecodeFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object": `)) // truncated
	}))
	t.Cleanup(srv.Close)

	var out struct {
		Object struct{ SHA string } `json:"object"`
	}
	notFound, err := ghGetJSON(context.Background(), srv.Client(), "tok", srv.URL+"/x", "read ref", &out)
	if err == nil {
		t.Error("a truncated 200 body must be an error, not a silent empty decode")
	}
	if notFound {
		t.Error("a decode failure is not a 404")
	}

	// And a well-formed body still decodes cleanly.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":{"sha":"abc"}}`))
	}))
	t.Cleanup(ok.Close)
	if notFound, err := ghGetJSON(context.Background(), ok.Client(), "tok", ok.URL+"/x", "read ref", &out); err != nil || notFound {
		t.Fatalf("well-formed body: notFound=%v err=%v", notFound, err)
	}
	if out.Object.SHA != "abc" {
		t.Errorf("decoded sha = %q, want abc", out.Object.SHA)
	}
}

// Both ends of the 2xx window. 200 and 299 are success; 199 and 300 are not —
// GitHub's 301/302 repo-moved redirects in particular must never read as OK.
func TestGHCheck2xxWindow(t *testing.T) {
	for _, tc := range []struct {
		code    int
		wantErr bool
	}{
		{199, true}, {200, false}, {201, false}, {204, false}, {299, false},
		{300, true}, {301, true}, {404, true}, {422, true}, {500, true},
	} {
		resp := &http.Response{StatusCode: tc.code, Body: io.NopCloser(strings.NewReader("body text"))}
		err := ghCheck2xx(resp, "tok", "do thing")
		if (err != nil) != tc.wantErr {
			t.Errorf("ghCheck2xx(%d) err = %v, wantErr = %v", tc.code, err, tc.wantErr)
		}
	}
}
