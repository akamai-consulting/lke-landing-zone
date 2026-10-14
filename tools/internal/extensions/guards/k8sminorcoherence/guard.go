package k8sminorcoherence

// guard.go implements `llz ci k8s-minor-coherence` — the gate that keeps the
// Kubernetes minor lint.yml's server-side dry-run validates against equal to the
// LKE-Enterprise minor this repo actually deploys.
//
// WHY (#427): `kubectl apply --dry-run=server -f rendered/` is the most
// load-bearing manifest gate in the repo — the only check that asks a real API
// server whether these manifests are acceptable, which is what catches a removed
// API, an immutable-field change, or a field only a newer server validates. It is
// answered by whatever kind boots, so a lint.yml that pins kind's VERSION and not
// its NODE IMAGE validates against kind's default minor rather than the one the
// cluster root pins, and stays green while minors of API churn sit outside its
// field of view.
//
// NOTHING ELSE CAN SEE IT, which is the class this repo keeps rediscovering. Both
// pins are individually well-formed; the defect is only visible as a RELATION
// between them — the same shape as version-pins, setup-go-sole-site and
// mutable-tag-guard — and it can be created by a change to NEITHER site, as
// bumping kubectl once did to a kubectl ↔ node-image pairing nobody had written
// down.
//
// WHAT IT COMPARES, AND AT WHAT PRECISION. The MINOR, and only the minor. The two
// sides cannot be equal and must not be allowed to drift: Linode offers full LKE-E
// build ids (`v1.34.6+lke2`) that kind has never heard of, and kind ships its own
// patch line (`v1.34.8`). Equality would be unsatisfiable; major-only would pass
// the 1.31-vs-1.34 gap this exists to catch. The minor is the precision at which
// the Kubernetes API surface actually changes.
//
// THE KUBECTL HALF IS A SKEW BOUND, NOT AN EQUALITY, and the difference is
// deliberate. kubectl's supported skew is ±1 minor from the API server, so that
// is what is checked. Requiring equality would chain every LKE-E bump to a
// dockerfiles/Dockerfile ARG bump (version-pins holds lint.yml's KUBECTL_VERSION
// equal to it) and therefore to an image republish, turning a one-line pin change
// into a cross-repo sequence. The bound catches what #427 names — a +3 skew — and
// leaves the ordinary one-minor lead alone.
//
// IT IS MEASURED AGAINST THE NODE IMAGE, not against the tfvars pin. The
// distinction is invisible on a healthy tree because the two are equal there, and
// a test written on one will not catch it — in the #427 state itself (node 1.31,
// kubectl 1.34) measuring against the pin reports a skew of 0.
//
// THE OTHER KUBECTL RULE IS AN EQUALITY, because it is a different question: the
// job installs kubectl TWICE (azure/setup-kubectl and kind-action) and both
// append to GITHUB_PATH, which prepends — so which binary every later step runs
// is decided by step order. Two pins one minor apart both satisfy the skew bound
// and still leave that undecided, so they must simply be the same version.
//
// A CLUSTER THIS GATE CANNOT READ IS REFUSED RATHER THAN IGNORED. It holds the
// deployed minor by reading one declared input, so a cluster stood up any other
// way — `run: kind create cluster`, a composite carrying its own kind step, a
// second kind-action step in another workflow — is not a case to reason about
// but a case that makes the answer meaningless. All three are errors.
//
// WHAT IS DELIBERATELY NOT CHECKED: that KIND_VERSION is new enough to know the
// node image. That mapping lives in kind's release notes and a copy of it here
// would be exactly the kind of table this repo has been burned by. It also does
// not need checking: a kind that cannot run the image fails at cluster create,
// loudly, in the same job. The silent failure is the one this guard covers.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfvars"
)

const (
	// authorityFile is where the LKE-Enterprise version this repo deploys is
	// DECIDED. `llz env add` seeds every new instance's spec.defaults.cluster
	// .k8sVersion from it (internal/shared/envdef reads the same key), so it is
	// the pin an adopter's cluster is actually built from rather than a
	// restatement of one.
	authorityFile = "tools/internal/shared/tfroots/roots/cluster/terraform.tfvars.example"
	authorityKey  = "k8s_version"

	// envdefFile carries the fallback envdef uses when the tfvars example cannot
	// be read. It is a SECOND copy of the authority and is checked against it —
	// with the two disagreeing there is no single answer to "the minor we
	// deploy", which would make every comparison below ambiguous.
	envdefFile = "tools/internal/shared/envdef/envdef.go"

	// workflowsDir is the one tree that MUST exist: without it, --root is wrong.
	// The rest of the scan roots are yamlRoots/scriptRoots below.
	workflowsDir = ".github/workflows"

	// kindActionPrefix identifies the step that stands up that cluster.
	//
	// LOWER CASE, AND MATCHED CASE-INSENSITIVELY (usesAction). GitHub resolves
	// `uses:` without regard to case, so `Helm/Kind-Action@…` is the same step —
	// and for setupKubectlPrefix below, whose canonical repo name is in fact
	// `Azure/setup-kubectl`, a case-sensitive match would have SKIPPED the
	// duplicate-kubectl check on the canonical spelling. That is a fail-open: the
	// guard would print OK having never looked.
	kindActionPrefix = "helm/kind-action"

	// shellCreate is a cluster stood up outside the action, where its node image
	// is an argument in a shell string rather than a declared input.
	//
	// REFUSED RATHER THAN PARSED. The whole gate rests on reading `node_image:` —
	// a `run: kind create cluster` (after `install_only: true`, or a curl install)
	// declares nothing this can hold to the deployed minor, so it would boot kind's
	// default node image with the gate green: #427 one file over. Matching the
	// action alone made that gap real and unstated.
	shellCreate = "kind create"

	// kubectlSkew is kubectl's supported client/server minor skew.
	kubectlSkew = 1

	// dryRunApply is the signature of the check this whole gate exists to protect.
	//
	// IT WAS THE ONE THING NOT ASSERTED. The guard failed closed on a missing kind
	// step and on a missing or disabled live half, and said nothing about the
	// server-side apply itself — so deleting that step, flipping it to
	// `--dry-run=client`, or disabling the job left the guard printing "OK —
	// validates against Kubernetes 1.34" over a job that validated nothing. A gate
	// that certifies the FIDELITY of a check it never confirms exists is the
	// purest form of the class it was written for.
	//
	// `--dry-run=client` is not itself an error: the job uses it legitimately to
	// render a Namespace before applying it. What is required is that a
	// SERVER-side apply is still there.
	dryRunApply = "--dry-run=server"

	// nodeImageEnv is the workflow-level variable that holds the node image.
	//
	// THE TWO HALVES HAVE TO READ ONE VALUE. This guard reads the kind step's
	// resolved `node_image:`; the live step in the same job reads this env var.
	// Nothing made those the same string — a literal `node_image:` with no env
	// var, or a job/step-level override, passed the guard and left the live check
	// comparing against nothing — so the resolved input is required to EQUAL the
	// workflow-level value here. Spelling the pin twice identically is fine and
	// still coherent; spelling it in one place and checking the other is not.
	nodeImageEnv = "KIND_NODE_IMAGE"
)

// reEnvdefFallback matches envdef's hardcoded default for the cluster k8s pin.
//
// A CLOSED CLASS: if this stops matching, the guard errors rather than skipping.
// A restatement the scanner can no longer see is indistinguishable from one that
// agrees, and reporting the second when the first is true is how a gate goes
// quietly blind.
var reEnvdefFallback = regexp.MustCompile(`tfvarsExampleValue\("cluster",\s*"k8s_version"\),\s*"([^"]+)"`)

// reNodeImage matches a kind node image reference: a kindest/node tag AND a
// digest, both required.
//
// THE TWO HALVES DO DIFFERENT JOBS AND NEITHER IS OPTIONAL. Docker pulls a
// `name:tag@digest` reference BY DIGEST, so the digest is what kind actually
// boots — kind's own release notes say the digest is what guarantees an image
// built for that kind release, and node images are not compatible across
// releases. The tag is a label, and it is the only half a static guard can read a
// minor out of.
//
// SO THE TAG CAN LIE, and this guard alone cannot tell. Bump the tag without the
// digest and the dry-run stays on the old server while the comparison below reads
// the new minor and passes: a consistent-looking pair that is wrong, which is the
// class the gate exists to end rather than to reproduce one level down. That is
// why lint.yml's dry-run job carries the LIVE half — one step asserting the
// cluster kind actually created reports the minor the tag claims. A static guard
// cannot distinguish a consistent pair from a correct pair; only the cluster can.
//
// A BARE TAG IS REFUSED for the other direction: without the digest the pin is
// not reproducible, and a re-pushed tag changes what CI runs with no commit here.
var reNodeImage = regexp.MustCompile(`^kindest/node:(v[0-9]+\.[0-9]+\.[0-9]+)@sha256:[0-9a-f]{64}$`)

// setupKubectlPrefix identifies the OTHER step in the job that installs a
// kubectl. Lower-case, like kindActionPrefix, and matched through usesAction.
const setupKubectlPrefix = "azure/setup-kubectl"

// reBareNodeTag is a kindest/node tag with no digest — the one malformed case
// that gets its own remedy, because the fix is to add something rather than to
// rewrite the reference.
var reBareNodeTag = regexp.MustCompile(`^kindest/node:v[0-9]+\.[0-9]+\.[0-9]+$`)

