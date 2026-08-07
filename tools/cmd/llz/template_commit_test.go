package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/sustain"
)

func TestPinnedImageTag(t *testing.T) {
	const sha = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"

	t.Run("a full-sha pin needs no round-trip", func(t *testing.T) {
		stubTemplateCommit(t, func(string, string) (string, bool) {
			t.Fatal("resolver called for a pin that is already a full sha")
			return "", false
		})
		got, ok := pinnedImageTag("o/r", sha)
		if !ok || got != "sha-"+sha {
			t.Fatalf("pinnedImageTag = %q,%v", got, ok)
		}
	})

	// A SHORT sha must NOT be turned into `sha-<short>`: build-images.yml tags with
	// the full github.sha, so that reference has never been published and pinning it
	// would swap a stale-image failure for an unpullable-image one.
	t.Run("a short-sha pin is resolved, not truncated", func(t *testing.T) {
		stubTemplateCommit(t, func(_, ref string) (string, bool) {
			if ref != sha[:12] {
				t.Errorf("resolver got ref %q, want the short sha", ref)
			}
			return sha, true
		})
		got, ok := pinnedImageTag("o/r", sha[:12])
		if !ok || got != "sha-"+sha {
			t.Fatalf("pinnedImageTag = %q,%v, want the FULL sha tag", got, ok)
		}
	})

	t.Run("a tag pin resolves to its commit", func(t *testing.T) {
		stubTemplateCommit(t, func(repo, ref string) (string, bool) {
			if repo != "o/r" || ref != "v0.0.39" {
				t.Errorf("resolver got (%q,%q)", repo, ref)
			}
			return sha, true
		})
		got, ok := pinnedImageTag("o/r", "v0.0.39")
		if !ok || got != "sha-"+sha {
			t.Fatalf("pinnedImageTag = %q,%v", got, ok)
		}
	})

	t.Run("an unresolvable pin reports not-ok rather than guessing", func(t *testing.T) {
		stubTemplateCommit(t, func(string, string) (string, bool) { return "", false })
		if got, ok := pinnedImageTag("o/r", "v0.0.39"); ok || got != "" {
			t.Fatalf("pinnedImageTag = %q,%v, want \"\",false", got, ok)
		}
	})
}

// stubImagePublished replaces the registry round-trip for the duration of a test.
// Every computeCIImageVars test installs one — the default would otherwise reach
// ghcr.io.
func stubImagePublished(t *testing.T, fn func(image string) (bool, bool)) {
	t.Helper()
	prev := imagePublished
	t.Cleanup(func() { imagePublished = prev })
	imagePublished = fn
}

