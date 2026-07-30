package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pinsFixture builds a miniature repo with the Dockerfile authority plus one
// restatement of each form the gate understands.
func pinsFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dockerfiles/Dockerfile"), ""+
		"FROM debian:bookworm-slim AS toolbox\n"+
		"ARG TOFU_VERSION=1.12.5\n"+
		"ARG KUBECTL_VERSION=1.31.0\n"+
		"ARG YQ_VERSION=4.44.3\n")
	writeFile(t, filepath.Join(root, ".github/workflows/build-images.yml"), ""+
		"          ALL='[\n"+
		`            {"key":"terraform","image":"ci-tofu","target":"ci-tofu","version":"1.12.5","alias":""},`+"\n"+
		`            {"key":"llz","image":"llz","target":"llz","version":"","alias":""}`+"\n"+
		"          ]'\n")
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), ""+
		"env:\n"+
		"  KUBECTL_VERSION: \"1.31.0\"\n"+
		"  YQ_VERSION: \"4.44.3\"\n"+
		"jobs:\n"+
		"  tf:\n"+
		"    container:\n"+
		"      image: ${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:1.12.5', github.repository_owner) }}\n")
	writeFile(t, filepath.Join(root, "tools/cmd/llz/tokens.go"), ""+
		"package main\n\nconst (\n\tciTofuTag       = \"1.12.5\"\n\tciKubernetesTag = \"1.31.0\"\n)\n")
	return root
}

func TestVersionPinsPassesWhenEveryRestatementAgrees(t *testing.T) {
	root := pinsFixture(t)
	var out strings.Builder
	if err := runVersionPins(root, false, &out, io.Discard); err != nil {
		t.Fatalf("a consistent tree must pass: %v", err)
	}
	if !strings.Contains(out.String(), "restatement(s) agree") {
		t.Errorf("unexpected report: %s", out.String())
	}
}

// The regression this gate exists for: the Go constants sat on the old version
// after the Dockerfile and build matrix moved, which would have scaffolded new
// instances onto a HashiCorp Terraform image while callers invoked `tofu`.
func TestVersionPinsCatchesTheStaleGoConstant(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, "tools/cmd/llz/tokens.go"),
		"package main\n\nconst (\n\tciTofuTag       = \"1.9.8\"\n\tciKubernetesTag = \"1.31.0\"\n)\n")

	var errOut strings.Builder
	err := runVersionPins(root, false, io.Discard, &errOut)
	if err == nil {
		t.Fatal("a stale ciTofuTag must fail the gate")
	}
	if !strings.Contains(errOut.String(), "ciTofuTag") || !strings.Contains(errOut.String(), "1.9.8") {
		t.Errorf("the report must name the constant and its stale value:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "1.12.5") {
		t.Errorf("the report must name the authoritative value:\n%s", errOut.String())
	}
}

func TestVersionPinsCatchesEveryRestatementForm(t *testing.T) {
	for _, tc := range []struct {
		name, file, body, want string
	}{
		{
			name: "build matrix",
			file: ".github/workflows/build-images.yml",
			body: `            {"key":"terraform","image":"ci-tofu","target":"ci-tofu","version":"9.9.9","alias":""}` + "\n",
			want: "build matrix version for ci-tofu",
		},
		{
			name: "container image fallback",
			file: ".github/workflows/lint.yml",
			body: "      image: ${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:9.9.9', github.repository_owner) }}\n",
			want: "ci-tofu image tag",
		},
		{
			name: "workflow env restatement",
			file: ".github/workflows/lint.yml",
			body: "env:\n  KUBECTL_VERSION: \"9.9.9\"\n",
			want: "KUBECTL_VERSION",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, filepath.FromSlash(tc.file)), tc.body)
			var errOut strings.Builder
			if err := runVersionPins(root, false, io.Discard, &errOut); err == nil {
				t.Fatalf("%s drift must fail the gate", tc.name)
			}
			if !strings.Contains(errOut.String(), tc.want) {
				t.Errorf("report should name %q:\n%s", tc.want, errOut.String())
			}
		})
	}
}

