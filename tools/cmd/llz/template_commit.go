package main

// template_commit.go resolves an instance's template pin to the COMMIT it names.
//
// WHY THIS EXISTS. Two facts an instance records are meant to be the same fact,
// and until now nothing could compare them because they are written in different
// alphabets:
//
//   - the template pin (`.copier-answers.yml`) is a release TAG — `v0.0.39`
//   - the ci-tofu image's baked llz is stamped `dev-<sha>` — a COMMIT
//
// `llz ci assert-image-fresh` had a tag on one side and a sha on the other, so it
// warned and passed. That skip is not a corner case: it is the DEFAULT shape of a
// freshly scaffolded instance, because `llz tokens` computed TF_IMAGE as
// `ci-tofu:<ciTofuTag>` — a tag build-images.yml republishes on EVERY push to main
// — while copier pinned the tree at the latest release. The two diverge the moment
// the next commit lands on main, and the instance's very first pipeline run fails
// on `llz render --check`: the image's newer llz renders manifests the release's
// llz did not.
//
// It fails in the worst possible way. The drift message says "run `llz render`",
// but the operator's local llz IS the pinned release, so rendering changes nothing
// and they loop. Nothing in the output names the actual cause. Resolving the tag to
// its commit is what lets both the guard and the pin-computation state it plainly.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// resolveTemplateCommit returns the full commit sha that ref names in the template
// repo, and whether the question could be ANSWERED at all.
//
// The two failure modes must not be conflated (same reasoning as ghFileSHA in
// build_preflight.go). ok=false means we could not ask — no `gh`, no token, offline,
// rate-limited — and treating that as a mismatch would fail runs that are perfectly
// fine, on evidence we never had. A genuine 404 for the ref also returns ok=false:
// a pin naming a ref that does not exist is a real problem, but it is not THIS
// guard's problem, and `llz lint`/copier report it where it belongs.
//
// Package var so tests substitute the whole round-trip.
var resolveTemplateCommit = func(repo, ref string) (sha string, ok bool) {
	if repo == "" || ref == "" {
		return "", false
	}
	path := "repos/" + repo + "/commits/" + url.PathEscape(ref)
	var r struct {
		SHA string `json:"sha"`
	}
	// `gh api` first: inside CI it carries the job token, and on an operator's
	// machine it carries their login — both authenticated, so neither is subject
	// to the 60/hr anonymous rate limit.
	if err := ghAPIJSON(path, &r); err == nil && hexSHARe.MatchString(r.SHA) {
		return r.SHA, true
	}
	// Fallback: the template repo is public, so an unauthenticated GET answers the
	// same question. This is the path that matters for an ALREADY-SCAFFOLDED
	// instance, whose vendored workflow predates the GH_TOKEN this step now gets —
	// exactly the instance most likely to be skewed.
	return anonTemplateCommit(repo, ref)
}

// anonTemplateCommit is the unauthenticated api.github.com leg of
// resolveTemplateCommit. Short timeout: this runs in a preflight whose whole point
// is to fail fast, and an unreachable api.github.com must degrade to "cannot ask"
// in seconds rather than stall the first job of every pipeline.
func anonTemplateCommit(repo, ref string) (string, bool) {
	u := "https://api.github.com/repos/" + repo + "/commits/" + url.PathEscape(ref)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	// Honour a token if the environment has one even though `gh` could not use it
	// (gh absent from the image, or `gh auth` never run in a container).
	if t := firstNonEmpty(os.Getenv("GH_TOKEN"), os.Getenv("GITHUB_TOKEN")); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false
	}
	var r struct {
		SHA string `json:"sha"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil || !hexSHARe.MatchString(r.SHA) {
		return "", false
	}
	return r.SHA, true
}

// ownerRepoRe matches a bare GitHub `<owner>/<name>` slug and nothing else — not a
// URL, not an absolute path. normalizeTemplateRepo deliberately returns a non-GitHub
// `_src_path` UNCHANGED (a local directory, another forge), and such a value is only
// harmful here: `repos//home/me/template/commits/v1` is a request that can only 404.
var ownerRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// instanceTemplateRepo is the template repo this instance was scaffolded from,
// read from copier's `_src_path`. Falls back to the first-party template so a
// pre-copier or hand-assembled instance still resolves.
func instanceTemplateRepo() string {
	if a, _ := readAnswers("."); a != nil {
		if r := normalizeTemplateRepo(a.SrcPath); ownerRepoRe.MatchString(r) {
			return r
		}
	}
	return defaultTemplateRepo
}

// pinnedImageTag returns the ci image tag that pins an instance to the SAME commit
// its template pin names — `sha-<full sha>`, the immutable tag build-images.yml
// publishes for every image on every push to main.
//
// ok=false when the pin cannot be resolved to a commit; callers fall back to the
// floating version tag rather than inventing a pin that may not have been built.
func pinnedImageTag(repo, ref string) (string, bool) {
	// A pin that is ALREADY a sha needs no round-trip. Short shas are left to the
	// fallback: `sha-<short>` is not a tag build-images.yml ever published.
	if len(ref) == 40 && hexSHARe.MatchString(ref) {
		return "sha-" + ref, true
	}
	sha, ok := resolveTemplateCommit(repo, ref)
	if !ok {
		return "", false
	}
	return "sha-" + sha, true
}

// ciImageRef assembles a ci image reference for the template org.
func ciImageRef(org, image, tag string) string {
	return fmt.Sprintf("ghcr.io/%s/%s:%s", strings.ToLower(org), image, tag)
}

// computeCIImageVars is the TF_IMAGE / KUBE_IMAGE an instance pinned at ref should
// run, plus whether that pin is IMMUTABLE (pinned=true) or the floating fallback.
//
// WHY NOT THE VERSION TAG, which is what this computed until an adopter's first
// pipeline run failed on it. ciTofuTag is the OpenTofu version (`1.12.5`) and
// ciKubernetesTag the kubectl one, and build-images.yml republishes BOTH on every
// push to main — they are `:latest` wearing a version number. An instance scaffolded
// at a release therefore got a tree rendered by the release's llz and an image
// running main's, guaranteed to diverge the moment the next commit landed. It did:
// main grew a compare-options annotation on carved Apps, so the image's renderer
// emitted four manifests the release's renderer did not, and the run died on `llz
// render --check` telling the operator to run `llz render` — which, on the pinned
// release they had installed, changed nothing.
//
// `sha-<commit>` is immutable and is published for EVERY image on EVERY main push,
// so a pin resolved from the template ref cannot drift out from under the tree.
//
// Existence is not checked here. `llz tokens` runs on an operator's machine, where
// docker (what pinManifestExists needs) may not exist, and the tag is published by
// construction for any commit on main. If one were ever missing the container pull
// fails at the FIRST job with "manifest unknown" — loud, immediate, and unambiguous,
// which is more than the floating tag ever offered.
func computeCIImageVars(templateRepo, ref string) (tfImage, kubeImage string, pinned bool) {
	tag, pinned := pinnedImageTag(templateRepo, ref)
	tfTag, kubeTag := tag, tag
	if !pinned {
		tfTag, kubeTag = ciTofuTag, ciKubernetesTag
	}
	return ciImageRef(defaultTemplateOrg, "ci-tofu", tfTag),
		ciImageRef(defaultTemplateOrg, "ci-kubernetes", kubeTag),
		pinned
}