// reEnvExpr matches one `${{ env.NAME }}` GitHub Actions expression.
//
// Not anchored: resolve() substitutes it wherever it appears, because a pin is
// routinely spelled as a prefix plus a reference (`v${{ env.KUBECTL_VERSION }}`).
var reEnvExpr = regexp.MustCompile(`\$\{\{\s*env\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// runDefaults is a `defaults: run:` block, read only for its `shell:`.
type runDefaults struct {
	Run struct {
		Shell string `yaml:"shell"`
	} `yaml:"run"`
}

// workflow is the slice of an Actions workflow this guard reads. A composite
// action parses into the same shape: its steps hang off `runs:` and its `jobs:`
// is empty, which is what makes a kind step there a second cluster rather than a
// candidate for the dry-run job.
type workflow struct {
	// Defaults carries the workflow-level `shell:`, which is what turns GitHub's
	// bare `bash -e {0}` default into `bash --noprofile --norc -eo pipefail {0}`.
	Defaults runDefaults `yaml:"defaults"`
	// On is the trigger, read only for its `paths:` filters — the last way the
	// dry-run can stop running that this guard did not look at.
	On   onSpec `yaml:"on"`
	Runs struct {
		Steps []step `yaml:"steps"`
	} `yaml:"runs"`
	Env  scalars `yaml:"env"`
	Jobs map[string]struct {
		// A job-level `if:` decides whether ANY of its steps run and a job-level
		// `continue-on-error:` discards the whole job's result, so each disables the
		// dry-run wholesale — the same question the per-step fields below answer.
		// Reading one and not the other left half the door open.
		Defaults        runDefaults `yaml:"defaults"`
		If              any         `yaml:"if"`
		ContinueOnError any         `yaml:"continue-on-error"`
		// A skipped dependency skips its dependents, so `needs:` is a third way to
		// disable this job — and the natural one to reach for here, because the
		// sibling job in the same workflow carries the fork guard. Reading `if:` and
		// `continue-on-error:` and not this left the fifth door in the family open.
		Needs stringList `yaml:"needs"`
		Env   scalars    `yaml:"env"`
		Steps []step     `yaml:"steps"`
	} `yaml:"jobs"`
}

// onSpec is a workflow's `on:`, in any of the three shapes Actions accepts:
// a scalar (`on: push`), a sequence (`on: [push, pull_request]`), or a mapping
// with per-event filters. Only the third can carry `paths:`; the other two run
// on every path, which is what an empty map means here.
type onSpec map[string]triggerSpec

func (o *onSpec) UnmarshalYAML(n *yaml.Node) error {
	out := onSpec{}
	switch n.Kind {
	// The scalar and sequence arms drop their decode errors on purpose: yaml
	// coerces any scalar into a string target, so `on: 5` yields the event name
	// "5" rather than an error, and there is no input that reaches these returns.
	// An unreachable arm is one no test can cover and no reader can trust —
	// and a nonsense event name carries no `paths:`, which is the only thing read
	// here.
	case yaml.ScalarNode:
		var one string
		_ = n.Decode(&one)
		out[one] = triggerSpec{}
	case yaml.SequenceNode:
		var many []string
		_ = n.Decode(&many)
		for _, e := range many {
			out[e] = triggerSpec{}
		}
	case yaml.MappingNode:
		m := map[string]triggerSpec{}
		if err := n.Decode(&m); err != nil {
			return err
		}
		out = m
	}
	*o = out
	return nil
}

// hasAny reports whether the trigger raises on any of the named events.
func (o onSpec) hasAny(events ...string) bool {
	for _, e := range events {
		if _, ok := o[e]; ok {
			return true
		}
	}
	return false
}

// triggerSpec is one entry under `on:`. A trigger written as a bare name
// (`on: [push]`, `on: push`) carries no filter and decodes to the zero value,
// which is the correct answer: no `paths:` means every path.
type triggerSpec struct {
	Paths []string `yaml:"paths"`
	// PathsIgnore is the same door from the other side, and reading only `paths:`
	// left it open: the two are mutually exclusive, so swapping to
	// `paths-ignore: ['tools/**', '.github/workflows/**']` made the event look
	// unfiltered and the check silent.
	PathsIgnore []string `yaml:"paths-ignore"`
}

// UnmarshalYAML takes the zero value for any shape that is not a mapping.
//
// `schedule:` IS A SEQUENCE — `- cron: '0 3 * * 1'` — and build-images.yml has
// one, so a plain struct decode failed on a real workflow and the gate refused
// the tree. It was deleted once for being uncovered; the right answer was a test
// for the shape rather than removing the code that handles it. A null value
// (`push:` with nothing under it) takes the same path and means the same thing:
// an event with no `paths:`, which runs on everything.
func (t *triggerSpec) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	type plain triggerSpec
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*t = triggerSpec(p)
	return nil
}

// step is one Actions step, in either a workflow job or a composite's `runs:`.
type step struct {
	Name string `yaml:"name"`
	Uses string `yaml:"uses"`
	Run  string `yaml:"run"`
	// A step's `if:` and `continue-on-error:` decide whether it can fail the
	// job. Both are read as `any` because Actions accepts a bool OR an
	// expression string in each, and a guard that could not parse
	// `continue-on-error: true` would drop the field rather than the step.
	If              any `yaml:"if"`
	ContinueOnError any `yaml:"continue-on-error"`
	// Shell decides whether a failing command aborts the step at all. GitHub's
	// named shells all carry `-e` (`bash` is `bash --noprofile --norc -eo pipefail
	// {0}`), but a CUSTOM template need not — `shell: bash {0}` runs on without it.
	Shell string  `yaml:"shell"`
	Env   scalars `yaml:"env"`
	With  scalars `yaml:"with"`
}

// scalars is a `with:` or `env:` map whose values are read as text.
//
// A VALUE MAY BE ANY YAML NODE. Typed `map[string]string`, a legal
// `with:\n  args:\n    - a` anywhere in the four scanned trees failed the whole
// gate with a bare `cannot unmarshal !!seq into string` — a parse error about
// somebody else's workflow, standing in for a verdict about the kind pin. The
// values this guard reads are scalars; the ones it does not are kept as empty
// rather than refused, because an unrelated step's shape is not its business.
type scalars map[string]string

func (m *scalars) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	out := scalars{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		var k string
		if err := n.Content[i].Decode(&k); err != nil {
			continue
		}
		var v string
		if n.Content[i+1].Kind == yaml.ScalarNode {
			_ = n.Content[i+1].Decode(&v)
		}
		out[k] = v
	}
	*m = out
	return nil
}

// stringList is a YAML field Actions accepts as either a scalar or a sequence
// (`needs: build` and `needs: [build, test]` are the same thing).
type stringList []string

func (l *stringList) UnmarshalYAML(n *yaml.Node) error {
	var one string
	if err := n.Decode(&one); err == nil {
		*l = stringList{one}
		return nil
	}
	var many []string
	if err := n.Decode(&many); err != nil {
		return err
	}
	*l = many
	return nil
}

// minor is a Kubernetes major.minor, kept as two ints so the skew comparison is
// arithmetic rather than string surgery.
type minor struct{ major, minor int }

func (m minor) String() string { return fmt.Sprintf("%d.%d", m.major, m.minor) }

// parseMinor extracts major.minor from any of the spellings these pins use:
// `v1.34.6+lke2`, `1.34.10`, `v1.34.8`.
func parseMinor(v string) (minor, error) {
	s := strings.TrimPrefix(strings.TrimSpace(v), "v")
	// Drop an LKE-E build suffix (`+lke2`) and any pre-release tail.
	if i := strings.IndexAny(s, "+-"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return minor{}, fmt.Errorf("%q is not a major.minor[.patch] version", v)
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return minor{}, fmt.Errorf("%q has a non-numeric major", v)
	}
	min, err := strconv.Atoi(parts[1])
	if err != nil {
		return minor{}, fmt.Errorf("%q has a non-numeric minor", v)
	}
	return minor{maj, min}, nil
}