// lint.yml carries ARGOCD_HELM_VERSION, ESO_HELM_VERSION and KYVERNO_HELM_VERSION
// — unrelated CHART versions that all end in the ARG name HELM_VERSION. A suffix
// match would report every one of them as drift and the gate would be useless on
// day one, so the name boundary is load-bearing.
func TestVersionPinsIgnoresLongerNamesEndingInAnArgName(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, "dockerfiles/Dockerfile"),
		"FROM scratch\nARG HELM_VERSION=3.17.3\n")
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), ""+
		"env:\n"+
		"  HELM_VERSION: \"3.17.3\"\n"+
		"  ARGOCD_HELM_VERSION: \"7.8.0\"\n"+
		"  ESO_HELM_VERSION: \"2.4.1\"\n"+
		"  KYVERNO_HELM_VERSION: \"3.4.4\"\n")
	var errOut strings.Builder
	if err := runVersionPins(root, false, io.Discard, &errOut); err != nil {
		t.Fatalf("chart versions must not be mistaken for the tool pin: %v\n%s", err, errOut.String())
	}
}

// A version inside a comment is prose, not a pin — the same reasoning that keeps
// docs/ out of scanRoots. Without masking, this file's own header (which
// documents the forms it matches) would fail the gate.
func TestVersionPinsIgnoresCommentedVersions(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/notes.yml"),
		"# we used to run ci-tofu:1.9.8 here before the OpenTofu migration\n")
	writeFile(t, filepath.Join(root, "tools/cmd/llz/notes.go"),
		"package main\n\n// historical: ciTofuTag = \"1.9.8\" and ci-tofu:1.9.8\n")
	var errOut strings.Builder
	if err := runVersionPins(root, false, io.Discard, &errOut); err != nil {
		t.Fatalf("commented versions must not count as pins: %v\n%s", err, errOut.String())
	}
}

// An empty `version` field is how build-images marks an image that carries no
// version tag (devcontainer, llz) — it is not drift against the ARG.
func TestVersionPinsSkipsMatrixEntriesWithNoVersion(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/build-images.yml"),
		`            {"key":"llz","image":"llz","target":"llz","version":"","alias":""}`+"\n")
	if err := runVersionPins(root, false, io.Discard, io.Discard); err != nil {
		t.Fatalf("a versionless matrix entry must not fail: %v", err)
	}
}

// A gate that passes because it found nothing is worse than no gate: it reports
// OK on a tree it never actually read.
func TestVersionPinsRefusesToPassVacuously(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dockerfiles/Dockerfile"), "FROM scratch\n")
	err := runVersionPins(root, false, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("a Dockerfile with no ARG versions must be an error, not a pass")
	}
	if !strings.Contains(err.Error(), "vacuously") {
		t.Errorf("error should explain why, got %v", err)
	}
}

func TestVersionPinsErrorsWithoutTheAuthorityFile(t *testing.T) {
	if err := runVersionPins(t.TempDir(), false, io.Discard, io.Discard); err == nil {
		t.Fatal("a missing Dockerfile must be an error, not a silent pass")
	}
}

// The command must be reachable under `llz ci`, or the Makefile target it backs
// silently does nothing.
func TestVersionPinsCommandWiring(t *testing.T) {
	var found bool
	for _, c := range ciCmd().Commands() {
		if c.Name() == "version-pins" {
			found = true
		}
	}
	if !found {
		t.Error("`llz ci version-pins` is not registered")
	}
}

func TestVersionPinsSkipsTestFilesAndMissingRoots(t *testing.T) {
	root := pinsFixture(t)
	// A test fixture legitimately pins a made-up version.
	writeFile(t, filepath.Join(root, "tools/cmd/llz/thing_test.go"),
		"package main\n\nconst fixtureTag = \"ci-tofu:0.0.1\"\n")
	if err := runVersionPins(root, false, io.Discard, io.Discard); err != nil {
		t.Errorf("_test.go files must be excluded from the scan: %v", err)
	}
	// template-scripts/ and Makefile are absent from the fixture entirely.
	if _, err := os.Stat(filepath.Join(root, "Makefile")); !os.IsNotExist(err) {
		t.Fatal("fixture unexpectedly has a Makefile")
	}
}