func TestComputeCIImageVars(t *testing.T) {
	const sha = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"
	allPublished := func(string) (bool, bool) { return true, true }

	// The regression this whole change exists for: a release-tag pin must produce an
	// IMMUTABLE image, not the version tag main republishes on every push.
	t.Run("a resolvable published pin gives both images the same immutable commit", func(t *testing.T) {
		stubTemplateCommit(t, func(string, string) (string, bool) { return sha, true })
		stubImagePublished(t, allPublished)
		tf, kube, pinned, why := computeCIImageVars("acme/tmpl", "v0.0.39")
		if !pinned || why != "" {
			t.Errorf("pinned = %v, reason = %q; want true, \"\"", pinned, why)
		}
		if want := "ghcr.io/akamai-consulting/ci-tofu:sha-" + sha; tf != want {
			t.Errorf("TF_IMAGE = %q, want %q", tf, want)
		}
		if want := "ghcr.io/akamai-consulting/ci-kubernetes:sha-" + sha; kube != want {
			t.Errorf("KUBE_IMAGE = %q, want %q", kube, want)
		}
		// The two images must name ONE commit. build-images.yml builds all four images
		// from a single push, so a split pin would run a kubectl image from a different
		// tree than the tofu image — the same skew class, one layer down.
		if tfTag, kubeTag := tagOf(tf), tagOf(kube); tfTag != kubeTag {
			t.Errorf("images pinned to different commits: %q vs %q", tfTag, kubeTag)
		}
	})

	// Degradation must be to the OLD behaviour exactly — an unresolvable pin is not a
	// reason to hand back an image reference that was never published.
	t.Run("an unresolvable pin falls back to the floating version tags", func(t *testing.T) {
		stubTemplateCommit(t, func(string, string) (string, bool) { return "", false })
		stubImagePublished(t, func(string) (bool, bool) {
			t.Error("registry asked about an image we never resolved")
			return false, false
		})
		tf, kube, pinned, why := computeCIImageVars("acme/tmpl", "v0.0.39")
		assertFloating(t, tf, kube, pinned, why)
		if !strings.Contains(why, "resolve") {
			t.Errorf("reason = %q, want it to name the resolution failure", why)
		}
	})

	// The case the adopter-pin gate turned up against real GHCR: v0.0.30 and earlier
	// sit on commits that predate build-images.yml's every-push trigger, so their
	// `sha-` images do not exist. Pinning to one would swap a stale image for an
	// unpullable one, and the floating tag at least runs.
	t.Run("an unpublished pin falls back rather than pinning an image that does not exist", func(t *testing.T) {
		stubTemplateCommit(t, func(string, string) (string, bool) { return sha, true })
		stubImagePublished(t, func(string) (bool, bool) { return false, true })
		tf, kube, pinned, why := computeCIImageVars("acme/tmpl", "v0.0.30")
		assertFloating(t, tf, kube, pinned, why)
		if !strings.Contains(why, "never published") {
			t.Errorf("reason = %q, want it to name the missing image", why)
		}
	})

	// A MISSING kubectl image must downgrade both, not leave a half-pinned pair
	// running two different trees.
	t.Run("either image being absent downgrades both", func(t *testing.T) {
		stubTemplateCommit(t, func(string, string) (string, bool) { return sha, true })
		stubImagePublished(t, func(image string) (bool, bool) {
			return !strings.Contains(image, "ci-kubernetes"), true
		})
		tf, kube, pinned, why := computeCIImageVars("acme/tmpl", "v0.0.30")
		assertFloating(t, tf, kube, pinned, why)
	})

	// An unreachable registry must NOT downgrade: doing so would hand an offline
	// operator the floating tag, which is precisely the mis-configuration this
	// function exists to stop producing.
	t.Run("an unreachable registry keeps the pin", func(t *testing.T) {
		stubTemplateCommit(t, func(string, string) (string, bool) { return sha, true })
		stubImagePublished(t, func(string) (bool, bool) { return false, false })
		_, _, pinned, why := computeCIImageVars("acme/tmpl", "v0.0.39")
		if !pinned || why != "" {
			t.Errorf("pinned = %v, reason = %q; an unanswered registry must not downgrade the pin", pinned, why)
		}
	})
}

func assertFloating(t *testing.T, tf, kube string, pinned bool, why string) {
	t.Helper()
	if pinned {
		t.Error("pinned = true, want the floating fallback")
	}
	if why == "" {
		t.Error("reason is empty, but the caller has to be able to explain the fallback")
	}
	if want := "ghcr.io/akamai-consulting/ci-tofu:" + ciTofuTag; tf != want {
		t.Errorf("TF_IMAGE = %q, want %q", tf, want)
	}
	if want := "ghcr.io/akamai-consulting/ci-kubernetes:" + ciKubernetesTag; kube != want {
		t.Errorf("KUBE_IMAGE = %q, want %q", kube, want)
	}
}

// The registry leg parses an image reference into the registry path + tag and
// distinguishes 200 / 404 / everything-else. A malformed reference must be "could
// not ask", never "not there" — the latter downgrades a pin on no evidence.
func TestImagePublished(t *testing.T) {
	for _, bad := range []string{"", "ghcr.io/acme/ci-tofu", "ghcr.io/:tag", "ghcr.io/acme/ci-tofu:"} {
		if published, asked := imagePublished(bad); published || asked {
			t.Errorf("imagePublished(%q) = %v,%v; want false,false", bad, published, asked)
		}
	}
}

