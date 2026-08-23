package k8sminorcoherence

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// img is a well-formed node image reference: kind's release notes give the tag
// and the digest together, so the fixtures carry both. The digest's VALUE is
// never compared — no offline check can corroborate it, which is why lint.yml
// carries the live half.
func img(tag string) string { return "kindest/node:" + tag + "@sha256:" + strings.Repeat("a", 64) }

// lintWorkflow is where the fixtures put the dry-run job. The guard discovers it
// by scanning workflowsDir rather than by name, so this is a fixture choice
// rather than a contract — TestASecondKindStepInAnotherWorkflowIsCaught is the
// arm that puts one somewhere else.
const lintWorkflow = workflowsDir + "/lint.yml"

// actionsDir is this repo's composite tree — one of yamlRoots, named here so the
// fixtures read as the tree they are exercising.
const actionsDir = ".github/actions"

func run(t *testing.T, root string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := Run(root, &out, &errOut)
	return out.String(), errOut.String(), err
}

// envdefBody is the shape of the real fallback line, so the closed-class regex is
// exercised against the source it was written for rather than a paraphrase.
func envdefBody(pin string) string {
	return "package envdef\n\nfunc x() {\n" +
		"\tk8s := OrElse(tfvarsExampleValue(\"cluster\", \"k8s_version\"), \"" + pin + "\")\n" +
		"\t_ = k8s\n}\n"
}

func tfvarsBody(pin string) string {
	return "region       = \"us-ord\"\nk8s_version = \"" + pin + "\"\nnode_count = \"5\"\n"
}

// lintBody builds a lint.yml whose kind step carries the given `with:` lines,
// with the pins held in `env:` exactly as the real workflow holds them.
func lintBody(kindVersion, nodeImage, kubectlVersion string, with ...string) string {
	b := &strings.Builder{}
	b.WriteString("name: Lint\n" + trigger + shellDefault + "\nenv:\n")
	b.WriteString("  KIND_VERSION: " + kindVersion + "\n")
	if nodeImage != "" {
		b.WriteString("  KIND_NODE_IMAGE: " + nodeImage + "\n")
	}
	b.WriteString("  KUBECTL_VERSION: \"" + kubectlVersion + "\"\n")
	b.WriteString("\njobs:\n  dry-run:\n    steps:\n" +
		"      - uses: actions/checkout@abc\n" +
		"      - name: Create kind cluster\n" +
		"        uses: helm/kind-action@ef37e7f390d99f746eb8b610417061a60e82a6cc # v1.14.0\n" +
		"        with:\n")
	for _, w := range with {
		b.WriteString("          " + w + "\n")
	}
	// The live half and the apply are both part of a well-formed dry-run job, so
	// both are part of the fixture; TestMissingLiveHalfIsCaught and
	// TestMissingDryRunApplyIsCaught are the arms that remove them.
	b.WriteString(liveStep)
	b.WriteString(applyStep)
	return b.String()
}

// trigger is a `paths:` filter that reaches every file this gate compares — the
// state a healthy workflow is in. TestUnreachableTriggerIsCaught narrows it.
// shellDefault is the workflow-level `shell: bash` the real lint.yml declares.
// Without it GitHub's default is `bash -e {0}`, which has no pipefail — so a
// well-formed fixture has to say it, like a well-formed workflow does.
const shellDefault = "defaults:\n  run:\n    shell: bash\n"

const trigger = "on:\n  pull_request:\n    paths:\n      - 'tools/**'\n      - '.github/**'\n" +
	"      - 'template-scripts/**'\n      - 'instance-template/.github/**'\n"

// applyStep is the check the whole gate exists to keep honest.
const applyStep = "      - name: Server-side dry-run of the rendered charts\n" +
	"        run: kubectl apply --dry-run=server -f rendered/\n"

// liveStep is the step that asks the cluster what minor it actually is —
// VERBATIM from .github/workflows/lint.yml, not a paraphrase.
//
// A fixture that has drifted from the thing it stands in for is worse than no
// fixture — it makes the suite agree with itself. Piping to `jq -e
// '.serverVersion.gitVersion'`, for instance, exits 0 for any server, so every
// test asserting "the live half is present and healthy" asserts it over a step
// that compares nothing.
const liveStep = "      - name: The cluster must run the version its tag names\n" +
	"        run: kubectl version -o json | jq -e --arg t \"${KIND_NODE_IMAGE#kindest/node:v}\" " +
	"'.serverVersion.gitVersion == \"v\" + ($t | split(\"@\")[0])'\n"

// tree writes a whole coherent repo, which each test then perturbs in exactly one
// place. The default is the post-#427 state: a 1.34 LKE-E pin and a 1.34 node image.
func tree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, authorityFile, tfvarsBody("v1.34.6+lke2"))
	write(t, root, envdefFile, envdefBody("v1.34.6+lke2"))
	write(t, root, lintWorkflow, lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}",
		"cluster_name: k8s-ci"))
	return root
}

// TestCoherentTreePasses is the baseline, and it is the shape of the fix: kind's
// node image and the LKE-E pin name the same minor without naming the same version.
func TestCoherentTreePasses(t *testing.T) {
	out, _, err := run(t, tree(t))
	if err != nil {
		t.Fatalf("coherent tree must pass, got: %v", err)
	}
	if !strings.Contains(out, "OK") || !strings.Contains(out, "1.34") {
		t.Errorf("want an OK line naming the minor, got %q", out)
	}
}

// TestTheDefectItIsNamedFor reproduces #427 exactly: kind pinned by VERSION only,
// so no node image is declared and the dry-run silently gets kind's default.
//
// This is the arm that matters. A gate proven only against a malformed tree proves
// it can fail, not that it catches the class it claims.
func TestTheDefectItIsNamedFor(t *testing.T) {
	root := t.TempDir()
	write(t, root, authorityFile, tfvarsBody("v1.34.6+lke2"))
	write(t, root, envdefFile, envdefBody("v1.34.6+lke2"))
	write(t, root, lintWorkflow, lintBody("v0.25.0", "", "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}",
		"cluster_name: k8s-ci"))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a kind step with no node_image must fail — that is #427")
	}
	if !strings.Contains(errOut, "no `node_image:`") {
		t.Errorf("the failure must name the absent pin, got %q", errOut)
	}
}

// TestDivergedMinorIsCaught is the same defect once someone HAS pinned an image
// and then moved only one of the two sites.
func TestDivergedMinorIsCaught(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, lintBody("v0.25.0", img("v1.31.2"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}"))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a 1.31 node image against a 1.34 LKE-E pin must fail")
	}
	// The reader needs BOTH sides and both file names: knowing only that they
	// disagree does not say which one to move.
	for _, want := range []string{"1.31", "1.34", lintWorkflow, authorityFile} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the failure must name %q, got %q", want, errOut)
		}
	}
}

// A patch difference is NOT drift: kind ships v1.34.8 and Linode offers
// v1.34.6+lke2, and requiring those to be equal would be unsatisfiable.
func TestPatchDifferenceIsNotDrift(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, lintBody("v0.32.0", img("v1.34.0"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}"))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("v1.34.0 against v1.34.6+lke2 is the same minor and must pass, got: %v", err)
	}
}

// A BARE TAG IS REFUSED. Docker pulls `name:tag@digest` BY DIGEST, so without one
// the pin is not a pin: a re-pushed tag changes what CI boots with no commit here,
// and kind's release notes publish the digest for exactly that reason.
func TestBareTagIsRefused(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, lintBody("v0.32.0", "kindest/node:v1.34.8", "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}"))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a node image with no digest must fail")
	}
	// The minor MATCHES here, so a reader told only "1.34 != 1.34" learns nothing.
	if !strings.Contains(errOut, "names no digest") {
		t.Errorf("the failure must say the digest is what is missing; got %q", errOut)
	}
}

// TestKubectlSkewBound is the issue's second consequence: kubectl 1.34 against a
// 1.31 server is +3, which is unsupported.
func TestKubectlSkewBound(t *testing.T) {
	for _, tc := range []struct {
		name, kubectl string
		wantFail      bool
	}{
		{"equal", "1.34.10", false},
		{"one ahead", "1.35.0", false},
		{"one behind", "1.33.4", false},
		{"three ahead", "1.37.0", true},
		{"two behind", "1.32.9", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t)
			write(t, root, lintWorkflow, lintBody("v0.32.0", img("v1.34.8"), tc.kubectl,
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}"))
			_, errOut, err := run(t, root)
			if tc.wantFail && err == nil {
				t.Fatalf("kubectl %s against a 1.34 server must fail", tc.kubectl)
			}
			if !tc.wantFail && err != nil {
				t.Fatalf("kubectl %s against a 1.34 server is inside the supported skew: %v", tc.kubectl, err)
			}
			if tc.wantFail && !strings.Contains(errOut, "skew") {
				t.Errorf("the failure must say what rule was broken, got %q", errOut)
			}
		})
	}
}

// The action installs its own kubectl and PREPENDS it to PATH, so an unset input
// silently decides which binary every later step runs.
func TestUnpinnedKubectlVersionIsCaught(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}"))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a kind step that does not pin kubectl_version must fail")
	}
	if !strings.Contains(errOut, "kubectl_version") {
		t.Errorf("the failure must name the input, got %q", errOut)
	}
}

// Unpinning kind's own version hands the node image's compatibility to whatever
// default the action ships.
func TestUnpinnedKindVersionIsCaught(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}"))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a kind step that does not pin `version:` must fail")
	}
	if !strings.Contains(errOut, "version") {
		t.Errorf("the failure must name the input, got %q", errOut)
	}
}

// ── fail-closed arms ────────────────────────────────────────────────────────────
//
// Each of these is a way the guard could examine nothing and report green, which
// is worse than not existing: it launders an absence of evidence into a check.

func TestMissingAuthorityFails(t *testing.T) {
	root := tree(t)
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(authorityFile))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, root); err == nil {
		t.Fatal("no authority file must fail, not pass over nothing")
	}
}

func TestAuthorityWithNoPinFails(t *testing.T) {
	root := tree(t)
	write(t, root, authorityFile, "region = \"us-ord\"\n")
	_, _, err := run(t, root)
	if err == nil || !strings.Contains(err.Error(), "vacuously") {
		t.Fatalf("an authority declaring no %s must refuse to pass vacuously, got: %v", authorityKey, err)
	}
}

func TestUnparseableAuthorityFails(t *testing.T) {
	root := tree(t)
	write(t, root, authorityFile, tfvarsBody("latest"))
	if _, _, err := run(t, root); err == nil {
		t.Fatal("an unparseable LKE-E pin must fail — 'could not tell' is not 'agrees'")
	}
}

// The two copies of the deployed pin must agree, or "the minor we deploy" has no
// single answer and every comparison below it is arbitrary.
func TestDisagreeingAuthorityCopiesFail(t *testing.T) {
	root := tree(t)
	write(t, root, envdefFile, envdefBody("v1.32.9+lke4"))
	_, _, err := run(t, root)
	if err == nil || !strings.Contains(err.Error(), "two values") {
		t.Fatalf("a drifted envdef fallback must fail, got: %v", err)
	}
}

// A closed class that stops matching is a scanner gone blind, not a clean tree.
func TestVanishedEnvdefFallbackFails(t *testing.T) {
	root := tree(t)
	write(t, root, envdefFile, "package envdef\n\nfunc x() string { return readItSomeOtherWay() }\n")
	_, _, err := run(t, root)
	if err == nil || !strings.Contains(err.Error(), "no longer states a fallback") {
		t.Fatalf("an unmatchable fallback site must fail, got: %v", err)
	}
}

func TestMissingWorkflowFails(t *testing.T) {
	root := tree(t)
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(lintWorkflow))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, root); err == nil {
		t.Fatal("no lint.yml must fail")
	}
}

func TestUnparseableWorkflowFails(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, "jobs:\n  a:\n   - this: [is\n  not: yaml\n")
	if _, _, err := run(t, root); err == nil {
		t.Fatal("a workflow this guard cannot parse is one it cannot vouch for")
	}
}

func TestWorkflowWithNoJobsFails(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, "name: Lint\n"+trigger+shellDefault+"env:\n  KIND_VERSION: v0.32.0\n")
	_, _, err := run(t, root)
	if err == nil || !strings.Contains(err.Error(), "no `uses: "+kindActionPrefix+"`") {
		t.Fatalf("a jobless workflow leaves no kind step to find and must say so, got: %v", err)
	}
}

// An empty workflow tree is what a wrong --root looks like.
func TestNoWorkflowsAtAllFails(t *testing.T) {
	root := tree(t)
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(lintWorkflow))); err != nil {
		t.Fatal(err)
	}
	_, _, err := run(t, root)
	if err == nil || !strings.Contains(err.Error(), "vacuously") {
		t.Fatalf("no workflows must refuse to pass vacuously, got: %v", err)
	}
}

// Losing the kind step is the guard's own obsolescence, and it has to say so
// rather than report green over a gate that no longer exists.
func TestNoKindStepFails(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, "name: Lint\n"+trigger+shellDefault+"jobs:\n  dry-run:\n    steps:\n      - uses: actions/checkout@abc\n")
	_, _, err := run(t, root)
	if err == nil || !strings.Contains(err.Error(), "no `uses: helm/kind-action`") {
		t.Fatalf("a workflow with no kind step must fail and name what it looked for, got: %v", err)
	}
}

// Two clusters, one comparison: the guard would vouch for whichever it read first.
func TestTwoKindStepsFail(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	body += "  second:\n    steps:\n      - uses: helm/kind-action@ef37e7f # v1.14.0\n        with:\n          version: v0.25.0\n"
	write(t, root, lintWorkflow, body)
	_, _, err := run(t, root)
	if err == nil || !strings.Contains(err.Error(), "2 `helm/kind-action`") {
		t.Fatalf("two kind steps must fail, got: %v", err)
	}
}

// ── expression resolution ───────────────────────────────────────────────────────

// A pin held in `env:` must be FOLLOWED, not skipped — the real workflow holds
// all three there. `version:` and `kubectl_version:` may equally be literals;
// `node_image:` may not, because the live step reads the env var and the two
// halves have to certify one value.
func TestLiteralPinsAreAccepted(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: v0.32.0",
		"node_image: "+img("v1.34.8"),
		"kubectl_version: v1.34.10"))
	if _, _, err := run(t, root); err != nil {
		t.Fatalf("literal pins must be read the same as env references, got: %v", err)
	}
}

// ...but a literal node image with NO env var behind it splits the two halves:
// the guard certifies the literal while the live step expands an unset variable.
func TestNodeImageDivergingFromTheEnvIsCaught(t *testing.T) {
	root := tree(t)
	// lintBody omits KIND_NODE_IMAGE when the image argument is empty.
	write(t, root, lintWorkflow, lintBody("v0.32.0", "", "1.34.10",
		"version: v0.32.0",
		"node_image: "+img("v1.34.8"),
		"kubectl_version: v1.34.10"))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("the static and live halves must read one value")
	}
	if !strings.Contains(errOut, "the live check reads env."+nodeImageEnv) {
		t.Errorf("the failure must say why the env var matters, got %q", errOut)
	}
}

