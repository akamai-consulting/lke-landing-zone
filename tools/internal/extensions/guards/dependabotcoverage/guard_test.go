package dependabotcoverage

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write materialises one file under root, creating parents.
func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, root string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := Run(root, &out, &errOut)
	return out.String(), errOut.String(), err
}

// gomodConfig scans /tools and nothing else.
const gomodConfig = `version: 2
updates:
  - package-ecosystem: "gomod"
    directory: "/tools"
    schedule:
      interval: "weekly"
`

// TestCoveredTreePasses is the baseline: one manifest, one entry that names it.
func TestCoveredTreePasses(t *testing.T) {
	root := t.TempDir()
	write(t, root, configPath, gomodConfig)
	write(t, root, "tools/go.mod", "module x\n")

	out, _, err := run(t, root)
	if err != nil {
		t.Fatalf("a covered tree must pass, got: %v", err)
	}
	if !strings.Contains(out, "OK") || !strings.Contains(out, "1 scanned") {
		t.Errorf("want an OK line naming what was scanned, got %q", out)
	}
}

// TestUncoveredManifestIsCaught is the arm that matters: a manifest nothing
// scans, which is invisible in every other check because the config is valid and
// the manifest is well-formed.
func TestUncoveredManifestIsCaught(t *testing.T) {
	root := t.TempDir()
	write(t, root, configPath, gomodConfig)
	write(t, root, "tools/go.mod", "module x\n")
	write(t, root, "dockerfiles/Dockerfile", "FROM debian:bookworm-slim\n")

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("an unscanned Dockerfile must fail the gate")
	}
	if !strings.Contains(errOut, "dockerfiles") {
		t.Errorf("want the uncovered directory named, got %q", errOut)
	}
	// The remedy must be in the message, in the form it has to be pasted into.
	if !strings.Contains(errOut, `package-ecosystem: "docker"`) {
		t.Errorf("want the config entry to add, got %q", errOut)
	}
	if !strings.Contains(errOut, exclusionPath) {
		t.Errorf("want the other option — recording the exclusion — offered, got %q", errOut)
	}
}

// TestRootEntryDoesNotCoverACompositeAction pins the EXACT defect this gate was
// written for. `directory: "/"` covers .github/workflows and a root-level
// action.yml, and nothing else — so the repo's only setup-go pin went unscanned
// for as long as it lived in a composite action, and stale by a major version.
func TestRootEntryDoesNotCoverACompositeAction(t *testing.T) {
	root := t.TempDir()
	write(t, root, configPath, `version: 2
updates:
  - package-ecosystem: "github-actions"
    directory: "/"
    schedule:
      interval: "weekly"
`)
	write(t, root, ".github/workflows/lint.yml", "on: push\n")
	write(t, root, ".github/actions/setup-llz/action.yml", "runs:\n  using: composite\n")

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal(`"/" must not be read as covering .github/actions/*`)
	}
	if !strings.Contains(errOut, ".github/actions/setup-llz") {
		t.Errorf("want the composite action reported, got %q", errOut)
	}
}

// TestDirectoriesGlobCoversCompositeActions is the fix for the case above: only
// `directories` takes a glob, and one entry then covers every composite.
func TestDirectoriesGlobCoversCompositeActions(t *testing.T) {
	root := t.TempDir()
	write(t, root, configPath, `version: 2
updates:
  - package-ecosystem: "github-actions"
    directories:
      - "/"
      - "/.github/actions/*"
    schedule:
      interval: "weekly"
`)
	write(t, root, ".github/workflows/lint.yml", "on: push\n")
	write(t, root, ".github/actions/setup-llz/action.yml", "runs:\n  using: composite\n")
	write(t, root, ".github/actions/other/action.yaml", "runs:\n  using: composite\n")

	if _, errOut, err := run(t, root); err != nil {
		t.Fatalf("the glob must cover both composites, got: %v\n%s", err, errOut)
	}
}

// TestExclusionWithAReasonPasses covers the sanctioned exception.
func TestExclusionWithAReasonPasses(t *testing.T) {
	root := t.TempDir()
	write(t, root, configPath, gomodConfig)
	write(t, root, "tools/go.mod", "module x\n")
	write(t, root, "instance-template/.github/workflows/llz-terraform.yml", "on: push\n")
	write(t, root, exclusionPath, `exclusions:
  - ecosystem: github-actions
    path: instance-template/.github
    reason: digest-locked delivered surface; a bump breaks the managed lock nothing can regenerate here
`)

	out, errOut, err := run(t, root)
	if err != nil {
		t.Fatalf("an excluded manifest must pass, got: %v\n%s", err, errOut)
	}
	if !strings.Contains(out, "1 excluded") {
		t.Errorf("want the exclusion counted in the summary, got %q", out)
	}
}

// TestPlaceholderReasonIsRejected. An exclusion is where a red gate goes to be
// made green, so the entry has to carry an argument rather than a word.
func TestPlaceholderReasonIsRejected(t *testing.T) {
	root := t.TempDir()
	write(t, root, configPath, gomodConfig)
	write(t, root, "tools/go.mod", "module x\n")
	write(t, root, "dockerfiles/Dockerfile", "FROM debian:bookworm-slim\n")
	write(t, root, exclusionPath, `exclusions:
  - ecosystem: docker
    path: dockerfiles
    reason: TODO
`)

	_, _, err := run(t, root)
	if err == nil {
		t.Fatal("a placeholder reason must not buy an exclusion")
	}
	if !strings.Contains(err.Error(), "reason") {
		t.Errorf("want the reason named as the problem, got %v", err)
	}
}

