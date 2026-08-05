package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
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
	// The org is lower-cased: GHCR paths are case-sensitive and `defaultTemplateOrg`
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
		if got := instanceTemplateRepo(); got != defaultTemplateRepo {
			t.Errorf("instanceTemplateRepo = %q, want %q", got, defaultTemplateRepo)
		}
	})
	t.Run("falls back outside an instance", func(t *testing.T) {
		writeInstanceDir(t, nil)
		if got := instanceTemplateRepo(); got != defaultTemplateRepo {
			t.Errorf("instanceTemplateRepo = %q, want %q", got, defaultTemplateRepo)
		}
	})
}

// resolveTemplateCommit prefers `gh api`; the anonymous leg exists for the
// container that has no usable credential. Both must reject a body that does not
// actually answer the question rather than pass "" up as a commit.
func TestResolveTemplateCommitViaGH(t *testing.T) {
	const sha = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"
	prev := ghAPIJSON
	t.Cleanup(func() { ghAPIJSON = prev })

	t.Run("decodes the sha", func(t *testing.T) {
		var gotPath string
		ghAPIJSON = func(path string, out any) error {
			gotPath = path
			return json.Unmarshal([]byte(`{"sha":"`+sha+`"}`), out)
		}
		got, ok := resolveTemplateCommit("acme/tmpl", "v0.0.39")
		if !ok || got != sha {
			t.Fatalf("resolveTemplateCommit = %q,%v", got, ok)
		}
		if want := "repos/acme/tmpl/commits/v0.0.39"; gotPath != want {
			t.Errorf("gh api path = %q, want %q", gotPath, want)
		}
	})

	t.Run("an empty repo or ref asks nothing", func(t *testing.T) {
		ghAPIJSON = func(string, any) error {
			t.Fatal("gh api called with an empty repo/ref")
			return nil
		}
		if _, ok := resolveTemplateCommit("", "v1"); ok {
			t.Error("empty repo resolved")
		}
		if _, ok := resolveTemplateCommit("o/r", ""); ok {
			t.Error("empty ref resolved")
		}
	})

	// A 404/403 from `gh` must fall through to the anonymous leg, and when that also
	// fails the answer is "could not ask" — never a fabricated sha.
	t.Run("a gh failure degrades to not-ok", func(t *testing.T) {
		ghAPIJSON = func(string, any) error { return errors.New("gh: HTTP 401") }
		t.Setenv("GH_TOKEN", "")
		t.Setenv("GITHUB_TOKEN", "")
		// Point the anonymous leg at a host that cannot resolve, so this test makes no
		// network request of its own.
		t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
		t.Setenv("https_proxy", "http://127.0.0.1:1")
		if got, ok := resolveTemplateCommit("acme/tmpl", "v0.0.39"); ok {
			t.Fatalf("resolveTemplateCommit = %q,%v, want not-ok", got, ok)
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
