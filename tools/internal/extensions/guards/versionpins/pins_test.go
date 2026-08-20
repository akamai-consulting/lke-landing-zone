package versionpins

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lintYAML renders a workflow carrying the two declared container images, with
// the ci-tofu one overridden by the caller.
//
// REAL WORKFLOW YAML, not a fragment. The rule is about WHERE the value sits —
// `jobs.<id>.container.image` — so a fixture that is a bare `image:` line tests
// nothing the gate does. Extra lines the test wants can be appended; they land
// outside any container and are judged by the pin rule, which is the point of
// several of the tests below.
func lintYAML(tofuImage string, extra ...string) string {
	return "" +
		"jobs:\n" +
		"  k8s:\n" +
		"    container:\n" +
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n" +
		"    steps:\n" +
		"      - run: make lint-k8s\n" +
		"  tf:\n" +
		"    container:\n" +
		"      image: " + tofuImage + "\n" +
		"    steps:\n" +
		strings.Join(append([]string{}, extra...), "") +
		"      - run: make lint-tf\n"
}

// lintEnv is the workflow env block the fixture carries — bare-ARG restatements,
// a different class from the container images below it.
const lintEnv = "env:\n  KUBECTL_VERSION: \"1.31.0\"\n  YQ_VERSION: \"4.44.3\"\n"

// lintFallbackLine is the canonical pair, both floating.
var lintFallbackLine = lintYAML(
	"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}")

// citagsFile is where CITofuTag/CIKubernetesTag really live — this package's own
// citags.go, which is a scanRoot precisely so the goConst rule keeps matching
// something after they moved out of tools/cmd/llz.
const citagsFile = "tools/internal/extensions/guards/versionpins/citags.go"

func writeCITags(t *testing.T, root, tofu, kubectl string) {
	t.Helper()
	writeFile(t, filepath.Join(root, filepath.FromSlash(citagsFile)),
		"package versionpins\n\nconst (\n\tCITofuTag       = \""+tofu+"\"\n\tCIKubernetesTag = \""+kubectl+"\"\n)\n")
}

// matrix renders build-images.yml's image matrix. Every fixture that rewrites
// that file has to carry BOTH ci rows: a missing row is itself an error, because
// reMatrixEntry reads two adjacent JSON fields in one regex and a purely
// cosmetic reordering is enough to stop it matching.
func matrix(tofu, kubectl string) string {
	return "          ALL='[\n" +
		`            {"key":"terraform","image":"ci-tofu","target":"ci-tofu","version":"` + tofu + `","alias":""},` + "\n" +
		`            {"key":"kubernetes","image":"ci-kubernetes","target":"ci-kubernetes","version":"` + kubectl + `","alias":""},` + "\n" +
		`            {"key":"llz","image":"llz","target":"llz","version":"","alias":""}` + "\n" +
		"          ]'\n"
}

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
	writeFile(t, filepath.Join(root, ".github/workflows/build-images.yml"), matrix("1.12.5", "1.31.0"))
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintFallbackLine)
	writeCITags(t, root, "1.12.5", "1.31.0")
	return root
}

func TestVersionPinsPassesWhenEveryRestatementAgrees(t *testing.T) {
	root := pinsFixture(t)
	var out strings.Builder
	if err := Run(root, false, &out, io.Discard); err != nil {
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
	writeCITags(t, root, "1.9.8", "1.31.0")

	var errOut strings.Builder
	err := Run(root, false, io.Discard, &errOut)
	if err == nil {
		t.Fatal("a stale CITofuTag must fail the gate")
	}
	if !strings.Contains(errOut.String(), "CITofuTag") || !strings.Contains(errOut.String(), "1.9.8") {
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
			body: matrix("9.9.9", "1.31.0"),
			want: "build matrix version for ci-tofu",
		},
		{
			// OUTSIDE YAML a version-tagged image reference is still a pin and
			// still has to agree with the ARG. Inside this repo's workflow YAML
			// it is not — see TestVersionPinsFloatsAnUnplacedYAMLReference.
			name: "image reference in a script",
			file: "template-scripts/pull.sh",
			body: "docker pull ghcr.io/akamai-consulting/ci-tofu:9.9.9\n",
			want: "ci-tofu image reference",
		},
		{
			// The floating fallback line rides along because this case rewrites
			// lint.yml wholesale, and a lint.yml with no image reference trips
			// the exemption-covers-nothing check before the drift is reached.
			name: "workflow env restatement",
			file: ".github/workflows/lint.yml",
			body: "env:\n  KUBECTL_VERSION: \"9.9.9\"\n" + lintFallbackLine,
			want: "KUBECTL_VERSION",
		},
		// Make's simply-expanded assignment. This form was INVISIBLE to the gate:
		// the separator was the single-character class [:=], so after consuming the
		// `:` the value had to begin at `=` and the match failed outright. The
		// Makefile is a scanRoot and carries `KUBECTL_VERSION := 1.31.0`, so the
		// one restatement in the file that declares the gate was the one it could
		// not see. Verified against the real tree before the fix: the same wrong
		// value written `=` failed the gate, written `:=` it reported "OK".
		{
			name: "makefile := restatement",
			file: "Makefile",
			body: "KUBECTL_VERSION  := 9.9.9\n",
			want: "KUBECTL_VERSION",
		},
		{
			name: "makefile = restatement",
			file: "Makefile",
			body: "KUBECTL_VERSION = 9.9.9\n",
			want: "KUBECTL_VERSION",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, filepath.FromSlash(tc.file)), tc.body)
			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
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
	writeFile(t, filepath.Join(root, "dockerfiles/Dockerfile"), ""+
		"FROM scratch\n"+
		"ARG HELM_VERSION=3.17.3\n"+
		"ARG TOFU_VERSION=1.12.5\n"+
		"ARG KUBECTL_VERSION=1.31.0\n")
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), ""+
		"env:\n"+
		"  HELM_VERSION: \"3.17.3\"\n"+
		"  ARGOCD_HELM_VERSION: \"7.8.0\"\n"+
		"  ESO_HELM_VERSION: \"2.4.1\"\n"+
		"  KYVERNO_HELM_VERSION: \"3.4.4\"\n"+lintFallbackLine)
	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err != nil {
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
		"package main\n\n// historical: CITofuTag = \"1.9.8\" and ci-tofu:1.9.8\n")
	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err != nil {
		t.Fatalf("commented versions must not count as pins: %v\n%s", err, errOut.String())
	}
}

// An empty `version` field is how build-images marks an image that carries no
// version tag (devcontainer, llz) — it is not drift against the ARG.
func TestVersionPinsSkipsMatrixEntriesWithNoVersion(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/build-images.yml"), matrix("1.12.5", "1.31.0")+
		`            {"key":"devcontainer","image":"devcontainer","target":"devcontainer","version":"","alias":""}`+"\n")
	if err := Run(root, false, io.Discard, io.Discard); err != nil {
		t.Fatalf("a versionless matrix entry must not fail: %v", err)
	}
}

// A gate that passes because it found nothing is worse than no gate: it reports
// OK on a tree it never actually read.
func TestVersionPinsRefusesToPassVacuously(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dockerfiles/Dockerfile"), "FROM scratch\n")
	err := Run(root, false, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("a Dockerfile with no ARG versions must be an error, not a pass")
	}
	if !strings.Contains(err.Error(), "vacuously") {
		t.Errorf("error should explain why, got %v", err)
	}
}

func TestVersionPinsErrorsWithoutTheAuthorityFile(t *testing.T) {
	if err := Run(t.TempDir(), false, io.Discard, io.Discard); err == nil {
		t.Fatal("a missing Dockerfile must be an error, not a silent pass")
	}
}

// The command must be reachable under `llz ci`, or the Makefile target it backs
// silently does nothing.
func TestVersionPinsSkipsTestFilesAndMissingRoots(t *testing.T) {
	root := pinsFixture(t)
	// A test fixture legitimately pins a made-up version.
	writeFile(t, filepath.Join(root, "tools/cmd/llz/thing_test.go"),
		"package main\n\nconst fixtureTag = \"ci-tofu:0.0.1\"\n")
	if err := Run(root, false, io.Discard, io.Discard); err != nil {
		t.Errorf("_test.go files must be excluded from the scan: %v", err)
	}
	// template-scripts/ and Makefile are absent from the fixture entirely.
	if _, err := os.Stat(filepath.Join(root, "Makefile")); !os.IsNotExist(err) {
		t.Fatal("fixture unexpectedly has a Makefile")
	}
}

// ── the container fallback must float ────────────────────────────────────────
//
// THE REGRESSION THESE THREE COVER. Requiring lint.yml's `vars.KUBE_IMAGE ||`
// fallback to restate the ARG made a version bump self-defeating: the bump
// pointed the Lint container at a tag build-images.yml had not published yet
// (it builds on pushes to main; Lint runs on the bump's own push), so every
// KUBECTL_VERSION / TOFU_VERSION bump cost one `manifest unknown` red and a
// manual re-run. The gate now enforces the opposite there — and still enforces
// the pin everywhere else.

// Re-pinning the fallback is what reintroduces the trap, so that is what fails.
//
// BOTH ROWS ARE LOAD-BEARING. With only the row whose tag EQUALS the ARG, the
// de-dup that stops a fallback also being scanned as a pin was untested: the
// duplicate pin site was `ok`, so it never reached errOut and deleting the skip
// changed no output. The 9.9.9 row makes the duplicate a DRIFT, so a fallback
// judged twice shows up as two verdicts on one line.
func TestVersionPinsRequiresTheContainerFallbackToFloat(t *testing.T) {
	for _, tc := range []struct{ name, tag string }{
		{"re-pinned to the current ARG", "1.12.5"},
		{"re-pinned to some other version", "9.9.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintYAML(
				"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:"+tc.tag+"', github.repository_owner) }}"))

			var errOut strings.Builder
			err := Run(root, false, io.Discard, &errOut)
			if err == nil {
				t.Fatal("a version-pinned container fallback must fail the gate — it is the ordering trap")
			}
			// One line, one rule. A fallback that is also scanned as a pin gets
			// two verdicts on the same source line, and with tc.tag == the ARG
			// they even contradict each other ("ok" and "must be :latest").
			if strings.Contains(errOut.String(), "ci-tofu image tag") {
				t.Errorf("a fallback must be judged by one rule, not also scanned as a pin:\n%s", errOut.String())
			}
			if strings.Count(errOut.String(), "container image tag") != 2 {
				t.Errorf("expected exactly one fallback verdict (annotation + summary line):\n%s", errOut.String())
			}
			if !strings.Contains(errOut.String(), "latest") {
				t.Errorf("the report must say what the fallback should name instead:\n%s", errOut.String())
			}
			// The remediation has to explain the ordering, or the next reader
			// "fixes" it by bumping the tag to match the ARG and lands straight
			// back in the trap.
			if !strings.Contains(errOut.String(), "build-images") || !strings.Contains(errOut.String(), "manifest unknown") {
				t.Errorf("the report must explain the publish ordering and name the symptom:\n%s", errOut.String())
			}
			if !strings.Contains(err.Error(), "float") {
				t.Errorf("the summary must not call a fallback a drifted pin, got %v", err)
			}
		})
	}
}

// The acceptance criterion of the issue, as a gate: bumping a tool version
// across every real restatement passes on the FIRST run, with no edit to the
// container fallback and no image published yet.
func TestVersionPinsPassesABumpThatLeavesTheFallbackFloating(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, "dockerfiles/Dockerfile"), ""+
		"FROM debian:bookworm-slim AS toolbox\n"+
		"ARG TOFU_VERSION=1.12.5\n"+
		"ARG KUBECTL_VERSION=1.34.10\n"+ // bumped from 1.31.0
		"ARG YQ_VERSION=4.44.3\n")
	writeFile(t, filepath.Join(root, ".github/workflows/build-images.yml"), matrix("1.12.5", "1.34.10"))
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"),
		"env:\n  KUBECTL_VERSION: \"1.34.10\"\n  YQ_VERSION: \"4.44.3\"\n"+lintFallbackLine)
	writeCITags(t, root, "1.12.5", "1.34.10")

	var out strings.Builder
	if err := Run(root, false, &out, io.Discard); err != nil {
		t.Fatalf("a bump that leaves the fallback floating must pass on the first run: %v", err)
	}
	// Both counts, so a reader can see the exemption is two sites and not the
	// whole scan quietly going silent.
	if !strings.Contains(out.String(), "2 container image(s) float") {
		t.Errorf("the report must say how many sites are exempt:\n%s", out.String())
	}
}

