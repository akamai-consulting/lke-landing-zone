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

// Endpoint bases, overridable so tests point the two network legs at an httptest
// server. NOT a convenience: without them a test can only stub the whole
// round-trip, which leaves the round-trip itself — the token dance, the Accept
// headers, the status-to-answer mapping — with no coverage at all. That is not
// hypothetical here. The GHCR leg 404s a published image if the request does not
// Accept the index media types (the ci images are multi-arch), a mistake that
// would silently downgrade every pin and that only an end-to-end request catches.
//
// The first attempt at this used HTTPS_PROXY + t.Setenv to intercept instead. It
// does not hold: net/http caches the proxy config in a process-global sync.Once
// (envProxyOnce), so once ANY earlier test in the binary has made a request the
// setting is ignored and the "offline" test quietly talks to api.github.com.
// Measured — with an earlier httptest request in the same process, a blackhole
// proxy stopped taking effect and the test went from 10s (blocked, as intended)
// to 0.5s (real round-trip).
var (
	githubAPIBase = "https://api.github.com"
	ghcrBase      = "https://ghcr.io"
)

// httpAskTimeout bounds every "can I ask?" request in this file. These run in
// preflights whose whole point is to fail fast, so an unreachable endpoint has to
// degrade to "could not ask" in seconds rather than stall a pipeline's first job.
const httpAskTimeout = 10 * time.Second

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
// NOT `gh api`, which is what this used at first and what every other GitHub
// lookup in the package uses. The call site is the FIRST preflight step of every
// pipeline job, and execOutput (exec.Command().Output(), no context) has no
// timeout — so a hung api.github.com would have stalled that step until the job's
// own timeout-minutes, turning a check that used to be purely local into a way to
// burn fifteen minutes. A bounded http.Client cannot do that. `gh` is still used,
// but only for the strictly LOCAL job of producing a credential (see githubToken).
//
// Package var so tests substitute the whole round-trip.
var resolveTemplateCommit = func(repo, ref string) (sha string, ok bool) {
	if repo == "" || ref == "" {
		return "", false
	}
	// PathEscape, not raw: a ref may legally contain characters a URL path treats
	// specially. Tags do not in practice, but --template-ref accepts a branch, and
	// `feat/x` unescaped would address a different endpoint shape entirely.
	u := githubAPIBase + "/repos/" + repo + "/commits/" + url.PathEscape(ref)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	// Anonymous works — the template repo is public — but a token lifts the 60/hr
	// per-IP limit that shared CI egress makes real, and is REQUIRED for a private
	// fork. This is the path that matters for an already-scaffolded instance, whose
	// vendored workflow predates the GH_TOKEN the preflight step now sets.
	if t := githubToken(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := (&http.Client{Timeout: httpAskTimeout}).Do(req)
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

// githubToken is a credential FOR github.com, or "" for anonymous.
//
// `gh auth token` reads the local config/keyring and makes NO network call, so
// leaning on it here does not reintroduce the unbounded shell-out this file
// deliberately avoids — it recovers the one thing dropping `gh api` would
// otherwise cost: an operator working against a PRIVATE template fork, who is
// authenticated to gh but has no token in their environment.
//
// HOST-SCOPED, and that is a security property, not tidiness. githubAPIBase is
// api.github.com — the template repo is a github.com repo in every path here
// (instanceTemplateRepo only accepts an owner/repo slug and otherwise falls back
// to the first-party default). But GH_HOST points the ambient environment at a
// different forge in the GHES e2e lane and in any GHE-hosted instance, and there:
//
//   - GH_TOKEN / GITHUB_TOKEN hold an APPLIANCE token (a GHES workflow's
//     `github.token` is issued by the appliance), and
//   - a bare `gh auth token` returns the token for GH_HOST, not for github.com.
//
// Attaching either to a request to api.github.com would disclose an enterprise
// credential to a third party that can only reject it. So the env token is used
// only when the environment is actually pointed at github.com, and `gh` is asked
// for the github.com token by name.
func githubToken() string {
	if ghHost() == "github.com" {
		if t := firstNonEmpty(os.Getenv("GH_TOKEN"), os.Getenv("GITHUB_TOKEN")); t != "" {
			return t
		}
	}
	out, err := execOutput("gh", "auth", "token", "--hostname", "github.com")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
// `sha-<commit>` is immutable, so a pin resolved from the template ref cannot drift
// out from under the tree.
//
// EXISTENCE IS CHECKED, and that is not belt-and-braces. build-images.yml only grew
// its "every main push" trigger in #102; releases before roughly v0.0.31 sit on
// commits that never got a build, and `ci-tofu:sha-<that commit>` has never existed
// (verified against GHCR: v0.0.38 and v0.0.39 resolve, v0.0.30 and earlier 404).
// Pinning to it unconditionally would trade a stale-image failure for an unpullable
// one — arguably worse, since the floating tag at least RUNS. An unpublished pin
// therefore falls back to the floating tags and reports why, and the skew that
// leaves is caught by name at the first job by `llz ci assert-image-fresh`.
//
// reason is non-empty exactly when pinned is false, and says WHICH of the two
// happened: they need different remedies, and a caller that cannot tell them apart
// can only print something vague.
func computeCIImageVars(templateRepo, ref string) (tfImage, kubeImage string, pinned bool, reason string) {
	tag, ok := pinnedImageTag(templateRepo, ref)
	if !ok {
		return floatingImageVars(fmt.Sprintf("could not resolve the template pin %q to a commit in %s", ref, templateRepo))
	}
	return ciImageVarsForTag(tag, ref)
}

// computeCIImageVarsForCommit is computeCIImageVars for a caller that has ALREADY
// resolved the pin — it skips the round-trip rather than repeating it.
//
// Not an optimisation. `llz ci assert-adopter-pin` resolves the tag as its own
// first step and then reports on what it finds; when this re-resolved
// independently, a blip between the two produced "`llz tokens` would not pin …
// could not resolve", a hard gate failure blaming the pin computation for a
// transient network error. One resolution, one verdict.
func computeCIImageVarsForCommit(commit, ref string) (tfImage, kubeImage string, pinned bool, reason string) {
	return ciImageVarsForTag("sha-"+commit, ref)
}

// floatingImageVars is the fallback pair: the version tags that track main.
func floatingImageVars(why string) (string, string, bool, string) {
	return ciImageRef(defaultTemplateOrg, "ci-tofu", ciTofuTag),
		ciImageRef(defaultTemplateOrg, "ci-kubernetes", ciKubernetesTag),
		false, why
}

// ciImageVarsForTag builds the pinned pair for an image tag and verifies both are
// pullable, falling back to the floating tags if either is definitively absent.
func ciImageVarsForTag(tag, ref string) (tfImage, kubeImage string, pinned bool, reason string) {
	tf := ciImageRef(defaultTemplateOrg, "ci-tofu", tag)
	kube := ciImageRef(defaultTemplateOrg, "ci-kubernetes", tag)
	for _, im := range []string{tf, kube} {
		// asked=false (registry unreachable) must NOT downgrade the pin: an offline
		// operator would then silently get the floating tag, which is the exact
		// mis-configuration this function exists to stop producing. Only a definite
		// "not there" falls back.
		if published, asked := imagePublished(im); asked && !published {
			return floatingImageVars(fmt.Sprintf("%s was never published — the commit %s names predates build-images.yml "+
				"running on every main push (#102), or that build failed", im, ref))
		}
	}
	return tf, kube, true, ""
}

// computeAndReportImageVars fills the ci image variables the caller still needs and
// explains a fallback if there was one. Split out of `llz tokens` so the fill/report
// decision is unit-testable rather than buried in a 200-line interactive wizard.
//
// Writes ONLY the variables asked for: an operator's existing TF_IMAGE is theirs,
// and this command's contract is to skip what is already satisfied.
func computeAndReportImageVars(vars map[string]string, needTF, needKube bool) {
	ref := pinnedTemplateRef()
	tfImage, kubeImage, pinned, why := computeCIImageVars(instanceTemplateRepo(), ref)
	if needTF {
		vars["TF_IMAGE"] = tfImage
	}
	if needKube {
		vars["KUBE_IMAGE"] = kubeImage
	}
	if pinned {
		return
	}
	// Not fatal — the floating tags are what every instance ran until now, and they
	// are right whenever the tree is at main. But this is the shape that broke an
	// adopter, so name it rather than let it look deliberate, and say what it costs:
	// the pin can be outrun, and `assert-image-fresh` is what will tell them.
	fmt.Printf("\n%s TF_IMAGE/KUBE_IMAGE are NOT pinned to this instance's template commit —\n"+
		"      %s.\n"+
		"      Falling back to the floating tags (%s / %s), which track main and can outrun\n"+
		"      pin %q. The first pipeline run will say so (`llz ci assert-image-fresh`) rather\n"+
		"      than fail obscurely later. Upgrading to a release whose ci images were published\n"+
		"      is the durable fix.\n",
		yellow("!"), why, ciTofuTag, ciKubernetesTag, ref)
}

// imagePublished reports whether an image reference resolves in the registry, and
// whether the registry could be ASKED at all. The two are not the same and callers
// must not conflate them (see computeCIImageVars).
//
// Deliberately NOT pinManifestExists, which shells out to `docker manifest
// inspect`: this runs on an operator's laptop and inside the ci-tofu container,
// neither guaranteed a docker daemon, and a missing daemon would read as a missing
// image — downgrading a perfectly good pin over a tool that was never installed.
// The first-party ci images are public, so an anonymous pull-scoped token answers
// the same question over plain HTTP.
//
// Package var so tests substitute the round-trip.
var imagePublished = func(image string) (published, asked bool) {
	// ghcr.io/<owner>/<name>:<tag>
	rest, tag, found := strings.Cut(strings.TrimPrefix(image, "ghcr.io/"), ":")
	if !found || rest == "" || tag == "" {
		return false, false
	}
	client := &http.Client{Timeout: httpAskTimeout}

	var tok struct {
		Token string `json:"token"`
	}
	tokenURL := ghcrBase + "/token?service=ghcr.io&scope=" + url.QueryEscape("repository:"+rest+":pull")
	resp, err := client.Get(tokenURL)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&tok) != nil || tok.Token == "" {
		return false, false
	}

	req, err := http.NewRequest(http.MethodHead, ghcrBase+"/v2/"+rest+"/manifests/"+url.PathEscape(tag), nil)
	if err != nil {
		return false, false
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	// The ci images are multi-arch (linux/amd64 + linux/arm64), so they are an INDEX,
	// not a manifest. Omit these Accept types and the registry 404s an image that is
	// plainly there — "no manifest in a media type you accept" reads identically to
	// "no such tag", and this function would report every image missing.
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	mResp, err := client.Do(req)
	if err != nil {
		return false, false
	}
	defer mResp.Body.Close()
	switch {
	case mResp.StatusCode >= 200 && mResp.StatusCode < 300:
		return true, true
	case mResp.StatusCode == http.StatusNotFound:
		return false, true
	default:
		// 401/403/5xx: the registry did not answer the question we asked.
		return false, false
	}
}
