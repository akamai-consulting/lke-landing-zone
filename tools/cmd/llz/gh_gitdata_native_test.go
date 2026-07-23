package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// setGHAPIBase points the shared ghAPIBase seam at a test server for the test's
// lifetime, restoring it on cleanup.
func setGHAPIBase(t *testing.T, url string) {
	t.Helper()
	prev := ghAPIBase
	ghAPIBase = url
	t.Cleanup(func() { ghAPIBase = prev })
}

func TestGHReadFileNative(t *testing.T) {
	// Base64 with embedded newlines (as GitHub's Contents API wraps it) must
	// still decode.
	raw := "hello: world\nkey: value\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))
	// Inject a newline into the middle of the base64 to exercise stripping.
	mid := len(encoded) / 2
	newlined := encoded[:mid] + "\n" + encoded[mid:]

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/contents/present.yaml"):
			if r.URL.Query().Get("ref") != "values" {
				t.Errorf("ref = %q", r.URL.Query().Get("ref"))
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"content": newlined, "encoding": "base64",
			})
		case strings.HasSuffix(r.URL.Path, "/contents/missing.yaml"):
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	setGHAPIBase(t, srv.URL)

	got, found, err := ghReadFileNative(context.Background(), srv.Client(), "tok", "acme/platform", "values", "present.yaml")
	if err != nil || !found {
		t.Fatalf("read present: found=%v err=%v", found, err)
	}
	if got != raw {
		t.Errorf("content = %q, want %q", got, raw)
	}

	_, found, err = ghReadFileNative(context.Background(), srv.Client(), "tok", "acme/platform", "values", "missing.yaml")
	if err != nil {
		t.Fatalf("read missing: err = %v", err)
	}
	if found {
		t.Errorf("missing file reported found=true")
	}
}

func TestGHGetBranchHeadNative(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/platform/git/ref/heads/values":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": map[string]string{"sha": "commit-sha-1"},
			})
		case r.URL.Path == "/repos/acme/platform/git/commits/commit-sha-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tree": map[string]string{"sha": "tree-sha-1"},
			})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	setGHAPIBase(t, srv.URL)

	commit, tree, err := ghGetBranchHeadNative(context.Background(), srv.Client(), "tok", "acme/platform", "values")
	if err != nil {
		t.Fatal(err)
	}
	if commit != "commit-sha-1" || tree != "tree-sha-1" {
		t.Errorf("commit=%q tree=%q", commit, tree)
	}
}

func TestGHGetBranchHeadNativeRefNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	setGHAPIBase(t, srv.URL)

	_, _, err := ghGetBranchHeadNative(context.Background(), srv.Client(), "tok", "acme/platform", "nope")
	if !errors.Is(err, errGHRefNotFound) {
		t.Errorf("err = %v, want errGHRefNotFound", err)
	}
}

func TestGHUpdateRefNative(t *testing.T) {
	// 200 → ok=true.
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srvOK.Close)
	setGHAPIBase(t, srvOK.URL)
	ok, err := ghUpdateRefNative(context.Background(), srvOK.Client(), "tok", "acme/platform", "values", "new-sha", false)
	if err != nil || !ok {
		t.Fatalf("200: ok=%v err=%v", ok, err)
	}

	// 422 → ok=false, no error (non-fast-forward).
	srv422 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	t.Cleanup(srv422.Close)
	setGHAPIBase(t, srv422.URL)
	ok, err = ghUpdateRefNative(context.Background(), srv422.Client(), "tok", "acme/platform", "values", "new-sha", false)
	if err != nil {
		t.Fatalf("422: err = %v, want nil", err)
	}
	if ok {
		t.Errorf("422: ok=true, want false")
	}
}

// gitDataServer is a minimal in-memory git-data server for the overlay tests. It
// serves a branch head, accepts blob/tree/commit creation, and lets the test
// script the ref-PATCH status codes (to exercise the ff-retry loop).
type gitDataServer struct {
	headCommit string
	headTree   string
	// newTree is the tree sha returned by tree creation; if equal to headTree the
	// overlay is a no-op.
	newTree string
	// refStatuses is consumed one per ref PATCH; e.g. [422, 200] retries once.
	refStatuses []int

	refPatchCount int
	lastTreeBody  map[string]any
	sawPaths      []string
}