// A reference the guard cannot follow leaves the real value unexamined, and
// "unexamined" must never read as "fine".
func TestUnresolvableReferenceFails(t *testing.T) {
	for _, expr := range []string{
		"node_image: ${{ env.NOT_DEFINED_ANYWHERE }}",
		"node_image: ${{ vars.SOME_IMAGE }}",
	} {
		t.Run(expr, func(t *testing.T) {
			root := tree(t)
			write(t, root, lintWorkflow, lintBody("v0.32.0", "", "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				expr,
				"kubectl_version: v${{ env.KUBECTL_VERSION }}"))
			if _, _, err := run(t, root); err == nil {
				t.Fatalf("%q must fail rather than be compared as a literal", expr)
			}
		})
	}
}

// A job-level env block wins over the workflow-level one, as it does in Actions —
// so the guard must READ the job-level value (1.31, not the workflow's 1.34), and
// refuse it twice over: it is the wrong minor, and it is not what the live step
// reads.
func TestJobLevelEnvWins(t *testing.T) {
	root := tree(t)
	body := "name: Lint\n" + trigger + shellDefault + "env:\n  KIND_NODE_IMAGE: " + img("v1.34.8") + "\n  KUBECTL_VERSION: \"1.34.10\"\n" +
		"jobs:\n  dry-run:\n    env:\n      KIND_NODE_IMAGE: " + img("v1.31.2") + "\n    steps:\n" +
		"      - uses: helm/kind-action@ef37e7f # v1.14.0\n        with:\n" +
		"          version: v0.32.0\n" +
		"          node_image: ${{ env.KIND_NODE_IMAGE }}\n" +
		"          kubectl_version: v${{ env.KUBECTL_VERSION }}\n" + liveStep + applyStep
	write(t, root, lintWorkflow, body)

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("the job-level override is the value kind receives, and it is 1.31")
	}
	if !strings.Contains(errOut, "1.31") {
		t.Errorf("the guard must read the job-level value, got %q", errOut)
	}
}

// A node_image that is not a kindest/node reference is not something this guard
// can read a minor out of.
func TestNonKindNodeImageFails(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, lintBody("v0.32.0", "ghcr.io/example/node:v1.34.8", "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}"))
	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("an image this guard cannot read a minor out of must fail")
	}
	if !strings.Contains(errOut, "kindest/node") {
		t.Errorf("the failure must name the form it wanted, got %q", errOut)
	}
}

// ── parseMinor ─────────────────────────────────────────────────────────────────

func TestParseMinorSpellings(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		bad  bool
	}{
		{in: "v1.34.6+lke2", want: "1.34"},
		{in: "1.34.10", want: "1.34"},
		{in: "v1.34.8", want: "1.34"},
		{in: "v1.34", want: "1.34"},
		{in: "v1.35.0-rc.1", want: "1.35"},
		{in: "v1", bad: true},
		{in: "latest", bad: true},
		{in: "v1.x.0", bad: true},
		{in: "vx.y.z", bad: true},
		{in: "", bad: true},
	} {
		got, err := parseMinor(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("parseMinor(%q) = %s, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMinor(%q): %v", tc.in, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("parseMinor(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// A version this guard cannot read is not a version it may pass over — on either
// input, and whether or not the surrounding form is well-shaped.
func TestUnparseableStepVersionsFail(t *testing.T) {
	for _, tc := range []struct{ name, nodeImage, kubectl string }{
		{"node image", img("v1.x.0"), "v1.34.10"},
		{"kubectl", img("v1.34.8"), "vlatest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t)
			write(t, root, lintWorkflow, lintBody("v0.32.0", "", "1.34.10",
				"version: v0.32.0",
				"node_image: "+tc.nodeImage,
				"kubectl_version: "+tc.kubectl))
			if _, _, err := run(t, root); err == nil {
				t.Fatalf("%s %q is unreadable and must fail", tc.name, tc.nodeImage+tc.kubectl)
			}
		})
	}
}

// The cobra surface is thin, but `--root` is the whole of what the gate driver
// hands it — a command that ignores it would check the wrong tree in silence.
func TestCmdTakesItsRoot(t *testing.T) {
	c := Cmd()
	if c.Use != "k8s-minor-coherence" {
		t.Fatalf("verb is %q; the registry row and the Makefile target name k8s-minor-coherence", c.Use)
	}
	c.SetArgs([]string{"--root", tree(t)})
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	if err := c.Execute(); err != nil {
		t.Fatalf("a coherent tree passed on --root must exit clean, got: %v", err)
	}

	bad := tree(t)
	if err := os.Remove(filepath.Join(bad, filepath.FromSlash(authorityFile))); err != nil {
		t.Fatal(err)
	}
	c = Cmd()
	c.SetArgs([]string{"--root", bad})
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err == nil {
		t.Fatal("--root must actually select the tree — a broken one has to fail through the command too")
	}
}

// An unresolvable `version:` reads as pinned unless it goes through resolve():
// Actions substitutes empty and kind-action falls back to its own default, which
// is the state the `version:` check exists to refuse.
func TestUnresolvableKindVersionIsCaught(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.NO_SUCH_VAR }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}"))
	if _, _, err := run(t, root); err == nil {
		t.Fatal("a `version:` this guard cannot resolve must fail, like the other two inputs")
	}
}

// A step-level env block is the INNERMOST scope and wins. Reading only the outer
// two is worse than not resolving at all — the guard answers confidently, and
// wrongly, about a value it never saw.
func TestStepLevelEnvWins(t *testing.T) {
	body := "name: Lint\n" + trigger + shellDefault + "env:\n  KIND_NODE_IMAGE: " + img("v1.34.8") + "\n  KUBECTL_VERSION: \"1.34.10\"\n" +
		"jobs:\n  dry-run:\n    env:\n      KIND_NODE_IMAGE: " + img("v1.33.7") + "\n    steps:\n" +
		"      - uses: helm/kind-action@ef37e7f # v1.14.0\n" +
		"        env:\n          KIND_NODE_IMAGE: " + img("v1.31.2") + "\n        with:\n" +
		"          version: v0.32.0\n" +
		"          node_image: ${{ env.KIND_NODE_IMAGE }}\n" +
		"          kubectl_version: v${{ env.KUBECTL_VERSION }}\n"

	root := tree(t)
	write(t, root, lintWorkflow, body)
	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("the step-level override is the image kind receives, and it is 1.31")
	}
	if !strings.Contains(errOut, "1.31") {
		t.Errorf("the failure must name the value that actually reaches kind, got %q", errOut)
	}
}

// setupKubectlStep is the OTHER kubectl installer, appended to the fixture's job.
func setupKubectlStep(version string) string {
	s := "      - uses: azure/setup-kubectl@829323\n"
	if version != "" {
		s += "        with:\n          version: " + version + "\n"
	}
	return s
}

// THE SKEW IS AGAINST THE SERVER, WHICH IS THE NODE IMAGE — not against the pin
// the node image is held to. On a healthy tree those are equal, which is exactly
// why the first cut measured the wrong one and every test still passed. This is
// the #427 state, and it must produce BOTH problems: the image is three minors
// behind the deployment, and the kubectl talking to it is three minors ahead.
func TestSkewIsMeasuredAgainstTheNodeImage(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, lintBody("v0.25.0", img("v1.31.2"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}"))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a 1.31 server with a 1.34 kubectl is both a fidelity gap and an unsupported skew")
	}
	if !strings.Contains(errOut, "2 problem(s)") {
		t.Errorf("want both the image mismatch and the skew, got %q", errOut)
	}
	if !strings.Contains(errOut, "3 minors from the 1.31 server") {
		t.Errorf("the skew must be reported against the node image, got %q", errOut)
	}
}

// Two installers, both prepending to PATH: which kubectl the job runs is decided
// by step order, and two pins that each satisfy the skew bound still leave that
// undecided.
func TestTwoDisagreeingKubectlInstallsAreCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v1.33.4")
	write(t, root, lintWorkflow, body+setupKubectlStep("v${{ env.KUBECTL_VERSION }}"))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("kubectl installed twice at two versions must fail — both are inside the skew bound")
	}
	for _, want := range []string{"installs kubectl twice", "1.34.10", "1.33.4"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the failure must name %q, got %q", want, errOut)
		}
	}
}

// The same two installers agreeing is the shape the real workflow has.
func TestTwoAgreeingKubectlInstallsPass(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, body+setupKubectlStep("v${{ env.KUBECTL_VERSION }}"))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("one version through two installers is one kubectl, got: %v", err)
	}
}

// A second installer that pins nothing is the same defect with the version
// supplied by the action instead of by a pin.
func TestUnpinnedSetupKubectlIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, body+setupKubectlStep(""))

	_, _, err := run(t, root)
	if err == nil {
		t.Fatal("an unpinned setup-kubectl must fail — its version is then the action's")
	}
}

// Two of them and the guard would vouch for whichever it read first, which is
// the very thing under comparison.
func TestTwoSetupKubectlStepsFail(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, body+setupKubectlStep("v1.34.10")+setupKubectlStep("v1.34.10"))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("two setup-kubectl steps in one job must fail")
	}
	if !strings.Contains(errOut, "appears 2 times") {
		t.Errorf("the failure must say how many it found, got %q", errOut)
	}
}

// A job with no second installer has nothing to disagree with, and the skew bound
// still applies on its own.
func TestNoSetupKubectlStepIsFine(t *testing.T) {
	if _, _, err := run(t, tree(t)); err != nil {
		t.Fatalf("the fixture has one installer and must pass, got: %v", err)
	}
}

// With no node image there is no server to measure against, so the skew falls
// back to the deployed pin — and the message has to SAY so. "3 minors from the
// 1.34 server" would otherwise name a server the guard never read.
func TestSkewNamesItsFallbackServer(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, lintBody("v0.32.0", "", "1.37.0",
		"version: ${{ env.KIND_VERSION }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}"))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("no node image and a +3 kubectl are two problems")
	}
	if !strings.Contains(errOut, "the node image could not be read") {
		t.Errorf("the skew must say which server it measured against, got %q", errOut)
	}
}

// A reference in the OTHER installer's version is unexamined the same way, and
// must fail the same way.
func TestUnresolvableSetupKubectlVersionFails(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, body+setupKubectlStep("v${{ env.NO_SUCH_VAR }}"))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a setup-kubectl version this guard cannot resolve must fail")
	}
}

// THE DIGEST RULE RESTS ON THE LIVE STEP. Without it the guard reads a tag docker
// does not pull and has no way to notice — #427 verbatim, green throughout — so
// losing the step has to be as loud as losing the pin.
func TestMissingLiveHalfIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, liveStep, "", 1))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a dry-run job with no live minor check must fail")
	}
	if !strings.Contains(errOut, "kubectl version") {
		t.Errorf("the failure must name the signature it looked for, got %q", errOut)
	}
}

// ── round 4: the guard's own fail-open holes ───────────────────────────────────

// GitHub resolves `uses:` without regard to case, and the setup-kubectl action's
// CANONICAL repo name is `Azure/setup-kubectl`. A case-sensitive match skipped the
// duplicate-kubectl check on the spelling the docs tell you to use — the guard
// printing OK having never looked.
func TestUsesMatchingIgnoresCase(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v1.33.4")
	write(t, root, lintWorkflow, body+"      - uses: Azure/setup-kubectl@829323\n        with:\n          version: v1.34.10\n")

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("the canonical `Azure/` spelling is the same action and must still be compared")
	}
	if !strings.Contains(errOut, "installs kubectl twice") {
		t.Errorf("want the duplicate-kubectl finding, got %q", errOut)
	}
}

func TestKindStepMatchingIgnoresCase(t *testing.T) {
	root := tree(t)
	body := strings.Replace(lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}"),
		"helm/kind-action@", "Helm/Kind-Action@", 1)
	write(t, root, lintWorkflow, body)

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("`Helm/Kind-Action@` is the same step and must be found, got: %v", err)
	}
}

// A step that cannot fail the job is not a check, and the digest rule's whole
// safety argument is that this one can.
func TestDisabledLiveHalfIsCaught(t *testing.T) {
	for _, tc := range []struct{ name, extra, want string }{
		{"continue-on-error", "        continue-on-error: true\n", "continue-on-error"},
		{"conditional", "        if: github.event_name == 'push'\n", "conditional"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			// Insert the disabling key into the live step, before its `run:`.
			write(t, root, lintWorkflow, strings.Replace(body, "        run: kubectl version", tc.extra+"        run: kubectl version", 1))

			_, errOut, err := run(t, root)
			if err == nil {
				t.Fatalf("a live check with %s must fail", tc.name)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("the failure must name what disabled it, got %q", errOut)
			}
		})
	}
}

// `continue-on-error: false` is the default written out, and disables nothing.
func TestExplicitlyGatingLiveHalfPasses(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, "        run: kubectl version", "        continue-on-error: false\n        run: kubectl version", 1))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("`continue-on-error: false` is the default spelled out, got: %v", err)
	}
}

// Commenting the check out is the loudest way to disable it, and a substring
// search read it as present.
func TestCommentedOutLiveHalfIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	commented := "      - name: The cluster must run the minor its tag claims\n" +
		"        run: |\n" +
		"          # temporarily disabled\n" +
		"          # kubectl version -o json | jq -e --arg t \"${KIND_NODE_IMAGE#kindest/node:v}\" 'x'\n" +
		"          echo skipped\n"
	write(t, root, lintWorkflow, strings.Replace(body, liveStep, commented, 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a commented-out live check is an absent one")
	}
}

// ...but the `#` in `${KIND_NODE_IMAGE#kindest/node:v}` is a parameter expansion,
// not a comment. Stripping from the first `#` anywhere would gut the real check.
func TestParameterExpansionIsNotAComment(t *testing.T) {
	if got := uncommented("kubectl version | grep \"${KIND_NODE_IMAGE#kindest/node:v}\""); !strings.Contains(got, nodeImageEnv) {
		t.Errorf("uncommented() ate a parameter expansion: %q", got)
	}
}

// A JOB-LEVEL pin is coherent: the action input and the live step's shell both
// resolve through the same scope stack, so both read it. Rejecting it was the
// guard comparing against the workflow level rather than against what the live
// step reads.
func TestJobLevelPinIsCoherent(t *testing.T) {
	root := tree(t)
	body := "name: Lint\n" + trigger + shellDefault + "env:\n  KUBECTL_VERSION: \"1.34.10\"\n" +
		"jobs:\n  dry-run:\n    env:\n      KIND_NODE_IMAGE: " + img("v1.34.8") + "\n    steps:\n" +
		"      - uses: helm/kind-action@ef37e7f # v1.14.0\n        with:\n" +
		"          version: v0.32.0\n" +
		"          node_image: ${{ env.KIND_NODE_IMAGE }}\n" +
		"          kubectl_version: v${{ env.KUBECTL_VERSION }}\n" + liveStep + applyStep
	write(t, root, lintWorkflow, body)

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("a job-level pin is read by both halves and must pass, got: %v", err)
	}
}

// A pin the LIVE step shadows for itself is the divergence that matters, and it
// is invisible to a workflow-level comparison.
func TestLiveStepShadowingThePinIsCaught(t *testing.T) {
	root := tree(t)
	body := "name: Lint\n" + trigger + shellDefault + "env:\n  KIND_NODE_IMAGE: " + img("v1.34.8") + "\n  KUBECTL_VERSION: \"1.34.10\"\n" +
		"jobs:\n  dry-run:\n    steps:\n" +
		"      - uses: helm/kind-action@ef37e7f # v1.14.0\n        with:\n" +
		"          version: v0.32.0\n" +
		"          node_image: ${{ env.KIND_NODE_IMAGE }}\n" +
		"          kubectl_version: v${{ env.KUBECTL_VERSION }}\n" +
		// The env block belongs to the live step itself — that is the shadowing.
		strings.Replace(liveStep, "        run:", "        env:\n          KIND_NODE_IMAGE: "+img("v1.31.2")+"\n        run:", 1) +
		applyStep
	write(t, root, lintWorkflow, body)

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("the live step reading a different pin than the cluster it measures must fail")
	}
	if !strings.Contains(errOut, "1.31") {
		t.Errorf("the failure must name what the live half would read, got %q", errOut)
	}
}