func tagOf(ref string) string {
	i := strings.LastIndex(ref, ":")
	if i < 0 {
		return ""
	}
	return ref[i+1:]
}

func TestCIImageRef(t *testing.T) {
	// The org is lower-cased: GHCR paths are case-sensitive and `templateid.DefaultOrg`
	// is the human-cased GitHub org.
	if got := ciImageRef("Akamai-Consulting", "ci-tofu", "sha-abc"); got != "ghcr.io/akamai-consulting/ci-tofu:sha-abc" {
		t.Errorf("ciImageRef = %q", got)
	}
}

func TestInstanceTemplateRepo(t *testing.T) {
	t.Run("reads copier's _src_path", func(t *testing.T) {
		writeInstanceDir(t, map[string]string{
			".copier-answers.yml": "_src_path: gh:acme/lke-landing-zone\n_commit: v0.0.39\n",
		})
		if got := instanceTemplateRepo(); got != "acme/lke-landing-zone" {
			t.Errorf("instanceTemplateRepo = %q", got)
		}
	})
	// A local-path _src_path (`copier copy ../template`) normalizes to something with
	// no owner/name shape. Resolving THAT against api.github.com would 404 forever;
	// the first-party template is the only useful answer.
	t.Run("falls back when _src_path is not a github slug", func(t *testing.T) {
		writeInstanceDir(t, map[string]string{
			".copier-answers.yml": "_src_path: /home/me/template\n_commit: v0.0.39\n",
		})
		if got := instanceTemplateRepo(); got != sustain.DefaultTemplateRepo {
			t.Errorf("instanceTemplateRepo = %q, want %q", got, sustain.DefaultTemplateRepo)
		}
	})
	t.Run("falls back outside an instance", func(t *testing.T) {
		writeInstanceDir(t, nil)
		if got := instanceTemplateRepo(); got != sustain.DefaultTemplateRepo {
			t.Errorf("instanceTemplateRepo = %q, want %q", got, sustain.DefaultTemplateRepo)
		}
	})
}

// writeInstanceDir runs the test in a fresh directory containing files
// (name → content) — chdirTemp plus the instance files the case needs.
func writeInstanceDir(t *testing.T, files map[string]string) {
	t.Helper()
	chdirTemp(t)
	for name, body := range files {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// ── The two network legs, against a real server ──────────────────────────────
//
// These exercise resolveTemplateCommit and imagePublished END TO END rather than
// stubbing them out: URL shape, headers, status handling, body decoding. Every
// other test in this file replaces the round-trip, so without these the
// round-trip itself has no coverage — and it is where the mistakes live (the GHCR
// Accept headers below are load-bearing and were found the hard way).

// serveGitHub points the api.github.com leg at h for the duration of the test.
func serveGitHub(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	prev := githubAPIBase
	t.Cleanup(func() { githubAPIBase = prev })
	githubAPIBase = s.URL
	// No ambient credential: these assert the anonymous shape unless a case says
	// otherwise, and a developer's real GH_TOKEN must not leak into the assertions.
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("no gh") })
}