func (g *gitDataServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/acme/platform/git/ref/heads/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": map[string]string{"sha": g.headCommit},
			})
		case strings.HasPrefix(r.URL.Path, "/repos/acme/platform/git/commits/") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tree": map[string]string{"sha": g.headTree},
			})
		case r.URL.Path == "/repos/acme/platform/git/blobs" && r.Method == http.MethodPost:
			var b struct {
				Content  string `json:"content"`
				Encoding string `json:"encoding"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			decoded, _ := base64.StdEncoding.DecodeString(b.Content)
			g.sawPaths = append(g.sawPaths, string(decoded))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "blob-" + string(decoded)})
		case r.URL.Path == "/repos/acme/platform/git/trees" && r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&g.lastTreeBody)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": g.newTree})
		case r.URL.Path == "/repos/acme/platform/git/commits" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "new-commit-sha"})
		case strings.HasPrefix(r.URL.Path, "/repos/acme/platform/git/refs/heads/") && r.Method == http.MethodPatch:
			status := http.StatusOK
			if g.refPatchCount < len(g.refStatuses) {
				status = g.refStatuses[g.refPatchCount]
			}
			g.refPatchCount++
			w.WriteHeader(status)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}
}

func TestGHOverlayCommitNativeHappyPath(t *testing.T) {
	g := &gitDataServer{
		headCommit:  "head-commit",
		headTree:    "head-tree",
		newTree:     "overlay-tree", // != headTree → a real change
		refStatuses: []int{http.StatusOK},
	}
	srv := httptest.NewServer(g.handler(t))
	t.Cleanup(srv.Close)
	setGHAPIBase(t, srv.URL)

	files := map[string]string{
		"z-last.yaml":  "zzz",
		"a-first.yaml": "aaa",
	}
	sha, changed, err := ghOverlayCommitNative(context.Background(), srv.Client(), "tok", "acme/platform", "values", files, "overlay", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || sha != "new-commit-sha" {
		t.Errorf("changed=%v sha=%q", changed, sha)
	}
	// base_tree must be sent for overlay semantics.
	if g.lastTreeBody["base_tree"] != "head-tree" {
		t.Errorf("base_tree = %v, want head-tree", g.lastTreeBody["base_tree"])
	}
	// Files must have been processed in sorted key order → blobs by content aaa then zzz.
	if len(g.sawPaths) != 2 || g.sawPaths[0] != "aaa" || g.sawPaths[1] != "zzz" {
		t.Errorf("blob content order = %v, want [aaa zzz]", g.sawPaths)
	}
}

func TestGHOverlayCommitNativeNoOp(t *testing.T) {
	g := &gitDataServer{
		headCommit:  "head-commit",
		headTree:    "same-tree",
		newTree:     "same-tree", // equal → nothing to commit
		refStatuses: []int{http.StatusOK},
	}
	srv := httptest.NewServer(g.handler(t))
	t.Cleanup(srv.Close)
	setGHAPIBase(t, srv.URL)

	sha, changed, err := ghOverlayCommitNative(context.Background(), srv.Client(), "tok", "acme/platform", "values", map[string]string{"a.yaml": "x"}, "overlay", 4)
	if err != nil {
		t.Fatal(err)
	}
	if changed || sha != "" {
		t.Errorf("changed=%v sha=%q, want no-op", changed, sha)
	}
	if g.refPatchCount != 0 {
		t.Errorf("ref was PATCHed %d times on a no-op, want 0", g.refPatchCount)
	}
}

func TestGHOverlayCommitNativeFFRetry(t *testing.T) {
	g := &gitDataServer{
		headCommit:  "head-commit",
		headTree:    "head-tree",
		newTree:     "overlay-tree",
		refStatuses: []int{http.StatusUnprocessableEntity, http.StatusOK}, // 422 then 200
	}
	srv := httptest.NewServer(g.handler(t))
	t.Cleanup(srv.Close)
	setGHAPIBase(t, srv.URL)

	sha, changed, err := ghOverlayCommitNative(context.Background(), srv.Client(), "tok", "acme/platform", "values", map[string]string{"a.yaml": "x"}, "overlay", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || sha != "new-commit-sha" {
		t.Errorf("changed=%v sha=%q", changed, sha)
	}
	if g.refPatchCount != 2 {
		t.Errorf("ref PATCH count = %d, want 2 (one retry)", g.refPatchCount)
	}
}

func TestGHOverlayCommitNativeRefNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	setGHAPIBase(t, srv.URL)

	_, _, err := ghOverlayCommitNative(context.Background(), srv.Client(), "tok", "acme/platform", "nope", map[string]string{"a.yaml": "x"}, "overlay", 4)
	if !errors.Is(err, errGHRefNotFound) {
		t.Errorf("err = %v, want errGHRefNotFound propagated", err)
	}
}