// ── round 5: the check the gate exists for, and the trailing-comment spelling ──

// THE GUARD CERTIFIED THE FIDELITY OF A CHECK IT NEVER CONFIRMED EXISTS. Holding
// a cluster to the deployed minor means nothing if no manifest is sent to it.
func TestMissingDryRunApplyIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, "", 1))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a dry-run job with no server-side apply must fail")
	}
	if !strings.Contains(errOut, dryRunApply) {
		t.Errorf("the failure must name the signature it looked for, got %q", errOut)
	}
}

// Flipping the apply to client-side removes the API server from the loop, which
// is the same thing as deleting it as far as this gate is concerned.
func TestClientSideApplyIsNotAServerSideDryRun(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, "--dry-run=server", "--dry-run=client", 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a client-side apply asks no API server anything and must fail")
	}
}

// ...but a client-side apply ALONGSIDE the server-side one is the job's real
// shape: it renders a Namespace before applying it.
func TestAClientSideApplyBesideTheServerSideOneIsFine(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	ns := "      - name: Create namespaces\n        run: kubectl create namespace x --dry-run=client -o yaml | kubectl apply -f -\n"
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, ns+applyStep, 1))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("--dry-run=client is a legitimate renderer beside the real apply, got: %v", err)
	}
}

// Each of the three load-bearing steps, and the job around them, has to be able
// to fail the run. A green check over a skipped job is the worst of both.
func TestNothingLoadBearingMayBeDisabled(t *testing.T) {
	for _, tc := range []struct{ name, find, insert string }{
		{"the kind step", "        uses: helm/kind-action@ef37e7f", "        continue-on-error: true\n        uses: helm/kind-action@ef37e7f"},
		{"the apply", "        run: kubectl apply --dry-run=server", "        continue-on-error: true\n        run: kubectl apply --dry-run=server"},
		{"the apply, conditionally", "        run: kubectl apply --dry-run=server", "        if: github.ref == 'refs/heads/main'\n        run: kubectl apply --dry-run=server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body, tc.find, tc.insert, 1))
			if _, _, err := run(t, root); err == nil {
				t.Fatalf("%s must be able to fail the job", tc.name)
			}
		})
	}
}

// A job-level `if:` disables every step at once, and did so invisibly.
func TestConditionalJobIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, "  dry-run:\n    steps:", "  dry-run:\n    if: false\n    steps:", 1))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a conditional dry-run job must fail — none of its steps are guaranteed to run")
	}
	if !strings.Contains(errOut, "conditional") {
		t.Errorf("the failure must say the job is conditional, got %q", errOut)
	}
}

// A TRAILING comment neuters the live check exactly as a leading one does, and
// stripping only whole-line comments left that spelling open.
func TestTrailingCommentDisablesTheLiveHalf(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	neutered := "      - name: The cluster must run the minor its tag claims\n" +
		"        run: echo skipping  # kubectl version -o json … ${KIND_NODE_IMAGE}\n"
	write(t, root, lintWorkflow, strings.Replace(body, liveStep, neutered, 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a live check commented out at the end of a line is still commented out")
	}
}

// commentStart is the whole difference between that and eating the real check.
func TestCommentStart(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{`kubectl version "${KIND_NODE_IMAGE#kindest/node:v}"`, `kubectl version "${KIND_NODE_IMAGE#kindest/node:v}"`},
		{`echo hi # a comment`, `echo hi `},
		{"\techo hi\t# tabbed", "\techo hi\t"},
		{`# whole line`, ``},
		{`  # indented whole line`, `  `},
		{`echo ${X#y}#z`, `echo ${X#y}#z`},
	} {
		if got := uncommented(tc.in); got != tc.want {
			t.Errorf("uncommented(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── round 6: the ways a step stays present and stops checking ──────────────────

// `continue-on-error` at JOB level discards the whole job's result. The guard
// closed this door at step level and for a job-level `if:`, and left it open here.
func TestJobLevelContinueOnErrorIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, "  dry-run:\n    steps:", "  dry-run:\n    continue-on-error: true\n    steps:", 1))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a continue-on-error dry-run job cannot fail anything and must be refused")
	}
	if !strings.Contains(errOut, "continue-on-error") {
		t.Errorf("the failure must name what discarded the result, got %q", errOut)
	}
}

// `|| true` is `continue-on-error` one layer down, where the YAML still looks
// gating. Both load-bearing `run:` steps are held to it.
func TestShellSwallowIsCaught(t *testing.T) {
	for _, tc := range []struct{ name, find, replace string }{
		{"the apply", "-f rendered/", "-f rendered/ || true"},
		// Quoted, because `|| :` cannot be a plain YAML scalar — which is how a real
		// workflow would have to spell it too.
		{"the apply, with a colon", "run: kubectl apply --dry-run=server -f rendered/", `run: "kubectl apply --dry-run=server -f rendered/ || :"`},
		{"the live check", "        run: kubectl version", "        run: set +e; kubectl version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body, tc.find, tc.replace, 1))

			_, errOut, err := run(t, root)
			if err == nil {
				t.Fatalf("%s swallows its exit status and must be refused", tc.name)
			}
			if !strings.Contains(errOut, "discard an exit status") {
				t.Errorf("the failure must name the swallow, got %q", errOut)
			}
		})
	}
}

// EVERY `||` IS REFUSED, including `|| exit 1`, which does fail the job. Deciding
// otherwise means enumerating right-hand sides — the guessing this list replaced
// — and the remedy is to drop it: under `set -e` it is redundant anyway.
func TestEvenAHarmlessOrIsRefused(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, "-f rendered/", "-f rendered/ || exit 1", 1))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("an `||` in a load-bearing step is refused whatever follows it")
	}
	if !strings.Contains(errOut, "give it its own step") {
		t.Errorf("the failure must carry the remedy, got %q", errOut)
	}
}

// A DISABLED SECOND COPY BEHIND A HEALTHY ONE used to pass, because only the
// first match was judged.
func TestASecondDisabledApplyIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	second := "      - name: and the per-env overlays\n        continue-on-error: true\n" +
		"        run: kubectl apply --dry-run=server -f overlays/\n"
	write(t, root, lintWorkflow, body+second)

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a non-blocking second apply is a lie about coverage and must be refused")
	}
}

// ...and a conditional apply placed AHEAD of the real one used to fail the gate
// on position alone. Both applies gating is the only thing that passes.
func TestTwoGatingAppliesPass(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	second := "      - name: and the per-env overlays\n        run: kubectl apply --dry-run=server -f overlays/\n"
	write(t, root, lintWorkflow, body+second)

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("two applies that can both fail are two real checks, got: %v", err)
	}
}

// Two live checks and there is no one answer to what env the live half reads,
// which is the value the coherence comparison turns on.
func TestTwoLiveChecksFail(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, body+liveStep)

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("two live minor checks must fail")
	}
	if !strings.Contains(errOut, "2 steps matching") {
		t.Errorf("the failure must say how many it found, got %q", errOut)
	}
}

// ── round 9: the same defect one file over, and the dependency door ────────────

// THE GUARD READ ONE HARDCODED FILE. A second workflow standing up kind was not a
// conflict but a blind spot: it boots kind's default node image and re-creates
// #427 with this gate green. Across the tree it is one finding.
func TestASecondKindStepInAnotherWorkflowIsCaught(t *testing.T) {
	root := tree(t)
	write(t, root, workflowsDir+"/e2e.yml",
		"name: E2E\njobs:\n  spin:\n    steps:\n      - uses: helm/kind-action@ef37e7f # v1.14.0\n        with:\n          version: v0.32.0\n")

	_, _, err := run(t, root)
	if err == nil {
		t.Fatal("two kind clusters and one comparison must fail")
	}
	if !strings.Contains(err.Error(), "e2e.yml") {
		t.Errorf("the failure must name the other site, got: %v", err)
	}
}

// The dry-run job is found wherever it lives, not by file name.
func TestTheKindStepIsFoundInAnyWorkflow(t *testing.T) {
	root := tree(t)
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(lintWorkflow)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(lintWorkflow))); err != nil {
		t.Fatal(err)
	}
	write(t, root, workflowsDir+"/manifests.yml", string(body))

	if _, _, rerr := run(t, root); rerr != nil {
		t.Fatalf("the job was renamed, not broken, and must still pass: %v", rerr)
	}
}

// A SKIPPED DEPENDENCY SKIPS ITS DEPENDENTS. `needs:` on a job that carries the
// fork guard is the tidy-looking edit that takes the dry-run out on fork PRs.
func TestSkippableDependencyIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	body = strings.Replace(body, "  dry-run:\n    steps:", "  dry-run:\n    needs: [kubernetes]\n    steps:", 1)
	body += "  kubernetes:\n    if: github.event.pull_request.head.repo.full_name == github.repository\n    steps:\n      - run: echo hi\n"
	write(t, root, lintWorkflow, body)

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("needing a conditional job makes the dry-run conditional too")
	}
	if !strings.Contains(errOut, "skips the dry-run with it") {
		t.Errorf("the failure must explain the mechanism, got %q", errOut)
	}
}

// ...and depending on an unconditional job is fine, in either spelling.
func TestUnconditionalDependencyIsFine(t *testing.T) {
	for _, spelling := range []string{"needs: [kubernetes]", "needs: kubernetes"} {
		t.Run(spelling, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			body = strings.Replace(body, "  dry-run:\n    steps:", "  dry-run:\n    "+spelling+"\n    steps:", 1)
			body += "  kubernetes:\n    steps:\n      - run: echo hi\n"
			write(t, root, lintWorkflow, body)

			if _, _, err := run(t, root); err != nil {
				t.Fatalf("an unconditional dependency always runs, got: %v", err)
			}
		})
	}
}

// A `needs:` naming a job that does not exist means the dry-run never runs at all.
func TestDependencyOnAMissingJobIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, "  dry-run:\n    steps:", "  dry-run:\n    needs: [gone]\n    steps:", 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a dependency that does not exist must fail")
	}
}

// `needs:` is a scalar or a sequence and nothing else; a mapping is a workflow
// this guard cannot read, not one it may wave through.
func TestMalformedNeedsFails(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, "  dry-run:\n    steps:", "  dry-run:\n    needs: {a: b}\n    steps:", 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a `needs:` shape this guard cannot parse must fail")
	}
}

// The kubectl pin gets the same treatment as the node image: a reference the
// guard cannot follow leaves the real value unexamined.
func TestUnresolvableKubectlVersionFails(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.NOT_DEFINED }}"))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("an unresolvable kubectl_version must fail rather than be skipped")
	}
}

// A `continue-on-error` upstream is NOT a skip: `needs:` resolves it as success
// even when it fails, so its dependents run. Calling it a skip would hard-fail a
// legitimate wiring change with a diagnosis that is untrue.
func TestContinueOnErrorDependencyIsNotASkip(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	body = strings.Replace(body, "  dry-run:\n    steps:", "  dry-run:\n    needs: [kubernetes]\n    steps:", 1)
	body += "  kubernetes:\n    continue-on-error: true\n    steps:\n      - run: echo hi\n"
	write(t, root, lintWorkflow, body)

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("a continue-on-error dependency still runs its dependents, got: %v", err)
	}
}

// ── round 11: the cluster a composite could stand up ───────────────────────────

// A COMPOSITE CAN STAND UP A CLUSTER TOO, and the contract is one such step in
// the REPO, not one per workflow. Scanning only .github/workflows left a second
// cluster — booting kind's own default node image — in a file the guard never
// opened.
func TestAKindStepInACompositeIsCaught(t *testing.T) {
	root := tree(t)
	write(t, root, actionsDir+"/spin-cluster/action.yml",
		"name: Spin\nruns:\n  using: composite\n  steps:\n    - uses: helm/kind-action@ef37e7f # v1.14.0\n      with:\n        version: v0.32.0\n")

	_, _, err := run(t, root)
	if err == nil {
		t.Fatal("a second cluster in a composite is still a second cluster")
	}
	if !strings.Contains(err.Error(), "composite") {
		t.Errorf("the failure must name where it found it, got: %v", err)
	}
}

// ...and if the ONLY one is in a composite, the dry-run job is gone: a composite
// has no `jobs:`, so nothing there can be the check this gate protects.
func TestOnlyKindStepInACompositeFails(t *testing.T) {
	root := tree(t)
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(lintWorkflow))); err != nil {
		t.Fatal(err)
	}
	write(t, root, workflowsDir+"/other.yml", "name: Other\njobs:\n  x:\n    steps:\n      - run: echo hi\n")
	write(t, root, actionsDir+"/spin-cluster/action.yml",
		"name: Spin\nruns:\n  using: composite\n  steps:\n    - uses: helm/kind-action@ef37e7f\n      with:\n        version: v0.32.0\n")

	_, _, err := run(t, root)
	if err == nil || !strings.Contains(err.Error(), "composite action") {
		t.Fatalf("a composite cannot hold the dry-run job and the failure must say so, got: %v", err)
	}
}

// Composites are optional — an instance checkout has none — so their absence is
// not the wrong-root signal a missing workflows directory is.
func TestNoCompositesIsFine(t *testing.T) {
	if _, _, err := run(t, tree(t)); err != nil {
		t.Fatalf("a tree with no .github/actions must pass, got: %v", err)
	}
}

// A missing workflows directory is the wrong --root, and it is a different
// failure from a directory that is there and empty — "could not tell" and
// "nothing there" must not collapse into one answer.
func TestMissingWorkflowsDirectoryFails(t *testing.T) {
	root := tree(t)
	if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(workflowsDir))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, root); err == nil {
		t.Fatal("no .github/workflows at all must fail")
	}
}

// ── round 12: the cluster that never went through the action ───────────────────

// A SHELL-CREATED CLUSTER DECLARES NOTHING. `run: kind create cluster` boots
// kind's own default node image, and the guard reads `node_image:` — so matching
// only the action left #427 available one `run:` away.
func TestShellCreatedClusterIsRefused(t *testing.T) {
	for _, where := range []string{workflowsDir + "/e2e.yml", actionsDir + "/spin/action.yml"} {
		t.Run(where, func(t *testing.T) {
			root := tree(t)
			if strings.HasPrefix(where, actionsDir) {
				write(t, root, where, "name: Spin\nruns:\n  using: composite\n  steps:\n    - run: kind create cluster --name x\n")
			} else {
				write(t, root, where, "name: E2E\njobs:\n  spin:\n    steps:\n      - run: kind create cluster --name x\n")
			}
			// A FINDING, not an abort — the same shape as the script spelling, so one
			// inline `kind create` no longer blanks every other verdict.
			_, errOut, err := run(t, root)
			if err == nil {
				t.Fatal("a cluster stood up outside the action declares no node image and must be refused")
			}
			if !strings.Contains(errOut, where) {
				t.Errorf("the failure must name the file, got: %v", errOut)
			}
		})
	}
}