func TestResolveTemplateCommitOverHTTP(t *testing.T) {
	const sha = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"

	t.Run("asks the right URL and decodes the sha", func(t *testing.T) {
		var gotPath, gotAccept, gotAuth string
		serveGitHub(t, func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotAccept, gotAuth = r.URL.Path, r.Header.Get("Accept"), r.Header.Get("Authorization")
			fmt.Fprintf(w, `{"sha":%q}`, sha)
		})
		got, ok := resolveTemplateCommit("acme/tmpl", "v0.0.39")
		if !ok || got != sha {
			t.Fatalf("resolveTemplateCommit = %q,%v", got, ok)
		}
		if want := "/repos/acme/tmpl/commits/v0.0.39"; gotPath != want {
			t.Errorf("path = %q, want %q", gotPath, want)
		}
		if gotAccept != "application/vnd.github+json" {
			t.Errorf("Accept = %q", gotAccept)
		}
		if gotAuth != "" {
			t.Errorf("Authorization = %q, want unset with no credential available", gotAuth)
		}
	})

	t.Run("sends a bearer token when one is in the environment", func(t *testing.T) {
		var gotAuth string
		serveGitHub(t, func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			fmt.Fprintf(w, `{"sha":%q}`, sha)
		})
		t.Setenv("GH_TOKEN", "s3cret")
		if _, ok := resolveTemplateCommit("acme/tmpl", "v0.0.39"); !ok {
			t.Fatal("resolve failed")
		}
		if gotAuth != "Bearer s3cret" {
			t.Errorf("Authorization = %q, want the env token", gotAuth)
		}
	})

	// A ref with a slash must not silently address a different endpoint.
	t.Run("escapes a ref containing a slash", func(t *testing.T) {
		var gotRaw string
		serveGitHub(t, func(w http.ResponseWriter, r *http.Request) {
			gotRaw = r.URL.EscapedPath()
			fmt.Fprintf(w, `{"sha":%q}`, sha)
		})
		if _, ok := resolveTemplateCommit("acme/tmpl", "feat/x"); !ok {
			t.Fatal("resolve failed")
		}
		if !strings.Contains(gotRaw, "feat%2Fx") {
			t.Errorf("escaped path = %q, want the ref percent-encoded", gotRaw)
		}
	})

	// Every not-an-answer is "could not ask", never a fabricated commit. A 404 in
	// particular must not become "", true — callers treat ok as permission to
	// compare, and an empty sha would compare against everything.
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"404", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }},
		{"403 rate limited", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) }},
		{"500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }},
		{"200 with no sha", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{}`) }},
		{"200 with a non-sha", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"sha":"nope"}`) }},
		{"200 with garbage", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `not json`) }},
	} {
		t.Run("degrades on "+tc.name, func(t *testing.T) {
			serveGitHub(t, tc.h)
			if got, ok := resolveTemplateCommit("acme/tmpl", "v0.0.39"); ok || got != "" {
				t.Fatalf("resolveTemplateCommit = %q,%v; want \"\",false", got, ok)
			}
		})
	}

	t.Run("an empty repo or ref asks nothing", func(t *testing.T) {
		serveGitHub(t, func(http.ResponseWriter, *http.Request) { t.Error("request made for an empty repo/ref") })
		if _, ok := resolveTemplateCommit("", "v1"); ok {
			t.Error("empty repo resolved")
		}
		if _, ok := resolveTemplateCommit("o/r", ""); ok {
			t.Error("empty ref resolved")
		}
	})
}

