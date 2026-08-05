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

func TestComputeCIImageVars(t *testing.T) {
	const sha = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"

	// The regression this whole change exists for: a release-tag pin must produce an
	// IMMUTABLE image, not the version tag main republishes on every push.
	t.Run("a resolvable pin gives both images the same immutable commit", func(t *testing.T) {
		stubTemplateCommit(t, func(string, string) (string, bool) { return sha, true })
		tf, kube, pinned := computeCIImageVars("acme/tmpl", "v0.0.39")
		if !pinned {
			t.Error("pinned = false for a resolvable ref")
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
		tf, kube, pinned := computeCIImageVars("acme/tmpl", "v0.0.39")
		if pinned {
			t.Error("pinned = true for an unresolvable ref")
		}
		if want := "ghcr.io/akamai-consulting/ci-tofu:" + ciTofuTag; tf != want {
			t.Errorf("TF_IMAGE = %q, want %q", tf, want)
		}
		if want := "ghcr.io/akamai-consulting/ci-kubernetes:" + ciKubernetesTag; kube != want {
			t.Errorf("KUBE_IMAGE = %q, want %q", kube, want)
		}
	})
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
