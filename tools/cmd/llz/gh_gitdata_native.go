package main

// gh_gitdata_native.go — a pure net/http client for GitHub's "git data" REST
// API (blobs/trees/commits/refs) plus the Contents API for reads. Used by the
// in-cluster reconciler, which runs on the slim distroless llz image: NO git
// binary, NO shell, NO gh CLI — so an overlay commit to a values branch can't
// shell out to `git`. Everything here is stdlib (net/http + encoding/*), the
// same seam-driven style as gh_secrets_native.go.
//
// The load-bearing primitive is ghOverlayCommitNative: it builds a tree with a
// base_tree so unlisted files are PRESERVED (an overlay, not a replace), then
// fast-forwards the branch ref. If the ref moved under it (apl-operator pushed
// concurrently) the ref PATCH 422s and it re-reads head + rebuilds — an
// optimistic-concurrency retry loop.
//
// Auth here is caller-supplied: the token and *http.Client are parameters, not
// read from GH_TOKEN/os.Getenv, because the reconciler already holds the token
// and wants control over the client (timeouts, transport). ghAPIBase is shared
// with gh_secrets_native.go.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// errGHRefNotFound is returned (wrapped) when a branch ref 404s, so a caller can
// tell "the branch does not exist" apart from any other failure with errors.Is.
var errGHRefNotFound = errors.New("github ref not found")

// ghAuthHeaders sets the standard GitHub REST auth + versioning headers on req
// (Bearer token, JSON accept, pinned API version) — the same header set the other
// native GitHub callers use inline.
func ghAuthHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

// ghTreeEntry is one entry in a git tree-creation request. Mode/Type/SHA are the
// git-data shapes: Mode "100644" (blob), Type "blob", SHA the created blob sha.
type ghTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// ghReadFileNative reads a single file from a ref via the Contents API. Returns
// found=false (no error) on 404 so a missing overlay file is a normal outcome.
// The Contents API base64-encodes the body (with embedded newlines), which we
// strip before decoding.
func ghReadFileNative(ctx context.Context, client *http.Client, token, repo, ref, path string) (content string, found bool, err error) {
	u := fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s",
		ghAPIBase, repo, path, url.QueryEscape(ref))
	var body struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	notFound, err := ghGetJSON(ctx, client, token, u, "read file "+path, &body)
	if err != nil {
		return "", false, err
	}
	if notFound {
		return "", false, nil
	}
	if body.Encoding != "base64" {
		return "", false, fmt.Errorf("read file %s: unexpected encoding %q", path, body.Encoding)
	}
	// GitHub wraps the base64 at 60 columns — strip the newlines before decoding.
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body.Content, "\n", ""))
	if err != nil {
		return "", false, fmt.Errorf("decode file %s: %w", path, err)
	}
	return string(raw), true, nil
}

// ghGetBranchHeadNative resolves a branch to its head commit sha and that
// commit's tree sha (two hops: git/ref/heads/<b> then git/commits/<sha>). A 404
// on the ref returns errGHRefNotFound so a missing branch is distinguishable.
func ghGetBranchHeadNative(ctx context.Context, client *http.Client, token, repo, branch string) (commitSHA, treeSHA string, err error) {
	refURL := fmt.Sprintf("%s/repos/%s/git/ref/heads/%s", ghAPIBase, repo, branch)
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	notFound, err := ghGetJSON(ctx, client, token, refURL, "get ref heads/"+branch, &ref)
	if err != nil {
		return "", "", err
	}
	if notFound {
		return "", "", fmt.Errorf("branch %q: %w", branch, errGHRefNotFound)
	}
	commitSHA = ref.Object.SHA
	if commitSHA == "" {
		return "", "", fmt.Errorf("get ref heads/%s: response missing object.sha", branch)
	}

	commitURL := fmt.Sprintf("%s/repos/%s/git/commits/%s", ghAPIBase, repo, commitSHA)
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	notFound, err = ghGetJSON(ctx, client, token, commitURL, "get commit "+commitSHA, &commit)
	if err != nil {
		return "", "", err
	}
	if notFound {
		return "", "", fmt.Errorf("get commit %s: not found", commitSHA)
	}
	if commit.Tree.SHA == "" {
		return "", "", fmt.Errorf("get commit %s: response missing tree.sha", commitSHA)
	}
	return commitSHA, commit.Tree.SHA, nil
}

// ghCreateBlobNative uploads file content as a git blob (base64-encoded) and
// returns its sha, ready to reference from a tree entry.
func ghCreateBlobNative(ctx context.Context, client *http.Client, token, repo, content string) (sha string, err error) {
	body := map[string]string{
		"content":  base64.StdEncoding.EncodeToString([]byte(content)),
		"encoding": "base64",
	}
	return ghPostForSHA(ctx, client, token,
		fmt.Sprintf("%s/repos/%s/git/blobs", ghAPIBase, repo), body, "create blob")
}

// ghCreateTreeNative creates a tree layered on base_tree: entries listed here
// are added/overwritten, everything else in base_tree is PRESERVED. That
// base_tree is what makes the commit an overlay rather than a full replacement.
func ghCreateTreeNative(ctx context.Context, client *http.Client, token, repo, baseTreeSHA string, entries []ghTreeEntry) (sha string, err error) {
	body := map[string]any{
		"base_tree": baseTreeSHA,
		"tree":      entries,
	}
	return ghPostForSHA(ctx, client, token,
		fmt.Sprintf("%s/repos/%s/git/trees", ghAPIBase, repo), body, "create tree")
}