func TestImagePublishedOverHTTP(t *testing.T) {
	// serveGHCR emulates the two-step registry dance: an anonymous pull-scoped
	// token, then the manifest request that carries it.
	serveGHCR := func(t *testing.T, manifest http.HandlerFunc) *[]*http.Request {
		t.Helper()
		var seen []*http.Request
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.Clone(r.Context()))
			if strings.HasPrefix(r.URL.Path, "/token") {
				fmt.Fprint(w, `{"token":"tok"}`)
				return
			}
			manifest(w, r)
		}))
		t.Cleanup(s.Close)
		prev := ghcrBase
		t.Cleanup(func() { ghcrBase = prev })
		ghcrBase = s.URL
		return &seen
	}

	t.Run("a published multi-arch image", func(t *testing.T) {
		seen := serveGHCR(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
		published, asked := imagePublished("ghcr.io/acme/ci-tofu:sha-abc")
		if !published || !asked {
			t.Fatalf("imagePublished = %v,%v; want true,true", published, asked)
		}
		if len(*seen) != 2 {
			t.Fatalf("made %d request(s), want 2 (token then manifest)", len(*seen))
		}
		tokenReq, manifestReq := (*seen)[0], (*seen)[1]
		if want := "repository:acme/ci-tofu:pull"; tokenReq.URL.Query().Get("scope") != want {
			t.Errorf("token scope = %q, want %q", tokenReq.URL.Query().Get("scope"), want)
		}
		if want := "/v2/acme/ci-tofu/manifests/sha-abc"; manifestReq.URL.Path != want {
			t.Errorf("manifest path = %q, want %q", manifestReq.URL.Path, want)
		}
		if manifestReq.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("manifest Authorization = %q, want the issued token", manifestReq.Header.Get("Authorization"))
		}
		// THE ONE THAT BIT. The ci images are multi-arch, so the registry serves an
		// INDEX. Without these Accept types it answers 404 for an image that is
		// plainly there, every pin silently downgrades to a floating tag, and the
		// bug this whole change exists to fix comes straight back.
		accept := manifestReq.Header.Get("Accept")
		for _, want := range []string{
			"application/vnd.oci.image.index.v1+json",
			"application/vnd.docker.distribution.manifest.list.v2+json",
		} {
			if !strings.Contains(accept, want) {
				t.Errorf("Accept %q is missing %q", accept, want)
			}
		}
	})

	t.Run("404 is a definite no", func(t *testing.T) {
		serveGHCR(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
		published, asked := imagePublished("ghcr.io/acme/ci-tofu:sha-abc")
		if published || !asked {
			t.Fatalf("imagePublished = %v,%v; want false,true — a 404 IS an answer", published, asked)
		}
	})

	// Anything else is "could not ask". Reporting 401/5xx as absent would downgrade
	// a perfectly good pin on no evidence.
	for _, code := range []int{401, 403, 500, 502} {
		t.Run(fmt.Sprintf("%d is not an answer", code), func(t *testing.T) {
			serveGHCR(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
			if published, asked := imagePublished("ghcr.io/acme/ci-tofu:sha-abc"); published || asked {
				t.Fatalf("imagePublished = %v,%v; want false,false", published, asked)
			}
		})
	}

	t.Run("a token endpoint that fails is not an answer", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
		t.Cleanup(s.Close)
		prev := ghcrBase
		t.Cleanup(func() { ghcrBase = prev })
		ghcrBase = s.URL
		if published, asked := imagePublished("ghcr.io/acme/ci-tofu:sha-abc"); published || asked {
			t.Fatalf("imagePublished = %v,%v; want false,false", published, asked)
		}
	})
}

func TestComputeAndReportImageVars(t *testing.T) {
	const sha = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"
	setup := func(t *testing.T) map[string]string {
		t.Helper()
		writeInstanceDir(t, map[string]string{
			".copier-answers.yml": "_src_path: gh:acme/tmpl\nllz_version: v0.0.39\n",
		})
		stubTemplateCommit(t, func(string, string) (string, bool) { return sha, true })
		stubImagePublished(t, func(string) (bool, bool) { return true, true })
		return map[string]string{}
	}

	// Writes ONLY what was asked for. An operator's existing TF_IMAGE is theirs, and
	// overwriting it would break this command's "skips anything already configreadiness.Satisfied"
	// contract in the one place it silently matters.
	t.Run("fills only the requested variables", func(t *testing.T) {
		vars := setup(t)
		computeAndReportImageVars(vars, false, true)
		if _, ok := vars["TF_IMAGE"]; ok {
			t.Error("TF_IMAGE written when it was not requested")
		}
		if want := "ghcr.io/akamai-consulting/ci-kubernetes:sha-" + sha; vars["KUBE_IMAGE"] != want {
			t.Errorf("KUBE_IMAGE = %q, want %q", vars["KUBE_IMAGE"], want)
		}
	})

	t.Run("fills both when both are requested", func(t *testing.T) {
		vars := setup(t)
		computeAndReportImageVars(vars, true, true)
		if want := "ghcr.io/akamai-consulting/ci-tofu:sha-" + sha; vars["TF_IMAGE"] != want {
			t.Errorf("TF_IMAGE = %q, want %q", vars["TF_IMAGE"], want)
		}
		if want := "ghcr.io/akamai-consulting/ci-kubernetes:sha-" + sha; vars["KUBE_IMAGE"] != want {
			t.Errorf("KUBE_IMAGE = %q, want %q", vars["KUBE_IMAGE"], want)
		}
	})

	t.Run("uses the instance's own template repo and pin", func(t *testing.T) {
		vars := setup(t)
		var gotRepo, gotRef string
		stubTemplateCommit(t, func(repo, ref string) (string, bool) { gotRepo, gotRef = repo, ref; return sha, true })
		computeAndReportImageVars(vars, true, true)
		if gotRepo != "acme/tmpl" || gotRef != "v0.0.39" {
			t.Errorf("resolved (%q,%q), want the answers file's repo + pin", gotRepo, gotRef)
		}
	})
}