// FINDING FROM REVIEW, and then overtaken by the rule that replaced it. The
// float rule was once keyed on the FILENAME, so every tagged reference anywhere
// in lint.yml had to be `:latest`, including one in an unrelated step, rejected
// with remediation about a repo variable that had nothing to do with it.
//
// The answer is no longer "that line is an ordinary pin". A version-tagged
// reference to one of our images in this repo's workflow YAML is the ordering
// trap wherever it sits — a `run: docker pull ci-kubernetes:1.31.0` on the bump's
// own push hits `manifest unknown` exactly as a container image would. So it
// floats too; what the POSITION still decides is how precisely the report can
// explain itself, and whether the reference can vouch for a declared container.
func TestVersionPinsFloatsAnUnplacedYAMLReference(t *testing.T) {
	root := pinsFixture(t)
	// The run: line is the ONLY ci-kubernetes reference, so the vouch assertion
	// below is about this reference and not about a container the fixture
	// happens to supply.
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), ""+
		"jobs:\n"+
		"  tf:\n"+
		"    container:\n"+
		"      image: ${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}\n"+
		"    steps:\n"+
		"      - run: docker pull ghcr.io/akamai-consulting/ci-kubernetes:1.31.0\n")

	var errOut strings.Builder
	err := Run(root, false, io.Discard, &errOut)
	if err == nil {
		t.Fatal("a version tag in workflow YAML is the ordering trap wherever it sits")
	}
	// The verdict that must NEVER be given here: "bump these to match" the ARG.
	// 1.31.0 IS the ARG, so the pin rule would have called this line ok.
	if strings.Contains(errOut.String(), "disagree with") {
		t.Errorf("this must not be judged as a pin — that verdict demands the version:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "ci-kubernetes image reference") {
		t.Errorf("the report must name the reference and ask it to float:\n%s", errOut.String())
	}
	// It cannot vouch for the declared ci-kubernetes container, either: nothing
	// here says a JOB runs it.
	if !strings.Contains(errOut.String(), "no ci-kubernetes container image found") {
		t.Errorf("an unplaced reference must not satisfy a declared container:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. A templated tag used to capture empty, so the report read
// "container fallback tag is  but this fallback must float" — a blank value, and
// a header claiming it restated a version when it did no such thing.
func TestVersionPinsReportsATemplatedFallbackTagAsWhatItIs(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintYAML(
		"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:${{ env.TAG }}', github.repository_owner) }}"))

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("a templated fallback tag must fail — nothing here can tell what it resolves to")
	}
	if !strings.Contains(errOut.String(), "env.TAG") {
		t.Errorf("the report must echo the expression, not a blank value:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "restate a version") {
		t.Errorf("a templated tag is not a restated version:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. The vacuity check was keyed on the FILE alone, so
// renaming lint.yml's ci-kubernetes fallback to another image left the ci-tofu
// one satisfying the check for the whole file: the gate reported OK and the
// Kubernetes lint job walked straight back into `manifest unknown`.
//
// Both entries are exercised. Covering only one left the other free to be
// deleted from expectedFallbacks with the suite still green.
func TestVersionPinsRefusesWhenOneDeclaredFallbackGoesMissing(t *testing.T) {
	kube := "      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"
	tofu := "      image: ${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}\n"
	for _, tc := range []struct{ missing, keep string }{
		{missing: "ci-kubernetes", keep: tofu},
		{missing: "ci-tofu", keep: kube},
	} {
		t.Run(tc.missing, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"),
				"jobs:\n  only:\n    container:\n"+tc.keep)

			err := Run(root, false, io.Discard, io.Discard)
			if err == nil {
				t.Fatal("a declared fallback with no matching reference must be an error, not a pass")
			}
			var errOut strings.Builder
			_ = Run(root, false, io.Discard, &errOut)
			if !strings.Contains(errOut.String(), tc.missing) {
				t.Errorf("the report must name the fallback that went missing:\n%s", errOut.String())
			}
			if !strings.Contains(errOut.String(), "expectedFallbacks") {
				t.Errorf("the report should name the table to edit:\n%s", errOut.String())
			}
			if !strings.Contains(err.Error(), "matched nothing") {
				t.Errorf("the summary must say a rule matched nothing, got %v", err)
			}
		})
	}
}

// FINDING FROM REVIEW, and the founding scar reopened. The tag constants moved
// out of tools/cmd/llz into this package and were renamed, which took them out
// of the scan — so the class the gate was WRITTEN for silently checked nothing,
// and CITofuTag = "1.9.8" passed, exactly as it did before the gate existed.
func TestVersionPinsRefusesWhenTheConstantClassMatchesNothing(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, filepath.FromSlash(citagsFile)),
		"package versionpins\n\nconst (\n\tRenamedTofuTag = \"1.12.5\"\n)\n")

	var errOut strings.Builder
	err := Run(root, false, io.Discard, &errOut)
	if err == nil {
		t.Fatal("a goConst that matches no file must be an error — the class would check nothing")
	}
	if !strings.Contains(errOut.String(), "CITofuTag") {
		t.Errorf("the report must name the constant it lost:\n%s", errOut.String())
	}
	if !strings.Contains(err.Error(), "matched nothing") {
		t.Errorf("the summary must say a rule matched nothing, got %v", err)
	}
}

// FINDING FROM REVIEW. reMatrixEntry reads two adjacent JSON fields in ONE
// regex, so writing "version" before "image" — a purely cosmetic edit nobody
// would think twice about — stops it matching and the row silently stops being
// a site. Reproduced against the real tree: with the fields reordered and a
// planted 9.9.9 for ci-kubernetes, the gate printed OK and exited 0.
func TestVersionPinsRefusesWhenAMatrixRowStopsMatching(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/build-images.yml"),
		"          ALL='[\n"+
			`            {"key":"terraform","image":"ci-tofu","target":"ci-tofu","version":"1.12.5","alias":""},`+"\n"+
			// Same data, fields transposed — and invisible to the pattern.
			`            {"key":"kubernetes","version":"9.9.9","image":"ci-kubernetes","target":"ci-kubernetes","alias":""}`+"\n"+
			"          ]'\n")

	var errOut strings.Builder
	err := Run(root, false, io.Discard, &errOut)
	if err == nil {
		t.Fatal("a matrix row the pattern can no longer see must be an error, not a silent pass")
	}
	if !strings.Contains(errOut.String(), "ci-kubernetes") {
		t.Errorf("the report must name the row that went missing:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "reorder") {
		t.Errorf("the report should name the cosmetic edit that causes this:\n%s", errOut.String())
	}
	if !strings.Contains(err.Error(), "matched nothing") {
		t.Errorf("the summary must say a rule matched nothing, got %v", err)
	}
}

// FINDING FROM REVIEW. A vacuity failure used to print one bare line with no
// `::error` annotation, so GitHub showed nothing against the file — unlike every
// other version-pins failure — and it returned before the --verbose listing, the
// one output that shows a reader WHICH sites were found and therefore where the
// hole is.
func TestVersionPinsAnnotatesAndListsOnAVacuityFailure(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), ""+
		"jobs:\n  tf:\n    container:\n"+
		"      image: ${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}\n")

	var out, errOut strings.Builder
	if err := Run(root, true, &out, &errOut); err == nil {
		t.Fatal("expected the vacuity failure this test is about")
	}
	if !strings.Contains(errOut.String(), "::error file=.github/workflows/lint.yml::") {
		t.Errorf("a vacuity failure must annotate the file like every other failure:\n%s", errOut.String())
	}
	if !strings.Contains(out.String(), "ci-tofu container image tag") {
		t.Errorf("--verbose must still list what WAS found — that is how the hole is seen:\n%s", out.String())
	}
}

// FINDING FROM REVIEW. The vacuity check used to return the moment it found a
// hole, which suppressed every drift the same run had already collected — and CI
// runs this gate WITHOUT --verbose, so that drift was emitted nowhere at all and
// cost a second red round-trip to discover. One red should say everything.
func TestVersionPinsReportsDriftAndVacuityTogether(t *testing.T) {
	root := pinsFixture(t)
	// Same data, fields transposed — invisible to reMatrixEntry.
	writeFile(t, filepath.Join(root, ".github/workflows/build-images.yml"),
		"          ALL='[\n"+
			`            {"key":"terraform","image":"ci-tofu","target":"ci-tofu","version":"1.12.5","alias":""},`+"\n"+
			`            {"key":"kubernetes","version":"1.31.0","image":"ci-kubernetes","target":"ci-kubernetes","alias":""}`+"\n"+
			"          ]'\n")
	writeFile(t, filepath.Join(root, "Makefile"), "KUBECTL_VERSION  := 9.9.9\n")

	var errOut strings.Builder
	err := Run(root, false, io.Discard, &errOut)
	if err == nil {
		t.Fatal("expected both failures")
	}
	if !strings.Contains(errOut.String(), "Makefile") {
		t.Errorf("the drift must still be reported alongside the vacuity:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "no build-matrix version was found") {
		t.Errorf("the vacuity must be reported too:\n%s", errOut.String())
	}
	if !strings.Contains(err.Error(), "drifted") || !strings.Contains(err.Error(), "matched nothing") {
		t.Errorf("the summary must name both classes, got %v", err)
	}
}

// FINDING FROM REVIEW. Renaming an ARG made vacuities() blame two innocent
// files: it reported that CITofuTag "was not found under any scanRoot" and that
// the matrix row "was removed, or its fields were reordered" — both untrue, both
// pointing at correct code, with the real cause (the ARG the two classes are
// keyed on no longer resolving) named nowhere.
func TestVersionPinsNamesTheMissingArgRatherThanBlamingItsDependents(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, "dockerfiles/Dockerfile"), ""+
		"FROM debian:bookworm-slim AS toolbox\n"+
		"ARG OPENTOFU_VERSION=1.12.5\n"+ // renamed from TOFU_VERSION
		"ARG KUBECTL_VERSION=1.31.0\n"+
		"ARG YQ_VERSION=4.44.3\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("an ARG that no longer resolves must fail — every rule keyed on it stops being a site")
	}
	if !strings.Contains(errOut.String(), "declares no TOFU_VERSION") {
		t.Errorf("the report must name the ARG that went missing:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "was not found under any scanRoot") {
		t.Errorf("the constant is where it always was — do not blame it:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "reordered") {
		t.Errorf("the matrix row is intact — do not blame it:\n%s", errOut.String())
	}
}

// A DIGEST IS REFUSED, and this test used to assert the opposite. Accepting it
// rested on half the test — immutable and by definition already published, both
// true — and ignored the other half, "stays correct as versions move", which a
// digest never does: it pins one build forever, so the job silently stops
// tracking the toolchain the tree declares. That is precisely why a `sha-` tag is
// rejected. Docker resolves `name:tag@digest` BY DIGEST and ignores the tag, so
// `:latest@sha256:…` read as floating and was frozen.
func TestVersionPinsRefusesADigestPinnedContainerImage(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct{ name, image string }{
		{"tagged and digested", "ghcr.io/akamai-consulting/ci-tofu:latest" + digest},
		{"digest only", "ghcr.io/akamai-consulting/ci-tofu" + digest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintYAML(tc.image))

			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
				t.Fatal("a digest freezes the job on one build — the same reason a sha- tag is refused")
			}
			if !strings.Contains(errOut.String(), "sha256") {
				t.Errorf("the report must show the digest it found:\n%s", errOut.String())
			}
			if !strings.Contains(errOut.String(), "frozen for exactly that reason") {
				t.Errorf("the remediation must give the freeze reason, not advertise digests:\n%s", errOut.String())
			}
		})
	}
}