// Run compares the Kubernetes minor lint.yml validates against with the one the
// cluster Terraform root pins, and fails when they differ.
func Run(root string, out, errOut io.Writer) error {
	repo := capability.RepoForGate(Extension(), root)

	deployed, deployedRaw, err := authority(repo)
	if err != nil {
		return err
	}

	all, err := readWorkflows(repo)
	if err != nil {
		return err
	}
	scripts, shellClusters := noShellClusters(repo)
	step, file, jobName, err := findKindStep(all)
	if err != nil {
		if shellClusters != "" {
			return fmt.Errorf("%w\n%s", err, shellClusters)
		}
		return err
	}
	wf := all[file]

	var problems []string
	// A FINDING RATHER THAN AN ABORT. Returning early here meant one refusal — and
	// a false one costs the same — hid every other check behind it, so a reader saw
	// a single sentence about a script and nothing about the pins. It is reported
	// beside the rest unless the kind step itself could not be found, where there
	// is nothing else to say.
	for _, m := range []string{shellClusters, inlineShellClusters(all)} {
		if m != "" {
			problems = append(problems, m)
		}
	}

	// The API server every kubectl in this job talks to is the NODE IMAGE, not the
	// pin it is held to. They are equal on a healthy tree, which is what hid this:
	// measuring skew against `deployed` reported 0 in the #427 state itself
	// (node 1.31, kubectl 1.34) — the header claimed the bound catches that +3 and
	// it did not. `deployed` stands in only while the image is unreadable, where a
	// problem has already been recorded for it.
	server, serverKnown := deployed, false

	// THE JOB, AND THE APPLY IT EXISTS FOR. Everything below asks whether the
	// dry-run is answered by the right server; these ask whether it is answered at
	// all. They are first because a disabled job makes every other finding moot.
	if why := disabledReason(fmt.Sprintf("%s's %q job", file, jobName),
		wf.Jobs[jobName].If, wf.Jobs[jobName].ContinueOnError); why != "" {
		problems = append(problems, why+", so the server-side dry-run may never run")
	}
	if why := skippableDependency(wf, file, jobName); why != "" {
		problems = append(problems, why)
	}
	problems = append(problems, unreachableOnChange(wf, file, append(sortedKeys(all), scripts...))...)
	if why := dryRunHalf(wf, jobName); why != "" {
		problems = append(problems, why)
	}

	// Resolved first because the node-image check below compares against what THIS
	// step reads: the live half is a shell `${KIND_NODE_IMAGE}`, whose value comes
	// from the same step/job/workflow env stack an expression resolves through.
	liveStep, liveErr := liveHalf(wf, jobName)
	if liveErr != nil {
		problems = append(problems, liveErr.Error())
	}

	// `install_only` means the step installs kind and creates NOTHING, so the
	// node image it declares is never booted and every comparison below is about a
	// cluster that does not exist.
	installOnly, ioErr := resolve(wf, jobName, step, "install_only")
	switch {
	case ioErr != nil:
		problems = append(problems, fmt.Sprintf("%s (job %q) `install_only:` — %v", file, jobName, ioErr))
	case truthy(installOnly):
		problems = append(problems, fmt.Sprintf(
			"%s (job %q) sets `install_only`, so the kind step creates no cluster and the `node_image:`\n"+
				"    this gate holds to the deployed minor is never booted", file, jobName))
	}

	// A kind step that cannot fail leaves the job applying against no cluster at
	// all, which the apply's own failure would then be blamed for.
	if why := disabledReason(fmt.Sprintf("job %q's kind step", jobName), step.If, step.ContinueOnError); why != "" {
		problems = append(problems, why+", so the dry-run could run against no cluster")
	}

	// The kind VERSION must be pinned. It is not compared to anything — see the
	// header — but unpinning it hands the node image's compatibility to whatever
	// default the action ships this month, which is the drift that produced #427
	// in the first place.
	switch kindVersion, verr := resolve(wf, jobName, step, "version"); {
	case verr != nil:
		problems = append(problems, fmt.Sprintf("%s (job %q) `version:` — %v", file, jobName, verr))
	case kindVersion == "":
		problems = append(problems, fmt.Sprintf(
			"%s (job %q) does not pin kind's `version:` — the action's own default would decide it", file, jobName))
	}

	nodeImage, err := resolve(wf, jobName, step, "node_image")
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("%s (job %q) `node_image:` — %v", file, jobName, err))
	case nodeImage == "":
		// FAIL CLOSED ON THE ABSENT PIN. This is the #427 state exactly: with no
		// node_image, kind picks its own, the dry-run runs against a minor nobody
		// declared, and there is nothing here for the comparison below to read.
		problems = append(problems, fmt.Sprintf(
			"%s (job %q) sets no `node_image:`, so kind boots its own default and the\n"+
				"    dry-run validates against a Kubernetes minor no file in this repo declares", file, jobName))
	default:
		m := reNodeImage.FindStringSubmatch(nodeImage)
		if m == nil {
			why := "is not a kindest/node:vX.Y.Z@sha256:<digest> reference"
			if reBareNodeTag.MatchString(nodeImage) {
				why = "names no digest. Docker pulls `name:tag@digest` BY DIGEST, so a bare tag is\n" +
					"    not a pin at all — a re-push changes what CI boots with no commit here, and kind's\n" +
					"    release notes give the digest for exactly that reason. Take both from the notes\n" +
					"    for the KIND_VERSION in force"
			}
			problems = append(problems, fmt.Sprintf(
				"%s (job %q) `node_image: %s` %s",
				file, jobName, nodeImage, why))
			break
		}
		// reNodeImage's own pattern already requires three numeric components, so
		// parseMinor cannot fail on its capture. The error is dropped rather than
		// handled: an arm no input can reach is an arm no test can cover and no
		// reader can trust, and the invariant is one line above it.
		got, _ := parseMinor(m[1])
		server, serverKnown = got, true
		// THE TWO HALVES MUST CERTIFY ONE PIN. This reads the kind step's resolved
		// input; the live step reads env.KIND_NODE_IMAGE out of its own process
		// environment. Compared against the WORKFLOW-level value this rejected a
		// job-level pin that both halves would have read correctly, so it is
		// compared against what the live step itself resolves — the same
		// step-then-job-then-workflow stack, rooted at that step.
		if liveErr == nil {
			switch livePin, ok := lookupEnv(wf, jobName, liveStep, nodeImageEnv); {
			case !ok:
				problems = append(problems, fmt.Sprintf(
					"the live check reads env.%s, which is defined at no step, job or workflow level —\n"+
						"    it would expand to nothing and compare the cluster against an empty string",
					nodeImageEnv))
			case strings.TrimSpace(livePin) != nodeImage:
				problems = append(problems, fmt.Sprintf(
					"the kind step boots %s but the live check reads env.%s = %q. The static half would\n"+
						"    certify one image while the live half measured another — hold the pin in one env\n"+
						"    var and reference it from the step",
					nodeImage, nodeImageEnv, strings.TrimSpace(livePin)))
			}
		}
		if got != deployed {
			problems = append(problems, fmt.Sprintf(
				"the dry-run validates against Kubernetes %s but we deploy %s.\n"+
					"    %s  node_image: %s\n"+
					"    %s  %s = %q",
				got, deployed, file, nodeImage, authorityFile, authorityKey, deployedRaw))
		}
	}

	kubectlPin, err := resolve(wf, jobName, step, "kubectl_version")
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("%s (job %q) `kubectl_version:` — %v", file, jobName, err))
	case kubectlPin == "":
		// kind-action downloads its own kubectl and appends that directory to
		// GITHUB_PATH, which PREPENDS it to PATH for every later step. Leaving the
		// input unset lets the action's default shadow whatever kubectl the job
		// installed, and nothing in the log names the binary that ran.
		problems = append(problems, fmt.Sprintf(
			"%s (job %q) sets no `kubectl_version:`. kind-action installs its own kubectl\n"+
				"    and prepends it to PATH, so its default silently shadows the one this job installed",
			file, jobName))
	default:
		got, perr := parseMinor(kubectlPin)
		if perr != nil {
			problems = append(problems, fmt.Sprintf("%s (job %q) `kubectl_version:` — %v", file, jobName, perr))
			break
		}
		if d := got.major*1000 + got.minor - (server.major*1000 + server.minor); d > kubectlSkew || d < -kubectlSkew {
			problems = append(problems, fmt.Sprintf(
				"kubectl %s is %s the %s server it talks to; the supported skew is ±%d minor.\n"+
					"    %s  kubectl_version: %s%s",
				got, distance(got, server), server, kubectlSkew, file, kubectlPin, serverFrom(serverKnown)))
		}
		// AND THE JOB MUST HOLD ONE KUBECTL. Both installers append to GITHUB_PATH,
		// which prepends, so whichever step runs later wins — a fact no step states
		// and no log names. Pinning kind-action's input only helps if it pins it to
		// the SAME version; two well-formed pins one minor apart pass the skew bound
		// above and still leave the job's kubectl decided by step order.
		if other, name, oerr := setupKubectlPin(wf, jobName); oerr != nil {
			problems = append(problems, fmt.Sprintf("%s (job %q) `%s` — %v", file, jobName, name, oerr))
		} else if other != "" && !sameVersion(other, kubectlPin) {
			problems = append(problems, fmt.Sprintf(
				"job %q installs kubectl twice, at %s and %s. Both installers prepend to PATH, so\n"+
					"    which one every later step runs is decided by step order rather than by either pin.\n"+
					"    %s  %s version: %s\n"+
					"    %s  kind-action kubectl_version: %s",
				jobName, other, kubectlPin, file, setupKubectlPrefix, other, file, kubectlPin))
		}
	}

	if len(problems) == 0 {
		fmt.Fprintf(out, "k8s-minor-coherence: OK — %s validates against Kubernetes %s, which is the minor %s pins (%s)\n",
			file, deployed, authorityFile, deployedRaw)
		return nil
	}

	fmt.Fprintf(errOut, "::error file=%s::the dry-run gate does not validate against the Kubernetes minor this repo deploys\n", file)
	fmt.Fprintf(errOut, "\n%s k8s-minor-coherence: %d problem(s):\n", color.Red("✗"), len(problems))
	for _, p := range problems {
		fmt.Fprintf(errOut, "  - %s\n", p)
	}
	fmt.Fprintf(errOut, "\n`kubectl apply --dry-run=server` is the only check that asks a REAL API server\n"+
		"whether the rendered manifests are acceptable. Against the wrong minor it answers a\n"+
		"question nobody asked: an API removed since, or a field only the deployed server\n"+
		"validates, passes here and fails on the cluster.\n\n"+
		"To move the deployed minor, change %s and %s's\n"+
		"KIND_NODE_IMAGE in the same commit — and pick a KIND_VERSION whose release notes\n"+
		"list that node image (kind will not boot one it does not know).\n", authorityFile, file)
	return fmt.Errorf("k8s-minor-coherence: %d problem(s)", len(problems))
}

// liveHalf requires the dry-run job to still carry the step that asks the CLUSTER
// what minor it is.
//
// THE DIGEST REQUIREMENT RESTS ON IT. This guard reads the minor off the node
// image's TAG while docker pulls the DIGEST beside it, so a bumped tag with a
// stale digest is a pair that agrees with itself and is wrong — undetectable from
// the repo, by construction. The live step is what closes that, and without it
// the digest rule makes the guard weaker rather than stronger. A gate whose
// safety argument depends on a step nobody asserts is a gate that goes quietly
// blind the day the step is renamed.
//
// It is matched by SIGNATURE — a `run:` naming both `kubectl version` and the env
// var — rather than by step name, because a name is prose and this is a claim
// about what the step does. If the check is legitimately rewritten, update the
// signature here in the same commit; a scanner that can no longer see its subject
// must say so rather than pass.
func liveHalf(wf workflow, jobName string) (actionStep, error) {
	var found []actionStep
	var bodies []string
	for _, st := range wf.Jobs[jobName].Steps {
		// BOTH IN ONE COMMAND LIST, not merely both in the step. `kubectl version -o
		// json > /tmp/v.json; echo "$KIND_NODE_IMAGE"` names them and compares
		// nothing, which removes the only tag-vs-digest check the digest rule rests
		// on. A `;` starts a new command; a pipe does not, so the real
		// `kubectl version … | jq … "${KIND_NODE_IMAGE#…}"` still reads as one.
		if !namesBothInOneCommand(st.Run) {
			continue
		}
		found = append(found, actionStep{If: st.If, ContinueOnError: st.ContinueOnError, Shell: st.Shell, Env: st.Env})
		bodies = append(bodies, st.Run)
	}
	// EVERY MATCH IS COUNTED, NOT JUST THE FIRST. Reading only the first meant a
	// second, disabled copy behind a healthy one passed — and the env scope this
	// returns for the coherence check came from whichever happened to be earlier.
	// With several there is no single answer to "what does the live half read",
	// which is exactly what this hands back.
	switch len(found) {
	case 0:
		return actionStep{}, fmt.Errorf("job %q has no step asserting the cluster it created actually runs the minor\n"+
			"    env.%s claims (a `run:` naming `kubectl version` and %s).\n"+
			"    That step is the only thing that can tell the node image's TAG from the DIGEST docker\n"+
			"    pulls, and requiring a digest is only safe while it exists", jobName, nodeImageEnv, nodeImageEnv)
	case 1:
		what := fmt.Sprintf("job %q's live minor check", jobName)
		why := disabledReason(what, found[0].If, found[0].ContinueOnError)
		if why == "" {
			why = shellWithoutExitOnError(what, effectiveShell(wf, jobName, found[0].Shell))
		}
		if why == "" {
			why = swallowReason(what, bodies[0])
		}
		// `kubectl version --client` NEVER CONTACTS THE SERVER: it reports the
		// binary's own version, so the comparison is the client against itself. The
		// step would be present, gating, and answering a question about nothing —
		// while the digest rule's whole argument is that this one asks the cluster.
		// A whole flag, so `--client-key=…` is not it.
		if why == "" && reClientFlag.MatchString(unquotedBody(uncommented(bodies[0]))) {
			why = fmt.Sprintf("%s runs `kubectl version --client`, which never contacts the API server", what)
		}
		// AND THE COMPARISON HAS TO REACH THE EXIT STATUS. `jq` without `-e` exits 0
		// whatever it prints, so dropping one flag leaves the step present, gating and
		// completely inert — the same "cannot fail" state as `continue-on-error`, one
		// character wide, under the one step the digest rule depends on.
		if why == "" {
			if m := jqWithoutExitStatus(bodies[0]); m != "" {
				why = fmt.Sprintf("%s runs a `jq` with no `-e`, whose result never reaches the exit status:\n"+
					"    %s\n"+
					"    Every jq in the step is held to it — which one decides the outcome depends on the\n"+
					"    pipeline around it, and this guard refuses to guess at that rather than model it",
					what, m)
			}
		}
		if why != "" {
			// Reported rather than skipped over: a disabled check and an absent one
			// need different remedies, and "I disabled it deliberately" is a decision
			// that belongs in a diff, not in a silent pass.
			return actionStep{}, fmt.Errorf("%s. It is the only thing that can tell env.%s's TAG from\n"+
				"    the DIGEST docker pulls; requiring a digest is only safe while it can fail the job",
				why, nodeImageEnv)
		}
		return found[0], nil
	default:
		return actionStep{}, fmt.Errorf("job %q has %d steps matching the live minor check. This resolves env.%s in the\n"+
			"    scope of THAT step, so with several there is no one answer to what the live half reads",
			jobName, len(found), nodeImageEnv)
	}
}