// ghCreateCommitNative creates a commit pointing at treeSHA with the given
// parents and message, returning the new commit sha.
func ghCreateCommitNative(ctx context.Context, client *http.Client, token, repo, message, treeSHA string, parents []string) (sha string, err error) {
	body := map[string]any{
		"message": message,
		"tree":    treeSHA,
		"parents": parents,
	}
	return ghPostForSHA(ctx, client, token,
		fmt.Sprintf("%s/repos/%s/git/commits", ghAPIBase, repo), body, "create commit")
}

// ghUpdateRefNative points heads/<branch> at newSHA. force=false requests a
// fast-forward-only update: GitHub 422s a non-fast-forward, which we surface as
// ok=false (NOT an error) so the caller can re-read head and retry.
func ghUpdateRefNative(ctx context.Context, client *http.Client, token, repo, branch, newSHA string, force bool) (ok bool, err error) {
	body, err := json.Marshal(map[string]any{"sha": newSHA, "force": force})
	if err != nil {
		return false, err
	}
	u := fmt.Sprintf("%s/repos/%s/git/refs/heads/%s", ghAPIBase, repo, branch)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	ghAuthHeaders(req, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	// 422 == non-fast-forward when force=false: the ref moved under us. Not an
	// error — the caller re-reads head and rebuilds the overlay.
	if resp.StatusCode == http.StatusUnprocessableEntity {
		return false, nil
	}
	return false, ghCheck2xx(resp, token, "update ref heads/"+branch)
}

// ghOverlayCommitNative overlays files onto branch as one commit, preserving
// every other file (base_tree semantics). It's an optimistic-concurrency loop:
// read head, build blobs+tree+commit, then fast-forward the ref; if a concurrent
// push moved the ref (422), re-read and rebuild, up to maxAttempts times.
//
// Returns changed=false, newSHA="" when the overlay is already in place (the new
// tree equals head's tree — nothing to commit). errGHRefNotFound propagates
// as-is when the branch does not exist.
func ghOverlayCommitNative(ctx context.Context, client *http.Client, token, repo, branch string, files map[string]string, message string, maxAttempts int) (newSHA string, changed bool, err error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	// Deterministic order → a stable, testable tree request.
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		headCommit, headTree, err := ghGetBranchHeadNative(ctx, client, token, repo, branch)
		if err != nil {
			return "", false, err // includes errGHRefNotFound
		}

		entries := make([]ghTreeEntry, 0, len(paths))
		for _, p := range paths {
			blobSHA, err := ghCreateBlobNative(ctx, client, token, repo, files[p])
			if err != nil {
				return "", false, err
			}
			entries = append(entries, ghTreeEntry{
				Path: p, Mode: "100644", Type: "blob", SHA: blobSHA,
			})
		}

		treeSHA, err := ghCreateTreeNative(ctx, client, token, repo, headTree, entries)
		if err != nil {
			return "", false, err
		}
		// The overlay is already present: the new tree is byte-for-byte the head
		// tree, so there's nothing to commit.
		if treeSHA == headTree {
			return "", false, nil
		}

		commitSHA, err := ghCreateCommitNative(ctx, client, token, repo, message, treeSHA, []string{headCommit})
		if err != nil {
			return "", false, err
		}

		ok, err := ghUpdateRefNative(ctx, client, token, repo, branch, commitSHA, false)
		if err != nil {
			return "", false, err
		}
		if ok {
			return commitSHA, true, nil
		}
		// Non-fast-forward: someone pushed concurrently. Loop to re-read head.
	}
	return "", false, fmt.Errorf("overlay commit on %q: ref kept moving (non-fast-forward after %d attempts)", branch, maxAttempts)
}

// ghPostForSHA POSTs a JSON body to a git-data endpoint and decodes the {sha}
// the blob/tree/commit creation endpoints all return. Shared by the three
// creators — the only variance is url + body + the label used in errors.
func ghPostForSHA(ctx context.Context, client *http.Client, token, u string, body any, what string) (string, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	ghAuthHeaders(req, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := ghCheck2xx(resp, token, what); err != nil {
		return "", err
	}
	var r struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if r.SHA == "" {
		return "", fmt.Errorf("%s: response missing .sha", what)
	}
	return r.SHA, nil
}

// ghGetJSON GETs a git-data/Contents endpoint, checks 2xx, and decodes the JSON
// body into out. A 404 returns notFound=true with err=nil (and no decode) so a
// caller can treat a missing ref/file as a normal outcome; every other non-2xx is
// an error. The GET twin of ghPostForSHA.
func ghGetJSON(ctx context.Context, client *http.Client, token, u, what string, out any) (notFound bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	ghAuthHeaders(req, token)
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return true, nil
	}
	if err := ghCheck2xx(resp, token, what); err != nil {
		return false, err
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return false, err
	}
	return false, nil
}

// ghCheck2xx returns nil for a 2xx response, otherwise an error carrying the
// status and the response body with the token redacted out of it.
func ghCheck2xx(resp *http.Response, token, what string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	b, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("%s: HTTP %d: %s", what, resp.StatusCode, redactSecret(string(b), token))
}