// FINDING FROM REVIEW, TWICE. The shape rule first reached instance-template/,
// where `:latest` is the WRONG answer — an instance's image has to match the
// template ref it is pinned to (`llz ci assert-image-fresh`), which is why the
// delivered workflows resolve ci-tofu:sha-<template pin>, so the remediation
// would have told an adopter to unpin exactly what the adjacent gate holds
// still. Exempting it was worse: reImageTag's digit-leading filter means such a
// fallback is not a pin site either, so it was checked by NO rule and the gate
// exited 0. It is forbidden there instead, which makes the gate ask the question
// rather than answer it wrongly by silence.
func TestVersionPinsForbidsAFallbackInTheDeliveredTree(t *testing.T) {
	for _, tag := range []string{"latest", "sha-abc1234", "1.12.5"} {
		t.Run(tag, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, "instance-template/.github/workflows/llz-terraform.yml"), ""+
				"jobs:\n  tf:\n    container:\n"+
				"      image: ${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:"+tag+"', github.repository_owner) }}\n")

			var errOut strings.Builder
			err := Run(root, false, io.Discard, &errOut)
			if err == nil {
				t.Fatal("a fallback in the delivered tree must be caught by SOME rule, not fall between two")
			}
			if !strings.Contains(errOut.String(), "delivered tree") {
				t.Errorf("the report must name the class:\n%s", errOut.String())
			}
			// The one thing it must never say here.
			if strings.Contains(errOut.String(), "must float on :latest") {
				t.Errorf("floating is the wrong advice for an instance:\n%s", errOut.String())
			}
			if !strings.Contains(errOut.String(), "assert-image-fresh") {
				t.Errorf("the report must point at the gate that owns instance image pinning:\n%s", errOut.String())
			}
		})
	}
}

// FINDING FROM REVIEW. The matrix vacuity check was keyed on the label alone, so
// a matching JSON pair in ANY scanned file satisfied it — and transposing the
// fields in build-images.yml's own row, the exact edit the check exists to
// catch, would then go silent while the row that actually decides what gets
// published sat unread. Same file-keying hole the fallback check had closed.
func TestVersionPinsScopesTheMatrixCheckToTheFileThatDrivesTheBuild(t *testing.T) {
	root := pinsFixture(t)
	// The real row, transposed and therefore invisible to reMatrixEntry...
	writeFile(t, filepath.Join(root, ".github/workflows/build-images.yml"),
		"          ALL='[\n"+
			`            {"key":"terraform","image":"ci-tofu","target":"ci-tofu","version":"1.12.5","alias":""},`+"\n"+
			`            {"key":"kubernetes","version":"1.31.0","image":"ci-kubernetes","target":"ci-kubernetes","alias":""}`+"\n"+
			"          ]'\n")
	// ...and a well-formed pair somewhere else, which must NOT stand in for it.
	writeFile(t, filepath.Join(root, ".github/workflows/other.yml"),
		`            {"image":"ci-kubernetes","version":"1.31.0"}`+"\n")

	var errOut strings.Builder
	err := Run(root, false, io.Discard, &errOut)
	if err == nil {
		t.Fatal("a matrix row that stopped matching must fail even when another file happens to match")
	}
	if !strings.Contains(errOut.String(), "build-images.yml") {
		t.Errorf("the report must name the file whose row went unread:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. The forbidden-class annotation formatted the PATH into
// the slot the drift and float branches use for the label, so two fallbacks in
// one delivered workflow produced two identical messages naming neither image.
// The annotation is what shows against the diff; the summary below it was fine.
func TestVersionPinsAnnotatesEachDeliveredTreeFallbackDistinctly(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, "instance-template/.github/workflows/llz-terraform.yml"), ""+
		"jobs:\n"+
		"  tf:\n    container:\n"+
		"      image: ${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}\n"+
		"  k8s:\n    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("expected the delivered-tree failure this test is about")
	}
	// Asserted on the ANNOTATION LINE, not on errOut as a whole: the summary
	// block below it prints s.what correctly either way, so a whole-output
	// substring check passes on the very bug this test exists for.
	// Lines 4 and 7 of the fixture below — each annotation must point at its own
	// job's image, which is the half that was broken.
	for _, tc := range []struct {
		line  int
		image string
	}{{4, "ci-tofu"}, {7, "ci-kubernetes"}} {
		want := fmt.Sprintf("::error file=instance-template/.github/workflows/llz-terraform.yml,line=%d::"+
			"%s container image in the delivered tree — the delivered tree must not carry one", tc.line, tc.image)
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("the annotation must name its own image, want %q in:\n%s", want, errOut.String())
		}
	}
}

// FINDING FROM REVIEW, and the correction of an over-correction. Loosening the
// shape to "some `vars.<X> ||` earlier on the line" turned an ordinary reference
// that merely SHARED a line with an unrelated default into a fallback: this repo
// carries `vars.X || 'default'` in a dozen places. The consequences were a false
// red demanding `:latest`, remediation telling the reader to set an image
// variable that has nothing to do with the line, an unfixable
// forbidden-delivered-tree error under instance-template/, and — worst — a bogus
// site registering in foundFallback, where it could stand in for a real fallback
// that had been deleted.
func TestVersionPinsDoesNotMistakeAnUnrelatedDefaultForAFallback(t *testing.T) {
	const noise = "      - run: docker run ${{ vars.EXTRA_FLAGS || '' }} " +
		"ghcr.io/akamai-consulting/ci-tofu:1.12.5 sh\n"

	t.Run("it is not treated as a container image", func(t *testing.T) {
		root := pinsFixture(t)
		writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintYAML(
			"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}", noise))

		var out, errOut strings.Builder
		// It fails — a version tag in workflow YAML floats wherever it sits —
		// but as an unplaced REFERENCE, not as a container image.
		if err := Run(root, false, io.Discard, &errOut); err == nil {
			t.Fatal("1.12.5 in a run: line is the same ordering trap as anywhere else")
		}
		if !strings.Contains(errOut.String(), "ci-tofu image reference") {
			t.Errorf("it must be reported as a reference of unknown position:\n%s", errOut.String())
		}
		if strings.Contains(errOut.String(), "ci-tofu container image") {
			t.Errorf("a `run:` line is not a container image, and the remediation differs:\n%s", errOut.String())
		}
		// And the thing that mattered most: it must not vouch. Delete the real
		// ci-tofu container and the declared entry has to go unsatisfied.
		writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), ""+
			"jobs:\n  k8s:\n    container:\n"+
			"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
			"    steps:\n"+noise)
		out.Reset()
		errOut.Reset()
		if err := Run(root, false, &out, &errOut); err == nil {
			t.Fatal("no job runs ci-tofu — the declared entry is unsatisfied")
		}
		if !strings.Contains(errOut.String(), "no ci-tofu container image found") {
			t.Errorf("an unplaced reference must not stand in for a deleted container:\n%s", errOut.String())
		}
	})

	// The one that made a silent pass possible: a bogus floating site satisfying
	// the vacuity check for a declared fallback that is gone.
	t.Run("it cannot stand in for a deleted fallback", func(t *testing.T) {
		root := pinsFixture(t)
		writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), ""+
			"jobs:\n  tf:\n    container:\n"+
			"      image: ${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}\n"+
			"    steps:\n"+
			"      - run: docker run ${{ vars.EXTRA_FLAGS || '' }} ghcr.io/akamai-consulting/ci-kubernetes:latest sh\n")

		var errOut strings.Builder
		if err := Run(root, false, io.Discard, &errOut); err == nil {
			t.Fatal("the ci-kubernetes fallback is gone — a lookalike line must not cover for it")
		}
		if !strings.Contains(errOut.String(), "no ci-kubernetes container image found") {
			t.Errorf("the report must name the fallback that is actually missing:\n%s", errOut.String())
		}
	})
}

// FINDING FROM REVIEW. --verbose marked every failing site "DRIFT", including
// the two classes whose whole point is that they are NOT pins. On a floating
// fallback that reads as "the tag disagrees with the Dockerfile", which invites
// precisely the bump-to-match edit the split reporting exists to prevent.
func TestVersionPinsVerboseLabelsEachClassAsItself(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintYAML(
		"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:9.9.9', github.repository_owner) }}"))
	writeFile(t, filepath.Join(root, "instance-template/.github/workflows/llz-terraform.yml"), ""+
		"jobs:\n  k8s:\n    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n")
	writeFile(t, filepath.Join(root, "Makefile"), "KUBECTL_VERSION  := 9.9.9\n")

	var out strings.Builder
	if err := Run(root, true, &out, io.Discard); err == nil {
		t.Fatal("expected the three failures this test is about")
	}
	for _, want := range []string{
		"FLOAT  .github/workflows/lint.yml",
		"FORBID instance-template/.github/workflows/llz-terraform.yml",
		"DRIFT  Makefile",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("--verbose must label each class as itself, want %q in:\n%s", want, out.String())
		}
	}
}

// FINDING FROM REVIEW. The float remediation said ":latest is the only tag that
// is already published", which is false — build-images.yml pushes :sha-<commit>
// from the same build. A sha-pinned fallback was rejected with an explanation
// that did not apply to it. It is still rejected (a sha freezes to one commit,
// so the fallback silently stops tracking the toolchain the tree declares), but
// the reason given has to be the real one.
func TestVersionPinsExplainsAShaTaggedFallbackHonestly(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintYAML(
		"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:sha-deadbeef', github.repository_owner) }}"))

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("a sha-pinned fallback must still fail — it freezes to one commit")
	}
	if !strings.Contains(errOut.String(), "sha- tag") || !strings.Contains(errOut.String(), "frozen to one commit") {
		t.Errorf("the report must give the reason that actually applies to a sha tag:\n%s", errOut.String())
	}
	// The false claim: a sha- tag IS published, so "already published" alone
	// cannot be the reason. Asserted on the surviving half of the sentence, which
	// is wrapped in the output — the earlier version quoted it unwrapped, so the
	// condition could never hold and the rationale it guards could have come back
	// with the test still green.
	if !strings.Contains(errOut.String(), "stays correct as versions move") {
		t.Errorf("'already published' alone does not distinguish :latest from :sha-:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. An empty tag (`ci-tofu:`) printed "tag is  but this
// fallback must float" — a sentence with a hole in it, the same blank-value
// defect that widening the templated-tag capture was meant to remove.
func TestVersionPinsRendersAnEmptyTagVisibly(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintYAML(
		"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:', github.repository_owner) }}"))

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("an empty tag must fail — it names nothing")
	}
	if !strings.Contains(errOut.String(), "(empty)") {
		t.Errorf("an empty capture must be rendered, not left as a hole in the sentence:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "tag is  but") {
		t.Errorf("the blank-value defect is back:\n%s", errOut.String())
	}
}

// THE SPELLINGS THAT DEFEATED FOUR REGEXES, kept as a table because each row was
// a separate silent failure at some point. Version-tagged, each fell through to
// the PIN rule, so the gate REQUIRED the version and re-mandated the ordering
// trap; written `:latest`, each matched no rule and the gate printed OK.
//
// None of them is special now: the rule reads `jobs.<id>.container.image` by
// POSITION, so how the value is spelled cannot decide whether it is judged. That
// is the property this table exists to hold, not the particular spellings.
func TestVersionPinsRecognisesEveryFallbackSpelling(t *testing.T) {
	for _, tc := range []struct{ name, ref string }{
		{"format with the owner interpolated", "format('ghcr.io/{0}/ci-tofu:%s', github.repository_owner)"},
		{"owner spelled out", "'ghcr.io/akamai-consulting/ci-tofu:%s'"},
		{"double-quoted", `"ghcr.io/akamai-consulting/ci-tofu:%s"`},
		{"registry composed into the format arg", "format('{0}/ci-tofu:%s', env.REPO)"},
		{"space after format(", "format( 'ghcr.io/{0}/ci-tofu:%s', github.repository_owner)"},
		{"wrapped in an extra paren", "(format('ghcr.io/{0}/ci-tofu:%s', github.repository_owner))"},
		{"github's non-empty ternary", "vars.TF_IMAGE != '' && vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:%s', github.repository_owner)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := func(tag string) string {
				return "${{ vars.TF_IMAGE || " + strings.Replace(tc.ref, "%s", tag, 1) + " }}"
			}
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintYAML(line("latest")))
			var out strings.Builder
			if err := Run(root, false, &out, io.Discard); err != nil {
				t.Fatalf("a floating fallback must pass whatever form it is written in: %v", err)
			}
			if !strings.Contains(out.String(), "2 container image(s) float") {
				t.Errorf("this form must count as a fallback, not vanish:\n%s", out.String())
			}

			root = pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintYAML(line("1.12.5")))
			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
				t.Fatal("a version-pinned fallback must fail however it is written — it is the ordering trap")
			}
			if !strings.Contains(errOut.String(), "container image") {
				t.Errorf("this form must be judged as a fallback, not as an ordinary pin:\n%s", errOut.String())
			}
			if strings.Contains(errOut.String(), "ci-tofu image tag") {
				t.Errorf("the pin rule must not also claim it:\n%s", errOut.String())
			}
		})
	}
}