// TestStaleExclusionIsCaught keeps the file from decaying into a graveyard.
func TestStaleExclusionIsCaught(t *testing.T) {
	root := t.TempDir()
	write(t, root, configPath, gomodConfig)
	write(t, root, "tools/go.mod", "module x\n")
	write(t, root, exclusionPath, `exclusions:
  - ecosystem: docker
    path: dockerfiles
    reason: this directory was deleted three releases ago and nothing here notices
`)

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("an exclusion matching nothing must fail")
	}
	if !strings.Contains(errOut, "matches nothing") {
		t.Errorf("want the stale exclusion reported, got %q", errOut)
	}
}

// TestStaleConfigEntryIsCaught. Coverage of a directory the repo no longer has
// reads exactly like coverage that works — Dependabot says nothing either way.
func TestStaleConfigEntryIsCaught(t *testing.T) {
	root := t.TempDir()
	write(t, root, configPath, `version: 2
updates:
  - package-ecosystem: "gomod"
    directory: "/tools"
    schedule:
      interval: "weekly"
  - package-ecosystem: "npm"
    directory: "/frontend"
    schedule:
      interval: "weekly"
`)
	write(t, root, "tools/go.mod", "module x\n")

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("an entry naming a directory with no manifest must fail")
	}
	if !strings.Contains(errOut, "/frontend") {
		t.Errorf("want the stale entry reported, got %q", errOut)
	}
}

// TestEmptyCorpusFailsClosed. A walk that found nothing is what a wrong --root
// looks like, and "0 uncovered" over it would be a vacuous pass.
func TestEmptyCorpusFailsClosed(t *testing.T) {
	root := t.TempDir()
	write(t, root, configPath, gomodConfig)

	if _, _, err := run(t, root); err == nil {
		t.Fatal("an empty corpus must fail closed")
	} else if !strings.Contains(err.Error(), "vacuously") {
		t.Errorf("want the vacuous-pass refusal, got %v", err)
	}
}

// TestMissingConfigFailsClosed — there is nothing to report coverage against.
func TestMissingConfigFailsClosed(t *testing.T) {
	root := t.TempDir()
	write(t, root, "tools/go.mod", "module x\n")

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a missing dependabot.yml must fail")
	}
}

// TestMalformedConfigFailsClosed — an unparseable config is not zero entries.
func TestMalformedConfigFailsClosed(t *testing.T) {
	root := t.TempDir()
	write(t, root, configPath, "updates: [ this is not: yaml\n")
	write(t, root, "tools/go.mod", "module x\n")

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a malformed dependabot.yml must fail")
	}
}

// TestNestedCheckoutIsSkipped. A worktree or clone under the repo root carries a
// whole second tree; reporting its manifests would fail the gate on a developer's
// machine for a directory CI cannot see.
func TestNestedCheckoutIsSkipped(t *testing.T) {
	root := t.TempDir()
	write(t, root, configPath, gomodConfig)
	write(t, root, "tools/go.mod", "module x\n")
	write(t, root, "wt-review/.git", "gitdir: /elsewhere\n")
	write(t, root, "wt-review/dockerfiles/Dockerfile", "FROM debian:bookworm-slim\n")

	if _, errOut, err := run(t, root); err != nil {
		t.Fatalf("a nested checkout must not be scanned, got: %v\n%s", err, errOut)
	}
}

// TestTerraformNeedsADeclaredProvider. Terraform that merely USES a provider has
// no version to bump; reporting every root would bury the ones that pin something.
func TestTerraformNeedsADeclaredProvider(t *testing.T) {
	root := t.TempDir()
	write(t, root, configPath, gomodConfig)
	write(t, root, "tools/go.mod", "module x\n")
	write(t, root, "stacks/app/main.tf", "resource \"linode_instance\" \"a\" {}\n")

	if _, errOut, err := run(t, root); err != nil {
		t.Fatalf("a .tf without required_providers is not a manifest, got: %v\n%s", err, errOut)
	}

	write(t, root, "stacks/app/versions.tf", "terraform {\n  required_providers {\n    linode = {\n      source = \"linode/linode\"\n    }\n  }\n}\n")
	if _, _, err := run(t, root); err == nil {
		t.Fatal("a .tf declaring required_providers must be reported when unscanned")
	}
}

// TestDevcontainerEntryIsThePARENTDirectory — the devcontainers ecosystem is
// pointed at the directory CONTAINING .devcontainer, so a manifest one level down
// must be reported (and covered) at the parent.
func TestDevcontainerEntryIsThePARENTDirectory(t *testing.T) {
	root := t.TempDir()
	write(t, root, configPath, `version: 2
updates:
  - package-ecosystem: "devcontainers"
    directory: "/instance-template"
    schedule:
      interval: "weekly"
`)
	write(t, root, "instance-template/.devcontainer/devcontainer.json", `{"image": "x"}`)

	if _, errOut, err := run(t, root); err != nil {
		t.Fatalf("the parent directory must count as coverage, got: %v\n%s", err, errOut)
	}
}