// skippableDependency reports a `needs:` that can skip this job out from under
// the gate.
//
// A SKIPPED DEPENDENCY SKIPS ITS DEPENDENTS, which makes `needs:` a third way to
// disable a job and the natural one to reach for here: the sibling job in the
// same workflow carries the fork-PR guard, so `needs: [kubernetes]` reads as
// tidy and silently takes the dry-run out on every fork PR. The guard closed the
// `if:` and `continue-on-error:` doors and left this one.
//
// It walks the graph TRANSITIVELY, which the first cut did not: `dry-run` needs
// `prep` and `prep` needs the job carrying the fork guard is the arrangement that
// actually occurs, and one hop saw none of it. Naming only unconditional jobs,
// all the way up, is the answer that keeps the dry-run running.
func skippableDependency(wf workflow, file, jobName string) string {
	// TRANSITIVE, because skipping is. One hop missed the arrangement that
	// actually occurs: `dry-run` needs `prep`, `prep` needs the job carrying the
	// fork guard, and the dry-run is skipped on every fork PR with the gate green.
	// A dependency chain is exactly as long as someone finds convenient.
	// The edge is carried with the node, so a missing job is reported against the
	// job that NAMES it rather than against the one the walk started from —
	// `dry-run needs "ghost"` when it is `prep` that does names an edge that does
	// not exist, and sends the reader to the wrong file.
	type edge struct{ from, to string }
	seen := map[string]bool{jobName: true}
	var queue []edge
	for _, d := range wf.Jobs[jobName].Needs {
		queue = append(queue, edge{jobName, d})
	}
	for len(queue) > 0 {
		e := queue[0]
		dep := e.to
		queue = queue[1:]
		if seen[dep] {
			continue
		}
		seen[dep] = true
		upstream, ok := wf.Jobs[dep]
		if !ok {
			return fmt.Sprintf("%s's %q job needs %q, which that workflow does not define — the job cannot run at all",
				file, e.from, dep)
		}
		// ONLY THE `if:` ARM. A `continue-on-error` upstream that FAILS still
		// resolves as success for `needs:`, so its dependents run — calling that a
		// skip would hard-fail a legitimate wiring change with a diagnosis that is
		// simply untrue. Whether a broken prerequisite makes the dry-run meaningless
		// is a different question and not this gate's.
		if upstream.If != nil && !alwaysRuns(upstream.If) {
			return fmt.Sprintf("%s's %q job, which %q reaches through `needs:`, is conditional (`if: %v`),\n"+
				"    and a skipped dependency skips the dry-run with it", file, dep, jobName, upstream.If)
		}
		for _, d := range upstream.Needs {
			queue = append(queue, edge{dep, d})
		}
	}
	return ""
}

// unreachableOnChange reports a `paths:` filter that cannot start this workflow
// on a change to the files that decide what the dry-run validates.
//
// THE LAST DOOR, AND THE ONE ALREADY AJAR. This guard closes six ways the dry-run
// stops running — deleted, disabled, conditional, swallowed, skipped via `needs:`,
// applied client-side — and never read `on:`. lint.yml IS path-filtered, and
// nothing asserted the filter still reaches the three files whose contents this
// gate compares: the LKE-E pin, its second copy, and the workflow itself. Drop
// `.github/workflows/**` and a PR moving KIND_NODE_IMAGE starts no run of the
// workflow it edits — so neither half of the gate sees the change that needed
// them. (`tools/**` is separately held by TestEveryLocalLintTreeCanTriggerCI,
// which reads the Makefile's lint recipe; the workflow tree is not, which is how
// this stayed open.)
//
// A trigger with NO `paths:` runs on everything and is silent here.
func unreachableOnChange(wf workflow, file string, read []string) []string {
	// EVERY FILE THIS GATE ACTUALLY READ, by its real path.
	//
	// IT USED TO NAME THE TREES AND GUESS, and the guess made the whole check inert
	// in the configuration it shipped with: asking "can this pattern reach some file
	// under `template-scripts`?" has no answer without the filenames, and the
	// approximation counted any leading-`**` pattern as reaching everything — so
	// lint.yml's own `'**.md'` entry satisfied all five directory targets and the
	// check passed having decided nothing. Two rounds of patching the approximation
	// did not fix that, because the approximation was the defect.
	//
	// The guard has the filenames: they are what it just read. A concrete path is
	// something a glob answers exactly, in both directions, with no invented
	// directory semantics — and the rule is the honest one, that a change to any
	// file whose contents this verdict rests on must be able to start the workflow.
	need := append([]string{authorityFile, envdefFile}, read...)

	var out []string
	// A FILTER IS NOT THE ONLY WAY TO HAVE NO TRIGGER. Reducing `on:` to
	// `workflow_dispatch:` — or to `workflow_call:` — leaves no event a PR or a
	// push can raise, so the dry-run runs only when someone remembers to press it.
	if !wf.On.hasAny("pull_request", "pull_request_target", "push") {
		out = append(out, fmt.Sprintf(
			"%s has no `pull_request` or `push` trigger, so nothing a contributor does starts the\n"+
				"    dry-run — it would run only when someone remembers to dispatch it by hand", file))
	}
	for _, event := range sortedKeys(wf.On) {
		spec := wf.On[event]
		// ONE LINE PER FILTER, NOT PER FILE. A filter that misses a tree misses every
		// file in it, and forty near-identical sentences bury the one fact the reader
		// needs — which key, and a path to look at.
		undecidable := false
		for _, key := range []struct {
			name    string
			filters []string
		}{{"paths", spec.Paths}, {"paths-ignore", spec.PathsIgnore}} {
			if bad := unmodelled(key.filters); len(bad) > 0 {
				undecidable = true
				out = append(out, fmt.Sprintf(
					"%s's `on.%s.%s` uses filter syntax this guard does not model (%s): `!` depends on\n"+
						"    order, and `?`/`+`/`[]` are regex quantifiers and classes rather than the shell\n"+
						"    wildcards they resemble. Whether a change to this gate's own inputs can still\n"+
						"    start the workflow is therefore undecided here, and a guard that guessed would\n"+
						"    be guessing about the rule it enforces. Express the filter with `*` and `**`",
					file, event, key.name, strings.Join(bad, ", ")))
			}
		}
		// AND THEN STOP. Feeding an unmodelled list to the matcher anyway produced
		// the honest "cannot decide" line AND a confident "leaves X unable to start
		// this workflow" beside it — the false accusation refusing `[]` was meant to
		// prevent, printed directly under the sentence explaining the refusal.
		if undecidable {
			continue
		}

		// AN EXPLICIT `paths: []` STARTS ON NOTHING, and reading it as "no filter" made
		// it indistinguishable from an unfiltered event. Go keeps nil apart from
		// empty, so the distinction was available and simply not asked for.
		if spec.Paths != nil && len(spec.Paths) == 0 {
			out = append(out, fmt.Sprintf(
				"%s's `on.%s.paths` is empty, so no path change starts this workflow at all", file, event))
		}
		var missed, ignored []string
		for _, want := range need {
			switch {
			case len(spec.Paths) > 0 && !matchesAny(want, spec.Paths):
				missed = append(missed, want)
			case len(spec.PathsIgnore) > 0 && matchesAny(want, spec.PathsIgnore):
				ignored = append(ignored, want)
			}
		}
		if len(missed) > 0 {
			out = append(out, triggerGap(file, event, "paths", missed))
		}
		if len(ignored) > 0 {
			out = append(out, triggerGap(file, event, "paths-ignore", ignored))
		}
	}
	return out
}

// triggerGap is the one sentence both filter directions produce.
func triggerGap(file, event, key string, missed []string) string {
	sort.Strings(missed)
	also := ""
	if n := len(missed) - 1; n > 0 {
		also = fmt.Sprintf(" (and %d more)", n)
	}
	return fmt.Sprintf(
		"%s's `on.%s.%s` leaves %s%s unable to start this workflow, so a change there runs\n"+
			"    neither this gate nor its live half — they would be absent from the very PR that\n"+
			"    moved what they check", file, event, key, missed[0], also)
}

// matchesAny reports whether any filter selects the file at want.
func matchesAny(want string, filters []string) bool {
	for _, f := range filters {
		if globMatch(strings.TrimSpace(f), want) {
			return true
		}
	}
	return false
}

// reUnmodelledGlob matches the filter syntax this guard does not implement.
//
// `?`, `+` AND `[]` ARE NOT WHAT THEY LOOK LIKE. GitHub's `?` means "zero or one
// of the PRECEDING character" and `+` means "one or more of it" — regex
// quantifiers, not the shell's single-character wildcard — and `[]` is a
// character class. Guessing `?` as "any one char" made
// `.github/workflow?/**` read as reaching lint.yml when GitHub would never start
// the workflow on a KIND_NODE_IMAGE edit, and guessing `[]` as literal made
// `[t]ools/**` a false accusation. One construct wrong in each direction.
//
// Refused, for the reason `!` already is: a guard that half-implements the rule
// it enforces is guessing about the rule it enforces.
var reUnmodelledGlob = regexp.MustCompile(`[?+\[]`)

// unmodelled returns the filters using syntax this guard cannot decide — the
// `!` negations, whose interaction with the positive patterns depends on order,
// and the regex-quantifier constructs above.
func unmodelled(filters []string) []string {
	//
	// DROPPING A NEGATION WAS A FAIL-OPEN IN THE ONE PLACE THAT MATTERS. It
	// SUBTRACTS from what the filter selects, so ignoring it can only make the guard
	// believe a path is reachable when it is not — `- '!.github/workflows/**'` beside
	// the existing entries stops a PR editing KIND_NODE_IMAGE starting Lint at all,
	// with the gate printing OK. "Cannot tell" is reported as such.
	var out []string
	for _, f := range filters {
		if f = strings.TrimSpace(f); strings.HasPrefix(f, "!") || reUnmodelledGlob.MatchString(f) {
			out = append(out, f)
		}
	}
	return out
}