// FINDING FROM REVIEW. build-images.yml publishes ci-tofu under ci-terraform too
// — a live deprecation window, because instances pin it through vars.TF_IMAGE.
// imagePins omitted the alias, so a ci-terraform reference was gated by NEITHER
// rule and the gate exited 0 on a version-pinned ci-terraform fallback.
func TestVersionPinsGatesThePublishedAlias(t *testing.T) {
	t.Run("as a fallback", func(t *testing.T) {
		root := pinsFixture(t)
		writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintYAML(
			"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-terraform:1.12.5', github.repository_owner) }}"))

		var errOut strings.Builder
		if err := Run(root, false, io.Discard, &errOut); err == nil {
			t.Fatal("the alias carries the same tag from the same build — a pinned fallback on it is the same trap")
		}
		if !strings.Contains(errOut.String(), "ci-terraform container image") {
			t.Errorf("the report must name the alias as written:\n%s", errOut.String())
		}
	})
	t.Run("as an unplaced reference", func(t *testing.T) {
		root := pinsFixture(t)
		writeFile(t, filepath.Join(root, ".github/workflows/publish-charts.yml"),
			"      image: ghcr.io/akamai-consulting/ci-terraform:9.9.9\n")

		var errOut strings.Builder
		if err := Run(root, false, io.Discard, &errOut); err == nil {
			t.Fatal("a version-tagged alias reference is the same trap under either name")
		}
		if !strings.Contains(errOut.String(), "ci-terraform image reference") {
			t.Errorf("the report must name the alias as written:\n%s", errOut.String())
		}
	})
	t.Run("as a pin outside YAML", func(t *testing.T) {
		root := pinsFixture(t)
		writeFile(t, filepath.Join(root, "template-scripts/pull.sh"),
			"docker pull ghcr.io/akamai-consulting/ci-terraform:9.9.9\n")

		var errOut strings.Builder
		if err := Run(root, false, io.Discard, &errOut); err == nil {
			t.Fatal("outside workflow YAML the alias is still a pin and must agree with the ARG")
		}
		if !strings.Contains(errOut.String(), "1.12.5") {
			t.Errorf("the report must name the authoritative value:\n%s", errOut.String())
		}
	})
}

// FINDING FROM REVIEW. The bare-ARG class's leading boundary `(?:^|[^A-Z0-9_])`
// consumes the preceding NEWLINE for an assignment at the start of a line, so
// the reported line was the one above it — the real tree annotated Makefile:12
// and :19 for assignments on 13 and 20. An `::error file=…,line=` that points at
// the wrong line puts the annotation on the wrong line of the diff.
func TestVersionPinsReportsTheLineTheRestatementIsActuallyOn(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, "Makefile"), ""+
		"# line 1\n"+
		"# line 2\n"+
		"KUBECTL_VERSION  := 9.9.9\n") // line 3

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("expected the drift this test is about")
	}
	if !strings.Contains(errOut.String(), "Makefile,line=3::") {
		t.Errorf("the annotation must name line 3, where the assignment is:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "Makefile,line=2::") {
		t.Errorf("the off-by-one is back:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW, and the case that ended the regex approach. A container
// image whose expression WRAPS — `vars.KUBE_IMAGE ||` at the end of one line and
// `format(…)` on the next, which is ordinary YAML — was not recognised as a
// fallback: version-tagged it fell to the PIN rule, so the gate REQUIRED the
// version and re-mandated the ordering trap; written `:latest` it matched no
// rule and the gate printed OK. A var not ending in _IMAGE failed identically.
// Locating the value by YAML position rather than by expression shape makes both
// cases impossible rather than handled.
func TestVersionPinsReadsAContainerImageHoweverTheValueIsWritten(t *testing.T) {
	for _, tc := range []struct{ name, image string }{
		{"wrapped across lines", "${{ vars.KUBE_IMAGE ||\n        format('ghcr.io/{0}/ci-tofu:%s', github.repository_owner) }}"},
		{"a var not ending in _IMAGE", "${{ vars.TOOLBOX || format('ghcr.io/{0}/ci-tofu:%s', github.repository_owner) }}"},
		{"no variable at all", "ghcr.io/akamai-consulting/ci-tofu:%s"},
		{"single-quoted scalar", "'ghcr.io/akamai-consulting/ci-tofu:%s'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"),
				lintYAML(strings.Replace(tc.image, "%s", "latest", 1)))
			var out strings.Builder
			if err := Run(root, false, &out, io.Discard); err != nil {
				t.Fatalf("a floating container image must pass however the value is written: %v", err)
			}
			if !strings.Contains(out.String(), "2 container image(s) float") {
				t.Errorf("this form must be read as a container image, not vanish:\n%s", out.String())
			}

			root = pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"),
				lintYAML(strings.Replace(tc.image, "%s", "1.12.5", 1)))
			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
				t.Fatal("a version-tagged container image must fail however it is written — it is the ordering trap")
			}
			if !strings.Contains(errOut.String(), "container image tag") {
				t.Errorf("it must be judged by position, not fall through to the pin rule:\n%s", errOut.String())
			}
			if strings.Contains(errOut.String(), "ci-tofu image tag") {
				t.Errorf("the pin rule must not also claim it:\n%s", errOut.String())
			}
		})
	}
}

// FINDING FROM REVIEW. Floating sites carried the PRIMARY image name even when
// written with an alias, so rewriting lint.yml's ci-tofu container as
// `ci-terraform:latest` satisfied the declared {lint.yml, ci-tofu} entry and the
// gate printed OK — the same rename-satisfies-the-check hole that (file, image)
// keying exists to close. It bites exactly when the ci-terraform deprecation
// window closes and that tag stops being republished.
func TestVersionPinsDoesNotLetAnAliasSatisfyTheDeclaredEntry(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintYAML(
		"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-terraform:latest', github.repository_owner) }}"))

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("the declared ci-tofu container image is gone — the alias must not stand in for it")
	}
	if !strings.Contains(errOut.String(), "no ci-tofu container image found") {
		t.Errorf("the report must name the declared image that is missing:\n%s", errOut.String())
	}
}

// Delivered-tree sites are keyed the same way, for the same reason.
func TestVersionPinsNamesTheAliasInADeliveredTreeVerdict(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, "instance-template/.github/workflows/llz-terraform.yml"), ""+
		"jobs:\n  tf:\n    container:\n"+
		"      image: ghcr.io/akamai-consulting/ci-terraform:latest\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("a hardcoded alias in the delivered tree must fail like the primary name")
	}
	if !strings.Contains(errOut.String(), "ci-terraform container image in the delivered tree") {
		t.Errorf("the verdict must name the image as written:\n%s", errOut.String())
	}
}

// Service containers are pulled the same way and fail the same way, so they are
// read the same way.
func TestVersionPinsJudgesServiceContainersToo(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintFallbackLine+""+
		"  extra:\n"+
		"    services:\n"+
		"      registry:\n"+
		"        image: ghcr.io/akamai-consulting/ci-kubernetes:1.31.0\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("a version-tagged service container is the same trap as a job container")
	}
	if !strings.Contains(errOut.String(), "ci-kubernetes container image tag") {
		t.Errorf("a service container must be judged by the same rule:\n%s", errOut.String())
	}
}

// GitHub's shorthand: `container: <image>` with no `image:` key. It is the same
// position and the same trap, so it is read the same way.
func TestVersionPinsReadsTheContainerShorthand(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintFallbackLine+""+
		"  shorthand:\n"+
		"    container: ghcr.io/akamai-consulting/ci-kubernetes:1.31.0\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("the shorthand form names a container image too")
	}
	if !strings.Contains(errOut.String(), "ci-kubernetes container image tag") {
		t.Errorf("the shorthand must be judged by the same rule:\n%s", errOut.String())
	}
}

// containerImages reads the RAW text, not the comment-masked copy the regex
// classes use. Raised in review as a live break — an indented comment inside a
// `run: |` block scalar becoming an all-space line that yaml.v3 rejects — which
// did not reproduce: masked and unmasked parse identically here, as they did in
// six other constructed cases, because a spaces-only line reads as empty.
//
// The test stays because the PROPERTY is worth holding whatever the parser does
// with the masked copy today: comments must not cost a file its container
// images. If they did, the file's image would fall through to the PIN rule and a
// version-tagged one would be REQUIRED rather than forbidden — the gate
// mandating the trap.
func TestVersionPinsFindsContainerImagesInACommentedWorkflow(t *testing.T) {
	steps := "      - run: |\n" +
		"          set -euo pipefail\n" +
		"            # a deeply indented comment inside the block scalar\n" +
		"          echo hi\n"

	t.Run("a floating image is still seen", func(t *testing.T) {
		root := pinsFixture(t)
		writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintYAML(
			"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}", steps))
		var out strings.Builder
		if err := Run(root, false, &out, io.Discard); err != nil {
			t.Fatalf("a comment must not cost the file its container images: %v", err)
		}
		if !strings.Contains(out.String(), "2 container image(s) float") {
			t.Errorf("both container images must still be found:\n%s", out.String())
		}
	})

	t.Run("a version-tagged image is still forbidden, not required", func(t *testing.T) {
		root := pinsFixture(t)
		writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintYAML(
			"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:1.12.5', github.repository_owner) }}", steps))
		var errOut strings.Builder
		if err := Run(root, false, io.Discard, &errOut); err == nil {
			t.Fatal("a version-tagged container image must fail whatever comments the file carries")
		}
		if strings.Contains(errOut.String(), "ci-tofu image tag") {
			t.Errorf("the pin rule must not claim it — that verdict demands the version:\n%s", errOut.String())
		}
	})
}