// githubToken feeds an Authorization header on requests to api.github.com, so it
// must only ever return a github.com credential. GH_HOST points the ambient
// environment at another forge in the GHES e2e lane and in any GHE-hosted
// instance, where GH_TOKEN holds an APPLIANCE token and a bare `gh auth token`
// returns the appliance's — sending either to github.com would disclose an
// enterprise credential to a third party.
func TestGithubTokenIsHostScoped(t *testing.T) {
	t.Run("uses the env token on github.com", func(t *testing.T) {
		t.Setenv("GH_HOST", "")
		t.Setenv("GITHUB_SERVER_URL", "")
		t.Setenv("GH_TOKEN", "dotcom")
		withExecOutput(t, func(string, ...string) ([]byte, error) {
			t.Error("shelled out to gh when the environment already had a github.com token")
			return nil, nil
		})
		if got := githubToken(); got != "dotcom" {
			t.Errorf("githubToken = %q, want the env token", got)
		}
	})

	// GITHUB_SERVER_URL is set by Actions to the forge that ISSUED GITHUB_TOKEN, and
	// is the only signal inside the vendored instance workflows — they do not set
	// GH_HOST, so without this a GHE-hosted instance would ship its appliance token
	// to github.com the day that plumbing is wired.
	t.Run("ignores the env token when Actions says the forge is an appliance", func(t *testing.T) {
		t.Setenv("GH_HOST", "")
		t.Setenv("GITHUB_SERVER_URL", "https://ghes.corp.example")
		t.Setenv("GH_TOKEN", "appliance-token")
		withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte("dotcom-from-gh"), nil })
		if got := githubToken(); got == "appliance-token" {
			t.Fatal("returned the APPLIANCE token for a request to api.github.com")
		}
	})

	t.Run("ignores the env token when GH_HOST is an appliance", func(t *testing.T) {
		t.Setenv("GITHUB_SERVER_URL", "")
		t.Setenv("GH_HOST", "ghes.corp.example")
		t.Setenv("GH_TOKEN", "appliance-token")
		withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
			return []byte("dotcom-from-gh\n"), nil
		})
		if got := githubToken(); got == "appliance-token" {
			t.Fatal("returned the APPLIANCE token for a request to api.github.com")
		} else if got != "dotcom-from-gh" {
			t.Errorf("githubToken = %q, want the github.com token from gh", got)
		}
	})

	// A bare `gh auth token` returns the token for GH_HOST — the appliance's. The
	// hostname must be named explicitly or the scoping above is undone one layer down.
	t.Run("asks gh for github.com by name", func(t *testing.T) {
		t.Setenv("GITHUB_SERVER_URL", "")
		t.Setenv("GH_HOST", "ghes.corp.example")
		t.Setenv("GH_TOKEN", "")
		t.Setenv("GITHUB_TOKEN", "")
		var gotArgs []string
		withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
			gotArgs = args
			return []byte("tok"), nil
		})
		githubToken()
		if !strings.Contains(strings.Join(gotArgs, " "), "--hostname github.com") {
			t.Errorf("gh args = %v, want an explicit --hostname github.com", gotArgs)
		}
	})

	t.Run("anonymous when gh has nothing", func(t *testing.T) {
		t.Setenv("GITHUB_SERVER_URL", "")
		t.Setenv("GH_HOST", "")
		t.Setenv("GH_TOKEN", "")
		t.Setenv("GITHUB_TOKEN", "")
		withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("not logged in") })
		if got := githubToken(); got != "" {
			t.Errorf("githubToken = %q, want empty", got)
		}
	})
}