// globMatch implements the part of GitHub's path-filter glob this guard models:
// `*` within one segment, `**` across `/`, everything else literal. `?`, `+`,
// `[]` and `!` never reach here — unmodelled() refuses them upstream, because
// each means something other than it looks like.
//
// `'**.md'` IS THE FORM THAT WORKS — this repo learned that from a filter written
// `'**/*.md'` that matched nothing — so `**` is deliberately not required to be a
// whole segment.
func globMatch(pattern, name string) bool {
	// p and n are byte offsets; star/nameStar remember the last `**` for backtracking.
	var p, n, star, nameStar int
	star = -1
	for n < len(name) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			if p+1 < len(pattern) && pattern[p+1] == '*' {
				p += 2
				star, nameStar = p, n
				continue
			}
			// A single `*` consumes anything but `/`, with its own local backtrack.
			if m := singleStar(pattern[p+1:], name[n:]); m {
				return true
			}
			if star >= 0 {
				p, nameStar = star, nameStar+1
				n = nameStar
				continue
			}
			return false
		case p < len(pattern) && pattern[p] == name[n]:
			p++
			n++
		case star >= 0:
			p, nameStar = star, nameStar+1
			n = nameStar
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// singleStar matches `*rest` against name, where the star may not cross a `/`.
func singleStar(rest, name string) bool {
	for i := 0; i <= len(name); i++ {
		if i > 0 && name[i-1] == '/' {
			return false
		}
		if globMatch(rest, name[i:]) {
			return true
		}
	}
	return false
}

// dryRunHalf requires the job to still carry a server-side apply that can fail.
//
// This gate's whole subject is the FIDELITY of `kubectl apply --dry-run=server`,
// and it asserted the cluster, the kubectl and the live minor check without ever
// asserting the apply. Deleting that step, flipping it to `--dry-run=client`, or
// marking it continue-on-error left the guard certifying the server a vanished
// check would have talked to.
func dryRunHalf(wf workflow, jobName string) string {
	seen := 0
	for _, st := range wf.Jobs[jobName].Steps {
		if !strings.Contains(unquotedBody(uncommented(st.Run)), dryRunApply) {
			continue
		}
		seen++
		// EVERY apply is held to it, not the first. A second, non-blocking apply
		// behind a healthy one used to pass, and a conditional one placed AHEAD of
		// the real one used to fail the gate on its own. Neither is a judgement this
		// should make by position.
		what := fmt.Sprintf("job %q's `%s` apply", jobName, dryRunApply)
		if why := disabledReason(what, st.If, st.ContinueOnError); why != "" {
			return why
		}
		if why := shellWithoutExitOnError(what, effectiveShell(wf, jobName, st.Shell)); why != "" {
			return why
		}
		if why := swallowReason(what, st.Run); why != "" {
			return why
		}
	}
	if seen > 0 {
		return ""
	}
	return fmt.Sprintf("job %q runs no `%s` apply. That check is what this whole gate exists to keep\n"+
		"    honest — holding a cluster to the deployed minor means nothing if no manifest is sent to it.\n"+
		"    If the dry-run moved, point this guard at its new home; if it was removed, remove the guard\n"+
		"    in the same commit rather than leaving it green over nothing", jobName, dryRunApply)
}

// uncommented drops `#` comments from a shell body.
//
// The signature match is a substring search, so a commented-out check read as a
// live one — the loudest possible way to disable a step while satisfying the
// thing that guards it. Stripping only WHOLE-LINE comments left the trailing
// spelling open (`echo skipping  # kubectl version … ${KIND_NODE_IMAGE}`), which
// is the same hole one character over.
//
// IT WAS TWO FUNCTIONS AND THE PAIR WAS THE BUG. This one applied the word-boundary
// rule and over-stripped a `#` inside a quoted string; its sibling dropped only
// whole-line comments and so kept a trailing one. Each was fail-closed
// for one direction of search and fail-open for the other, so every call site had a
// direction to get wrong and one of them always had:
//
//	`echo "phase #2" && kind create cluster`   the quoted `#` hid a second cluster
//	`helm repo update  # never kind create`    the prose refused the whole tree
//
// A `#` starts a comment when it begins a WORD — line start, or preceded by
// whitespace — AND is not inside quotes. That is the shell's actual rule and it
// answers both: `${KIND_NODE_IMAGE#kindest/node:v}` keeps its parameter expansion
// (no whitespace before the `#`), a quoted `#` stays, and a real comment goes.
func uncommented(run string) string {
	var keep []string
	for _, line := range strings.Split(run, "\n") {
		if i := commentStart(line); i >= 0 {
			line = line[:i]
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// reShellCreate matches a `kind create`, however the binary is reached.
//
// COMMAND POSITION WAS TOO NARROW, and the gap was an idiom this repo uses:
// `template-scripts/ci/with-retry.sh kind create cluster` puts a wrapper in front
// of it, as do `timeout 600 kind create` and `nohup kind create … &`, and the
// same-line `then`/`do` forms. Every one stands up a second cluster and every one
// slid past a rule anchored on the first word.
//
// So the phrase is searched for ANYWHERE in the unquoted text. That reverses the
// round-24 trade deliberately: stripping quotes is fail-open for a refusal, and
// what it now misses is a create hidden inside a quoted `-c` string — which the
// second pattern picks up — while what command position missed was every ordinary
// wrapper. A narrow miss traded for a wide one.
var reShellCreate = regexp.MustCompile(
	// A NAMED kind — bare, by path, or with a runner in front — takes any `create`.
	// Anything ELSE has to say `create cluster`, which is kind's own subcommand and
	// nothing else's here: `"$KUBECTL" create namespace "$ns"` is not a cluster, and
	// hard-failing it with "use helm/kind-action" would be a false accusation of the
	// loudest kind. Matched without requiring the binary's token, because a quoted
	// `"$KIND"` is gone by the time this reads the line.
	`(^|[\s;&|(])((kind|[^\s;&|()]*/kind)\s+create(\s|$)|create\s+cluster(\s|$))`)

// reShellCreateQuoted catches the one spelling stripping quotes would lose: a
// create handed to another shell as a `-c` argument.
var reShellCreateQuoted = regexp.MustCompile(`-c\s*['"][^'"]*\bkind\s+create\b`)

// shellCreatesCluster reports whether a body stands up a cluster outside the
// action, in either spelling.
func shellCreatesCluster(body string) bool {
	// unquotedBody, NOT unquoted: quote state must not cross a newline. Run
	// line-scoped over a whole file, one unbalanced quote swallows every later
	// line — live on this repo's own corpus, where install-llz.sh's multi-line
	// `tag_query='…'` hid 235 characters from the refusal.
	clean := uncommented(body)
	return reShellCreate.MatchString(unquotedBody(clean)) || reShellCreateQuoted.MatchString(clean)
}

// disablers are the shell constructs that can stop a load-bearing step failing.
//
// ────────────────────────────────────────────────────────────────────────────
// REFUSED WHEREVER THEY APPEAR, RATHER THAN TRACED TO THE COMMAND.
//
// This began as an analysis: find the apply, find what reaches it, decide whether
// its exit status survives. Six review rounds took fifteen findings out of that
// analysis and showed no sign of stopping, in both directions at once — `|| true`
// on a cleanup blamed on the apply, `set +e` in a subshell latching for the
// parent, a pipeline continued by a trailing `|` read as two commands, `||`
// counted as a pipe, a quoted mention taken for the command. Each fix was right
// and each exposed the next, because deciding what an arbitrary shell body does
// to an exit status is a shell interpreter, and a regex approximation of one is
// wrong in a new way every time somebody looks.
//
// So the guard stops approximating and applies the rule it already applies to
// negated path filters, to `?`/`+`/`[]`, and to a live check written without jq:
// DECIDE THE SIMPLE SHAPE, REFUSE THE REST. A step this gate depends on may not
// contain a construct that can discard an exit status — anywhere in it, no
// scoping, no interpretation. That question has an exact answer.
//
// WHAT IT COSTS, PLAINLY: a legitimate `rm -rf rendered/ || true` cleanup beside
// the apply is refused, and the remedy is to put it in its own step. That is a
// one-line edit, it is what the failure says, and it buys a rule whose answer
// does not depend on how well anybody modelled bash. The alternative on offer was
// a mini-interpreter nobody can finish.
var disablers = []struct {
	re   *regexp.Regexp
	what string
}{
	// ANY `||`, not the two spellings that read as obviously inert. `a || echo
	// failed` discards a's status exactly as `a || true` does, and enumerating
	// right-hand sides is the guessing this list replaced.
	{regexp.MustCompile(`\|\|`), "an `||`, whose left side cannot fail the job"},
	// A TRAILING `&` BACKGROUNDS THE COMMAND, so the step's status is the shell's
	// and not the check's. `&&` is fine and must not match.
	{regexp.MustCompile(`[^&]&(\s|$)`), "a `&` backgrounding a command"},
	// `+o pipefail` TOO: without it a failure anywhere but the end of a pipeline is
	// discarded, which is the very hole shellWithoutExitOnError refuses GitHub's
	// default shell to prevent — reachable from inside the step until now.
	{regexp.MustCompile(`(^|[\s;(])set\s+(\+[a-zA-Z]*e[a-zA-Z]*|\+o\s+(errexit|pipefail))(\s|;|$)`), "a `set` disabling exit-on-error or pipefail"},
	// `!` INVERTS IT AND A CONDITION SWALLOWS IT. Under `set -e` a command in an
	// `if`/`while`/`until` condition is exempt from errexit by definition, and `!`
	// turns a failure into a success — both leave a step that runs the check and
	// cannot fail on it. The list is finite and enumerable, which is the whole
	// reason this replaced tracing statuses through a shell.
	{regexp.MustCompile(`(^|[;&|(]|&&|\|\|)\s*!\s`), "a `!` inverting an exit status"},
	{regexp.MustCompile(`(^|[;&|(]|&&|\|\|)\s*(if|elif|while|until)\s`), "a condition, which exempts what it tests from exit-on-error"},
}

// reDisablerInC matches a disabling construct passed to another shell as a `-c`
// argument, which stripping quotes would otherwise lose.
// A SHELL's `-c`, not any `-c`. jq's own `-c` is the compact-output flag, so
// `jq -c 'if … then … end'` was reported as handing a construct to another shell
// — the jq false accusation the neighbouring unquotedBody was added to prevent,
// arriving through the clause that was supposed to keep its fail-open narrow.
var reDisablerInC = regexp.MustCompile(`\b(ba|z|da|k)?sh\s+(-[a-z]*\s+)*-c\s*['"][^'"]*(\|\||set\s+\+[a-zA-Z]|(^|\s)!\s|\b(if|while|until)\s)`)

// swallowReason reports a disabler anywhere in a load-bearing step's body.
func swallowReason(what, run string) string {
	// FLATTENED, because a body-wide rule has no use for line boundaries — and
	// because they were where the last three findings lived: `… ||` wrapped onto
	// the next line, a pipeline continued on `|`, a `\` fold. None of that can
	// hide anything from a search over the whole step as one string.
	// UNQUOTED, like its two sibling checks on the same body. A jq program is a
	// quoted argument, and jq's own `(if … then … end)` was being read as a shell
	// condition — a false accusation under the one step whose rewrite is most
	// likely to reach for it. The `-c` clause keeps the fail-open narrow: a
	// disabler handed to another shell as a string is still one.
	body := flatten(unquotedBody(uncommented(run)))
	if m := reDisablerInC.FindString(flatten(uncommented(run))); m != "" {
		return fmt.Sprintf("%s hands a disabling construct to another shell (%q). A step this gate\n"+
			"    depends on may not carry anything that can discard an exit status — this guard decides\n"+
			"    the simple shape and refuses the rest rather than interpreting shell. If that is\n"+
			"    cleanup, give it its own step", what, strings.TrimSpace(m))
	}
	for _, d := range disablers {
		if m := d.re.FindString(body); m != "" {
			return fmt.Sprintf("%s contains %s (%q). A step this gate depends on may not carry anything\n"+
				"    that can discard an exit status — this guard decides the simple shape and refuses the\n"+
				"    rest rather than interpreting shell. If that is cleanup, give it its own step",
				what, d.what, strings.TrimSpace(m))
		}
	}
	return ""
}

// reJQExitFlag matches the flag that makes jq report its result.
var reJQExitFlag = regexp.MustCompile(`(^|\s)(--exit-status|-[a-zA-Z]*e[a-zA-Z]*)(\s|$)`)

// jqWithoutExitStatus returns the first `jq` invocation in body that carries no
// `-e`, or "".
func jqWithoutExitStatus(body string) string {
	for _, seg := range splitCommands(flatten(unquotedBody(uncommented(body)))) {
		if !strings.Contains(seg, "jq ") || reJQExitFlag.MatchString(seg) {
			continue
		}
		return strings.TrimSpace(seg)
	}
	return ""
}

// flatten makes an ALREADY-CLEANED body one line, preserving what each newline
// MEANT.
//
// IT USED TO UNCOMMENT AS WELL, and that let a caller write
// `unquotedBody(flatten(run))` — which is backwards: flatten removes the
// newlines, so the per-line quote scoping unquotedBody exists for was gone by the
// time it ran, and one `don't` in a heredoc hid every later `|| true`. Cleaning
// is the caller's, in the order the caller needs.
//
// A NEWLINE IS A COMMAND SEPARATOR and a continuation is not, and flattening both
// to a space erased the difference: with `set -euo pipefail` on line one and a
// bare `jq` on line two, the two ended up in one segment and the `set`'s `-e`
// satisfied the jq's requirement. So a line that ends mid-command joins with a
// space, and every other newline becomes the `;` it already was.
func flatten(body string) string {
	var out []string
	pending := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimRight(line, " \t")
		if rest, ok := strings.CutSuffix(trimmed, "\\"); ok {
			pending += rest + " "
			continue
		}
		if strings.HasSuffix(trimmed, "&&") || strings.HasSuffix(trimmed, "|") {
			pending += trimmed + " "
			continue
		}
		out = append(out, pending+trimmed)
		pending = ""
	}
	if pending != "" {
		out = append(out, pending)
	}
	return strings.Join(out, " ; ")
}

// splitCommands cuts a line at the shell's command separators, so each `jq` is
// judged on its own invocation rather than on whatever shares its line.
func splitCommands(line string) []string {
	return regexp.MustCompile(`\|\||&&|[|;&]`).Split(line, -1)
}

// reClientFlag matches `--client` as a whole flag, so `--client-key=…` is not it.
// `--client=false` CONTACTS THE SERVER, so only the bare flag and `=true` are the
// thing being refused; rejecting the negated form told its author their check
// never reaches the API server when it does.
var reClientFlag = regexp.MustCompile(`--client(\s|$|=true\b)`)

// unquotedBody applies unquoted LINE BY LINE.
//
// QUOTE STATE MUST NOT CROSS A NEWLINE HERE. Run over a whole body, one
// unbalanced quote — `echo "a \" b"`, since escapes are not modelled — swallowed
// every later line, and the gate reported "job runs no `--dry-run=server` apply"
// on a workflow that has one. Per line, a malformed line costs that line and
// nothing else.
func unquotedBody(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = unquoted(line)
	}
	return strings.Join(lines, "\n")
}

// unquoted removes single- and double-quoted spans from a shell line.
//
// A MENTION IS NOT AN INVOCATION. The must-contain searches are substring
// searches, so `echo 'we used to run kubectl apply --dry-run=server here'`
// satisfied the apply's signature and the guard certified a job that applies
// nothing — the same "certifies a check it never asserts" the signature exists to
// prevent, one character-class over from the commented-out spelling.
//
// ONLY THE COMMAND-SHAPED HALVES GO THROUGH IT. The live check's own body quotes
// `"${KIND_NODE_IMAGE#kindest/node:v}"` and its jq program, so stripping quotes
// before looking for the env var would lose the real step; the env var may appear
// anywhere, while `kubectl version` and `--dry-run=server` may not appear only
// inside quotes. An apply genuinely wrapped in `bash -c "…"` reads as absent,
// which is loud and the safe direction.
func unquoted(line string) string {
	// COMMAND SUBSTITUTION IS EXECUTED, so quotes around it hide nothing. Deleting
	// `"$(kubectl version -o json)"` made the one-line `test "$(…)" = "v${…}"`
	// spelling read as an ABSENT live step, and the gate told the author to add a
	// step that was already there. The quotes are literal; what is inside `$( … )`
	// is a command like any other, and is handled by the SAME rules — hence the
	// recursion, without which a nested `"$(b "$(c)")"` came apart.
	var b strings.Builder
	var quote byte
	for i := 0; i < len(line); i++ {
		// NOT INSIDE SINGLE QUOTES, where the shell does not expand it either —
		// `echo 'we used to run $(kubectl apply --dry-run=server …)'` is a string,
		// and treating it as a command let a mention stand in for the apply. The
		// same hole this function exists to close, one quote character over.
		if line[i] == '$' && i+1 < len(line) && line[i+1] == '(' && quote != '\'' {
			end, ok := matchParen(line, i+1)
			if !ok {
				// Unbalanced: treat the remainder as command text rather than
				// discarding it, which is the fail-closed direction for a search that
				// must not lose a command.
				b.WriteString(unquoted(line[i+2:]))
				return b.String()
			}
			b.WriteString(unquoted(line[i+2 : end]))
			i = end
			continue
		}
		switch {
		case quote != 0:
			if line[i] == quote {
				quote = 0
			}
		case line[i] == '\'' || line[i] == '"':
			quote = line[i]
		default:
			b.WriteByte(line[i])
		}
	}
	return b.String()
}

// matchParen returns the index of the `)` closing the `(` at open.
func matchParen(s string, open int) (int, bool) {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// commentStart returns the index where a shell comment begins on line, or -1.
//
// A `#` counts when it starts a word — line start, or preceded by whitespace —
// AND is not inside a single- or double-quoted span.
func commentStart(line string) int {
	var quote rune
	prevSpace := true
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '#' && prevSpace:
			return i
		}
		prevSpace = r == ' ' || r == '\t'
	}
	return -1
}

// runAlways is the set of `if:` expressions that GUARANTEE the step runs.
//
// A FALSE ACCUSATION IS WORSE THAN A MISS, and flagging every `if:` was one:
// `if: always()` makes a step run more, not less, and refusing it told the reader
// to remove the thing keeping their check unconditional. The list is exact
// spellings rather than an expression evaluator — anything outside it is still a
// condition this guard cannot decide, which is the honest answer.
var runAlways = map[string]bool{
	"always()": true,
	// `success()` is the DEFAULT written out, and `true` is a step that always
	// runs. Refusing either told the reader their no-op was a condition.
	"success()":              true,
	"true":                   true,
	"!cancelled()":           true,
	"! cancelled()":          true,
	"success() || failure()": true,
	"failure() || success()": true,
}

// alwaysRuns reports whether an `if:` value is one of those spellings, with the
// `${{ }}` wrapper and whitespace normalised away.
func alwaysRuns(ifExpr any) bool {
	e := strings.TrimSpace(fmt.Sprint(ifExpr))
	if inner, ok := strings.CutPrefix(e, "${{"); ok {
		if inner, ok = strings.CutSuffix(inner, "}}"); ok {
			e = strings.TrimSpace(inner)
		}
	}
	return runAlways[e]
}

// truthy reads an Actions boolean-or-expression field. Anything that is not
// absent, `false`, or empty counts as set — including an expression, which this
// guard cannot evaluate and must not assume evaluates to false.
func truthy(v any) bool {
	if v == nil {
		return false
	}
	switch s := strings.TrimSpace(fmt.Sprint(v)); s {
	case "", "false":
		return false
	default:
		return true
	}
}

// serverFrom names where the server minor came from, so a skew failure does not
// look like it was measured against the tfvars pin when it was not (and vice
// versa).
func serverFrom(fromNodeImage bool) string {
	if fromNodeImage {
		return "  (server = the node image above)"
	}
	return "  (server = the deployed pin; the node image could not be read)"
}

// lookupEnv resolves name through the three scopes Actions composes a step's
// environment from — step, then job, then workflow, innermost first.
//
// It is the ONE walk, used both for `${{ env.X }}` in a `with:` and for a shell
// `$X` in a `run:`, because those two see the same environment. Two copies of
// this would be two answers to "what does the step read", which is the question
// the whole gate turns on.
func lookupEnv(wf workflow, jobName string, step actionStep, name string) (string, bool) {
	for _, scope := range []scalars{step.Env, wf.Jobs[jobName].Env, wf.Env} {
		if val, ok := scope[name]; ok {
			return val, true
		}
	}
	return "", false
}

// usesAction reports whether a step's `uses:` names the action, which must be
// given lower-case as owner/repo with NO ref.
//
// THE REF IS STRIPPED, NOT REQUIRED. Matching on `owner/repo@` missed
// `uses: helm/kind-action` with no ref at all — a legal step, and a second
// cluster the "one kind step in the repo" rule never saw. Case is ignored for the
// same class of reason: GitHub resolves owner/repo without regard to it, and the
// canonical spelling of the OTHER action here is `Azure/setup-kubectl`.
func usesAction(uses, name string) bool {
	ref, _, _ := strings.Cut(strings.TrimSpace(uses), "@")
	return strings.EqualFold(ref, name)
}

// sameVersion compares two version pins ignoring a leading `v`, which the two
// steps spell differently: azure/setup-kubectl wants `v1.34.10`, kind-action
// accepts either.
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// setupKubectlPin returns the version the job's azure/setup-kubectl step installs,
// empty if the job has no such step.
//
// Two of them is an error rather than a pick: the guard would vouch for whichever
// it read first, and the thing being checked IS which install wins.
func setupKubectlPin(wf workflow, jobName string) (pin, name string, err error) {
	var found []actionStep
	for _, st := range wf.Jobs[jobName].Steps {
		if usesAction(st.Uses, setupKubectlPrefix) {
			found = append(found, actionStep{If: st.If, ContinueOnError: st.ContinueOnError, Shell: st.Shell, Env: st.Env, With: st.With})
		}
	}
	switch len(found) {
	case 0:
		return "", setupKubectlPrefix, nil
	case 1:
		v, rerr := resolve(wf, jobName, found[0], "version")
		if rerr != nil {
			return "", setupKubectlPrefix, rerr
		}
		if v == "" {
			return "", setupKubectlPrefix, fmt.Errorf("installs kubectl without pinning `version:`")
		}
		return v, setupKubectlPrefix, nil
	default:
		return "", setupKubectlPrefix, fmt.Errorf("appears %d times in one job; which kubectl the job runs is what this compares", len(found))
	}
}

// distance describes how far apart two versions are, in the units they are
// actually apart in.
//
// THE SCALE WAS AN IMPLEMENTATION DETAIL AND IT LEAKED. The comparison packs
// major and minor into `major*1000 + minor`, and the message printed that number
// as minors — so kubectl v2.0.0 against a 1.34 server read "966 minors from".
// Correct verdict, nonsense diagnosis, which is the half of a gate's output the
// reader actually acts on.
func distance(a, b minor) string {
	if a.major != b.major {
		return "a different major from"
	}
	n := abs(a.minor - b.minor)
	unit := "minors"
	if n == 1 {
		unit = "minor"
	}
	return fmt.Sprintf("%d %s from", n, unit)
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}

// authority reads the LKE-Enterprise pin this repo deploys, and refuses to
// proceed unless its two copies agree.
func authority(repo capability.Repo) (minor, string, error) {
	b, err := repo.ReadFile(authorityFile)
	if err != nil {
		return minor{}, "", fmt.Errorf("k8s-minor-coherence: read %s: %w", authorityFile, err)
	}
	raw := strings.Trim(tfvars.Value(string(b), authorityKey), `"`)
	if raw == "" {
		// FAIL CLOSED. An authority that says nothing makes every comparison below
		// vacuous, and a guard that compares against nothing reports the same green
		// as one that compared and agreed.
		return minor{}, "", fmt.Errorf("k8s-minor-coherence: %s declares no %s — refusing to pass vacuously",
			authorityFile, authorityKey)
	}
	m, err := parseMinor(raw)
	if err != nil {
		return minor{}, "", fmt.Errorf("k8s-minor-coherence: %s: %s = %q: %w", authorityFile, authorityKey, raw, err)
	}

	eb, err := repo.ReadFile(envdefFile)
	if err != nil {
		return minor{}, "", fmt.Errorf("k8s-minor-coherence: read %s: %w", envdefFile, err)
	}
	sub := reEnvdefFallback.FindSubmatch(eb)
	if sub == nil {
		return minor{}, "", fmt.Errorf("k8s-minor-coherence: %s no longer states a fallback for %s.\n"+
			"  It is the second copy of the deployed pin, and this guard checks the two agree before\n"+
			"  comparing anything to them. If the fallback moved, point reEnvdefFallback at it — a\n"+
			"  restatement the scanner cannot see is not a restatement that agrees", envdefFile, authorityKey)
	}
	if fallback := string(sub[1]); fallback != raw {
		return minor{}, "", fmt.Errorf("k8s-minor-coherence: the deployed pin has two values.\n"+
			"  %s  %s = %q\n"+
			"  %s  fallback %q\n"+
			"  Nothing can be held to \"the minor we deploy\" while there are two of them; make them equal",
			authorityFile, authorityKey, raw, envdefFile, fallback)
	}
	return m, raw, nil
}

// readWorkflows parses every Actions workflow in the tree.
//
// A workflow it cannot parse is one it cannot vouch for, and an empty directory
// is what a wrong --root looks like — both are failures rather than an empty set
// with a clean verdict.
func readWorkflows(repo capability.Repo) (map[string]workflow, error) {
	out := map[string]workflow{}
	collect := func(path string, body []byte) error {
		var wf workflow
		if err := yaml.Unmarshal(body, &wf); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		out[path] = wf
		return nil
	}
	for _, root := range yamlRoots {
		// Every root but the first is OPTIONAL — a checkout may carry no composites
		// and no scaffold — while a missing .github/workflows is a wrong --root.
		if err := walkFiles(repo, root, root != workflowsDir, yamlSuffixes, collect); err != nil {
			return nil, fmt.Errorf("k8s-minor-coherence: %w", err)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("k8s-minor-coherence: no workflows under %s — refusing to pass vacuously", workflowsDir)
	}
	return out, nil
}

// yamlSuffixes and shSuffixes are the two corpora this guard reads.
var (
	yamlSuffixes = []string{".yml", ".yaml"}
	// nil means every file: what makes a file shell is decided by isShell, not by
	// its name. `template-scripts/hooks/pre-commit` and `pre-push` already prove
	// the `.sh` convention is not universal here, and a `kind create` in one of
	// them was invisible to the refusal.
	everyFile []string = nil
)

// yamlRoots is every tree that can hold a step standing up a cluster: workflows
// and composite actions, here and in the scaffold an adopter carries.
//
// THE CLAIM IS "ONE SUCH STEP IN THE REPO" AND IT HAS TO BE CHECKED THAT WIDELY.
// A composite can stand up a cluster and carries no `jobs:`, so a kind step there
// is always a second one; and scanning only this repo's own .github left
// instance-template's workflows outside a rule stated over the whole tree, which
// is the same half-measure as reading one file was. It is also what the sibling
// guards do — setup-go-sole-site holds its rule across both trees for the same
// reason.
var yamlRoots = []string{
	workflowsDir,
	".github/actions",
	"instance-template/.github/workflows",
	"instance-template/.github/actions",
}

// scriptRoots is where a `run:` routes its shell.
//
// SCANNED FOR THE SAME REASON THE `run:` BODIES ARE, and this repo makes it
// likelier than it sounds: `workflow-inline-bash` is a ratchet at zero headroom
// for BLOCK scalars, so the sanctioned way to add more than a line of shell to a
// workflow is to move it into a script.
// Reading only inline bodies meant the very refactor this repo asks for would
// hide a `kind create` from the check that refuses it — and reading only
// template-scripts left the scripts sitting beside the workflows themselves.
var scriptRoots = []string{"template-scripts", ".github", "instance-template/.github"}

// walkFiles hands every file under root with one of suffixes to fn.
//
// ONE WALK FOR BOTH CORPORA. It was two, and the duplicate carried its own copy
// of the absent-root rule and its own unread error arms — the shape this repo
// keeps paying for. `optional` is the only thing that differs between the
// callers, so it is the only thing that is a parameter.
func walkFiles(repo capability.Repo, root string, optional bool, suffixes []string, fn func(path string, body []byte) error) error {
	return repo.WalkDir(filepath.FromSlash(root), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if optional && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if suffixes != nil {
			match := false
			for _, suf := range suffixes {
				match = match || strings.HasSuffix(d.Name(), suf)
			}
			if !match {
				return nil
			}
		}
		b, rerr := repo.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return fn(filepath.ToSlash(p), b)
	})
}

// shellWithoutExitOnError reports a custom `shell:` template that drops `-e`.
//
// GitHub's NAMED shells all carry it — `bash` expands to
// `bash --noprofile --norc -eo pipefail {0}`, `sh` to `sh -e {0}` — so a failing
// command aborts the step and a line after it changes nothing. A CUSTOM template
// (one containing `{0}`) is whatever the author wrote, and `bash {0}` runs on
// past a failure: the check is present, gating by its YAML, and unable to fail.
//
// This is the honest version of a finding that arrived as "the jq must be the
// step's LAST line, or a trailing `echo ok` makes it inert". Under `-e` that is
// not so — the failing line aborts first — and it is measurably not so: `bash
// -eo pipefail -c 'false; echo ok'` exits 1. Dropping `-e` is the way the premise
// becomes true, so that is what is checked.
func shellWithoutExitOnError(what, shell string) string {
	// THE RULE IS PIPEFAIL, and only one spelling gives it. GitHub expands a named
	// `shell: bash` to `bash --noprofile --norc -eo pipefail {0}`; everything else
	// on offer lacks it — the unset default is `bash -e {0}`, `shell: sh` is
	// `sh -e {0}`, and a custom template is whatever was written. Refusing only the
	// unset case waved through the two spellings that carry the very hazard the
	// refusal exists for, while the live half is a pipeline.
	switch {
	case shell == "bash":
		return ""
	case strings.Contains(shell, "{0}") && strings.Contains(shell, "pipefail") && reExitOnError.MatchString(shell):
		return ""
	}
	named := "GitHub's default shell (`bash -e {0}`)"
	if shell != "" {
		named = fmt.Sprintf("`shell: %s`", shell)
	}
	return fmt.Sprintf("%s runs under %s, which does not set `pipefail`, so a failure anywhere\n"+
		"    but the last command of a pipeline is discarded. Declare `shell: bash` (workflow\n"+
		"    `defaults.run`, the job, or the step) — that is the one spelling GitHub expands to\n"+
		"    `-eo pipefail`", what, named)
}

// reExitOnError matches the errexit flag in a custom shell template.
var reExitOnError = regexp.MustCompile(`(^|\s)(-[a-zA-Z]*e[a-zA-Z]*|-o\s+errexit)(\s|$)`)

// effectiveShell resolves a step's shell through the scopes GitHub composes it
// from: the step, then the job's `defaults: run:`, then the workflow's.
func effectiveShell(wf workflow, jobName, stepShell string) string {
	for _, sh := range []string{stepShell, wf.Jobs[jobName].Defaults.Run.Shell, wf.Defaults.Run.Shell} {
		if sh = strings.TrimSpace(sh); sh != "" {
			return sh
		}
	}
	return ""
}

// disabledReason says why a step (or job) cannot fail the run, or "" if it can.
//
// A CHECK THAT CANNOT FAIL IS NOT A CHECK, and the three load-bearing steps in
// this job are each held to that: the kind create, the live minor assertion, and
// the server-side apply. `continue-on-error` discards the exit status outright;
// an `if:` makes running it conditional on something this guard cannot evaluate,
// which is indistinguishable from `if: false` from here.
func disabledReason(what string, ifExpr, continueOnError any) string {
	if truthy(continueOnError) {
		return fmt.Sprintf("%s sets `continue-on-error`, so its exit status is discarded", what)
	}
	if ifExpr != nil && !alwaysRuns(ifExpr) {
		return fmt.Sprintf("%s is conditional (`if: %v`), so whether it runs at all depends on a condition this guard cannot evaluate", what, ifExpr)
	}
	return ""
}

// actionStep is the shape findKindStep and setupKubectlPin hand back — declared so
// the caller reads `step.With[...]` rather than an anonymous struct literal.
//
// It carries the step's own `env:` because that is the innermost of the three
// scopes an `${{ env.X }}` in its `with:` resolves against, and therefore the one
// that decides. Reading only the outer two is worse than not resolving at all: an
// unknown name fails closed, but a name shadowed a scope in is answered
// CONFIDENTLY AND WRONGLY, which is the silent-disagreement class this whole gate
// is about.
type actionStep struct {
	If              any
	ContinueOnError any
	Shell           string
	Env             scalars
	With            scalars
}

// findKindStep returns the one step that stands up the kind cluster.
//
// Two of them would mean two API servers and one comparison, so that is an error
// rather than a choice this guard makes silently.
func findKindStep(all map[string]workflow) (step actionStep, file, jobName string, err error) {
	var found []actionStep
	var where []string
	for _, f := range sortedKeys(all) {
		for _, name := range sortedKeys(all[f].Jobs) {
			for _, s := range all[f].Jobs[name].Steps {
				if usesAction(s.Uses, kindActionPrefix) {
					found = append(found, actionStep{If: s.If, ContinueOnError: s.ContinueOnError, Shell: s.Shell, Env: s.Env, With: s.With})
					where = append(where, f+" (job "+name+")")
				}
			}
		}
		// A composite's steps carry no job, so one here can only ever be a SECOND
		// cluster — it lands in the same tally and the count below refuses it.
		for _, s := range all[f].Runs.Steps {
			if usesAction(s.Uses, kindActionPrefix) {
				found = append(found, actionStep{If: s.If, ContinueOnError: s.ContinueOnError, Shell: s.Shell, Env: s.Env, With: s.With})
				where = append(where, f+" (composite)")
			}
		}
	}
	switch len(found) {
	case 0:
		// FAIL CLOSED. No kind step means either the dry-run job was deleted — in
		// which case this guard is protecting a gate that no longer exists and
		// should be deleted with it — or it moved and the guard has gone blind.
		return actionStep{}, "", "", fmt.Errorf("k8s-minor-coherence: no `uses: %s` step anywhere under %s.\n"+
			"  That step is the server-side dry-run's API server. If the dry-run job was removed, remove\n"+
			"  this guard in the same commit rather than leaving it green over nothing", kindActionPrefix, workflowsDir)
	case 1:
		f, j, isJob := strings.Cut(where[0], " (job ")
		if !isJob {
			// A composite cannot hold the dry-run job — it has no `jobs:` — so the
			// one kind step in the repo being there means the dry-run is gone.
			return actionStep{}, "", "", fmt.Errorf("k8s-minor-coherence: the only `%s` step is in %s, a composite action.\n"+
				"  A composite has no `jobs:`, so nothing there can be the server-side dry-run this gate\n"+
				"  exists to keep honest", kindActionPrefix, strings.TrimSuffix(where[0], " (composite)"))
		}
		return found[0], f, strings.TrimSuffix(j, ")"), nil
	default:
		// SEVERAL CLUSTERS, ONE COMPARISON. This used to scan one hardcoded file, so
		// a second kind step in a DIFFERENT workflow was not a conflict but a blind
		// spot: it would boot kind's default node image and re-create #427 one file
		// over, with this gate green. Across the tree they are the same finding.
		return actionStep{}, "", "", fmt.Errorf("k8s-minor-coherence: %d `%s` steps under %s (%s).\n"+
			"  This guard holds ONE cluster to the deployed minor; with several it would vouch for\n"+
			"  whichever it happened to read first, and the others would validate against whatever\n"+
			"  node image kind defaults to", len(found), kindActionPrefix,
			strings.Join(yamlRoots, "/, ")+"/", strings.Join(where, ", "))
	}
}

// namesBothInOneCommand reports whether some `;`-delimited command list in the
// step runs `kubectl version` AND references the node-image env var.
func namesBothInOneCommand(run string) bool {
	for _, group := range strings.Split(flatten(uncommented(run)), ";") {
		if strings.Contains(unquotedBody(group), "kubectl version") && strings.Contains(group, nodeImageEnv) {
			return true
		}
	}
	return false
}

// inlineShellClusters refuses a `kind create` written straight into a `run:`.
//
// A FINDING, LIKE ITS SCRIPT SIBLING. It used to abort findKindStep before that
// function had looked at what it found, so one inline `kind create` blanked the
// whole verdict — a tree with a drifted node image AND an inline create reported
// only the create, hiding the thing the gate is for. The match is a
// heuristic, so one false positive did the same. The two spellings of one defect
// now read the same way.
func inlineShellClusters(all map[string]workflow) string {
	var hits []string
	for _, f := range sortedKeys(all) {
		for _, s := range allSteps(all[f]) {
			if shellCreatesCluster(s.Run) {
				hits = append(hits, f)
			}
		}
	}
	if len(hits) == 0 {
		return ""
	}
	return shellClusterMessage(dedupe(hits))
}

// shellClusterMessage is the one sentence both spellings produce.
func shellClusterMessage(where []string) string {
	return fmt.Sprintf("`%s` is run from a shell in %s.\n"+
		"    This gate holds a cluster to the deployed minor by reading the kind step's `node_image:`\n"+
		"    input; a cluster stood up in a shell declares no such input, so it would boot kind's own\n"+
		"    default node image with this gate green. Stand it up through `uses: %s` instead",
		shellCreate, strings.Join(where, ", "), kindActionPrefix)
}

// noShellClusters refuses a `kind create` in the script tree a `run:` calls into.
//
// The inline half of this rule lives in findKindStep, which reads `run:` bodies.
// A workflow that calls `./template-scripts/ci/spin-kind.sh` has the same effect
// and none of the text, so both halves are needed for either to mean anything.
func noShellClusters(repo capability.Repo) (read []string, problem string) {
	var hits, walkErrs []string
	// Optional: an instance checkout carries no template-scripts of its own.
	for _, root := range scriptRoots {
		// All optional: an instance checkout has no template-scripts, and a
		// template checkout's trees are the ones the required YAML root covers.
		err := walkFiles(repo, root, true, everyFile, func(path string, body []byte) error {
			if !isShell(path, body) {
				return nil
			}
			read = append(read, path)
			if shellCreatesCluster(string(body)) {
				hits = append(hits, path)
			}
			return nil
		})
		if err != nil {
			// KEEP WALKING AND KEEP THE HITS. Returning here discarded what the
			// earlier roots had already found and skipped the later ones, so an I/O
			// problem in one tree turned a real refusal into a message about that
			// tree. The walk error is its own finding, reported beside them.
			walkErrs = append(walkErrs, fmt.Sprintf("could not read %s: %v", root, err))
		}
	}
	switch {
	case len(hits) > 0 && len(walkErrs) > 0:
		return read, shellClusterMessage(hits) + "\n    " + strings.Join(walkErrs, "; ")
	case len(hits) > 0:
		return read, shellClusterMessage(hits)
	case len(walkErrs) > 0:
		return read, "k8s-minor-coherence: " + strings.Join(walkErrs, "; ") +
			" — the no-shell-cluster refusal did not see that tree"
	}
	return read, ""
}

// isShell reports whether a file is shell, by name or by shebang.
//
// BY CONTENT, BECAUSE THE NAMING CONVENTION IS NOT UNIVERSAL. Filtering on `.sh`
// left `template-scripts/hooks/pre-commit` and `pre-push` — real shell in a
// scanned tree — outside the no-shell-clusters refusal entirely.
func isShell(path string, body []byte) bool {
	if strings.HasSuffix(path, ".sh") {
		return true
	}
	line := string(body)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if !strings.HasPrefix(line, "#!") {
		return false
	}
	return strings.Contains(line, "sh") // sh, bash, zsh, dash, env sh…
}

// allSteps is every step in a document, job steps and composite steps alike.
func allSteps(wf workflow) []step {
	out := append([]step(nil), wf.Runs.Steps...)
	for _, name := range sortedKeys(wf.Jobs) {
		out = append(out, wf.Jobs[name].Steps...)
	}
	return out
}

// dedupe keeps one entry per file, in order — a workflow that creates a cluster
// in three steps is one site to fix.
func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v], out = true, append(out, v)
		}
	}
	return out
}