// A commented-out one is not a cluster.
func TestCommentedShellCreateIsNotACluster(t *testing.T) {
	root := tree(t)
	write(t, root, workflowsDir+"/e2e.yml",
		"name: E2E\njobs:\n  spin:\n    steps:\n      - run: |\n          # kind create cluster --name x\n          echo nope\n")

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("a commented-out create is not a create, got: %v", err)
	}
}

// `install_only` means the step creates no cluster at all, so the node image it
// declares is never booted and every comparison is about something that does not
// exist.
func TestInstallOnlyIsCaught(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}",
		"install_only: true"))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("install_only creates no cluster and must be refused")
	}
	if !strings.Contains(errOut, "install_only") {
		t.Errorf("the failure must name the input, got %q", errOut)
	}
}

// ...and spelling the default out changes nothing.
func TestInstallOnlyFalseIsFine(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}",
		"install_only: false"))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("install_only: false is the default spelled out, got: %v", err)
	}
}

// ── round 13: three ways past a rule the guard states ──────────────────────────

// SKIPPING IS TRANSITIVE AND THE WALK WAS NOT. `dry-run` needs `prep`, `prep`
// needs the job carrying the fork guard: one hop saw nothing and the dry-run was
// skipped on every fork PR with the gate green.
func TestTransitivelySkippableDependencyIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	body = strings.Replace(body, "  dry-run:\n    steps:", "  dry-run:\n    needs: [prep]\n    steps:", 1)
	body += "  prep:\n    needs: [kubernetes]\n    steps:\n      - run: echo hi\n"
	body += "  kubernetes:\n    if: github.event.pull_request.head.repo.full_name == github.repository\n    steps:\n      - run: echo hi\n"
	write(t, root, lintWorkflow, body)

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a dependency chain is exactly as long as someone finds convenient")
	}
	if !strings.Contains(errOut, "skips the dry-run with it") {
		t.Errorf("the failure must explain the mechanism, got %q", errOut)
	}
}

// A cycle in `needs:` is Actions' problem, not an infinite loop in ours.
func TestNeedsCycleTerminates(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	body = strings.Replace(body, "  dry-run:\n    steps:", "  dry-run:\n    needs: [a]\n    steps:", 1)
	body += "  a:\n    needs: [b]\n    steps:\n      - run: echo hi\n"
	body += "  b:\n    needs: [a]\n    steps:\n      - run: echo hi\n"
	write(t, root, lintWorkflow, body)

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("no job in the cycle is conditional, so there is nothing to report: %v", err)
	}
}

// THE REFACTOR THIS REPO ASKS FOR WOULD HAVE HIDDEN IT. `workflow-inline-bash`
// is a ratchet at zero headroom, so shell moves into template-scripts — and the
// `kind create` refusal read only inline `run:` bodies.
func TestShellCreateInAScriptIsRefused(t *testing.T) {
	root := tree(t)
	write(t, root, "template-scripts/ci/spin-kind.sh", "#!/usr/bin/env bash\nset -euo pipefail\nkind create cluster --name x\n")

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a cluster stood up from a script declares no node image either")
	}
	if !strings.Contains(errOut, "spin-kind.sh") {
		t.Errorf("the failure must name the script, got: %v", errOut)
	}
}

// A commented-out one in a script is not a cluster, same as inline.
func TestCommentedShellCreateInAScriptIsFine(t *testing.T) {
	root := tree(t)
	write(t, root, "template-scripts/ci/spin-kind.sh", "#!/usr/bin/env bash\n# kind create cluster --name x\necho nope\n")

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("a commented-out create is not a create, got: %v", err)
	}
}

// `uses:` NEEDS NO REF. `helm/kind-action` with nothing after it is a legal step
// and was a second cluster the "one in the repo" rule never saw.
func TestUsesWithNoRefIsStillTheAction(t *testing.T) {
	root := tree(t)
	write(t, root, workflowsDir+"/e2e.yml",
		"name: E2E\njobs:\n  spin:\n    steps:\n      - uses: helm/kind-action\n        with:\n          version: v0.32.0\n")

	if _, _, err := run(t, root); err == nil {
		t.Fatal("an unpinned `uses:` is the same action and the same second cluster")
	}
}

func TestUsesActionMatching(t *testing.T) {
	for _, tc := range []struct {
		uses string
		want bool
	}{
		{"helm/kind-action@ef37e7f", true},
		{"helm/kind-action", true},
		{"Helm/Kind-Action@v1.14.0", true},
		{"  helm/kind-action@v1  ", true},
		{"helm/kind-action-fork@v1", false},
		{"other/kind-action@v1", false},
		{"", false},
	} {
		if got := usesAction(tc.uses, kindActionPrefix); got != tc.want {
			t.Errorf("usesAction(%q) = %v, want %v", tc.uses, got, tc.want)
		}
	}
}

// ── round 14: two ways the guard answered about the wrong text ─────────────────

// ...but `set +e` is body-wide, because its effect is.
func TestSetPlusEIsJudgedAgainstTheWholeStep(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	loose := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" +
		"          set +e\n          kubectl apply --dry-run=server -f rendered/\n"
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, loose, 1))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("`set +e` disables everything after it, the apply included")
	}
	if !strings.Contains(errOut, "set +e") {
		t.Errorf("the failure must name what disabled it, got %q", errOut)
	}
}

// A REFUSAL THAT FAILS OPEN IS WORSE THAN NO REFUSAL. The word-boundary comment
// rule over-strips a `#` inside a quoted string — harmless for a must-CONTAIN
// search, and cover for a second cluster in a must-NOT-contain one.
func TestQuotedHashDoesNotHideAShellCreate(t *testing.T) {
	root := tree(t)
	// A BLOCK SCALAR, because a plain one would not survive YAML: ` #` starts a
	// comment there too, so `run: echo "phase #2" && kind create …` is a workflow
	// that runs neither half. Under `run: |` the whole line reaches the shell, and
	// the quoted `#` is the guard's problem rather than the parser's.
	write(t, root, workflowsDir+"/e2e.yml",
		"name: E2E\njobs:\n  spin:\n    steps:\n      - run: |\n          echo \"phase #2\" && kind create cluster --name extra\n")

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a quoted # must not buy cover for a second cluster")
	}
}

func TestQuotedHashDoesNotHideAShellCreateInAScript(t *testing.T) {
	root := tree(t)
	write(t, root, "template-scripts/ci/spin.sh", "#!/usr/bin/env bash\necho \"phase #2\" && kind create cluster --name extra\n")

	if _, _, err := run(t, root); err == nil {
		t.Fatal("same rule in the script tree")
	}
}

// ONE STRIPPER, BOTH DIRECTIONS. A quoted `#` must not hide a second cluster, and
// a real trailing comment must not refuse the tree — the two spellings that used
// to need two functions with opposite biases.
func TestCommentStrippingHonoursQuotes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`echo "phase #2" && kind create cluster`, `echo "phase #2" && kind create cluster`},
		{`helm repo update  # never kind create here`, `helm repo update  `},
		{`kubectl version "${KIND_NODE_IMAGE#kindest/node:v}"`, `kubectl version "${KIND_NODE_IMAGE#kindest/node:v}"`},
		{`# whole line`, ``},
		{`  # indented whole line`, `  `},
		{`echo 'a # b' # real`, `echo 'a # b' `},
	} {
		if got := uncommented(tc.in); got != tc.want {
			t.Errorf("uncommented(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── round 15: the trees it did not read, and the condition it misread ──────────

// `always()` makes a step run MORE, not less. Refusing every `if:` told the
// reader to remove the thing keeping their check unconditional — a false
// accusation, which this file's own doctrine calls worse than a miss.
func TestUnconditionalIfExpressionsAreAccepted(t *testing.T) {
	// `!cancelled()` is absent unwrapped on purpose: a leading `!` is a YAML tag
	// indicator, so a real workflow can only spell that one inside `${{ }}`.
	for _, expr := range []string{"always()", "${{ always() }}", "${{ !cancelled() }}", "success() || failure()"} {
		t.Run(expr, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body,
				"        run: kubectl apply --dry-run=server",
				"        if: "+expr+"\n        run: kubectl apply --dry-run=server", 1))

			if _, _, err := run(t, root); err != nil {
				t.Fatalf("`if: %s` guarantees the step runs, got: %v", expr, err)
			}
		})
	}
}

// ...and a condition that does not guarantee it is still refused.
func TestOrdinaryIfExpressionsAreStillRefused(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body,
		"        run: kubectl apply --dry-run=server",
		"        if: ${{ always() && github.ref == 'refs/heads/main' }}\n        run: kubectl apply --dry-run=server", 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("an expression outside the exact-spelling list is one this guard cannot decide")
	}
}

// A `kind create` in a script BESIDE the workflows was outside the scan, and
// `.github` is where a workflow's shell most naturally moves.
func TestShellCreateInADotGithubScriptIsRefused(t *testing.T) {
	root := tree(t)
	write(t, root, ".github/scripts/spin.sh", "#!/usr/bin/env bash\nkind create cluster --name extra\n")

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a script under .github stands up just as real a cluster")
	}
	if !strings.Contains(errOut, ".github/scripts/spin.sh") {
		t.Errorf("the failure must name the script, got: %v", errOut)
	}
}

// THE CLAIM IS "ONE SUCH STEP IN THE REPO", and instance-template's workflows are
// in the repo. Scanning only this repo's own .github left nineteen of them
// outside a rule stated over the whole tree.
func TestAKindStepInTheScaffoldIsCaught(t *testing.T) {
	root := tree(t)
	write(t, root, "instance-template/.github/workflows/e2e.yml",
		"name: E2E\njobs:\n  spin:\n    steps:\n      - uses: helm/kind-action@ef37e7f\n        with:\n          version: v0.32.0\n")

	_, _, err := run(t, root)
	if err == nil {
		t.Fatal("the scaffold is part of the repo the one-cluster rule is stated over")
	}
	if !strings.Contains(err.Error(), "instance-template") {
		t.Errorf("the failure must name the other site, got: %v", err)
	}
}

// A tree with no scaffold at all is an ordinary instance checkout, not a gap.
func TestNoScaffoldIsFine(t *testing.T) {
	if _, _, err := run(t, tree(t)); err != nil {
		t.Fatalf("no instance-template is a legitimate layout, got: %v", err)
	}
}

// ── round 16: the last door, and the stripper pointed the wrong way ────────────

// A `paths:` FILTER THAT CANNOT REACH THE PIN IS THE GATE'S ABSENCE, dressed as
// its silence. Drop the workflow tree and a PR moving KIND_NODE_IMAGE starts no
// run of the workflow it edits.
func TestUnreachableTriggerIsCaught(t *testing.T) {
	for _, tc := range []struct{ name, paths, want string }{
		{"the workflow itself", "      - 'tools/**'\n      - 'template-scripts/**'\n      - 'instance-template/.github/**'\n", ".github/workflows/lint.yml"},
		{"the authority", "      - '.github/**'\n      - 'template-scripts/**'\n      - 'instance-template/.github/**'\n", authorityFile},
		{"a scanned tree", "      - 'tools/**'\n      - '.github/**'\n      - 'instance-template/.github/**'\n", "template-scripts/ci/one.sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t)
			// A real script, so there is a real path for the filter to miss.
			write(t, root, "template-scripts/ci/one.sh", "#!/usr/bin/env bash\necho hi\n")
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body, trigger,
				"on:\n  pull_request:\n    paths:\n"+tc.paths, 1))

			_, errOut, err := run(t, root)
			if err == nil {
				t.Fatalf("a filter that cannot reach %s makes the gate absent from the PR that needs it", tc.want)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("the failure must name the unreachable file, got %q", errOut)
			}
		})
	}
}

// A trigger with no `paths:` at all runs on everything, and a bare event name is
// that same thing spelled shorter.
func TestUnfilteredTriggersAreFine(t *testing.T) {
	for _, on := range []string{"on: [push, pull_request]\n", "on:\n  pull_request:\n  push:\n", "on:\n  pull_request:\n    branches: [main]\n"} {
		t.Run(on, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body, trigger, on, 1))

			if _, _, err := run(t, root); err != nil {
				t.Fatalf("no `paths:` means every path, got: %v", err)
			}
		})
	}
}

// The trigger check works on REAL paths now, so there is one question and the
// glob answers it exactly in both directions.
func TestMatchesAny(t *testing.T) {
	for _, tc := range []struct {
		want    string
		filters []string
		match   bool
	}{
		{"tools/a/b.go", []string{"tools/**"}, true},
		{"tools/a/b.go", []string{"tools/*"}, false},
		{"tools/x.go", []string{"tools/*"}, true},
		{"tools/a/b.go", []string{"tools/a/b.go"}, true},
		{"tools/a/b.go", []string{"docs/**"}, false},
		{"tools/a/b.go", []string{"!tools/**"}, false},
		{"tools/a/b.go", []string{"docs/**", "tools/**"}, true},
		{"tools/a/b.go", nil, false},
		{".github/workflows/lint.yml", []string{"**.yml"}, true},
		{".github/workflows/lint.yml", []string{"**/lint.yml"}, true},
		{"template-scripts/ci/one.sh", []string{"template-scripts/**"}, true},
		// A leading-`**` pattern for a suffix nothing here has reaches nothing here —
		// the case the directory approximation got wrong, which made lint.yml's own
		// `'**.md'` entry satisfy every target and the whole check inert.
		{"template-scripts/ci/one.sh", []string{"**.md"}, false},
		{"template-scripts/ci/one.sh", []string{"*"}, false},
	} {
		if got := matchesAny(tc.want, tc.filters); got != tc.match {
			t.Errorf("matchesAny(%q, %v) = %v, want %v", tc.want, tc.filters, got, tc.match)
		}
	}
}

// A `#` IN A QUOTED STRING ERASED THE SWALLOW. swallowReason is a
// must-NOT-contain search and was stripping with the word-boundary rule, so
// `echo "phase #2"; set +e` before the apply passed with the gate printing OK.
func TestQuotedHashDoesNotHideASwallow(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	hidden := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" +
		"          echo \"phase #2\"; set +e\n" +
		"          kubectl apply --dry-run=server -f rendered/\n"
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, hidden, 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a quoted # must not buy cover for a swallow either")
	}
}

// `on:` comes in three shapes and the guard has to read all of them; the two
// filterless ones are also how a workflow says "every path".
func TestOnSpecShapes(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		want      map[string][]string
	}{
		{"scalar", "on: push\n", map[string][]string{"push": nil}},
		{"sequence", "on: [push, pull_request]\n", map[string][]string{"push": nil, "pull_request": nil}},
		{"mapping", "on:\n  pull_request:\n    paths: ['a/**']\n  push:\n", map[string][]string{"pull_request": {"a/**"}, "push": nil}},
		{"null", "on:\n", map[string][]string{}},
		{"filterless event", "on:\n  push:\n", map[string][]string{"push": nil}},
		// A SEQUENCE-VALUED EVENT, and build-images.yml has one. A plain struct
		// decode fails on it, which refused the whole tree until this shape was
		// handled — the case that proves the unmarshaler is not dead code.
		{"schedule", "on:\n  schedule:\n    - cron: '0 3 * * 1'\n  push:\n    paths: ['a/**']\n",
			map[string][]string{"schedule": nil, "push": {"a/**"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wf workflow
			if err := yaml.Unmarshal([]byte(tc.doc), &wf); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(wf.On) != len(tc.want) {
				t.Fatalf("got %d events, want %d (%#v)", len(wf.On), len(tc.want), wf.On)
			}
			for event, paths := range tc.want {
				got, ok := wf.On[event]
				if !ok {
					t.Fatalf("event %q missing from %#v", event, wf.On)
				}
				if strings.Join(got.Paths, ",") != strings.Join(paths, ",") {
					t.Errorf("event %q paths = %v, want %v", event, got.Paths, paths)
				}
			}
		})
	}
}