// FINDING FROM REVIEW. The container/pin de-dup was keyed on the value's FIRST
// line, but a value may wrap, so the pin rule matched the continuation and the
// same reference got two contradictory verdicts — FLOAT on one line, DRIFT on
// the next, the drift remediation saying "bump these to match it", the edit that
// re-arms the trap.
//
// THE TAG MUST DIFFER FROM THE ARG. With 1.12.5 the duplicate pin site is `ok`
// and never printed, so the test passes on the bug — the same vacuity that hid
// this de-dup once already.
func TestVersionPinsGivesOneVerdictToAWrappedContainerImage(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintYAML(
		"${{ vars.TF_IMAGE ||\n        format('ghcr.io/{0}/ci-tofu:9.9.9', github.repository_owner) }}"))

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("expected the float failure this test is about")
	}
	if strings.Contains(errOut.String(), "ci-tofu image tag") {
		t.Errorf("a wrapped value must get ONE verdict, not a second from the pin rule:\n%s", errOut.String())
	}
	if strings.Count(errOut.String(), "container image tag") != 2 {
		t.Errorf("expected exactly one float verdict (annotation + summary line):\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. The continuation-indent anchor was the VALUE's own first
// line, which is right only when the value starts on the same line as the key.
// Written across a line break after `image:` the continuation sits at the SAME
// indent as the value's first line, so `<= start` stopped the range early, the
// pin rule matched the remainder, and one reference got two contradictory
// verdicts again. YAML anchors continuations on the KEY, and so does this now.
func TestVersionPinsGivesOneVerdictWhenTheValueStartsBelowTheKey(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
		"  tf:\n"+
		"    container:\n"+
		"      image:\n"+
		"        ${{ vars.TF_IMAGE ||\n"+
		"        format('ghcr.io/{0}/ci-tofu:9.9.9', github.repository_owner) }}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("expected the float failure this test is about")
	}
	if strings.Contains(errOut.String(), "ci-tofu image tag") {
		t.Errorf("the continuation must be inside the container range, not a second pin verdict:\n%s", errOut.String())
	}
	if strings.Count(errOut.String(), "container image tag") != 2 {
		t.Errorf("expected exactly one float verdict (annotation + summary line):\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. A tagless reference is Docker's implicit `:latest` —
// exactly what the rule wants — but the matcher required an explicit colon, so
// it produced no site and the vacuity check then reported the job as "moved or
// genuinely gone" while it sat right there in the file.
func TestVersionPinsReadsATaglessReferenceAsImplicitLatest(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ghcr.io/akamai-consulting/ci-kubernetes\n"+
		"  tf:\n"+
		"    container:\n"+
		"      image: ghcr.io/akamai-consulting/ci-tofu\n")

	var out strings.Builder
	if err := Run(root, false, &out, io.Discard); err != nil {
		t.Fatalf("a tagless reference already means :latest and must pass: %v", err)
	}
	if !strings.Contains(out.String(), "2 container image(s) float") {
		t.Errorf("both tagless references must count as container images:\n%s", out.String())
	}
}

// ...and the right boundary that keeps the tagless matcher honest.
func TestVersionPinsDoesNotMatchALongerImageName(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintFallbackLine+""+
		"  other:\n"+
		"    container:\n"+
		"      image: ghcr.io/akamai-consulting/ci-tofu-experimental\n")

	var out strings.Builder
	if err := Run(root, false, &out, io.Discard); err != nil {
		t.Fatalf("a different image that merely starts with ours is not ours: %v", err)
	}
	if !strings.Contains(out.String(), "2 container image(s) float") {
		t.Errorf("ci-tofu-experimental must not count as a third site:\n%s", out.String())
	}
}

// FINDING FROM REVIEW, three spellings with one root: the float rule read a
// SINGLE occurrence of the image name and assumed the rest of the expression
// inert. Each of these lands on the `manifest unknown` trap the gate exists to
// prevent, and each exited 0 before.
func TestVersionPinsJudgesTheWholeContainerImageValue(t *testing.T) {
	for _, tc := range []struct{ name, image string }{
		{
			// The name occurs TAGLESS (inside a quoted format argument) and the
			// version is attached to {1}, so the tagless branch called it an
			// implicit :latest.
			name:  "version attached to a format placeholder",
			image: "${{ format('ghcr.io/{0}/{1}:1.34.10', github.repository_owner, 'ci-kubernetes') }}",
		},
		{
			// Only the leftmost match was judged; the second occurrence was
			// invisible to this rule and skipped by the pin rule, its line being
			// a container line.
			name: "a second occurrence after a floating first",
			image: "${{ vars.KUBE_IMAGE && 'ghcr.io/akamai-consulting/ci-kubernetes:latest' " +
				"|| 'ghcr.io/akamai-consulting/ci-kubernetes:1.34.10' }}",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
				"jobs:\n"+
				"  k8s:\n"+
				"    container:\n"+
				"      image: "+tc.image+"\n"+
				"  tf:\n"+
				"    container:\n"+
				"      image: ${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}\n")

			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
				t.Fatal("a version tag anywhere in a container image value is the ordering trap")
			}
			if !strings.Contains(errOut.String(), "ci-kubernetes container image tag") {
				t.Errorf("the report must name the container image:\n%s", errOut.String())
			}
		})
	}
}

// FINDING FROM REVIEW. The continuation scan broke on a blank line, but a blank
// line is legal inside a plain multi-line scalar and does not end the value.
// endLine truncated, the pin rule re-judged the remainder, and the same
// reference got FLOAT and DRIFT — with the "Either bump these to match it"
// remediation that re-arms the trap. This is the bug endLine was added to fix.
func TestVersionPinsGivesOneVerdictAcrossABlankLineInTheValue(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
		"  tf:\n"+
		"    container:\n"+
		"      image: ${{ vars.TF_IMAGE ||\n"+
		"\n"+
		"        format('ghcr.io/{0}/ci-tofu:9.9.9', github.repository_owner) }}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("expected the float failure this test is about")
	}
	if strings.Contains(errOut.String(), "ci-tofu image tag") {
		t.Errorf("a blank line must not end the value and hand the rest to the pin rule:\n%s", errOut.String())
	}
	if strings.Count(errOut.String(), "container image tag") != 2 {
		t.Errorf("expected exactly one float verdict (annotation + summary line):\n%s", errOut.String())
	}
}

// A registry port is not a tag: `ghcr.io` is a literal segment that is not one
// of ours, so its `:443` is somebody else's business — the same rule that leaves
// `debian:16` alone.
func TestVersionPinsDoesNotReadAPortAsAVersion(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintYAML(
		"ghcr.io:443/akamai-consulting/ci-tofu:latest"))

	var out strings.Builder
	if err := Run(root, false, &out, io.Discard); err != nil {
		t.Fatalf("a registry port is not a tag: %v", err)
	}
	if !strings.Contains(out.String(), "2 container image(s) float") {
		t.Errorf("the ported reference must still count as floating:\n%s", out.String())
	}
}

// The whole-value scan catches a second occurrence carrying a VERSION; this is
// the other half — a second occurrence carrying a tag that is merely wrong.
// Without it, judging only the leftmost match survives mutation.
func TestVersionPinsJudgesEveryOccurrenceNotJustTheFirst(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintYAML(
		"${{ vars.TF_IMAGE && 'ghcr.io/akamai-consulting/ci-tofu:latest' "+
			"|| 'ghcr.io/akamai-consulting/ci-tofu:sha-abc1234' }}"))

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("a second occurrence that does not float must fail, even behind one that does")
	}
	if !strings.Contains(errOut.String(), "sha-abc1234") {
		t.Errorf("the report must name the occurrence that fails:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. The image can be named through a format PLACEHOLDER, so
// the name occurs tagless (it is a quoted argument) while the tag rides on
// `{1}`. A name-anchored rule reads that as an implicit :latest and passes it.
// The dotted-version scan closed only the version spelling; sha- and templated
// tags still went through.
func TestVersionPinsJudgesATagAttachedToAFormatPlaceholder(t *testing.T) {
	for _, tc := range []struct{ name, image, want string }{
		{"version", "${{ format('ghcr.io/{0}/{1}:1.34.10', github.repository_owner, 'ci-kubernetes') }}", "1.34.10"},
		{"sha", "${{ format('ghcr.io/{0}/{1}:sha-abcdef0', github.repository_owner, 'ci-kubernetes') }}", "sha-abcdef0"},
		{"templated", "${{ format('ghcr.io/{0}/{1}:{2}', github.repository_owner, 'ci-kubernetes', env.TAG) }}", "{2}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
				"jobs:\n"+
				"  k8s:\n"+
				"    container:\n"+
				"      image: "+tc.image+"\n"+
				"  tf:\n"+
				"    container:\n"+
				"      image: ${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}\n")

			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
				t.Fatalf("a %s tag on a placeholder is the same trap as one written directly", tc.name)
			}
			if !strings.Contains(errOut.String(), tc.want) {
				t.Errorf("the report must name the tag it found (%s):\n%s", tc.want, errOut.String())
			}
		})
	}
}

// FINDING FROM REVIEW. Make has more assignment operators than the `:=` scar
// recorded. Rewriting `KUBECTL_VERSION := 1.34.10` as `?=` — an ordinary,
// meaning-preserving edit — silently removed the Makefile from the scanned set,
// and the bare-ARG class is deliberately exempt from vacuities(), so nothing
// would have reported the loss: a later Dockerfile bump says OK while
// `make k8s-validate` validates against the stale version.
func TestVersionPinsSeesEveryMakeAssignmentOperator(t *testing.T) {
	for _, op := range []string{"=", ":=", "::=", "?=", "+="} {
		t.Run(op, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, "Makefile"), "KUBECTL_VERSION "+op+" 9.9.9\n")

			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
				t.Fatalf("a drifted pin written %q must fail the gate", op)
			}
			if !strings.Contains(errOut.String(), "Makefile") {
				t.Errorf("the report must name the Makefile:\n%s", errOut.String())
			}
		})
	}
}

// FINDING FROM REVIEW. Requiring `latest` of EVERY colon in the value
// over-reached: a conditional image naming a third-party fallback failed with
// `ci-tofu container image tag is bookworm-20240101`, blaming an image this
// guard's own docs say it does not gate, and the only edit satisfying the
// message was retagging Debian.
func TestVersionPinsDoesNotJudgeAThirdPartyImageInTheSameValue(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintYAML(
		"${{ vars.TF_IMAGE && 'ghcr.io/akamai-consulting/ci-tofu:latest' || 'debian:bookworm-20240101' }}"))

	var out strings.Builder
	if err := Run(root, false, &out, io.Discard); err != nil {
		t.Fatalf("a third-party image's tag is not this guard's business: %v", err)
	}
	if !strings.Contains(out.String(), "2 container image(s) float") {
		t.Errorf("ours must still be judged and floating:\n%s", out.String())
	}
}

