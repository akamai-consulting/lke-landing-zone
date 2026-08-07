package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// llzImageTagFor must only ever emit a tag build-images.yml / llz-release.yml
// actually publish: :latest, :sha-<40-hex>, or :vX.Y.Z. Returning llz_version
// verbatim (the old behaviour) rendered ghcr.io/…/llz:<sha> and :v0.0.28, both of
// which 404 — an ImagePullBackOff for the reconciler and harbor-provisioner.
func TestLLZImageTagFor(t *testing.T) {
	const sha = "13e8941a8fc04a8096c90695f7005626b4384b78"

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"full commit sha gets the sha- prefix", sha, "sha-" + sha},
		{"release tag passes through", "v0.0.28", "v0.0.28"},
		{"pre-release tag passes through", "v0.0.29-rc.1", "v0.0.29-rc.1"},
		{"build-metadata tag passes through", "v1.2.3+build.5", "v1.2.3+build.5"},

		// No published tag matches these, so :latest is the only pullable answer.
		{"branch name falls back", "main", "latest"},
		{"abbreviated sha falls back", "13e8941", "latest"},
		{"bare selfupgrade.Semver without v falls back", "0.0.28", "latest"},
		{"empty falls back", "", "latest"},
		{"uppercase sha is not the published form", "13E8941A8FC04A8096C90695F7005626B4384B78", "latest"},
		{"41 hex chars is not a sha", sha + "a", "latest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := llzImageTagFor(tc.in); got != tc.want {
				t.Errorf("llzImageTagFor(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The :latest fallback means the shared platform-apl manifests (fetched at the
// pin) and the llz image that runs them come from different commits. That is
// allowed — a branch-pinned dev instance needs it — but it must not be SILENT,
// which is how the same skew shape went unnoticed for TF_IMAGE ↔ template-ref.
func TestLLZImageTagForWarnsOnlyWhenUnmappable(t *testing.T) {
	const sha = "13e8941a8fc04a8096c90695f7005626b4384b78"

	capture := func(t *testing.T, in string) string {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		orig := os.Stderr
		os.Stderr = w
		llzImageTagFor(in)
		os.Stderr = orig
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}

	t.Run("unmappable pin warns and names the pin", func(t *testing.T) {
		out := capture(t, "feat/some-branch")
		if !strings.Contains(out, "::warning::") || !strings.Contains(out, "feat/some-branch") {
			t.Errorf("expected a warning naming the pin, got %q", out)
		}
	})

	// A release tag and a full SHA both map to a published image, so there is no
	// divergence to report — warning on them would train operators to ignore it.
	for _, in := range []string{"v0.0.28", sha} {
		t.Run("mapped pin is silent: "+in, func(t *testing.T) {
			if out := capture(t, in); out != "" {
				t.Errorf("expected no warning for %q, got %q", in, out)
			}
		})
	}
}

// $LLZ_IMAGE_REF still wins over the answers file, and the tag is taken after the
// last ':' — release-e2e exports a full ghcr.io/…/llz:sha-<SHA> reference.
func TestResolveLLZImageTagPrefersEnv(t *testing.T) {
	const sha = "13e8941a8fc04a8096c90695f7005626b4384b78"
	t.Setenv("LLZ_IMAGE_REF", "ghcr.io/akamai-consulting/llz:sha-"+sha)
	if got, want := resolveLLZImageTag(), "sha-"+sha; got != want {
		t.Errorf("resolveLLZImageTag() = %q, want %q", got, want)
	}
}

// A registry reference carrying a port must not be mistaken for a tag separator.
func TestResolveLLZImageTagEnvWithoutTag(t *testing.T) {
	t.Setenv("LLZ_IMAGE_REF", "registry.example.com:5000/llz")
	if got, want := resolveLLZImageTag(), "registry.example.com:5000/llz"; got != want {
		t.Errorf("resolveLLZImageTag() = %q, want %q", got, want)
	}
}