// A `needs:` written as a bare scalar is the same thing as a one-element list.
// A workflow this guard cannot parse is one it cannot vouch for — in every shape
// `on:` can take.
func TestOnSpecRefusesShapesItCannotRead(t *testing.T) {
	// The mapping form is the only one that can fail: yaml coerces any scalar into
	// a string target, so `on: 5` and `on: [1, 2]` parse to nonsense event names
	// rather than errors — and a nonsense event carries no `paths:`, which is all
	// this guard reads.
	for _, doc := range []string{"on:\n  pull_request:\n    paths: {a: b}\n"} {
		var wf workflow
		if err := yaml.Unmarshal([]byte(doc), &wf); err == nil {
			t.Errorf("%q parsed to %#v; it is not a trigger this guard can read", doc, wf.On)
		}
	}
}

func TestStringListShapes(t *testing.T) {
	for _, tc := range []struct {
		doc  string
		want string
	}{
		{"needs: build\n", "build"},
		{"needs: [build, test]\n", "build,test"},
		{"needs:\n", ""},
	} {
		var v struct {
			Needs stringList `yaml:"needs"`
		}
		if err := yaml.Unmarshal([]byte(tc.doc), &v); err != nil {
			t.Fatalf("parse %q: %v", tc.doc, err)
		}
		if got := strings.Join(v.Needs, ","); got != tc.want {
			t.Errorf("%q -> %q, want %q", tc.doc, got, tc.want)
		}
	}
}

// ── round 17: four ways a rule read past the thing it was about ────────────────

// THE SWALLOW LIVED ON THE OTHER HALF OF THE COMMAND. A `\`-continued apply puts
// the signature on one physical line and the `|| true` on the next, and lint.yml
// formats most multi-command steps that way.
func TestContinuedLineSwallowIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	continued := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" +
		"          kubectl apply --dry-run=server \\\n            -f rendered/ || true\n"
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, continued, 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("one command split over two lines is still one command")
	}
}

// ...and a continuation with no swallow is just a long command.
func TestContinuedLineWithoutSwallowPasses(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	continued := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" +
		"          kubectl apply --dry-run=server \\\n            -f rendered/\n"
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, continued, 1))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("a wrapped command still fails the job, got: %v", err)
	}
}

// `set +eo pipefail` mirrors the `set -eo pipefail` these workflows open with,
// and `set +ex` is the debugging spelling. Both disable exit-on-error.
func TestSetPlusEFlagGroupsAreCaught(t *testing.T) {
	for _, spelling := range []string{"set +e", "set +ex", "set +eo pipefail", "set +xe"} {
		t.Run(spelling, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			loose := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" +
				"          " + spelling + "\n          kubectl apply --dry-run=server -f rendered/\n"
			write(t, root, lintWorkflow, strings.Replace(body, applyStep, loose, 1))

			if _, _, err := run(t, root); err == nil {
				t.Fatalf("`%s` disables exit-on-error for everything after it", spelling)
			}
		})
	}
}

// ...but a `set -e` family, or an unrelated `+x`, is not a swallow.
func TestSetWithoutEIsNotASwallow(t *testing.T) {
	for _, spelling := range []string{"set -euo pipefail", "set +x"} {
		t.Run(spelling, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			fine := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" +
				"          " + spelling + "\n          kubectl apply --dry-run=server -f rendered/\n"
			write(t, root, lintWorkflow, strings.Replace(body, applyStep, fine, 1))

			if _, _, err := run(t, root); err != nil {
				t.Fatalf("`%s` leaves exit-on-error alone, got: %v", spelling, err)
			}
		})
	}
}

// `paths-ignore:` IS THE SAME DOOR FROM THE OTHER SIDE. The two keys are mutually
// exclusive, so swapping to it made the event look unfiltered and the check silent.
func TestPathsIgnoreIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, trigger,
		"on:\n  pull_request:\n    paths-ignore:\n      - 'tools/**'\n", 1))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("ignoring the authority's tree is the same as not matching it")
	}
	if !strings.Contains(errOut, "paths-ignore") {
		t.Errorf("the failure must name the key it read, got %q", errOut)
	}
}

// A `paths-ignore:` that ignores something else entirely is fine.
func TestUnrelatedPathsIgnoreIsFine(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, trigger,
		"on:\n  pull_request:\n    paths-ignore:\n      - 'docs/**'\n", 1))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("ignoring docs leaves every file this gate reads reachable, got: %v", err)
	}
}

// ── round 18: a mention is not an invocation ───────────────────────────────────

// PROSE SATISFIED THE SIGNATURE. `echo 'we used to run kubectl apply
// --dry-run=server here'` certified a job that applies nothing — the exact
// failure the signature exists to prevent, one character-class over from the
// commented-out spelling.
func TestQuotedMentionDoesNotSatisfyASignature(t *testing.T) {
	for _, tc := range []struct{ name, find, replace string }{
		{"the apply", applyStep,
			"      - name: Server-side dry-run of the rendered charts\n        run: echo 'we used to run kubectl apply --dry-run=server here'\n"},
		{"the live half", liveStep,
			"      - name: The cluster must run the version its tag names\n        run: echo 'kubectl version once checked ${KIND_NODE_IMAGE}'\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body, tc.find, tc.replace, 1))

			if _, _, err := run(t, root); err == nil {
				t.Fatalf("a quoted mention of %s is not %s", tc.name, tc.name)
			}
		})
	}
}

// ...and the real live check quotes its own arguments, which must not read as
// prose. This is the fixture the whole suite runs on, asserted directly.
func TestTheRealLiveCheckSurvivesQuoteStripping(t *testing.T) {
	if _, _, err := run(t, tree(t)); err != nil {
		t.Fatalf("the live step quotes its jq program and its env expansion, got: %v", err)
	}
}

func TestUnquoted(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`kubectl apply --dry-run=server -f rendered/`, `kubectl apply --dry-run=server -f rendered/`},
		{`echo 'kubectl apply --dry-run=server'`, `echo `},
		{`echo "kubectl apply --dry-run=server"`, `echo `},
		{`kubectl version -o json | jq -e --arg t "${X#y}" '.a'`, `kubectl version -o json | jq -e --arg t  `},
		{`unterminated 'quote`, `unterminated `},
	} {
		if got := unquoted(tc.in); got != tc.want {
			t.Errorf("unquoted(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// `paths: ['**']` SELECTS EVERY FILE. Reading it as matching nothing hard-failed
// the gate prescribing a fix already in force — the loudest kind of false
// accusation.
func TestCatchAllPathFiltersMatchEverything(t *testing.T) {
	// `*` is deliberately absent: it does not cross a `/`, so it reaches root-level
	// files only. TestSingleStarIsNotACatchAll is that half.
	for _, filter := range []string{"**", "**/*"} {
		t.Run(filter, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body, trigger,
				"on:\n  pull_request:\n    paths:\n      - '"+filter+"'\n", 1))

			if _, _, err := run(t, root); err != nil {
				t.Fatalf("`paths: ['%s']` reaches everything, got: %v", filter, err)
			}
		})
	}
}

// ...and as a `paths-ignore:` the same catch-all excludes everything.
func TestCatchAllPathsIgnoreIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, trigger,
		"on:\n  pull_request:\n    paths-ignore:\n      - '**'\n", 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("ignoring everything starts no run at all")
	}
}

// `set +o errexit` is `set +e` spelled out.
func TestSetPlusOErrexitIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	loose := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" +
		"          set +o errexit\n          kubectl apply --dry-run=server -f rendered/\n"
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, loose, 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("`set +o errexit` disables exit-on-error like the short form")
	}
}

// `paths: ['*']` REACHES ROOT-LEVEL FILES AND NOTHING ELSE. Reading it as a
// catch-all was the same mistake as reading `**` as nothing, in the fail-open
// direction — and this repo already carries that scar with `'**.md'`.
func TestSingleStarIsNotACatchAll(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, trigger,
		"on:\n  pull_request:\n    paths:\n      - '*'\n", 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("`*` does not cross a slash, so it reaches none of the trees this gate reads")
	}
}

// A workflow with no `pull_request` or `push` trigger runs only when someone
// remembers to press it — the seventh door, and no `paths:` filter involved.
func TestNoContributorTriggerIsCaught(t *testing.T) {
	for _, on := range []string{"on:\n  workflow_dispatch:\n", "on:\n  workflow_call:\n", "on:\n  schedule:\n    - cron: '0 3 * * 1'\n"} {
		t.Run(on, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body, trigger, on, 1))

			_, errOut, err := run(t, root)
			if err == nil {
				t.Fatal("nothing a contributor does would start the dry-run")
			}
			if !strings.Contains(errOut, "pull_request") {
				t.Errorf("the failure must name what is missing, got %q", errOut)
			}
		})
	}
}

// `kubectl version --client` NEVER CONTACTS THE SERVER. The step would be
// present, gating, and comparing the client against itself — while the digest
// rule's whole safety argument is that this one asks the cluster.
func TestClientOnlyLiveCheckIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	clientOnly := "      - name: The cluster must run the version its tag names\n" +
		"        run: kubectl version --client -o json | jq -e --arg t \"${KIND_NODE_IMAGE#kindest/node:v}\" '.clientVersion.gitVersion == \"v\" + ($t | split(\"@\")[0])'\n"
	write(t, root, lintWorkflow, strings.Replace(body, liveStep, clientOnly, 1))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a client-only version check asks the cluster nothing")
	}
	if !strings.Contains(errOut, "--client") {
		t.Errorf("the failure must name the flag, got %q", errOut)
	}
}

// THE PACKING SCALE LEAKED INTO THE MESSAGE. The comparison packs major and
// minor into `major*1000 + minor`, and the failure printed that as minors — so
// kubectl v2.0.0 against a 1.34 server read "966 minors from".
func TestSkewDistanceReadsInRealUnits(t *testing.T) {
	for _, tc := range []struct{ kubectl, want string }{
		{"2.0.0", "a different major from"},
		{"1.37.0", "3 minors from"},
	} {
		t.Run(tc.kubectl, func(t *testing.T) {
			root := tree(t)
			write(t, root, lintWorkflow, lintBody("v0.32.0", img("v1.34.8"), tc.kubectl,
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}"))

			_, errOut, err := run(t, root)
			if err == nil {
				t.Fatalf("kubectl %s is outside the supported skew", tc.kubectl)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("want %q in the diagnosis, got %q", tc.want, errOut)
			}
		})
	}
}

func TestDistance(t *testing.T) {
	for _, tc := range []struct {
		a, b minor
		want string
	}{
		{minor{1, 34}, minor{1, 35}, "1 minor from"},
		{minor{1, 37}, minor{1, 34}, "3 minors from"},
		{minor{2, 0}, minor{1, 34}, "a different major from"},
	} {
		if got := distance(tc.a, tc.b); got != tc.want {
			t.Errorf("distance(%v, %v) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

// ── round 20: the ignore side, and a comment that refused the tree ─────────────

// `paths-ignore:` NEEDED AN ACCURATE MATCHER, NOT A BIASED ONE. A narrow prefix
// heuristic fails closed for `paths:` and OPEN here — so `**.yml`, which excludes
// lint.yml from starting on a change to itself, slid straight through.
func TestSuffixGlobPathsIgnoreIsCaught(t *testing.T) {
	for _, filter := range []string{"**.yml", "**/lint.yml", ".github/workflows/*.yml"} {
		t.Run(filter, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body, trigger,
				"on:\n  pull_request:\n    paths-ignore:\n      - '"+filter+"'\n", 1))

			_, errOut, err := run(t, root)
			if err == nil {
				t.Fatalf("`paths-ignore: ['%s']` stops the workflow starting on a change to itself", filter)
			}
			if !strings.Contains(errOut, "paths-ignore") {
				t.Errorf("the failure must name the key, got %q", errOut)
			}
		})
	}
}

// A PROSE MENTION IN A TRAILING COMMENT REFUSED THE WHOLE TREE. The scan roots are
// this repo's most heavily commented shell, so the false accusation was likely
// rather than contrived.
func TestCommentedKindCreateInProseIsNotACluster(t *testing.T) {
	root := tree(t)
	write(t, root, "template-scripts/ci/helm.sh",
		"#!/usr/bin/env bash\nhelm repo update   # we never kind create here; LKE only\n")

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("a comment is not a cluster, got: %v", err)
	}
}

// ...and the quoted-hash case still does not buy cover, which is the other half
// the single stripper has to serve.
func TestQuotedHashStillDoesNotHideAShellCreate(t *testing.T) {
	root := tree(t)
	write(t, root, "template-scripts/ci/spin.sh", "#!/usr/bin/env bash\necho \"phase #2\" && kind create cluster --name extra\n")

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a quoted # must not hide a second cluster")
	}
}

// ...including the one-liner spelling, where the `set +e` precedes the command on
// the same line.
func TestSetPlusEBeforeTheCommandOnOneLineIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	oneLine := "      - name: Server-side dry-run of the rendered charts\n" +
		"        run: set +e; kubectl apply --dry-run=server -f rendered/\n"
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, oneLine, 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("`set +e; kubectl …` disables the command that follows it")
	}
}

func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"**", "a/b/c.go", true},
		{"**/*", "a/b/c.go", true},
		{"*", "a.go", true},
		{"*", "a/b.go", false},
		{"a/*", "a/b.go", true},
		{"a/*", "a/b/c.go", false},
		{"a/**", "a/b/c.go", true},
		{"**.yml", ".github/workflows/lint.yml", true},
		{"**/lint.yml", ".github/workflows/lint.yml", true},
		// `?` never reaches globMatch — unmodelled() refuses it upstream, because
		// GitHub's `?` is a quantifier on the preceding character, not a wildcard.
		{"tools/**", "tools", false},
		{"exact/path.go", "exact/path.go", true},
		{"exact/path.go", "exact/other.go", false},
		// A trailing star may match nothing at all.
		{"a*", "a", true},
		{"a/**", "a/", true},
		// A single star that runs out of name without matching the rest.
		{"a*c", "ab", false},
		{"a*c", "abc", true},
	} {
		if got := globMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

// ── round 21: two readings of "where is the command", and a latch ──────────────

// A QUOTED MENTION ON AN EARLIER LINE ENDED THE SCAN. dryRunHalf finds its step
// through unquoted() and swallowReason located the command with a raw substring,
// so the mention was taken for the command and the real one's `|| true` passed.
func TestQuotedMentionDoesNotShortCircuitTheSwallowScan(t *testing.T) {
	for _, tail := range []string{"|| true", ""} {
		name := "swallowed"
		if tail == "" {
			name = "clean"
		}
		t.Run(name, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			step := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" +
				"          echo \"Server-side dry-run (--dry-run=server) of the rendered charts\"\n" +
				"          kubectl apply --dry-run=server -f rendered/ " + tail + "\n"
			write(t, root, lintWorkflow, strings.Replace(body, applyStep, step, 1))

			_, _, err := run(t, root)
			if tail != "" && err == nil {
				t.Fatal("the mention is not the command; the command still swallows")
			}
			if tail == "" && err != nil {
				t.Fatalf("a mention beside a clean apply is not a problem: %v", err)
			}
		})
	}
}