// ...and the segment test must not become a way to smuggle ours past it: an
// unresolvable segment is judged precisely because it MIGHT be ours.
func TestVersionPinsStillJudgesAnUnresolvableSegment(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ format('ghcr.io/{0}/{1}:sha-abcdef0', github.repository_owner, 'ci-kubernetes') }}\n"+
		"  tf:\n"+
		"    container:\n"+
		"      image: ${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("a tag on a placeholder segment must still be judged — it might be ours")
	}
	if !strings.Contains(errOut.String(), "sha-abcdef0") {
		t.Errorf("the report must name the tag it found:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. Flow style has no continuation lines: in
// `job: {container: {image: x}, steps: [...]}` the value ends inside the braces,
// and the indent heuristic swallowed the NEXT line into the container range
// anyway.
//
// A FLOW-STYLE EARLY RETURN WAS ADDED FOR THIS AND HAS BEEN REMOVED. It could
// not change a verdict: judgedAsContainer requires the matched reference to
// appear in the container's VALUE as well as on one of its lines, and an
// over-long line range with no matching text suppresses nothing. Both tests stay
// — the behaviour is what matters — but the mechanism they exercise is the
// containment check, not a style guard.
func TestVersionPinsDoesNotSwallowTheLineAfterAFlowStyleJob(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
		// QUOTED, so the `${{ … }}` braces are a scalar and not flow indicators —
		// unquoted this is not valid flow YAML, the file fails to parse, and the
		// test then passes for the wrong reason (no container images at all, so
		// nothing is skipped) whether or not the flow guard is there.
		"  tf: {container: {image: \"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}\"},\n"+
		"       steps: [{run: 'docker pull ghcr.io/akamai-consulting/ci-kubernetes:9.9.9'}]}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("the drifted reference on the following line must still be judged")
	}
	if !strings.Contains(errOut.String(), "9.9.9") {
		t.Errorf("the report must name the drift it would otherwise have skipped:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. A flow-style job written on ONE line puts the container
// image and an unrelated reference on the same line, and the de-dup blanked the
// whole line for the pin rule — so a genuinely drifted `ci-kubernetes:9.9.9` in
// the job's own `run:` was judged by NO rule and the gate exited 0. The earlier
// flow test put `steps:` on the next line and missed this.
func TestVersionPinsJudgesADriftOnTheSameLineAsAFlowStyleContainer(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
		"  tf: {container: {image: \"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}\"}, steps: [{run: 'docker pull ghcr.io/akamai-consulting/ci-kubernetes:9.9.9'}]}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("a drifted reference sharing a line with a container image must still be judged")
	}
	if !strings.Contains(errOut.String(), "9.9.9") {
		t.Errorf("the report must name the drift no rule was judging:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. Keyed on existence alone, one value naming BOTH of our
// images registered a floating site for each and satisfied both declared
// entries — so deleting the entire Kubernetes lint job passed with "2 container
// image(s) float". "There are two of them" has to mean two jobs.
func TestVersionPinsRefusesWhenOneValueCoversBothDeclaredImages(t *testing.T) {
	root := pinsFixture(t)
	// The k8s job is gone; the surviving value mentions both images.
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  tf:\n"+
		"    container:\n"+
		"      image: ${{ vars.TF_IMAGE && 'ghcr.io/akamai-consulting/ci-tofu:latest' "+
		"|| 'ghcr.io/akamai-consulting/ci-kubernetes:latest' }}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("one value cannot stand in for both declared container images")
	}
	if !strings.Contains(errOut.String(), "unambiguously runs") {
		t.Errorf("the report must say no job unambiguously runs the declared image:\n%s", errOut.String())
	}
	// And it must say WHY, not just that it is absent: the value is there, it
	// simply names two images and answers for neither.
	if strings.Contains(errOut.String(), "container image found in") {
		t.Errorf("the value is present but ambiguous — 'not found' misdescribes it:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. Each image gets its own site from a value that names
// several, so each must judge only ITS tag — accepting any of our names reported
// the ci-kubernetes tag under the ci-tofu verdict.
func TestVersionPinsAttributesATagToTheImageItBelongsTo(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
		"  tf:\n"+
		"    container:\n"+
		"      image: ${{ vars.TF_IMAGE && 'ghcr.io/akamai-consulting/ci-tofu:latest' "+
		"|| 'ghcr.io/akamai-consulting/ci-kubernetes:9.9.9' }}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("the 9.9.9 tag must fail — it is one of ours and does not float")
	}
	if strings.Contains(errOut.String(), "ci-tofu container image tag = 9.9.9") {
		t.Errorf("9.9.9 belongs to ci-kubernetes; blaming ci-tofu sends the reader to the wrong line:\n%s",
			errOut.String())
	}
	if !strings.Contains(errOut.String(), "ci-kubernetes container image tag = 9.9.9") {
		t.Errorf("the verdict must name the image whose tag it is:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW, and the case that showed distinctness was the wrong
// property. Two ci-tofu jobs — one whose value merely MENTIONS ci-kubernetes —
// with the ci-kubernetes job deleted outright gave three sites over two entries.
// That satisfied a distinct-line COUNT, and it also satisfies a MATCHING
// (ci-tofu → job one, ci-kubernetes → job two), while no job runs ci-kubernetes
// at all. The question was never whether the entries can be told apart; it is
// whether a job unambiguously runs THIS image.
func TestVersionPinsRefusesWhenAnExtraJobStandsInForAMissingOne(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  tf:\n"+
		"    container:\n"+
		"      image: ${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}\n"+
		"  tf2:\n"+
		"    container:\n"+
		"      image: ${{ vars.TF_IMAGE && 'ghcr.io/akamai-consulting/ci-tofu:latest' "+
		"|| 'ghcr.io/akamai-consulting/ci-kubernetes:latest' }}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("no job runs ci-kubernetes — an extra ci-tofu job must not cover for it")
	}
	if !strings.Contains(errOut.String(), "unambiguously runs ci-kubernetes") {
		t.Errorf("the report must say no job unambiguously runs ci-kubernetes:\n%s", errOut.String())
	}
}

// ...and the other direction: a MISSING entry is reported as missing, once. Run
// unconditionally, the distinctness check fired here too and added a second
// error whose stated cause and remediation were both false for this case.
func TestVersionPinsReportsAMissingImageWithoutTheDistinctnessComplaint(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("the declared ci-tofu container image is gone and must be reported")
	}
	if !strings.Contains(errOut.String(), "no ci-tofu container image found") {
		t.Errorf("the report must name the image that is missing:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "unambiguously runs") {
		t.Errorf("nothing is ambiguous — one image is simply gone, and that cause is false here:\n%s",
			errOut.String())
	}
	if !strings.Contains(errOut.String(), "1 rule(s) matched nothing") {
		t.Errorf("one cause, one complaint:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. The templated-tag alternative could not span a NESTED
// `}`, so `ci-tofu:${{ format('{0}', env.T) }}` truncated to `${{` — the same
// fragment the alternative was added to eliminate. The verdict was already
// right (a templated tag never floats); what was wrong is that the value shown
// was not the value written, which is how a reader ends up editing the wrong
// thing.
func TestVersionPinsReportsATemplatedTagContainingBraces(t *testing.T) {
	for _, tc := range []struct{ name, tag, want string }{
		{"plain", "${{ env.TAG }}", "${{ env.TAG }}"},
		{"nested braces", "${{ format('{0}', env.T) }}", "${{ format('{0}', env.T) }}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintYAML(
				"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:"+tc.tag+"', github.repository_owner) }}"))

			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
				t.Fatal("a templated tag must fail — nothing here can tell what it resolves to")
			}
			if !strings.Contains(errOut.String(), tc.want) {
				t.Errorf("the report must echo the whole expression, want %q in:\n%s", tc.want, errOut.String())
			}
		})
	}
}

// Two templated tags in one value must stay two, not merge into one match
// running to the last `}}`.
func TestVersionPinsDoesNotMergeTwoTemplatedTags(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintYAML(
		"${{ vars.X && 'ghcr.io/akamai-consulting/ci-tofu:${{ env.A }}' "+
			"|| 'ghcr.io/akamai-consulting/ci-tofu:${{ env.B }}' }}"))

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("a templated tag must fail")
	}
	// Asserted as the WHOLE reported value, not as a substring: merged, the
	// capture runs to the last `}}` and still contains env.A, so a substring
	// check passes on the bug.
	if !strings.Contains(errOut.String(), "= ${{ env.A }} (want") {
		t.Errorf("the first tag must be reported on its own, not merged with the second:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. The image name had a right word boundary but no LEFT
// one, so a longer name CONTAINING ours matched: pointing the Terraform job at
// `ghcr.io/<org>/mirror-ci-tofu:1.9.9` made the gate print
// `ok … ci-tofu container image tag = latest` and exit 0. reImageRef matched
// inside the longer name so the image counted as named, the segment test then
// correctly refused to judge `mirror-ci-tofu`'s tag, and it fell through to
// "nothing disagreed, so it floats" — while the container de-dup suppressed the
// pin rule on the same line. A different, version-pinned image satisfied the
// declared {lint.yml, ci-tofu} entry.
func TestVersionPinsDoesNotMatchOurNameInsideALongerOne(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/akamai-consulting/mirror-ci-tofu:1.9.9",
		"ghcr.io/akamai-consulting/ci-tofu-experimental:1.9.9",
	} {
		t.Run(image, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintYAML(image))

			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
				t.Fatal("no job runs ci-tofu — a different image whose name contains it must not stand in")
			}
			if !strings.Contains(errOut.String(), "no ci-tofu container image found") {
				t.Errorf("the declared image is absent and must be reported so:\n%s", errOut.String())
			}
			// And it must not be judged as ours, either way round.
			if strings.Contains(errOut.String(), "ci-tofu container image tag") {
				t.Errorf("somebody else's image must not get our verdict:\n%s", errOut.String())
			}
		})
	}
}

// FINDING FROM REVIEW. reImageTag never got the left anchor its three siblings
// were given, so it still matched inside a longer name: a `run:` step pulling
// `ghcr.io/<org>/mirror-ci-tofu:1.9.9` was reported as OUR ci-tofu pin
// disagreeing with the Dockerfile — a red on somebody else's image.
func TestVersionPinsDoesNotClaimALongerNameAsAPin(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintYAML(
		"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}",
		"      - run: docker pull ghcr.io/akamai-consulting/mirror-ci-tofu:1.9.9\n"))

	var out strings.Builder
	if err := Run(root, false, &out, io.Discard); err != nil {
		t.Fatalf("a different image whose name contains ours is not ours: %v", err)
	}
	if strings.Contains(out.String(), "1.9.9") {
		t.Errorf("mirror-ci-tofu's tag must not be judged against our ARG:\n%s", out.String())
	}
}

// FINDING FROM REVIEW. Ambiguity counted published NAMES, so a value naming ONE
// image under both `ci-tofu` and its `ci-terraform` alias counted as two and was
// called ambiguous — both its sites judged `ok` while the gate failed with a
// "give it its own job" remediation that cannot be satisfied, there being only
// one image there.
func TestVersionPinsDoesNotCallAnImageAndItsAliasTwoImages(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
		"  tf:\n"+
		"    container:\n"+
		"      image: ${{ vars.TF_IMAGE && 'ghcr.io/akamai-consulting/ci-tofu:latest' "+
		"|| 'ghcr.io/akamai-consulting/ci-terraform:latest' }}\n")

	var out strings.Builder
	if err := Run(root, false, &out, io.Discard); err != nil {
		t.Fatalf("one image under two of its own names is still one image: %v", err)
	}
}

// FINDING FROM REVIEW. The container/pin de-dup compared the pin rule's MATCH
// TEXT, which now begins with the byte imageLeftBoundary consumed, against the
// YAML scalar — and for a container image with no registry prefix that byte is
// the space after `image:` or the opening quote, neither of which is in the
// value. Containment failed and the same reference got two contradictory
// verdicts: FLOAT plus DRIFT, the DRIFT remediation being "bump these to match"
// the ARG, which is the re-pin edit that re-arms the trap.
func TestVersionPinsGivesOneVerdictToAnUnprefixedContainerImage(t *testing.T) {
	for _, image := range []string{"ci-tofu:9.9.9", `"ci-tofu:9.9.9"`} {
		t.Run(image, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintYAML(image))

			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
				t.Fatal("expected the float failure this test is about")
			}
			if strings.Contains(errOut.String(), "ci-tofu image tag") {
				t.Errorf("the pin rule must not also claim it — that verdict demands the version:\n%s",
					errOut.String())
			}
			if strings.Count(errOut.String(), "container image tag") != 2 {
				t.Errorf("expected exactly one float verdict (annotation + summary line):\n%s", errOut.String())
			}
		})
	}
}

// FINDING FROM REVIEW. reGoConst was the one image-related regex still without a
// left boundary, so `LegacyCITofuTag = "1.12.5"` matched reGoConst("CITofuTag"):
// the goConst vacuity check passed, and --verbose printed `CITofuTag = 1.12.5`,
// with no CITofuTag anywhere in the tree. That is the silent vacuity vacuities()
// exists to close, arriving through the regex that had not been anchored.
func TestVersionPinsDoesNotAcceptALongerConstantName(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, filepath.FromSlash(citagsFile)),
		"package versionpins\n\nconst (\n\tLegacyCITofuTag = \"1.12.5\"\n\tCIKubernetesTag = \"1.31.0\"\n)\n")

	var errOut strings.Builder
	err := Run(root, false, io.Discard, &errOut)
	if err == nil {
		t.Fatal("CITofuTag is gone — a longer name containing it must not stand in")
	}
	if !strings.Contains(errOut.String(), "the constant CITofuTag was not found") {
		t.Errorf("the report must name the constant that is actually missing:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. containerImages discarded the parse error, so an
// unparseable workflow contributed no container sites and its `container.image`
// fell through to the PIN rule — where a version-tagged one is ACCEPTED, and
// then REQUIRED on the next bump. The comment called vacuities() the backstop and
// it is not: that covers only the files named in expectedFallbacks.
func TestVersionPinsRefusesAnUnparseableWorkflowNamingOurImage(t *testing.T) {
	root := pinsFixture(t)
	// A tab where YAML forbids one: valid-looking, and yaml.v3 rejects it.
	writeFile(t, filepath.Join(root, ".github/workflows/other.yml"), ""+
		"jobs:\n"+
		"  a:\n"+
		"\tcontainer:\n"+
		"      image: ghcr.io/akamai-consulting/ci-tofu:1.12.5\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("a file that does not parse cannot be read as floating — and must not be read as a pin")
	}
	if !strings.Contains(errOut.String(), "does not parse as YAML") {
		t.Errorf("the report must name the parse failure as the reason:\n%s", errOut.String())
	}
	// The trap in its precise form: the pin rule must not have accepted it.
	if strings.Contains(errOut.String(), "ci-tofu image tag") {
		t.Errorf("the pin rule must not claim a container image the float rule could not read:\n%s",
			errOut.String())
	}
}

// ...but a file that is only comments must not red. It yields an empty document,
// which containerImages also reports as unparsed — harmlessly, because the name
// check reads the MASKED body and a comment names nothing.
func TestVersionPinsAcceptsACommentOnlyWorkflow(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/notes.yml"),
		"# we used to run ci-tofu:1.9.8 here before the OpenTofu migration\n")

	if err := Run(root, false, io.Discard, io.Discard); err != nil {
		t.Fatalf("an empty document is a true answer, not a parse failure: %v", err)
	}
}

// A Docker container action names its image at runs.image — the same position
// and the same trap.
func TestVersionPinsJudgesADockerContainerAction(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/actions/thing/action.yml"), ""+
		"name: thing\n"+
		"runs:\n"+
		"  using: docker\n"+
		"  image: docker://ghcr.io/akamai-consulting/ci-tofu:1.12.5\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("an action's container image is the same ordering trap as a job's")
	}
	if !strings.Contains(errOut.String(), "ci-tofu container image tag") {
		t.Errorf("it must be judged by the positional rule, not as an ordinary pin:\n%s", errOut.String())
	}
}