// sortedKeys makes the scan order deterministic, so a failure names the same
// site on every run.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resolve returns the kind step's `input`, expanding any `${{ env.NAME }}` in it
// against the three env scopes Actions resolves such a reference through — step,
// then job, then workflow, innermost first — so a pin held in `env:` is compared
// rather than skipped.
//
// It resolves substrings too (`v${{ env.KUBECTL_VERSION }}` is how a workflow
// spells a `v`-prefixed pin), and an unknown name is an ERROR: a reference this
// guard cannot follow leaves the real value unexamined, and "unexamined" must
// never read as "fine".
func resolve(wf workflow, jobName string, step actionStep, input string) (string, error) {
	v := strings.TrimSpace(step.With[input])
	if v == "" {
		return "", nil
	}
	var missing []string
	outStr := reEnvExpr.ReplaceAllStringFunc(v, func(expr string) string {
		name := reEnvExpr.FindStringSubmatch(expr)[1]
		if val, ok := lookupEnv(wf, jobName, step, name); ok {
			return val
		}
		missing = append(missing, name)
		return expr
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("references env.%s, which is defined at no step, job or workflow level",
			strings.Join(missing, ", env."))
	}
	// Anything still carrying an expression is a form this guard does not
	// understand (a `vars.`/`inputs.` reference, a function call). Refuse rather
	// than compare a literal `${{ … }}` against a version.
	if strings.Contains(outStr, "${{") {
		return "", fmt.Errorf("%q holds an expression this guard cannot resolve; keep the pin a literal or an `env.` reference", outStr)
	}
	return outStr, nil
}