// `install_only` was the only kind-action input read raw from `with:`, so an
// `${{ env.X }}` holding "false" was truthy and hard-failed a step that does
// create the cluster.
func TestInstallOnlyIsResolved(t *testing.T) {
	root := tree(t)
	body := "name: Lint\n" + trigger + shellDefault + "env:\n  KIND_NODE_IMAGE: " + img("v1.34.8") +
		"\n  KUBECTL_VERSION: \"1.34.10\"\n  INSTALL_ONLY: \"false\"\n" +
		"jobs:\n  dry-run:\n    steps:\n" +
		"      - uses: helm/kind-action@ef37e7f\n        with:\n" +
		"          version: v0.32.0\n" +
		"          node_image: ${{ env.KIND_NODE_IMAGE }}\n" +
		"          kubectl_version: v${{ env.KUBECTL_VERSION }}\n" +
		"          install_only: ${{ env.INSTALL_ONLY }}\n" + liveStep + applyStep
	write(t, root, lintWorkflow, body)

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("`install_only` resolving to false creates the cluster, got: %v", err)
	}
}

func TestInstallOnlyReferenceMustResolve(t *testing.T) {
	root := tree(t)
	write(t, root, lintWorkflow, lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}",
		"install_only: ${{ env.NOT_DEFINED }}"))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("an install_only this guard cannot resolve leaves it unexamined")
	}
}

// A suffix glob DOES reach a directory, which the literal-segment probe denied —
// and in the ignore direction that silence was the fail-open.
func TestSuffixGlobReachesAScannedTree(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, trigger,
		"on:\n  pull_request:\n    paths:\n      - '**'\n", 1))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("`**` reaches every tree this gate reads, got: %v", err)
	}
}

// ── round 22: the check that was inert as shipped ──────────────────────────────

// THE TRIGGER CHECK DECIDED NOTHING IN THE CONFIGURATION IT SHIPPED WITH. It
// named the scan TREES and guessed whether a filter could reach inside one; the
// guess counted any leading-`**` pattern as reaching everything, so lint.yml's
// own `'**.md'` entry satisfied every target. It works on the paths the guard
// actually read now, where a glob is exact.
func TestTriggerCheckUsesRealPaths(t *testing.T) {
	root := tree(t)
	write(t, root, "template-scripts/ci/one.sh", "#!/usr/bin/env bash\necho hi\n")
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	// The old approximation passed on exactly this: `**.md` reaches no file here.
	write(t, root, lintWorkflow, strings.Replace(body, trigger,
		"on:\n  pull_request:\n    paths:\n      - 'tools/**'\n      - '.github/**'\n      - '**.md'\n", 1))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("`**.md` reaches no script in this tree and must not stand in for one")
	}
	if !strings.Contains(errOut, "one.sh") {
		t.Errorf("the failure must name a real unreachable path, got %q", errOut)
	}
}

// One line per filter, not per file: a filter that misses a tree misses every
// file in it, and forty sentences bury the fact.
func TestTriggerGapIsReportedOncePerFilter(t *testing.T) {
	root := tree(t)
	for _, n := range []string{"one", "two", "three"} {
		write(t, root, "template-scripts/ci/"+n+".sh", "#!/usr/bin/env bash\necho hi\n")
	}
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, trigger,
		"on:\n  pull_request:\n    paths:\n      - 'tools/**'\n      - '.github/**'\n", 1))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("three unreachable scripts is still a gap")
	}
	if !strings.Contains(errOut, "1 problem(s)") || !strings.Contains(errOut, "and 2 more") {
		t.Errorf("want one line naming a path and a count, got %q", errOut)
	}
}

// ONE UNBALANCED QUOTE USED TO SWALLOW EVERY LATER LINE, so a valid workflow was
// reported as running no apply at all. Quote state does not cross a newline.
func TestUnbalancedQuoteDoesNotHideLaterLines(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	ragged := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" +
		"          echo \"a \\\" b\"\n          kubectl apply --dry-run=server -f rendered/\n"
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, ragged, 1))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("the apply is on its own line and must still be seen: %v", err)
	}
}

// `if: success()` is the DEFAULT written out and `if: true` always runs; refusing
// either told the reader their no-op was a condition.
func TestNoOpIfExpressionsAreAccepted(t *testing.T) {
	for _, expr := range []string{"success()", "${{ success() }}", "true"} {
		t.Run(expr, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body,
				"        run: kubectl apply --dry-run=server",
				"        if: "+expr+"\n        run: kubectl apply --dry-run=server", 1))

			if _, _, err := run(t, root); err != nil {
				t.Fatalf("`if: %s` changes nothing about whether the step runs, got: %v", expr, err)
			}
		})
	}
}

// ── round 23: the constructs it declined to read ───────────────────────────────

// A NEGATION SUBTRACTS, so ignoring one can only make the guard believe a path is
// reachable when it is not — and `!.github/workflows/**` beside the existing
// entries stops a PR editing KIND_NODE_IMAGE starting Lint at all.
func TestNegatedPathFilterIsRefused(t *testing.T) {
	for _, key := range []string{"paths", "paths-ignore"} {
		t.Run(key, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body, trigger,
				"on:\n  pull_request:\n    "+key+":\n      - 'tools/**'\n      - '.github/**'\n"+
					"      - 'template-scripts/**'\n      - 'instance-template/.github/**'\n"+
					"      - '!.github/workflows/**'\n", 1))

			_, errOut, err := run(t, root)
			if err == nil {
				t.Fatal("a negation decides the answer and this guard does not model order")
			}
			if !strings.Contains(errOut, "does not model") {
				t.Errorf("the failure must say what it could not read, got %q", errOut)
			}
		})
	}
}

// `jq` WITHOUT `-e` EXITS 0 WHATEVER IT PRINTS, so dropping one flag leaves the
// live half present, gating and completely inert — the same "cannot fail" state
// as continue-on-error, one character wide, under the step the digest rule needs.
func TestLiveHalfWithoutJQExitStatusIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, "jq -e --arg t", "jq --arg t", 1))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a jq with no -e reports nothing to the shell")
	}
	if !strings.Contains(errOut, "-e") {
		t.Errorf("the failure must name the missing flag, got %q", errOut)
	}
}

// ...and the long and bundled spellings are the same flag.
func TestJQExitStatusSpellings(t *testing.T) {
	for _, spelling := range []string{"jq -e --arg t", "jq --exit-status --arg t", "jq -re --arg t"} {
		t.Run(spelling, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body, "jq -e --arg t", spelling, 1))

			if _, _, err := run(t, root); err != nil {
				t.Fatalf("`%s` does report the comparison, got: %v", spelling, err)
			}
		})
	}
}

// A live check written without jq at all is not refused — that is the bounded
// limit the signature has always had, and it is stated rather than pretended away.
func TestLiveHalfWithoutJQIsNotRefused(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	// `kubectl version` unquoted, because a command that appears only inside quotes
	// reads as absent — that is the fail-closed half of the same rule.
	plain := "      - name: The cluster must run the version its tag names\n" +
		"        run: kubectl version -o json | grep -q \"${KIND_NODE_IMAGE#kindest/node:v}\"\n"
	write(t, root, lintWorkflow, strings.Replace(body, liveStep, plain, 1))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("a non-jq comparison is outside what this can judge, got: %v", err)
	}
}

// ── round 24: three constructs that are not what they look like ────────────────

// GitHub's `?` IS A QUANTIFIER ON THE PRECEDING CHARACTER, not a single-character
// wildcard, and `[]` is a class. Guessing them was wrong in both directions:
// `.github/workflow?/**` read as reaching lint.yml when GitHub would never start
// the workflow, and `[t]ools/**` read as reaching nothing.
func TestUnmodelledGlobSyntaxIsRefused(t *testing.T) {
	for _, filter := range []string{".github/workflow?/**", "[t]ools/**", "tools/+(a|b)/**"} {
		t.Run(filter, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body, trigger,
				"on:\n  pull_request:\n    paths:\n      - 'tools/**'\n      - '.github/**'\n"+
					"      - 'template-scripts/**'\n      - 'instance-template/.github/**'\n"+
					"      - '"+filter+"'\n", 1))

			_, errOut, err := run(t, root)
			if err == nil {
				t.Fatalf("`%s` is syntax this guard cannot decide", filter)
			}
			if !strings.Contains(errOut, "does not model") {
				t.Errorf("the failure must say what it could not read, got %q", errOut)
			}
		})
	}
}

// AN EXPLICIT `paths: []` STARTS ON NOTHING, and reading it as "no filter" made
// it indistinguishable from an unfiltered event.
func TestEmptyPathsListIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, trigger,
		"on:\n  pull_request:\n    paths: []\n", 1))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("an empty paths list starts on no path at all")
	}
	if !strings.Contains(errOut, "is empty") {
		t.Errorf("the failure must say the list is empty, got %q", errOut)
	}
}

// A MENTION IS NOT AN INVOCATION — and this refusal was the last raw substring
// search, so `echo "never use kind create here"` refused the whole tree. AGENTS.md
// now names the phrase, which makes that likely rather than contrived.
func TestKindCreateInProseIsNotACluster(t *testing.T) {
	for _, line := range []string{
		`          echo "never use kind create here"`,
		`          echo 'we use kind create nowhere'`,
	} {
		t.Run(line, func(t *testing.T) {
			root := tree(t)
			write(t, root, workflowsDir+"/e2e.yml", "name: E2E\njobs:\n  x:\n    steps:\n      - run: |\n"+line+"\n")

			if _, _, err := run(t, root); err != nil {
				t.Fatalf("a mention is not a cluster, got: %v", err)
			}
		})
	}
}

// ...and every real command position still is one.
func TestKindCreateInCommandPositionIsACluster(t *testing.T) {
	for _, line := range []string{
		`          kind create cluster --name x`,
		`          docker info; kind create cluster --name x`,
		`          docker info && kind create cluster --name x`,
		`          (kind create cluster --name x)`,
	} {
		t.Run(line, func(t *testing.T) {
			root := tree(t)
			write(t, root, workflowsDir+"/e2e.yml", "name: E2E\njobs:\n  x:\n    steps:\n      - run: |\n"+line+"\n")

			if _, _, err := run(t, root); err == nil {
				t.Fatalf("`%s` stands up a second cluster", strings.TrimSpace(line))
			}
		})
	}
}

// ── round 25: two heuristics that over-reached ─────────────────────────────────

// A REFUSAL IS A FINDING, NOT AN ABORT. Returning early meant one refusal hid
// every other check behind it, so a reader saw a sentence about a script and
// nothing about the pins — and a FALSE refusal costs exactly the same.
func TestShellClusterIsReportedBesideTheOtherProblems(t *testing.T) {
	root := tree(t)
	write(t, root, "template-scripts/ci/spin.sh", "#!/usr/bin/env bash\nkind create cluster --name x\n")
	// ...and break the pin too, so there is something else to hear about.
	write(t, root, lintWorkflow, lintBody("v0.25.0", img("v1.31.2"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}"))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("both the script and the drifted pin are problems")
	}
	for _, want := range []string{"spin.sh", "we deploy 1.34"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the report must carry %q, got %q", want, errOut)
		}
	}
}

// ── round 26: `||` is not a pipe, and kind has more than one name ──────────────

// KIND HAS MORE THAN ONE NAME. A bare-`kind`-only match let every ordinary way of
// reaching the binary past the rule.
func TestShellCreateReachedAnyWay(t *testing.T) {
	for _, line := range []string{
		`          sudo kind create cluster --name x`,
		`          ./kind create cluster --name x`,
		`          /usr/local/bin/kind create cluster --name x`,
		`          $KIND create cluster --name x`,
		`          ${KIND} create cluster --name x`,
	} {
		t.Run(strings.TrimSpace(line), func(t *testing.T) {
			root := tree(t)
			write(t, root, workflowsDir+"/e2e.yml", "name: E2E\njobs:\n  x:\n    steps:\n      - run: |\n"+line+"\n")

			if _, _, err := run(t, root); err == nil {
				t.Fatalf("`%s` stands up a cluster this gate cannot read", strings.TrimSpace(line))
			}
		})
	}
}

// ...and prose still is not a command, in any of those shapes.
func TestShellCreateProseStillPasses(t *testing.T) {
	root := tree(t)
	write(t, root, workflowsDir+"/e2e.yml",
		"name: E2E\njobs:\n  x:\n    steps:\n      - run: |\n          echo \"do not sudo kind create anything\"\n")

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("a mention is still not a cluster, got: %v", err)
	}
}

// A MISSING JOB IS REPORTED AGAINST THE JOB THAT NAMES IT. Formatting the message
// with the walk's starting job claimed an edge that does not exist.
func TestMissingDependencyNamesTheJobThatNeedsIt(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	body = strings.Replace(body, "  dry-run:\n    steps:", "  dry-run:\n    needs: [prep]\n    steps:", 1)
	body += "  prep:\n    needs: [ghost]\n    steps:\n      - run: echo hi\n"
	write(t, root, lintWorkflow, body)

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a dependency that does not exist means the job never runs")
	}
	if !strings.Contains(errOut, `"prep" job needs "ghost"`) {
		t.Errorf("the failure must name the job that carries the edge, got %q", errOut)
	}
}

// The epilogue names the workflow the guard FOUND, not a hardcoded lint.yml.
func TestEpilogueNamesTheDiscoveredWorkflow(t *testing.T) {
	root := tree(t)
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(lintWorkflow)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(lintWorkflow))); err != nil {
		t.Fatal(err)
	}
	moved := workflowsDir + "/manifests.yml"
	write(t, root, moved, strings.Replace(string(body), img("v1.34.8"), img("v1.31.2"), 1))

	_, errOut, rerr := run(t, root)
	if rerr == nil {
		t.Fatal("a 1.31 node image against a 1.34 pin must fail")
	}
	if strings.Contains(errOut, "lint.yml's\nKIND_NODE_IMAGE") {
		t.Errorf("the remedy must name %s, got %q", moved, errOut)
	}
}

// ── round 27: command substitution, and one refusal printing two verdicts ──────

// COMMAND SUBSTITUTION IS EXECUTED, so quotes around it hide nothing. Deleting
// `"$(kubectl version -o json)"` made the one-line `test` spelling read as an
// ABSENT live step, and the gate told the author to add a step already there.
func TestCommandSubstitutionIsNotAQuotedString(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	subst := "      - name: The cluster must run the version its tag names\n" +
		"        run: test \"$(kubectl version -o json | jq -er .serverVersion.gitVersion)\" = \"v${KIND_NODE_IMAGE#kindest/node:v}\"\n"
	write(t, root, lintWorkflow, strings.Replace(body, liveStep, subst, 1))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("the live step is there and it runs the command: %v", err)
	}
}