// The image-tag class is anchored too, so its match starts one byte early — and
// for a reference at COLUMN 0 that byte is the preceding newline, putting the
// GitHub annotation on the line above. Same off-by-one as the bare-ARG class,
// through the anchor added later.
func TestVersionPinsReportsAnImageTagOnItsOwnLine(t *testing.T) {
	root := pinsFixture(t)
	// THE IMAGE NAME ITSELF AT COLUMN 0, not a registry prefix before it: with a
	// prefix the boundary consumes the `/`, which is on the right line already,
	// and the test cannot see the defect.
	writeFile(t, filepath.Join(root, "template-scripts/pull.sh"), ""+
		"#!/usr/bin/env bash\n"+ // line 1
		"set -euo pipefail\n"+ // line 2
		"ci-tofu:9.9.9\n"+ // line 3, column 0
		"if true; then\n"+
		"\techo done\n"+ // a TAB: this file is not YAML and must not be asked to be
		"fi\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("expected the drift this test is about")
	}
	if !strings.Contains(errOut.String(), "pull.sh,line=3::") {
		t.Errorf("the annotation must name line 3, where the reference is:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "pull.sh,line=2::") {
		t.Errorf("the off-by-one is back:\n%s", errOut.String())
	}

}

// The parse-failure check is scoped to .yml/.yaml, and that scoping is
// load-bearing: a Makefile naming one of our images does not parse as YAML, and
// reporting it as a broken workflow is both wrong and noisy. Found on the real
// tree, where the unscoped version reported the Makefile and two of this
// package's own source files.
func TestVersionPinsDoesNotAskANonYAMLFileToParseAsYAML(t *testing.T) {
	root := pinsFixture(t)
	// A Makefile: a target line reads as a YAML mapping key and the TAB-indented
	// recipe under it is a YAML indentation error, so this file genuinely fails
	// to parse — which is what makes the scoping observable at all. (Most
	// non-YAML text parses as YAML happily; that is why the unscoped version was
	// found on the real tree rather than by a fixture.)
	writeFile(t, filepath.Join(root, "Makefile"), ""+
		"KUBECTL_VERSION  := 1.31.0\n"+
		"pull:\n"+
		"\tdocker pull ghcr.io/akamai-consulting/ci-tofu:1.12.5\n")

	var out, errOut strings.Builder
	if err := Run(root, true, &out, &errOut); err != nil {
		t.Fatalf("a Go file naming our image at the right version must pass: %v\n%s", err, errOut.String())
	}
	if strings.Contains(errOut.String(), "does not parse as YAML") {
		t.Errorf("a Makefile was never YAML and must not be reported as broken YAML:\n%s", errOut.String())
	}
	// ...and it is still judged as an ordinary pin.
	if !strings.Contains(out.String(), "ci-tofu image reference") {
		t.Errorf("the reference must still be checked against the ARG:\n%s", out.String())
	}
}

// FINDING FROM REVIEW. A step's `uses: docker://<image>:<tag>` runs a container
// too, and it sat outside the positional rule — so the PIN rule claimed it and
// REQUIRED the version, pointing that step at an unpublished tag on the next
// bump. Written `:latest` it matched no rule and the gate printed OK. Only the
// docker:// form: `uses: owner/repo@sha` is an action, not an image.
func TestVersionPinsJudgesAStepThatUsesADockerImage(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintYAML(
		"${{ vars.TF_IMAGE || format('ghcr.io/{0}/ci-tofu:latest', github.repository_owner) }}",
		"      - uses: docker://ghcr.io/akamai-consulting/ci-kubernetes:1.31.0\n"))

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("a version-tagged docker:// step is the same ordering trap as a job container")
	}
	if !strings.Contains(errOut.String(), "ci-kubernetes container image tag") {
		t.Errorf("it must be judged by position, not fall through to the pin rule:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "ci-kubernetes image tag") {
		t.Errorf("the pin rule must not claim it — that verdict demands the version:\n%s", errOut.String())
	}
}

// ...and an ordinary action reference is NOT an image. Asserted where it is
// observable: with the ci-tofu container job gone, an action `uses:` naming
// ci-tofu must not vouch for the declared entry. Asserted on a tree that merely
// passes, it does not discriminate — a tagless reference judges as `latest` and
// is silently `ok` either way.
func TestVersionPinsDoesNotLetAnActionUsesVouchForAContainer(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
		"    steps:\n"+
		"      - uses: akamai-consulting/ci-tofu@v1\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("no job runs ci-tofu — an action reference must not stand in for one")
	}
	if !strings.Contains(errOut.String(), "no ci-tofu container image found") {
		t.Errorf("the declared container image is absent and must be reported so:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. A container image resolved through a matrix names no
// image, so it produced no floating site — while the version-tagged literal in
// `strategy.matrix` fell through to the PIN rule, which REQUIRED the version and
// re-armed the trap on the next bump. `matrix` is a context GitHub allows at that
// position, so "how the value is spelled cannot decide whether it is judged" was
// not true of indirection.
func TestVersionPinsFollowsAMatrixIndirectedContainerImage(t *testing.T) {
	job := func(tag string) string {
		return "" +
			"  tf:\n" +
			"    strategy:\n" +
			"      matrix:\n" +
			"        img:\n" +
			"          - ghcr.io/akamai-consulting/ci-tofu:" + tag + "\n" +
			"    container:\n" +
			"      image: ${{ matrix.img }}\n"
	}
	head := "jobs:\n" +
		"  k8s:\n" +
		"    container:\n" +
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"

	t.Run("version-tagged is refused", func(t *testing.T) {
		root := pinsFixture(t)
		writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+head+job("1.12.5"))

		var errOut strings.Builder
		if err := Run(root, false, io.Discard, &errOut); err == nil {
			t.Fatal("a version-tagged matrix image is the same ordering trap as a literal one")
		}
		if !strings.Contains(errOut.String(), "ci-tofu container image tag") {
			t.Errorf("it must be judged by position, not fall through to the pin rule:\n%s", errOut.String())
		}
		if strings.Contains(errOut.String(), "ci-tofu image tag") {
			t.Errorf("the pin rule must not claim it — that verdict demands the version:\n%s", errOut.String())
		}
	})

	t.Run("floating passes and vouches for the entry", func(t *testing.T) {
		root := pinsFixture(t)
		writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+head+job("latest"))

		var out strings.Builder
		if err := Run(root, false, &out, io.Discard); err != nil {
			t.Fatalf("a floating matrix image must pass: %v", err)
		}
		if !strings.Contains(out.String(), "2 container image(s) float") {
			t.Errorf("the resolved image must count as the declared ci-tofu container:\n%s", out.String())
		}
	})
}

// An indirection this does NOT follow stays an unjudged value rather than
// silently becoming a pin: the point of the matrix case is that the literal was
// reachable, not that every expression is.
func TestVersionPinsLeavesAnUnresolvableContainerImageAlone(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
		"  tf:\n"+
		"    container:\n"+
		"      image: ${{ needs.build.outputs.image }}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("no job resolvably runs ci-tofu — the declared entry is unsatisfied")
	}
	if !strings.Contains(errOut.String(), "no ci-tofu container image found") {
		t.Errorf("an unresolvable image names nothing and must not vouch for the entry:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. Matrix indirection was followed only for the plain axis
// on the `container.image` mapping form. A `matrix.include` row, and the
// `container: ${{ matrix.x }}` shorthand, both yielded no container site — so
// the version-tagged literal fell to the PIN rule, which REQUIRED the version
// and re-armed the trap.
func TestVersionPinsFollowsEveryMatrixShape(t *testing.T) {
	head := "jobs:\n" +
		"  k8s:\n" +
		"    container:\n" +
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"
	for _, tc := range []struct{ name, job string }{
		{
			name: "include row",
			job: "  tf:\n" +
				"    strategy:\n" +
				"      matrix:\n" +
				"        include:\n" +
				"          - img: ghcr.io/akamai-consulting/ci-tofu:1.12.5\n" +
				"    container:\n" +
				"      image: ${{ matrix.img }}\n",
		},
		{
			name: "container shorthand",
			job: "  tf:\n" +
				"    strategy:\n" +
				"      matrix:\n" +
				"        img:\n" +
				"          - ghcr.io/akamai-consulting/ci-tofu:1.12.5\n" +
				"    container: ${{ matrix.img }}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+head+tc.job)

			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
				t.Fatal("a version-tagged matrix image is the ordering trap whatever shape carries it")
			}
			if !strings.Contains(errOut.String(), "ci-tofu container image tag") {
				t.Errorf("it must be judged by position, not fall through to the pin rule:\n%s", errOut.String())
			}
			if strings.Contains(errOut.String(), "ci-tofu image tag") {
				t.Errorf("the pin rule must not claim it — that verdict demands the version:\n%s", errOut.String())
			}
		})
	}
}