func TestUnquotedKeepsCommandSubstitutions(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`test "$(kubectl version)" = "x"`, `test kubectl version = `},
		{`echo "plain string"`, `echo `},
		{`a "$(b "$(c)")" d`, `a b c d`},
		{`echo "$(unclosed`, `echo unclosed`},
	} {
		if got := unquoted(tc.in); got != tc.want {
			t.Errorf("unquoted(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// `kubectl apply … ; set +e` LEFT THE LATCH UNSET, so a second apply on the next
// line was judged under exit-on-error it no longer had.
func TestSetPlusEAfterOneApplyReachesTheNext(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	two := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" +
		"          kubectl apply --dry-run=server -f rendered/ ; set +e\n" +
		"          kubectl apply --dry-run=server -f overlays/\n"
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, two, 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("the second apply runs with exit-on-error off")
	}
}

// ONE REFUSAL, ONE VERDICT. Feeding an unmodelled filter list to the matcher
// anyway printed the honest "cannot decide" line AND a confident accusation
// beside it — under the very sentence explaining that it cannot decide.
func TestUndecidableFilterDoesNotAlsoAccuse(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, trigger,
		"on:\n  pull_request:\n    paths:\n      - '[t]ools/**'\n", 1))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("`[t]ools/**` is syntax this guard cannot decide")
	}
	if !strings.Contains(errOut, "1 problem(s)") {
		t.Errorf("cannot-decide and a confident accusation must not both print, got %q", errOut)
	}
}

// ── round 28: one quote character, one abort, one file extension ───────────────

// THE SHELL DOES NOT EXPAND `$( … )` INSIDE SINGLE QUOTES, and treating it as a
// command let a mention stand in for the apply — the hole unquoted() exists to
// close, one quote character over.
func TestSingleQuotesSuppressSubstitution(t *testing.T) {
	for _, tc := range []struct{ name, find, replace string }{
		{"the apply", applyStep,
			"      - name: Server-side dry-run of the rendered charts\n        run: echo 'we used to run $(kubectl apply --dry-run=server -f rendered/)'\n"},
		{"the live half", liveStep,
			"      - name: The cluster must run the version its tag names\n        run: echo 'once ran $(kubectl version) against ${KIND_NODE_IMAGE}'\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body, tc.find, tc.replace, 1))

			if _, _, err := run(t, root); err == nil {
				t.Fatalf("a single-quoted %s is a string, not a command", tc.name)
			}
		})
	}
}

func TestUnquotedRespectsSingleQuotes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`echo '$(kubectl version)'`, `echo `},
		{`echo "$(kubectl version)"`, `echo kubectl version`},
		{`test "$(a)" = 'b $(c)'`, `test a = `},
	} {
		if got := unquoted(tc.in); got != tc.want {
			t.Errorf("unquoted(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ONE INLINE `kind create` USED TO BLANK THE WHOLE VERDICT. It aborted before the
// other checks ran, so a tree with a drifted node image AND an inline create
// reported only the create — hiding the thing the gate is for. And since the
// match is a heuristic, one false positive did the same.
func TestInlineShellClusterIsReportedBesideTheRest(t *testing.T) {
	root := tree(t)
	write(t, root, workflowsDir+"/e2e.yml",
		"name: E2E\njobs:\n  x:\n    steps:\n      - run: |\n          kind create cluster --name extra\n")
	write(t, root, lintWorkflow, lintBody("v0.25.0", img("v1.30.0"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}"))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("the extra cluster and the drifted pin are both problems")
	}
	for _, want := range []string{"e2e.yml", "we deploy 1.34", "minors from"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the report must carry %q, got %q", want, errOut)
		}
	}
}

// SHELL IS DECIDED BY CONTENT, because the `.sh` convention is not universal
// here — template-scripts/hooks/pre-commit and pre-push already prove it.
func TestExtensionlessShellScriptIsScanned(t *testing.T) {
	root := tree(t)
	write(t, root, "template-scripts/hooks/pre-commit", "#!/usr/bin/env bash\nkind create cluster --name x\n")

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a hook with no .sh is still shell, and still stands up a cluster")
	}
	if !strings.Contains(errOut, "pre-commit") {
		t.Errorf("the failure must name the file, got %q", errOut)
	}
}

// ...and a file that is not shell is not read as shell.
func TestNonShellFilesAreNotScanned(t *testing.T) {
	root := tree(t)
	write(t, root, "template-scripts/README.md", "Do not `kind create cluster` here.\n")
	write(t, root, "template-scripts/tool.py", "#!/usr/bin/env python3\n# kind create cluster\n")

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("neither file is shell, got: %v", err)
	}
}

func TestIsShell(t *testing.T) {
	for _, tc := range []struct {
		path string
		body string
		want bool
	}{
		{"a/b.sh", "echo hi\n", true},
		{"a/pre-commit", "#!/usr/bin/env bash\n", true},
		{"a/pre-push", "#!/bin/sh\n", true},
		{"a/tool", "#!/usr/bin/env python3\n", false},
		{"a/README.md", "prose\n", false},
		{"a/noshebang", "echo hi\n", false},
	} {
		if got := isShell(tc.path, []byte(tc.body)); got != tc.want {
			t.Errorf("isShell(%q, %q) = %v, want %v", tc.path, tc.body, got, tc.want)
		}
	}
}

// ── round 29: two body-wide searches, and the way the third premise is true ────

// ...and `--client` on the signature's own line is still the real thing.
func TestClientFlagOnTheSignatureLineIsStillCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, "kubectl version -o json", "kubectl version --client -o json", 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a client-only version check asks the cluster nothing")
	}
}

// PIPEFAIL IS THE RULE, and `shell: bash` is the one spelling that gives it.
// Everything else on offer lacks it — the unset default is `bash -e {0}`,
// `shell: sh` is `sh -e {0}`, and a custom template is whatever was written — so
// a pipeline in a load-bearing step discards everything but its last status.
func TestEffectiveShellMustSetPipefail(t *testing.T) {
	for _, tc := range []struct {
		shell string
		ok    bool
	}{
		{"bash", true},
		{"bash --noprofile --norc -eo pipefail {0}", true},
		{"bash -e -o pipefail {0}", true},
		// Each of these was waved through by a rule that only refused the unset case.
		{"sh", false},
		{"bash -e {0}", false},
		{"bash -o errexit {0}", false},
		{"bash {0}", false},
		{"python", false},
	} {
		t.Run(tc.shell, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body, shellDefault,
				"defaults:\n  run:\n    shell: "+tc.shell+"\n", 1))

			_, errOut, err := run(t, root)
			if tc.ok && err != nil {
				t.Fatalf("`shell: %s` sets pipefail: %v", tc.shell, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("`shell: %s` has no pipefail", tc.shell)
				}
				if !strings.Contains(errOut, "pipefail") {
					t.Errorf("the failure must name what is missing, got %q", errOut)
				}
			}
		})
	}
}

func TestShellWithoutExitOnError(t *testing.T) {
	for _, tc := range []struct {
		shell string
		ok    bool
	}{
		{"bash", true},
		{"bash -eo pipefail {0}", true},
		{"", false},
		{"sh", false},
		{"python", false},
		{"bash -e {0}", false},
		{"bash {0}", false},
	} {
		got := shellWithoutExitOnError("x", tc.shell)
		if tc.ok != (got == "") {
			t.Errorf("shellWithoutExitOnError(%q) = %q, want ok=%v", tc.shell, got, tc.ok)
		}
	}
}

// With no kind step there is nothing to report the script cluster BESIDE, so the
// two travel together rather than one being lost.
func TestShellClusterSurvivesAMissingKindStep(t *testing.T) {
	root := tree(t)
	write(t, root, "template-scripts/ci/spin.sh", "#!/usr/bin/env bash\nkind create cluster --name x\n")
	write(t, root, lintWorkflow, "name: Lint\n"+trigger+shellDefault+"jobs:\n  dry-run:\n    steps:\n      - run: echo hi\n")

	_, _, err := run(t, root)
	if err == nil {
		t.Fatal("no kind step at all is a failure")
	}
	for _, want := range []string{"no `uses: " + kindActionPrefix, "spin.sh"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure must carry %q, got %v", want, err)
		}
	}
}

// The YAML walk reads YAML. `.github/workflows/AGENTS.md` is real, and a guard
// that tried to parse it would refuse the tree.
func TestNonYamlFilesInTheWorkflowTreeAreSkipped(t *testing.T) {
	root := tree(t)
	write(t, root, workflowsDir+"/AGENTS.md", "# Conventions\n\nNot a workflow.\n")

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("a Markdown file beside the workflows is not a workflow: %v", err)
	}
}

// ── round 30: three more ways one command spans two readings ───────────────────

// ...and an unscoped one still latches.
func TestUnscopedSetPlusEStillLatches(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	loose := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" +
		"          set +e\n          kubectl apply --dry-run=server -f rendered/\n"
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, loose, 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a top-level `set +e` reaches the apply")
	}
}

// ...but `&&` keeps counting, because `a && b || true` really does swallow a.
func TestSwallowAcrossAndStillCounts(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	chained := "      - name: Server-side dry-run of the rendered charts\n" +
		"        run: kubectl apply --dry-run=server -f rendered/ && echo done || true\n"
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, chained, 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("`a && b || true` exits 0 when a fails")
	}
}

// ── the decidable rule, and what it costs ──────────────────────────────────────
//
// A step this gate depends on may not carry a construct that can discard an exit
// status — anywhere in it. These are the arms of that, including the legitimate
// spellings it refuses, which are written out rather than left to be discovered.

func TestDisablerAnywhereInALoadBearingStepIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, step string }{
		{"|| true on the command", "          kubectl apply --dry-run=server -f rendered/ || true\n"},
		{"|| : on the command", "          kubectl apply --dry-run=server -f rendered/ || :\n"},
		{"set +e before it", "          set +e\n          kubectl apply --dry-run=server -f rendered/\n"},
		{"set +o errexit before it", "          set +o errexit\n          kubectl apply --dry-run=server -f rendered/\n"},
		{"wrapped onto the next line", "          kubectl apply --dry-run=server -f rendered/ ||\n            true\n"},
		// THE COSTS, stated: each of these is a reasonable thing to write, and each
		// is refused because deciding whether it reaches the apply is the analysis
		// that would not converge. The remedy is a separate step, and the failure
		// says so.
		{"a cleanup after it", "          kubectl apply --dry-run=server -f rendered/\n          rm -rf rendered/ || true\n"},
		{"a guard clause before it", "          [ -d rendered ] || true\n          kubectl apply --dry-run=server -f rendered/\n"},
		{"a bracketed set", "          set +e\n          helm repo update\n          set -e\n          kubectl apply --dry-run=server -f rendered/\n"},
		{"a set inside a subshell", "          (set +e; helm repo update)\n          kubectl apply --dry-run=server -f rendered/\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			step := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" + tc.step
			write(t, root, lintWorkflow, strings.Replace(body, applyStep, step, 1))

			_, errOut, err := run(t, root)
			if err == nil {
				t.Fatalf("%s is a construct this gate refuses to reason about", tc.name)
			}
			if !strings.Contains(errOut, "give it its own step") {
				t.Errorf("the failure must carry the remedy, got %q", errOut)
			}
		})
	}
}

// `set -e` and friends ENABLE exit-on-error; only the `+` spellings disable it,
// and `set -euo pipefail` is the standard preamble.
func TestEnablingSetsAreNotDisablers(t *testing.T) {
	for _, spelling := range []string{"set -euo pipefail", "set -e", "set -o errexit", "set +x"} {
		t.Run(spelling, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			step := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" +
				"          " + spelling + "\n          kubectl apply --dry-run=server -f rendered/\n"
			write(t, root, lintWorkflow, strings.Replace(body, applyStep, step, 1))

			if _, _, err := run(t, root); err != nil {
				t.Fatalf("`%s` does not discard anything: %v", spelling, err)
			}
		})
	}
}