// FINDING FROM REVIEW. runs.image was scanned but runs.steps[] was not, so a
// COMPOSITE action's `uses: docker://…` was judged as an ordinary pin and
// required to restate the ARG. Job steps and action steps carry the same shape
// and fail the same way, so they are scanned by the same code.
func TestVersionPinsJudgesACompositeActionsDockerStep(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/actions/thing/action.yml"), ""+
		"name: thing\n"+
		"runs:\n"+
		"  using: composite\n"+
		"  steps:\n"+
		"    - uses: docker://ghcr.io/akamai-consulting/ci-tofu:1.12.5\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("a composite action's docker:// step runs a container like any other")
	}
	if !strings.Contains(errOut.String(), "ci-tofu container image tag") {
		t.Errorf("it must be judged by position, not as an ordinary pin:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "ci-tofu image tag") {
		t.Errorf("the pin rule must not claim it — that verdict demands the version:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. The matrix resolver was wired to the job container's two
// forms but not to a SERVICE container, so `${{ matrix.img }}` there named no
// image and the version-tagged literal fell to the PIN rule, which REQUIRED the
// version. Wiring the resolver one position at a time is how the shorthand came
// to be missing it once already; all three go through it now.
func TestVersionPinsFollowsAMatrixIndirectedServiceImage(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+lintFallbackLine+""+
		"  svc:\n"+
		"    strategy:\n"+
		"      matrix:\n"+
		"        img:\n"+
		"          - ghcr.io/akamai-consulting/ci-kubernetes:9.9.9\n"+
		"    services:\n"+
		"      registry:\n"+
		"        image: ${{ matrix.img }}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("a version-tagged service image is the same ordering trap as a job container")
	}
	if !strings.Contains(errOut.String(), "ci-kubernetes container image tag") {
		t.Errorf("it must be judged by position, not fall through to the pin rule:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "ci-kubernetes image tag") {
		t.Errorf("the pin rule must not claim it — that verdict demands the version:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. Matrix entries were built directly instead of through
// spanOf, so an entry whose value WRAPS de-dupped on its first line only and the
// pin rule re-judged the continuation — one reference with two contradictory
// verdicts, the DRIFT half telling the reader to re-pin to the ARG. That is the
// bug endLine exists to prevent, arriving through the one path that skipped it.
func TestVersionPinsGivesOneVerdictToAWrappedMatrixEntry(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
		"  tf:\n"+
		"    strategy:\n"+
		"      matrix:\n"+
		"        img:\n"+
		"          - ghcr.io/akamai-consulting/\n"+
		"            ci-tofu:9.9.9\n"+
		"    container:\n"+
		"      image: ${{ matrix.img }}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("expected the float failure this test is about")
	}
	if strings.Contains(errOut.String(), "ci-tofu image tag") {
		t.Errorf("the continuation must be inside the entry's range, not a second pin verdict:\n%s",
			errOut.String())
	}
}

// FINDING FROM REVIEW. Only the first YAML document was inspected, so a later
// document's container images contributed nothing and a version-tagged one there
// fell to the PIN rule — silently, since the file parses and the parse-failure
// check never fires.
func TestVersionPinsReadsEveryYAMLDocument(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/other.yml"), ""+
		"name: first\n"+
		"---\n"+
		"jobs:\n"+
		"  a:\n"+
		"    container:\n"+
		"      image: ghcr.io/akamai-consulting/ci-kubernetes:9.9.9\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("a container image in a later document is the same ordering trap")
	}
	if !strings.Contains(errOut.String(), "ci-kubernetes container image tag") {
		t.Errorf("it must be judged by position, not fall through to the pin rule:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "ci-kubernetes image tag") {
		t.Errorf("the pin rule must not claim it — that verdict demands the version:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW, as a note rather than a defect, and it deserved to be a
// defect: an `env`-indirected image did not merely go unjudged, it left the
// LITERAL to the pin rule, which REQUIRED the version. That is the inverted
// verdict — the trap itself — so env is followed like matrix, job scope first.
func TestVersionPinsFollowsAnEnvIndirectedContainerImage(t *testing.T) {
	for _, tc := range []struct{ name, doc string }{
		{
			name: "workflow scope",
			doc: "env:\n" +
				"  TF_IMG: ghcr.io/akamai-consulting/ci-tofu:9.9.9\n" +
				"jobs:\n  tf:\n    container:\n      image: ${{ env.TF_IMG }}\n",
		},
		{
			name: "job scope shadows the workflow",
			doc: "env:\n" +
				"  TF_IMG: ghcr.io/akamai-consulting/ci-tofu:latest\n" +
				"jobs:\n  tf:\n" +
				"    env:\n      TF_IMG: ghcr.io/akamai-consulting/ci-tofu:9.9.9\n" +
				"    container:\n      image: ${{ env.TF_IMG }}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), tc.doc+""+
				"  k8s:\n"+
				"    container:\n"+
				"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n")

			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
				t.Fatal("an env-indirected version tag is the ordering trap, not an unjudged value")
			}
			if !strings.Contains(errOut.String(), "ci-tofu container image tag") {
				t.Errorf("it must be judged by position:\n%s", errOut.String())
			}
			if strings.Contains(errOut.String(), "ci-tofu image tag") {
				t.Errorf("the pin rule must not claim it — that verdict demands the version:\n%s", errOut.String())
			}
		})
	}
}

// FINDING FROM REVIEW. Indirection was followed only when the reference was the
// ENTIRE image value, so writing the same expression a slightly different way
// escaped it — and the version-tagged literal then fell to the PIN rule, which
// REQUIRES the version. Same inverted verdict, reached by a spelling, which is
// the failure mode this rule was rewritten to stop being susceptible to.
func TestVersionPinsFollowsAnEmbeddedIndirection(t *testing.T) {
	for _, tc := range []struct{ name, doc string }{
		{
			name: "env behind a vars fallback",
			doc: "env:\n" +
				"  TF_FALLBACK: ghcr.io/akamai-consulting/ci-tofu:9.9.9\n" +
				"jobs:\n  tf:\n    container:\n      image: ${{ vars.TF_IMAGE || env.TF_FALLBACK }}\n",
		},
		{
			name: "matrix inside a longer reference",
			doc: "jobs:\n  tf:\n" +
				"    strategy:\n      matrix:\n        img:\n" +
				"          - ci-tofu:9.9.9\n" +
				"    container:\n      image: ghcr.io/akamai-consulting/${{ matrix.img }}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), tc.doc+""+
				"  k8s:\n"+
				"    container:\n"+
				"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n")

			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
				t.Fatal("an embedded indirection hides the same version tag as a whole-value one")
			}
			if !strings.Contains(errOut.String(), "ci-tofu container image tag") {
				t.Errorf("it must be judged by position:\n%s", errOut.String())
			}
			if strings.Contains(errOut.String(), "ci-tofu image tag") {
				t.Errorf("the pin rule must not claim it — that verdict demands the version:\n%s", errOut.String())
			}
		})
	}
}

// ...and the value itself is still judged on its own terms: a templated TAG on a
// directly-named image is a verdict the references cannot supply.
func TestVersionPinsStillJudgesTheValueThatCarriesAnIndirection(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), lintEnv+""+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
		"  tf:\n"+
		"    strategy:\n      matrix:\n        tag:\n          - latest\n"+
		"    container:\n"+
		"      image: ghcr.io/akamai-consulting/ci-tofu:${{ matrix.tag }}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("a templated tag on a named image must fail — nothing here resolves it per run")
	}
	if !strings.Contains(errOut.String(), "matrix.tag") {
		t.Errorf("the report must echo the expression it could not resolve:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. `inputs.<key>` was not followed, though GitHub allows it
// at container.image in more places than it allows `env`, which WAS followed. A
// version-tagged reusable-workflow input default therefore left the literal to
// the PIN rule, which REQUIRES the version — the inverted verdict again.
func TestVersionPinsFollowsAnInputsIndirectedContainerImage(t *testing.T) {
	for _, trigger := range []string{"workflow_call", "workflow_dispatch"} {
		t.Run(trigger, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), ""+
				"on:\n"+
				"  "+trigger+":\n"+
				"    inputs:\n"+
				"      img:\n"+
				"        default: ghcr.io/akamai-consulting/ci-tofu:9.9.9\n"+
				"jobs:\n"+
				"  k8s:\n"+
				"    container:\n"+
				"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
				"  tf:\n"+
				"    container:\n"+
				"      image: ${{ inputs.img }}\n")

			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
				t.Fatal("a version-tagged input default is the same ordering trap as any other indirection")
			}
			if !strings.Contains(errOut.String(), "ci-tofu container image tag") {
				t.Errorf("it must be judged by position:\n%s", errOut.String())
			}
			if strings.Contains(errOut.String(), "ci-tofu image tag") {
				t.Errorf("the pin rule must not claim it — that verdict demands the version:\n%s", errOut.String())
			}
		})
	}
}

// FINDING FROM REVIEW. One indirection target referenced from TWO positions
// resolved to the same literal twice, annotating one file:line twice and
// inflating the "N container image(s) float" count. The verdict was never wrong;
// the report was, and a count that does not match what a reader can see is how a
// gate stops being believed.
func TestVersionPinsCountsOneSharedIndirectionTargetOnce(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), ""+
		"env:\n"+
		"  TF_IMG: ghcr.io/akamai-consulting/ci-tofu:latest\n"+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
		"  tf:\n"+
		"    container:\n"+
		"      image: ${{ env.TF_IMG }}\n"+
		"  tf2:\n"+
		"    container:\n"+
		"      image: ${{ env.TF_IMG }}\n")

	var out strings.Builder
	if err := Run(root, false, &out, io.Discard); err != nil {
		t.Fatalf("two jobs sharing one floating image is fine: %v", err)
	}
	// Two distinct images exist: the ci-kubernetes fallback and the shared
	// ci-tofu literal. Counted per referencing position it reads 3.
	if !strings.Contains(out.String(), "2 container image(s) float") {
		t.Errorf("the shared target must be counted once:\n%s", out.String())
	}
}

// FINDING FROM REVIEW. A decode error discarded the container images already
// found in documents that DID parse, handing them back to the PIN rule — which
// answered "bump these to match" the ARG, the re-pin edit this gate exists to
// prevent, printed as the remediation. It failed closed (the parse vacuity fires
// too), so this was wrong advice rather than a silent pass.
func TestVersionPinsKeepsImagesFromDocumentsThatParsed(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/other.yml"), ""+
		"jobs:\n"+
		"  a:\n"+
		"    container:\n"+
		"      image: ghcr.io/akamai-consulting/ci-kubernetes:9.9.9\n"+
		"---\n"+
		"a:\n  b: 1\n c: broken\n") // second document does not parse

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("expected both the float verdict and the parse vacuity")
	}
	if !strings.Contains(errOut.String(), "ci-kubernetes container image tag") {
		t.Errorf("the readable document's image must still be judged by position:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "ci-kubernetes image tag") {
		t.Errorf("the pin rule must not claim it — that remediation is the re-pin edit:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "does not parse as YAML") {
		t.Errorf("the parse failure must still be reported:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. indirectValues returned on the FIRST trigger that
// supplied an input default, so a workflow declaring the same input under both
// `workflow_call` and `workflow_dispatch` had one default judged and the other
// handed to the PIN rule — which answers "bump these to match" the ARG, the
// inverted verdict, on a container image.
func TestVersionPinsJudgesTheInputDefaultOfEveryTrigger(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), ""+
		"on:\n"+
		"  workflow_call:\n"+
		"    inputs:\n"+
		"      img:\n"+
		"        default: ghcr.io/akamai-consulting/ci-tofu:latest\n"+
		"  workflow_dispatch:\n"+
		"    inputs:\n"+
		"      img:\n"+
		"        default: ghcr.io/akamai-consulting/ci-tofu:9.9.9\n"+
		"jobs:\n"+
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
		"  tf:\n"+
		"    container:\n"+
		"      image: ${{ inputs.img }}\n")

	var errOut strings.Builder
	if err := Run(root, false, io.Discard, &errOut); err == nil {
		t.Fatal("the second trigger's default carries a version tag and must be judged")
	}
	if !strings.Contains(errOut.String(), "ci-tofu container image tag") {
		t.Errorf("it must be judged by position:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "ci-tofu image tag") {
		t.Errorf("the pin rule must not claim it — that remediation is the re-pin edit:\n%s", errOut.String())
	}
}

// FINDING FROM REVIEW. A container image written as a YAML alias has Kind
// AliasNode, so a bare ScalarNode test dropped it and the anchored literal fell
// to the PIN rule. GitHub's parser does not expand anchors, so such a workflow
// would not run — which makes this a wrong verdict on a broken file rather than
// a live trap, and it is still the verdict that teaches the wrong lesson.
func TestVersionPinsFollowsAnAliasedContainerImage(t *testing.T) {
	for _, tc := range []struct{ name, job string }{
		{"mapping form", "  tf:\n    container:\n      image: *img\n"},
		{"shorthand", "  tf:\n    container: *img\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := pinsFixture(t)
			writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), ""+
				"anchors:\n"+
				"  img: &img ghcr.io/akamai-consulting/ci-tofu:9.9.9\n"+
				"jobs:\n"+
				"  k8s:\n"+
				"    container:\n"+
				"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
				tc.job)

			var errOut strings.Builder
			if err := Run(root, false, io.Discard, &errOut); err == nil {
				t.Fatal("an aliased container image carries the same version tag as a written one")
			}
			if !strings.Contains(errOut.String(), "ci-tofu container image tag") {
				t.Errorf("it must be judged by position:\n%s", errOut.String())
			}
			if strings.Contains(errOut.String(), "ci-tofu image tag") {
				t.Errorf("the pin rule must not claim it — that remediation is the re-pin edit:\n%s", errOut.String())
			}
		})
	}
}

// THE CLASS, NOT THE CASE. Eight review rounds each found another container
// position the positional rule did not recognise — the `container:` shorthand,
// service containers, matrix and matrix.include, env, inputs, a second document,
// an alias, `uses: docker://`, a composite action's steps — and every one failed
// the SAME way: it fell through to the pin rule and was told to restate the ARG,
// which is the re-pin edit that re-arms the trap.
//
// `needs.<job>.outputs` is the position that remains unresolvable, because
// resolving it means running the workflow. It is the standing proof that an
// unrecognised position now degrades to "must float" — the right advice even
// when the guard cannot say what it is looking at.
func TestVersionPinsDegradesToFloatOnAnUnresolvablePosition(t *testing.T) {
	root := pinsFixture(t)
	writeFile(t, filepath.Join(root, ".github/workflows/lint.yml"), ""+
		"jobs:\n"+
		"  build:\n"+
		"    outputs:\n"+
		"      img: ghcr.io/akamai-consulting/ci-tofu:1.12.5\n"+ // the ARG's own value
		"  k8s:\n"+
		"    container:\n"+
		"      image: ${{ vars.KUBE_IMAGE || format('ghcr.io/{0}/ci-kubernetes:latest', github.repository_owner) }}\n"+
		"  tf:\n"+
		"    container:\n"+
		"      image: ${{ needs.build.outputs.img }}\n")

	var errOut strings.Builder
	err := Run(root, false, io.Discard, &errOut)
	if err == nil {
		t.Fatal("the literal carries a version tag and must be told to float")
	}
	// THE VERDICT THAT MUST NEVER APPEAR: the tag already equals the ARG, so the
	// old pin rule called this line ok and, on the next bump, would have demanded
	// the new version — pointing the job at a tag not yet published.
	if strings.Contains(errOut.String(), "disagree with") {
		t.Errorf("an unrecognised position must never be judged as a pin:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "ci-tofu image reference") {
		t.Errorf("it must be reported as a reference that has to float:\n%s", errOut.String())
	}
}