// EVERY jq IN THE STEP CARRIES `-e`, for the same reason: which one decides the
// outcome depends on the pipeline around it. `jq -er … | grep -qx` is refused
// without the flag and accepted with it, and adding it costs nothing.
func TestEveryJQNeedsExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name, jq string
		ok       bool
	}{
		{"the comparison itself", `jq --arg t "${KIND_NODE_IMAGE}" '.a == $t'`, false},
		{"with -e", `jq -e --arg t "${KIND_NODE_IMAGE}" '.a == $t'`, true},
		{"with --exit-status", `jq --exit-status --arg t "${KIND_NODE_IMAGE}" '.a == $t'`, true},
		{"feeding grep, unflagged", `jq -r .a | grep -qx "${KIND_NODE_IMAGE}"`, false},
		{"feeding grep, flagged", `jq -er .a | grep -qx "${KIND_NODE_IMAGE}"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			// Both halves of the signature in ONE command list — a `;` between them
			// would be two commands comparing nothing.
			step := "      - name: The cluster must run the version its tag names\n" +
				"        run: kubectl version -o json | " + tc.jq + "\n"
			write(t, root, lintWorkflow, strings.Replace(body, liveStep, step, 1))

			_, _, err := run(t, root)
			if tc.ok && err != nil {
				t.Fatalf("`%s` reports its result: %v", tc.jq, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("`%s` has no -e and this guard does not model the pipeline", tc.jq)
			}
		})
	}
}

func TestFlatten(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// A newline is a command SEPARATOR and becomes one; a continuation is not.
		{"a\nb", "a ; b"},
		{"a \\\nb", "a  b"},
		{"a |\nb", "a | b"},
		{"a &&\nb", "a && b"},
		// flatten no longer uncomments — cleaning is the caller's, in the order the
		// caller needs, which is what stopped `unquotedBody(flatten(…))` reading a
		// body with its newlines already gone.
		{"a # c\nb", "a # c ; b"},
		{uncommented("a # c\nb"), "a ; b"},
		// A body ending mid-continuation still yields its pending half.
		{"a |", "a | "},
		{"a", "a"},
	} {
		if got := flatten(tc.in); got != tc.want {
			t.Errorf("flatten(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── the finite list, completed ─────────────────────────────────────────────────

// `!` INVERTS AN EXIT STATUS AND A CONDITION EXEMPTS ONE. Under `set -e` a
// command in an `if`/`while`/`until` condition is not subject to errexit at all,
// so the check runs and cannot fail on its result.
func TestInvertedAndConditionalCommandsAreRefused(t *testing.T) {
	for _, tc := range []struct{ name, step string }{
		{"a bang", "          ! kubectl apply --dry-run=server -f rendered/\n"},
		{"an if", "          if kubectl apply --dry-run=server -f rendered/; then echo ok; fi\n"},
		{"a negated if", "          if ! kubectl apply --dry-run=server -f rendered/; then echo bad; fi\n"},
		{"an until", "          until kubectl apply --dry-run=server -f rendered/; do sleep 1; done\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			step := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" + tc.step
			write(t, root, lintWorkflow, strings.Replace(body, applyStep, step, 1))

			if _, _, err := run(t, root); err == nil {
				t.Fatalf("%s leaves the apply unable to fail the job", tc.name)
			}
		})
	}
}

// A NEWLINE IS A SEPARATOR: an `-e` belonging to some other command must not
// satisfy a jq that has none.
func TestFlagsDoNotLeakAcrossCommands(t *testing.T) {
	for _, prelude := range []string{"          set -euo pipefail\n", "          echo -e done\n"} {
		t.Run(strings.TrimSpace(prelude), func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			step := "      - name: The cluster must run the version its tag names\n        run: |\n" + prelude +
				"          KIND=${KIND_NODE_IMAGE}; kubectl version -o json | jq '.serverVersion.gitVersion'\n"
			write(t, root, lintWorkflow, strings.Replace(body, liveStep, step, 1))

			if _, _, err := run(t, root); err == nil {
				t.Fatalf("the `-e` in %q belongs to another command", strings.TrimSpace(prelude))
			}
		})
	}
}

// AN UNSET `shell:` IS NOT `shell: bash`. GitHub's default is `bash -e {0}` — no
// pipefail — so a pipe discards everything but the last command's status.
func TestEffectiveShellMustCarryPipefail(t *testing.T) {
	for _, tc := range []struct {
		name, where string
		ok          bool
	}{
		{"workflow default", "workflow", true},
		{"job default", "job", true},
		{"step", "step", true},
		{"nowhere", "none", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			switch tc.where {
			case "job":
				body = strings.Replace(body, shellDefault, "", 1)
				body = strings.Replace(body, "  dry-run:\n    steps:", "  dry-run:\n    defaults:\n      run:\n        shell: bash\n    steps:", 1)
			case "step":
				body = strings.Replace(body, shellDefault, "", 1)
				body = strings.Replace(body, "        run: kubectl apply", "        shell: bash\n        run: kubectl apply", 1)
				body = strings.Replace(body, "        run: kubectl version", "        shell: bash\n        run: kubectl version", 1)
			case "none":
				body = strings.Replace(body, shellDefault, "", 1)
			}
			write(t, root, lintWorkflow, body)

			_, errOut, err := run(t, root)
			if tc.ok && err != nil {
				t.Fatalf("a declared `shell: bash` at %s brings pipefail: %v", tc.where, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("GitHub's default shell has no pipefail")
				}
				if !strings.Contains(errOut, "pipefail") {
					t.Errorf("the failure must name what is missing, got %q", errOut)
				}
			}
		})
	}
}

func TestEffectiveShell(t *testing.T) {
	// Parsed rather than constructed: spelling the anonymous job struct out in a
	// test makes every field addition a compile error somewhere unrelated.
	var wf workflow
	if err := yaml.Unmarshal([]byte(
		"defaults:\n  run:\n    shell: bash\n"+
			"jobs:\n  a:\n    defaults:\n      run:\n        shell: sh\n  b:\n    steps: []\n"), &wf); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ job, step, want string }{
		{"b", "", "bash"},      // falls through to the workflow
		{"a", "", "sh"},        // the job's own default wins
		{"a", "pwsh", "pwsh"},  // the step wins over both
		{"absent", "", "bash"}, // an unknown job still sees the workflow
	} {
		if got := effectiveShell(wf, tc.job, tc.step); got != tc.want {
			t.Errorf("effectiveShell(job=%q, step=%q) = %q, want %q", tc.job, tc.step, got, tc.want)
		}
	}
}

// ── round 32: the rest of the list ─────────────────────────────────────────────

func TestRemainingDisablersAreRefused(t *testing.T) {
	for _, tc := range []struct{ name, step string }{
		{"|| echo", "          kubectl apply --dry-run=server -f rendered/ || echo failed\n"},
		{"a trailing &", "          kubectl apply --dry-run=server -f rendered/ &\n"},
		{"set +o pipefail", "          set +o pipefail\n          kubectl apply --dry-run=server -f rendered/ | tee log\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			step := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" + tc.step
			write(t, root, lintWorkflow, strings.Replace(body, applyStep, step, 1))

			if _, _, err := run(t, root); err == nil {
				t.Fatalf("%s leaves the apply unable to fail the job", tc.name)
			}
		})
	}
}

// `&&` chains a command and is not backgrounding; the `&` rule must not eat it.
func TestAndChainIsNotBackgrounding(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	chained := "      - name: Server-side dry-run of the rendered charts\n" +
		"        run: kubectl apply --dry-run=server -f rendered/ && echo done\n"
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, chained, 1))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("`a && b` fails when a fails: %v", err)
	}
}

// NAMING BOTH HALVES IS NOT COMPARING THEM. `kubectl version -o json >
// /tmp/v.json; echo "$KIND_NODE_IMAGE"` satisfied the signature and compared
// nothing, removing the only tag-vs-digest check the digest rule rests on.
func TestSignatureHalvesMustShareACommandList(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	split := "      - name: The cluster must run the version its tag names\n        run: |\n" +
		"          kubectl version -o json > /tmp/v.json\n          echo \"$KIND_NODE_IMAGE\"\n"
	write(t, root, lintWorkflow, strings.Replace(body, liveStep, split, 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("two commands that mention the two halves compare nothing")
	}
}

// ...and a pipe does NOT start a new command, so the real one-liner still reads
// as a single comparison.
func TestPipedSignatureHalvesAreOneCommand(t *testing.T) {
	if _, _, err := run(t, tree(t)); err != nil {
		t.Fatalf("the real live check is one pipeline: %v", err)
	}
}

func TestNamesBothInOneCommand(t *testing.T) {
	for _, tc := range []struct {
		run  string
		want bool
	}{
		{`kubectl version -o json | jq -e "${KIND_NODE_IMAGE}"`, true},
		{"kubectl version -o json > v.json\necho $KIND_NODE_IMAGE", false},
		{`kubectl version; echo ${KIND_NODE_IMAGE}`, false},
		{`echo ${KIND_NODE_IMAGE} && kubectl version`, true},
		{`kubectl version -o json`, false},
	} {
		if got := namesBothInOneCommand(tc.run); got != tc.want {
			t.Errorf("namesBothInOneCommand(%q) = %v, want %v", tc.run, got, tc.want)
		}
	}
}

// A WRAPPER IN FRONT OF IT IS STILL A CLUSTER, and the first-word rule missed
// every ordinary one — including this repo's own retry idiom.
func TestWrappedShellCreateIsRefused(t *testing.T) {
	for _, line := range []string{
		`          template-scripts/ci/with-retry.sh kind create cluster --name x`,
		`          timeout 600 kind create cluster --name x`,
		`          nohup kind create cluster --name x &`,
		`          if true; then kind create cluster --name x; fi`,
		`          bash -c "kind create cluster --name x"`,
	} {
		t.Run(strings.TrimSpace(line), func(t *testing.T) {
			root := tree(t)
			write(t, root, workflowsDir+"/e2e.yml", "name: E2E\njobs:\n  x:\n    steps:\n      - run: |\n"+line+"\n")

			if _, _, err := run(t, root); err == nil {
				t.Fatalf("`%s` stands up a cluster this gate cannot read", strings.TrimSpace(line))
			}
		})
	}
}

// ...and prose is still prose, in the spellings that matter.
func TestShellCreateProseIsStillNotACluster(t *testing.T) {
	for _, line := range []string{
		`          echo "never use kind create here"`,
		`          echo 'we kind create nowhere'`,
	} {
		t.Run(strings.TrimSpace(line), func(t *testing.T) {
			root := tree(t)
			write(t, root, workflowsDir+"/e2e.yml", "name: E2E\njobs:\n  x:\n    steps:\n      - run: |\n"+line+"\n")

			if _, _, err := run(t, root); err != nil {
				t.Fatalf("a mention is not a cluster, got: %v", err)
			}
		})
	}
}

func TestShellCreatesCluster(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{"kind create cluster", true},
		{"sudo kind create cluster", true},
		{"./kind create cluster", true},
		{"timeout 600 kind create cluster", true},
		{"$KIND create cluster", true},
		{`bash -c "kind create cluster"`, true},
		{`echo "kind create cluster"`, false},
		{"# kind create cluster", false},
		{"kubectl create namespace x", false},
		{"helm repo update", false},
	} {
		if got := shellCreatesCluster(tc.body); got != tc.want {
			t.Errorf("shellCreatesCluster(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// ── round 34: quote state across lines, and two accusations too wide ───────────

// A LINE-SCOPED STRIPPER OVER A WHOLE FILE loses everything after one unbalanced
// quote — live on this repo's corpus, where a multi-line `tag_query='…'` hid 235
// characters from the refusal.
func TestUnbalancedQuoteDoesNotHideALaterShellCreate(t *testing.T) {
	root := tree(t)
	write(t, root, "template-scripts/ci/spin.sh",
		"#!/usr/bin/env bash\nq='select(\n  .a\n)'\nkind create cluster --name x\n")

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("the create is on its own line and must still be seen")
	}
	if !strings.Contains(errOut, "spin.sh") {
		t.Errorf("the failure must name the script, got %q", errOut)
	}
}

// A VARIABLE TAKES ONLY `create cluster`. `"$KUBECTL" create namespace "$ns"` is
// not a cluster, and telling its author to use helm/kind-action would be a false
// accusation of the loudest kind.
func TestVariableCreateOfSomethingElseIsNotACluster(t *testing.T) {
	root := tree(t)
	write(t, root, "template-scripts/ci/ns.sh",
		"#!/usr/bin/env bash\n\"$KUBECTL\" create namespace \"$ns\"\n")

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("creating a namespace is not creating a cluster: %v", err)
	}
}

// ...and `$KIND create cluster` still is one.
func TestVariableCreateClusterIsStillACluster(t *testing.T) {
	root := tree(t)
	write(t, root, "template-scripts/ci/spin.sh", "#!/usr/bin/env bash\n\"$KIND\" create cluster --name x\n")

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a variable holding kind still stands up a cluster")
	}
}

// A jq PROGRAM IS A QUOTED ARGUMENT, and jq's own `if … then … end` was read as a
// shell condition — under the one step whose rewrite most naturally reaches for it.
func TestJQConditionalIsNotAShellCondition(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	jqIf := "      - name: The cluster must run the version its tag names\n" +
		"        run: kubectl version -o json | jq -e --arg t \"${KIND_NODE_IMAGE#kindest/node:v}\" 'if .serverVersion.gitVersion == \"v\" + ($t | split(\"@\")[0]) then true else false end'\n"
	write(t, root, lintWorkflow, strings.Replace(body, liveStep, jqIf, 1))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("that `if` is jq's, not the shell's: %v", err)
	}
}

// ...but a disabler handed to another shell as a string is still one.
func TestDisablerInsideDashCIsStillRefused(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	wrapped := "      - name: Server-side dry-run of the rendered charts\n" +
		"        run: bash -c \"kubectl apply --dry-run=server -f rendered/ || true\"\n"
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, wrapped, 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("a swallow in a -c string is still a swallow")
	}
}

// ── round 35: composition order, and three accusations too wide ────────────────

// `unquotedBody(flatten(…))` IS BACKWARDS: flatten removes the newlines, so the
// per-line quote scoping is gone by the time it runs and one apostrophe in a
// heredoc hides every later disabler.
func TestApostropheDoesNotHideALaterDisabler(t *testing.T) {
	for _, tc := range []struct{ name, find, replace string }{
		{"a swallowed apply", applyStep,
			"      - name: Server-side dry-run of the rendered charts\n        run: |\n" +
				"          echo \"don't stop\"\n          kubectl apply --dry-run=server -f rendered/ || true\n"},
		{"a jq with no -e", liveStep,
			"      - name: The cluster must run the version its tag names\n        run: |\n" +
				"          echo \"don't stop\"\n          kubectl version -o json | jq '.serverVersion.gitVersion' \"${KIND_NODE_IMAGE}\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t)
			body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
				"version: ${{ env.KIND_VERSION }}",
				"node_image: ${{ env.KIND_NODE_IMAGE }}",
				"kubectl_version: v${{ env.KUBECTL_VERSION }}")
			write(t, root, lintWorkflow, strings.Replace(body, tc.find, tc.replace, 1))

			if _, _, err := run(t, root); err == nil {
				t.Fatalf("%s is on its own line and the apostrophe is on another", tc.name)
			}
		})
	}
}

// jq's `-c` IS COMPACT OUTPUT, not a shell handing off a command.
func TestJQCompactFlagIsNotAShellDashC(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	compact := "      - name: The cluster must run the version its tag names\n" +
		"        run: kubectl version -o json | jq -ce --arg t \"${KIND_NODE_IMAGE#kindest/node:v}\" 'if .serverVersion.gitVersion == \"v\" + ($t | split(\"@\")[0]) then true else false end'\n"
	write(t, root, lintWorkflow, strings.Replace(body, liveStep, compact, 1))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("`jq -c` is jq's flag: %v", err)
	}
}

// A `with:` VALUE MAY BE ANY YAML NODE. A legal sequence anywhere in the four
// scanned trees used to fail the whole gate with a parse error about somebody
// else's workflow.
func TestSequenceValuedWithDoesNotBreakTheGate(t *testing.T) {
	root := tree(t)
	write(t, root, workflowsDir+"/other.yml",
		"name: Other\njobs:\n  x:\n    steps:\n      - uses: some/action@v1\n        with:\n          args:\n            - a\n            - b\n        env:\n          MAP:\n            k: v\n"+
			// …and a `with:` that is not a mapping at all.
			"      - uses: other/action@v1\n        with: [a, b]\n")

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("another workflow's shape is not this gate's business: %v", err)
	}
}

// `--client=false` CONTACTS THE SERVER; refusing it said the opposite.
func TestNegatedClientFlagIsFine(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, "kubectl version -o json", "kubectl version --client=false -o json", 1))

	if _, _, err := run(t, root); err != nil {
		t.Fatalf("`--client=false` asks the server: %v", err)
	}
}

// ...and `--client=true` is the real thing.
func TestExplicitTrueClientFlagIsCaught(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	write(t, root, lintWorkflow, strings.Replace(body, "kubectl version -o json", "kubectl version --client=true -o json", 1))

	if _, _, err := run(t, root); err == nil {
		t.Fatal("`--client=true` never contacts the API server")
	}
}

// A disabler handed to a shell as a `-c` string is refused even when the command
// this gate looks for sits outside the quotes.
func TestDashCDisablerBesideTheApply(t *testing.T) {
	root := tree(t)
	body := lintBody("v0.32.0", img("v1.34.8"), "1.34.10",
		"version: ${{ env.KIND_VERSION }}",
		"node_image: ${{ env.KIND_NODE_IMAGE }}",
		"kubectl_version: v${{ env.KUBECTL_VERSION }}")
	step := "      - name: Server-side dry-run of the rendered charts\n        run: |\n" +
		"          kubectl apply --dry-run=server -f rendered/\n" +
		"          bash -c \"helm repo update || true\"\n"
	write(t, root, lintWorkflow, strings.Replace(body, applyStep, step, 1))

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a swallow in a -c string is still in this step")
	}
	if !strings.Contains(errOut, "another shell") {
		t.Errorf("the failure must say where it found it, got %q", errOut)
	}
}

// AN UNREADABLE TREE IS A FINDING, NOT A SUBSTITUTE FOR THE OTHERS. The walk used
// to return on the first I/O error, discarding hits already found and skipping
// the remaining roots — so a permissions problem in one tree replaced a real
// refusal in another.
func TestUnreadableScriptTreeIsReportedBesideTheHits(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads an unreadable directory anyway")
	}
	root := tree(t)
	// A real hit in one root...
	write(t, root, "template-scripts/ci/spin.sh", "#!/usr/bin/env bash\nkind create cluster --name x\n")
	// ...and an unreadable directory in another.
	blocked := filepath.Join(root, ".github", "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("the create in the readable tree is still a finding")
	}
	for _, want := range []string{"spin.sh", "could not read"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the report must carry %q, got %q", want, errOut)
		}
	}
}

// ...and an unreadable tree on its own says so rather than passing quietly.
func TestUnreadableScriptTreeAloneIsAFinding(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads an unreadable directory anyway")
	}
	root := tree(t)
	blocked := filepath.Join(root, "template-scripts", "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a tree the refusal could not read is not a tree it cleared")
	}
	if !strings.Contains(errOut, "did not see that tree") {
		t.Errorf("the failure must say what it could not read, got %q", errOut)
	}
}
