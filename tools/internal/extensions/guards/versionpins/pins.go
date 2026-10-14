package versionpins

// ci_version_pins.go implements `llz ci version-pins` — the consistency gate over
// tool-version pins.
//
// dockerfiles/Dockerfile declares itself "the single source of truth for tool
// versions". It is not: the same numbers are restated in the build matrix, in
// workflow env blocks, in the Makefile, and in Go constants that derive
// TF_IMAGE/KUBE_IMAGE. Nothing checked them against each other.
//
// This has already gone wrong once, and the confession is still in the tree —
// citags.go, in this package:
//
//	// … It was still on 1.9.8 after the matrix moved, which would have
//	// scaffolded new instances onto a HashiCorp Terraform image while every
//	// caller invoked `tofu`.
//
// That was caught by hand. This catches it in CI: the Dockerfile ARG block is the
// authority, every restatement must agree, and a restatement nobody knew about
// still gets checked because the scan is by pattern, not by a hand-kept file list.
//
// NOT REQUIRED HERE: digest-pinning. `vars.TF_IMAGE` is the real pin and the
// `format(…ci-tofu:…)` fallback is a convenience default; requiring a digest
// there would churn on every image rebuild without making the pin more honest.
// Agreement is the property worth gating.
//
// It is REFUSED on a job's container image, though, for the same reason a `sha-`
// tag is: a digest freezes that job on one build. See reImageDigest.
//
// THE ONE INVERSION: a job's container image is required to name `:latest`
// instead of the ARG, because requiring the version there made a version bump
// self-defeating. See containerImages / expectedFallbacks below.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

const versionAuthorityFile = "dockerfiles/Dockerfile"

// buildMatrixFile is where the image matrix that actually drives the published
// tags lives. The matrix vacuity check is scoped to it, not to "anywhere in the
// scanned tree": a matching JSON pair in some other file would satisfy a
// tree-wide check while THIS file's row — the one that decides what gets
// published — sat unread. Same reasoning as keying expectedFallbacks on
// (file, image) rather than on the file alone.
const buildMatrixFile = ".github/workflows/build-images.yml"

// imagePin ties a published CI image to the Dockerfile ARG that drives its tag,
// and to the Go constant that restates it for TF_IMAGE/KUBE_IMAGE derivation.
// This table IS the mapping — declared once, read by every scanner below.
type imagePin struct {
	image   string // published image name, e.g. ci-tofu
	arg     string // Dockerfile ARG that sets its tag
	goConst string // constant restating it ("" if none)
	// aliases are other names the SAME image is published under. They carry the
	// same tag from the same build, so a reference to one is a restatement of
	// this pin and has to be gated identically.
	//
	// ci-terraform is a live deprecation window, not history: build-images.yml
	// publishes ci-tofu under both names because instances pin it through
	// vars.TF_IMAGE, an explicit pin nobody's migration is obliged to update on
	// our schedule. Omitting it here left a `ci-terraform:1.12.5` fallback gated
	// by NEITHER rule — the gate exited 0 on it — and the expectedFallbacks
	// remediation ("only drop the entry if the fallback is genuinely gone") is a
	// signposted path in. An alias is only a scan name: it has no matrix row of
	// its own (build-images carries it in the row's `alias` field) and no
	// constant, so the vacuity checks stay keyed on the primary name.
	aliases []string
}

// names is every published name this pin covers.
func (p imagePin) names() []string { return append([]string{p.image}, p.aliases...) }

var imagePins = []imagePin{
	{image: "ci-tofu", arg: "TOFU_VERSION", goConst: "CITofuTag", aliases: []string{"ci-terraform"}},
	{image: "ci-kubernetes", arg: "KUBECTL_VERSION", goConst: "CIKubernetesTag"},
}

// floatingTag is the tag a job's container image must name.
const floatingTag = "latest"

// The rule below is about a POSITION: `jobs.<id>.container.image` (and a
// service container's), the image a workflow job runs in. Usually written
// `vars.<X>_IMAGE || format('ghcr.io/{0}/<image>:<tag>', …)` — a repo variable
// with a fallback for when it is unset — but the rule does not depend on that
// or any other spelling, which is the point; see containerImages.
//
// THIS IS A SCAR. Requiring the `vars.KUBE_IMAGE ||` fallback to restate the
// Dockerfile ARG makes every KUBECTL_VERSION or TOFU_VERSION bump cost exactly one
// red Lint run — `docker pull …/ci-kubernetes:1.34.10` → `manifest unknown` —
// because build-images runs on pushes to main and Lint runs on the bump's own push,
// so the tag does not exist yet. It reads as a broken image reference rather than
// an ordering artefact, and self-heals on a re-run after the merge.
//
// So a fallback is gated the OTHER way: it must name `:latest`, the one tag
// build-images.yml republishes on every main push and therefore the only tag
// that is already published when the bump lands. The fallback exists for "no
// KUBE_IMAGE/TF_IMAGE variable is set", where a moving tag is already the
// pragmatic answer; the reproducible pin is the repo variable, which still wins
// over the fallback. That is not a hypothetical about forks — this repo sets
// NEITHER variable, so the fallback is what every Lint run here resolves, which
// is why the scar above happened here.
// Every OTHER restatement stays gated as a pin: the build matrix, the workflow
// env blocks, the Makefile, and the Go constants that derive
// TF_IMAGE/KUBE_IMAGE all still have to equal the ARG.
//
// Gated in BOTH directions on purpose. `:latest` already fell through
// reImageTag's digit-leading filter, so "tolerate it" would have been a one-line
// workflow edit and no code change — and would have left someone free to re-pin
// the fallback in good faith, restoring the trap with nothing to say so.

// ── container images, located by YAML POSITION ───────────────────────────────
//
// DO NOT GO BACK TO MATCHING THE EXPRESSION. Recognising the fallback form
// (`vars.<X>_IMAGE || format('ghcr.io/{0}/<image>:<tag>', …)`) with a regex either
// misses a legal spelling or sweeps up something innocent, in these four ways:
//
//	keyed on format('ghcr.io/{0}/…') exactly   → the owner spelled out escaped
//	loosened to "a vars.<X> || on the line"    → `${{ vars.EXTRA_FLAGS || '' }}
//	                                             ghcr.io/…/ci-tofu:1.12.5` caught
//	tightened, owner segment widened           → registry composed into the arg,
//	                                             `format( '…'`, `(format('…'))`
//	first-quoted-string-after-||               → a YAML-wrapped fallback, and any
//	                                             var not ending in _IMAGE
//
// Each escape has the same consequence and it is the worst available: a
// version-tagged fallback the float rule misses falls through to the PIN rule,
// which then REQUIRES the version — so the gate mandates the exact ordering trap it
// exists to prevent. Written `:latest` the same line matches no rule at all and the
// gate prints OK.
//
// An expression grammar is not a thing to chase with literal prefixes. So this does
// not look at the expression: it asks YAML where the value SITS. A
// `jobs.<id>.container.image` is a container image whatever it is spelled like,
// and the property that matters there does not mention fallbacks at all —
//
//	a container image must not name one of our CI images at a VERSION tag
//
// — which is true of a bare `'ghcr.io/<org>/ci-tofu:1.12.5'` with no `vars.X ||`
// anywhere, a case every version of the regex missed by construction. The
// false-positive class goes away for free: a `run:` line is not a container
// image, so it cannot be mistaken for one, and it stays covered by the pin rule
// exactly as it always was.

// containerImage is one image expression and the lines it occupies.
//
// endLine is not decoration. A value may WRAP — `image: ${{ vars.TF_IMAGE ||` on
// one line and `format('…/ci-tofu:9.9.9', …) }}` on the next is ordinary YAML — and
// with the de-dup keyed on the first line only, the pin rule matches the
// continuation and one reference gets two contradictory verdicts: FLOAT on one line
// and DRIFT on the next, whose remediation tells the reader to "bump these to
// match" the ARG, the edit that re-arms the trap.
type containerImage struct {
	value   string
	line    int
	endLine int
}

// containerImages returns every jobs.<id>.container.image and
// jobs.<id>.services.<id>.image in a workflow document.
//
// The second return says whether the document PARSED, and the caller has to act
// on it. Swallowing that was a silent pass: an unparseable workflow contributed
// no container sites, so its `container.image` fell through to the PIN rule and a
// version-tagged one was ACCEPTED — then required on the next bump. The comment
// here used to call vacuities() the backstop, and it is not: that covers only the
// files named in expectedFallbacks.
//
// It is still not an error on its own. actionlint gates workflow syntax and a
// guard failing on a file another gate is already failing on is noise — so the
// caller reports it only when the file names one of our images, which is exactly
// when this rule going quiet would matter.
//
// An EMPTY document reports false here too, and that is not a lie the caller can
// act on wrongly: it asks the MASKED body for the name, and a document with no
// content has nothing outside its comments to name one with. A separate "empty
// is fine" branch was written first and removed — it could not change an outcome,
// which makes it a second mechanism for one rule.
func containerImages(body string) (out []containerImage, parsed bool) {
	// A DECODER, not Unmarshal. Unmarshal returns the FIRST document only, so a
	// later document's container images contributed nothing and a version-tagged
	// one there fell through to the PIN rule, which REQUIRED it to restate the
	// ARG — silently, since the file parses and the parse-failure check never
	// fires. Iterating doc.Content was the first attempt at this and iterates one
	// document's roots, which is the same single document.
	dec := yaml.NewDecoder(strings.NewReader(body))
	lines := strings.Split(body, "\n")
	for n := 0; ; n++ {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, n > 0
		}
		if err != nil {
			// THE IMAGES ALREADY FOUND ARE KEPT. Discarding them handed a
			// perfectly readable document's container image back to the PIN rule,
			// which answered with "bump these to match" the ARG — the re-pin edit
			// this gate exists to prevent, printed as the remediation. It still
			// failed closed, because the parse vacuity fires too; wrong advice
			// rather than a silent pass, which is its own kind of bad.
			return dedupeImages(out), false
		}
		for _, root := range doc.Content {
			out = append(out, docImages(lines, root)...)
		}
		out = dedupeImages(out)
	}
}

// dedupeImages collapses images that are the same value at the same line.
//
// One indirection target referenced from two positions resolved to the SAME
// literal twice, which annotated one file:line twice and inflated the "N
// container image(s) float" count. The verdict was never wrong; the report was,
// and a count that does not match what a reader can see is how a gate stops
// being believed.
func dedupeImages(in []containerImage) []containerImage {
	seen := map[containerImage]bool{}
	var out []containerImage
	for _, ci := range in {
		if seen[ci] {
			continue
		}
		seen[ci] = true
		out = append(out, ci)
	}
	return out
}

func docImages(lines []string, root *yaml.Node) (out []containerImage) {
	// An ACTION runs images two ways: a Docker container action names one at
	// runs.image, and a composite action's runs.steps[] can carry
	// `uses: docker://…` exactly as a workflow job's steps do. Covering only the
	// first left the second judged as an ordinary pin and REQUIRED to restate the
	// ARG — the same claim of coverage this file has now over-stated twice, so
	// both are scanned by the same code as the job forms below.
	if runs := mapValue(root, "runs"); runs != nil {
		if ik, img := mapEntry(runs, "image"); isScalar(img) {
			out = append(out, newContainerImage(lines, ik, deref(img))...)
		}
		out = append(out, stepImages(lines, runs)...)
	}
	jobs := mapValue(root, "jobs")
	if jobs == nil {
		return out
	}
	for i := 1; i < len(jobs.Content); i += 2 {
		job := jobs.Content[i]
		// `container:` is either a mapping with an `image:` key or, in GitHub's
		// shorthand, the image string itself.
		if ck, c := mapEntry(job, "container"); c != nil {
			switch c.Kind {
			case yaml.ScalarNode, yaml.AliasNode:
				if v := deref(c); v != nil {
					out = append(out, resolveContainerImage(lines, root, job, ck, v)...)
				}
			case yaml.MappingNode:
				if ik, img := mapEntry(c, "image"); isScalar(img) {
					out = append(out, resolveContainerImage(lines, root, job, ik, deref(img))...)
				}
			}
		}
		out = append(out, stepImages(lines, job)...)
		// Service containers are pulled the same way and fail the same way — and
		// indirect the same way, which is why they go through the same resolver
		// as the job container rather than a near-copy of its call. Wiring the
		// resolver to one position at a time is how the shorthand came to be
		// missing it once already.
		if svcs := mapValue(job, "services"); svcs != nil {
			for j := 1; j < len(svcs.Content); j += 2 {
				if ik, img := mapEntry(svcs.Content[j], "image"); isScalar(img) {
					out = append(out, resolveContainerImage(lines, root, job, ik, deref(img))...)
				}
			}
		}
	}
	return out
}

// newContainerImage records the value and the line range it occupies. yaml.Node
// carries only the START line, so the end is found the way YAML itself decides
// where a plain scalar stops: continuation lines are indented deeper than the
// line the value began on.
func newContainerImage(lines []string, key, val *yaml.Node) []containerImage {
	return []containerImage{spanOf(lines, key, val)}
}

// reIndirect matches a `matrix.<key>` / `env.<key>` reference ANYWHERE in a
// container image value.
//
// NOT ANCHORED, AND THAT IS THE CORRECTION. Requiring the reference to be the
// ENTIRE value covered `${{ matrix.img }}` and missed
// `${{ vars.KUBE_IMAGE || env.KUBE_FALLBACK }}` and `ghcr.io/<org>/${{ matrix.img }}`
// — where the version-tagged literal then fell to the PIN rule, which REQUIRES
// the version. Same inverted verdict, reached by writing the same expression a
// slightly different way, which is the failure mode this whole rule was rewritten
// to stop being susceptible to.
var reIndirect = regexp.MustCompile(`\b(matrix|env|inputs)\.([A-Za-z0-9_-]+)`)

// resolveContainerImage follows one level of indirection: a container image that
// is exactly `${{ matrix.<key> }}` is judged as the matrix VALUES it can take.
//
// WITHOUT THIS THE RULE WAS BYPASSABLE. `${{ matrix.img }}` names no image, so it
// produced no floating site, while the version-tagged literal in
// `strategy.matrix.img` fell through to the PIN rule — which then REQUIRED the
// version and re-armed the `manifest unknown` trap on the next bump. matrix is a
// context GitHub allows at that position, so "how the value is spelled cannot
// decide whether it is judged" was not true of indirection.
//
// `env` IS FOLLOWED FOR THE SAME REASON, and the reason is sharper than "not
// covered": an unresolved indirection does not leave the image unjudged, it
// leaves the LITERAL to the pin rule, which then REQUIRES the version — the
// inverted verdict, which is the trap. So every indirection reachable from the
// document is followed.
//
// `inputs` likewise — GitHub allows it at container.image in more places than it
// allows `env`.
//
// A `needs.<job>.outputs.<x>` image is not followed, because resolving it means
// running the workflow, and it lands on that same inverted verdict. Named as the
// one exception rather than left as an implied claim of completeness, which this
// file has had to retract four times — the count is here on purpose, because the
// lesson each time was that the claim was made one shape too early.
func resolveContainerImage(lines []string, root, job, key, val *yaml.Node) []containerImage {
	// The value ITSELF is always judged: it may name an image directly, or name
	// one with a templated tag, and both are verdicts of their own. What the
	// references add is the literal each one stands for.
	out := newContainerImage(lines, key, val)
	if !strings.Contains(val.Value, "${{") {
		return out
	}
	for _, m := range reIndirect.FindAllStringSubmatch(val.Value, -1) {
		out = append(out, indirectValues(lines, root, job, m[1], m[2])...)
	}
	return out
}

// indirectValues resolves one `matrix.<key>` or `env.<key>` reference to the
// literal values it can take.
//
// Nothing is returned when it cannot be resolved — no such key, wrong shape, a
// scope that does not exist. Fallback branches for those were written first and
// were all equivalent to returning nothing: the raw value is judged by the caller
// regardless, so an unresolved reference adds no site and suppresses nothing.
func indirectValues(lines []string, root, job *yaml.Node, kind, key string) []containerImage {
	if kind == "inputs" {
		// A reusable workflow's input default. GitHub allows `inputs` at
		// container.image — it allows it in more places than `env`, which was
		// already followed — so a version-tagged default reached this way left the
		// literal to the pin rule and the inverted verdict.
		//
		// `on` SURVIVES AS A STRING KEY here: YAML 1.1 parsers famously turn a
		// bare `on` into the boolean true, and yaml.v3 does not. Checked rather
		// than assumed, because the workaround for the other behaviour is silent
		// when it is not needed and this lookup would just return nothing.
		on := mapValue(root, "on")
		var out []containerImage
		// BOTH TRIGGERS, not the first that answers. Returning early meant a
		// workflow declaring the same input under both had one default judged and
		// the other handed to the PIN rule, which answers "bump these to match"
		// the ARG — the inverted verdict, on a container image.
		for _, trigger := range []string{"workflow_call", "workflow_dispatch"} {
			in := mapValue(mapValue(mapValue(on, trigger), "inputs"), key)
			if k, v := mapEntry(in, "default"); isScalar(v) {
				out = append(out, newContainerImage(lines, k, deref(v))...)
			}
		}
		return out
	}
	if kind == "env" {
		// A job's env shadows the workflow's, as GitHub resolves it.
		for _, scope := range []*yaml.Node{job, root} {
			if k, v := mapEntry(mapValue(scope, "env"), key); isScalar(v) {
				return newContainerImage(lines, k, deref(v))
			}
		}
		return nil
	}
	var out []containerImage
	matrix := mapValue(mapValue(job, "strategy"), "matrix")
	if values := mapValue(matrix, key); values != nil && values.Kind == yaml.SequenceNode {
		for _, v := range values.Content {
			if v.Kind == yaml.ScalarNode {
				// THROUGH spanOf, like every other position. Built directly, a
				// matrix entry whose value WRAPS de-dupped on its first line only
				// and the pin rule re-judged the continuation.
				out = append(out, spanOf(lines, v, v))
			}
		}
	}
	// INCLUDE ROWS CARRY THE KEY TOO, and covering only the plain axis left a
	// version-tagged literal in `matrix.include` falling to the pin rule.
	if include := mapValue(matrix, "include"); include != nil && include.Kind == yaml.SequenceNode {
		for _, row := range include.Content {
			if k, v := mapEntry(row, key); v != nil && v.Kind == yaml.ScalarNode {
				out = append(out, spanOf(lines, k, v))
			}
		}
	}
	return out
}

func spanOf(lines []string, key, val *yaml.Node) containerImage {
	ci := containerImage{value: val.Value, line: val.Line, endLine: val.Line}
	if key.Line-1 >= len(lines) || val.Line-1 >= len(lines) {
		return ci
	}
	// ANCHORED ON THE KEY'S INDENT, not the value's own first line. YAML says a
	// plain scalar's continuation lines must be more indented than the KEY; the
	// two coincide for `image: <value>` and diverge for
	//
	//	image:
	//	  ${{ vars.TF_IMAGE ||
	//	  format('…/ci-tofu:9.9.9', …) }}
	//
	// where the value starts on its own line and its continuation sits at the
	// SAME indent. Anchored on the value's line that is `<= start`, so the range
	// stopped one line early and the pin rule matched the remainder — the exact
	// two-contradictory-verdicts bug endLine exists to prevent.
	start := indentOf(lines[key.Line-1])
	for i := val.Line; i < len(lines); i++ {
		// A BLANK LINE DOES NOT END A PLAIN SCALAR — it is legal inside one, and
		// breaking on it truncated endLine, after which the pin rule re-judged
		// the remainder and the same reference got FLOAT and DRIFT, the drift
		// remediation saying to re-pin to the ARG. Skip blanks; only a non-blank
		// line at or above the key's indent ends the value.
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if indentOf(lines[i]) <= start {
			break
		}
		ci.endLine = i + 1
	}
	return ci
}

func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " \t")) }

// judgedAsContainer reports whether a reference the pin rule matched is part of
// a container image the float rule already judged.
//
// THE REFERENCE, NOT THE MATCH TEXT. reImageTag's match now begins with the byte
// imageLeftBoundary consumed, and that byte is not in the YAML scalar: for a
// container image with no registry prefix — `image: ci-tofu:9.9.9`, quoted or
// not — it is the space after `image:` or the quote itself, so the containment
// test failed and the same reference got two contradictory verdicts. FLOAT plus
// DRIFT, the DRIFT remediation being "bump these to match" the ARG, which is the
// re-pin edit that re-arms the trap. The caller passes `<name>:<tag>`, which is
// what the value actually contains.
//
// BOTH CONDITIONS, and the line alone was not enough. Skipping every match on a
// container image's LINE is right for a wrapped block value and wrong for a
// flow-style job written on one line — `tf: {container: {image: …}, steps: [{run:
// 'docker pull …/ci-kubernetes:9.9.9'}]}` puts a genuinely drifted reference on
// the same line as the container image, and blanking the line meant NO rule
// judged it and the gate exited 0. So the reference must also actually appear in
// the container's value.
func judgedAsContainer(cis []containerImage, line int, ref string) bool {
	for _, ci := range cis {
		if line >= ci.line && line <= ci.endLine && strings.Contains(ci.value, ref) {
			return true
		}
	}
	return false
}

// stepImages returns the container images a `steps:` sequence runs. A step's
// `uses: docker://<image>:<tag>` runs a container, and the pin rule was claiming
// it — REQUIRING the version, which points that step at an unpublished tag on the
// next bump. Only the docker:// form: `uses: owner/repo@ref` is an action, not an
// image.
//
// Shared by a workflow job and a composite action's `runs:`, which carry the
// same shape and fail the same way.
func stepImages(lines []string, owner *yaml.Node) []containerImage {
	steps := mapValue(owner, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		return nil
	}
	var out []containerImage
	for _, step := range steps.Content {
		uk, u := mapEntry(step, "uses")
		if u == nil || u.Kind != yaml.ScalarNode || !strings.HasPrefix(u.Value, "docker://") {
			continue
		}
		out = append(out, newContainerImage(lines, uk, u)...)
	}
	return out
}

// isScalar reports whether a node is a scalar, following a YAML alias to reach
// one. deref returns the node the value actually is.
//
// AN ALIAS IS A SCALAR WEARING A DIFFERENT KIND. `image: *img` has Kind
// AliasNode, so a bare `Kind == yaml.ScalarNode` test dropped it — and the
// anchored literal then fell to the PIN rule, which answers "bump these to
// match" the ARG. GitHub's own parser does not expand anchors, so such a
// workflow would not run; that makes this a wrong VERDICT on a broken file
// rather than a live trap, and the verdict is still the one that teaches the
// wrong lesson.
func isScalar(n *yaml.Node) bool { return deref(n) != nil }

func deref(n *yaml.Node) *yaml.Node {
	if n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n == nil || n.Kind != yaml.ScalarNode {
		return nil
	}
	return n
}

// mapEntry returns a mapping's key and value nodes for one key. The KEY node is
// what the continuation-indent anchor in spanOf needs; mapValue is the shorthand
// for callers that do not.
func mapEntry(n *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i], n.Content[i+1]
		}
	}
	return nil, nil
}

func mapValue(n *yaml.Node, key string) *yaml.Node {
	_, v := mapEntry(n, key)
	return v
}

// reImageRef matches a reference to one published image inside a container
// image expression, capturing its tag. Unlike reImageTag it does not require a
// digit-leading tag: at this position `:latest` is the answer we are checking
// FOR, not a value to skip over.
func reImageRef(image string) *regexp.Regexp {
	return regexp.MustCompile(imageLeftBoundary + regexp.QuoteMeta(image) + `:([^'"@]*)`)
}

// imageLeftBoundary anchors an image name to the start of its path segment.
//
// WITHOUT IT, A LONGER NAME CONTAINING OURS MATCHED. Pointing lint.yml's
// Terraform job at `ghcr.io/<org>/mirror-ci-tofu:1.9.9` made the gate print
// `ok … ci-tofu container image tag = latest` and exit 0: reImageRef matched
// inside the longer name so the image counted as named, the segment test then
// correctly refused to judge `mirror-ci-tofu`'s tag, and it fell through to
// "nothing disagreed, so it floats" — while judgedAsContainer suppressed the pin
// rule on the same line. A different, version-pinned image satisfied the declared
// {lint.yml, ci-tofu} entry, which is the rename-satisfies-the-check hole the
// (file, image) keying exists to close.
//
// There is a right boundary already; this is the left one. `/` and quotes open a
// segment, `-` does not.
const imageLeftBoundary = `(?:^|[^A-Za-z0-9._-])`

// reImageBare matches the same image with NO tag — Docker's implicit `:latest`.
// reImageRef cannot see it, and left unmatched a tagless reference produced no
// site at all, so the vacuity check reported the job as "moved or genuinely
// gone" while it sat right there in the file.
//
// The trailing class is a right boundary: `ci-tofu` must not match inside
// `ci-tofu-experimental`.
func reImageBare(image string) *regexp.Regexp {
	return regexp.MustCompile(imageLeftBoundary + regexp.QuoteMeta(image) + `(?:@[^'"\s]*)?(?:$|[^A-Za-z0-9._:@-])`)
}

// reImageDigest matches this image pinned by digest, with or without a tag.
//
// A DIGEST FREEZES EXACTLY LIKE A sha- TAG, which is why it is rejected for the
// same reason rather than accepted as a reproducibility escape hatch. It was
// accepted at first, on the grounds that a digest is immutable and by definition
// already published — true, and only half the test. The other half is "stays
// correct as versions move", and a digest never does: it pins one build forever,
// so the image silently stops tracking the toolchain the tree declares. Docker
// resolves `name:tag@digest` BY DIGEST and ignores the tag, so `:latest@sha256:…`
// was frozen too while reading as floating — the failure message's own "a digest
// IS accepted" line was a signposted path into the freeze the sha- arm rejects.
// Reproducibility belongs in the repo variable, which overrides the fallback.
func reImageDigest(image string) *regexp.Regexp {
	return regexp.MustCompile(imageLeftBoundary + regexp.QuoteMeta(image) + `(?::[^'"@\s]*)?@sha256:[0-9a-fA-F]+`)
}

// reTagInValue matches every `:<tag>` in a container image value, plus the
// character after it, so a registry port can be told from a tag.
//
// The leading group is the path SEGMENT the tag hangs off — everything back to
// the preceding `/`, which the character class excludes. That segment is how a
// tag of ours is told from a tag that is none of our business; see
// judgeContainerImage.
//
// A `${{ … }}` tag is its own alternative, and it comes first, so a templated
// tag is reported as the whole expression. Without it the tag class stopped at
// the first space and the report read `tag is ${{`, which is the blank-value
// defect in a different costume: the value shown is not the value written.
//
// IT HAS TO TOLERATE A NESTED `}`. Written `[^}]*` the alternative could not
// span one, so `ci-tofu:${{ format('{0}', env.T) }}` truncated to `${{` — the
// same fragment the alternative exists to prevent. RE2 has no lookahead, so a
// `}` NOT followed by another is spelled as its own two-character alternative.
//
// That alternation is also what stops two templated tags in one value merging
// into one match: neither branch can cross `}}`, so the quantifier's greediness
// is immaterial. It was written lazy for that reason and the `?` came back off —
// verified equivalent on both shapes rather than assumed, and a modifier that
// cannot change an outcome is a thing to notice, not to carry.
var reTagInValue = regexp.MustCompile(`([A-Za-z0-9_.${}-]*):(\$\{\{(?:[^}]|\}[^}])*\}\}|[A-Za-z0-9_.${}-]*)`)

// judgeContainerImage decides one container image VALUE for one image name.
//
// IT READS THE WHOLE VALUE, AND EVERY TAG IN IT. Reading a single occurrence of the
// image name and assuming the rest of the expression inert lets these spellings
// through, each landing on the `manifest unknown` trap the gate exists to prevent:
//
//	format('ghcr.io/{0}/{1}:1.34.10', owner, 'ci-kubernetes')
//	format('ghcr.io/{0}/{1}:sha-abcdef0', owner, 'ci-tofu')
//	format('ghcr.io/{0}/{1}:{2}', owner, 'ci-tofu', env.TAG)
//	    the name occurs TAGLESS — it is a quoted ARGUMENT — and the tag is
//	    attached to a placeholder, so a name-anchored match called it an implicit
//	    :latest and the gate exited 0
//
//	vars.X && '…/ci-kubernetes:latest' || '…/ci-kubernetes:1.34.10'
//	    only the leftmost match is judged; the second occurrence is invisible to
//	    this rule and skipped by the pin rule, its line being a container line
//
// A name-anchored rule cannot see any of them, and scanning only for DOTTED version
// tags closes the first and leaves the other two open. So the rule is stated over
// the value:
//
//	if the value names one of our images, every tag in it must be `latest`
//
// EVERY TAG OF OURS, WHICH IS NOT EVERY TAG. Requiring `latest` of every colon in
// the value over-reaches: a conditional image `… && 'ghcr.io/o/ci-tofu:latest' ||
// 'debian:bookworm-20240101'` then fails with `ci-tofu container image tag is
// bookworm-20240101`, naming a third-party image this guard does not gate, and the
// only edit that satisfies the message is retagging Debian.
//
// So a tag is judged when the path SEGMENT it hangs off is one of ours, or is a
// placeholder or expression whose value cannot be read here — `{1}` in
// `format('ghcr.io/{0}/{1}:sha-abcdef0', owner, 'ci-tofu')` is the case that
// forced the whole-value rule, and it has to stay covered. A literal segment
// that is not one of our images (`debian`, and `ghcr.io` in a `host:port`) is
// somebody else's pin. `sha256` is not a segment at all — it is a digest, and
// digests are neither required nor forbidden here.
//
// Everything that IS judged and is not `latest` — sha-, templated, empty, a
// version — reaches the same "does not float" verdict, correctly, for the
// reasons the remediation gives.
func judgeContainerImage(value, image string) (tag string, named bool) {
	if !reImageRef(image).MatchString(value) && !reImageBare(image).MatchString(value) {
		return "", false
	}
	// reImageDigest mandates the `@sha256:` literal, so Index cannot miss — an
	// `else` arm here was unreachable and is gone.
	if m := reImageDigest(image).FindString(value); m != "" {
		return shortDigest(m[strings.Index(m, "@sha256:"):]), true
	}
	for _, m := range reTagInValue.FindAllStringSubmatch(value, -1) {
		segment, t := m[1], m[2]
		if !thisOneOrUnknown(segment, image) || t == floatingTag {
			continue
		}
		if t == "" {
			return "", true // names nothing; shown() renders it
		}
		return t, true
	}
	return floatingTag, true
}

// thisOneOrUnknown reports whether a path segment is the image being judged, or
// something this guard cannot resolve (a format placeholder or an expression)
// and therefore must not wave through.
//
// THIS IMAGE, NOT ANY OF OURS. Accepting every one of our names meant a value
// naming both — `… 'ghcr.io/o/ci-tofu:latest' … 'ghcr.io/o/ci-kubernetes:9.9.9'`
// — reported the ci-kubernetes tag under the ci-tofu verdict. Each image gets
// its own site from the same value, so each should judge only its own tag; the
// other one is not missed, it is the other site's business.
//
// `sha256` needs no arm of its own: it is not this image's name and contains no
// placeholder, so it falls out here. An explicit `segment == "sha256"` check was
// written first and deleted — it could not change any outcome, and dead code in
// a guard reads as a handled case.
func thisOneOrUnknown(segment, image string) bool {
	return segment == image || strings.ContainsAny(segment, "{$")
}

// deliveredTree reports whether a path is scaffold content an INSTANCE runs
// rather than this repo's own CI.
//
// `:latest` is right for a container image in THIS repo — build-images.yml
// republishes it on every main push, so it is always already there. In an
// instance it is wrong: an instance's image has to match the template ref it is
// pinned to, which is what `llz ci assert-image-fresh` enforces and why the
// delivered workflows resolve it from vars.TF_IMAGE / vars.KUBE_IMAGE. Applying
// the float rule there would tell an adopter to unpin the thing the adjacent
// gate exists to hold still.
//
// SO IT IS FORBIDDEN THERE, NOT EXEMPT. Exempting it was the first fix and it
// was worse than the bug: reImageTag's digit-leading filter means a hardcoded
// `…ci-tofu:latest` under instance-template/ is not a pin site either, so it was
// checked by NO rule and the gate exited 0. StaleCIImageVars reads instance repo
// VARIABLES, not workflow images, so nothing else covered it. Nothing there
// hardcodes one today; if one is ever wanted, what it should resolve to is a
// decision to make deliberately — this makes the gate ask rather than answer it
// wrongly by silence.
func deliveredTree(rel string) bool {
	return strings.HasPrefix(rel, "instance-template/")
}

// floatingFallback is one fallback the tree is expected to carry.
type floatingFallback struct{ file, image string }

// expectedFallbacks exists for vacuity, not for matching: containerImages finds
// container images wherever they are (a new one in another workflow is gated the
// moment it is written, because the rule is about the POSITION, not about
// recognising a fallback expression). This declares the ones that must still be
// there, KEYED ON THE IMAGE AS WELL AS THE FILE. Keyed on the file alone,
// renaming lint.yml's ci-kubernetes fallback to some other image name left
// ci-tofu satisfying the check for the whole file — the gate reported OK and the
// Kubernetes lint job walked straight back into `manifest unknown`.
var expectedFallbacks = []floatingFallback{
	{file: ".github/workflows/lint.yml", image: "ci-kubernetes"},
	{file: ".github/workflows/lint.yml", image: "ci-tofu"},
}

// scanRoots are the trees a version restatement may legitimately live in.
//
// docs/ is deliberately absent: an ADR that says "CI runs 1.12.5" is a historical
// record of a decision, not a live pin. Gating it would force every ADR to be
// rewritten on every bump, which is both wrong and the fastest way to get a gate
// switched off.
var scanRoots = []string{
	".github",
	"instance-template/.github",
	"tools/cmd/llz",
	// This package's own citags.go, which holds CITofuTag/CIKubernetesTag. They
	// used to live under tools/cmd/llz (lowercase, in the token-minting file),
	// and the move took them out of the scan with nothing to notice: the goConst
	// rule matched no file on the real tree for as long as that lasted.
	// vacuities() now fails if this root stops finding them, so the next move
	// cannot be silent.
	"tools/internal/extensions/guards/versionpins",
	"dockerfiles",
	"template-scripts",
	"Makefile",
}

var (
	reDockerfileArg = regexp.MustCompile(`(?m)^ARG\s+([A-Z0-9_]+)=(\S+)`)
	// "image":"ci-tofu", … "version":"1.12.5" within one build-matrix JSON object.
	reMatrixEntry = regexp.MustCompile(`"image"\s*:\s*"([^"]+)"[^{}]*?"version"\s*:\s*"([^"]*)"`)
)

// reImageTag matches a published image reference carrying a version tag, e.g.
// ghcr.io/{0}/ci-tofu:1.12.5. The tag must start with a digit so a floating
// `:latest` or a `${{ … }}` expression is not mistaken for a pin.
func reImageTag(image string) *regexp.Regexp {
	// The same left anchor its three siblings carry. Without it this one still
	// matched inside a longer name, so `ghcr.io/<org>/mirror-ci-tofu:1.9.9` in a
	// `run:` step was reported as OUR ci-tofu pin disagreeing with the Dockerfile.
	return regexp.MustCompile(imageLeftBoundary + regexp.QuoteMeta(image) + `:([0-9][A-Za-z0-9._-]*)`)
}

// reArgRestatement matches `NAME: "1.2.3"` / `NAME=1.2.3` / `NAME := 1.2.3` for
// exactly NAME.
//
// The leading boundary is load-bearing. lint.yml carries ARGOCD_HELM_VERSION,
// ESO_HELM_VERSION and KYVERNO_HELM_VERSION — unrelated CHART versions that all
// end in the ARG name HELM_VERSION. A suffix match would report them as drift and
// the gate would be useless on day one.
//
// EVERY MAKE ASSIGNMENT OPERATOR, LONGEST FIRST, AND THE LIST HAS BEEN SHORT
// TWICE. `?=`, `::=` and `+=` were missing after `:=` was added, so rewriting
// the Makefile's `KUBECTL_VERSION := 1.34.10` as `?=` — an ordinary, meaning-
// preserving edit — silently removed the file from the scanned set. The bare-ARG
// class is deliberately exempt from vacuities() (it is an open-ended catch-all,
// see there), so nothing would have reported the loss: a later Dockerfile bump
// says OK while `make k8s-validate` validates against the stale version. That is
// the same shape as the scar below, which is why the list is now exhaustive
// rather than incremental.
//
// `:=` IS A SEPARATE ALTERNATIVE AND IT HAS TO COME FIRST. The separator was
// `[:=]` — one character — so it matched YAML's `:` and env/ARG's `=` and missed
// Make's `:=` entirely: after consuming the `:` the value had to start at `=`,
// which is not [0-9v], so the whole match failed and the site was silently not a
// site. `Makefile` is a scanRoot and the header above claims "a restatement
// nobody knew about still gets checked because the scan is by pattern" — but the
// Makefile's own `KUBECTL_VERSION := <version>`, the idiomatic Make form, was
// invisible to it. Demonstrated by setting a pin to a wrong value twice: written
// `=` the gate failed and named the line; written `:=` the same wrong value
// reported "OK — 9 restatements agree".
func reArgRestatement(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)(?:^|[^A-Z0-9_])` + regexp.QuoteMeta(name) + `\s*(?:::=|:=|\?=|\+=|[:=])\s*"?([0-9v][A-Za-z0-9._-]*)"?`)
}

// reGoConst matches a Go constant restating a tag.
//
// ANCHORED LIKE THE IMAGE NAMES, and for the same reason one level over:
// unanchored, `LegacyCITofuTag = "1.12.5"` matched reGoConst("CITofuTag"), so
// the goConst vacuity check passed — and --verbose printed `CITofuTag = 1.12.5`
// — with no CITofuTag anywhere in the tree. That is the silent vacuity
// vacuities() was added to close, reappearing through the one regex that had not
// been given a boundary. Go identifiers, so the class is identifier characters.
func reGoConst(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^A-Za-z0-9_])` + regexp.QuoteMeta(name) + `\s*=\s*"([^"]*)"`)
}

// pinSite is one place a version is restated.
type pinSite struct {
	file string
	line int
	what string // human-readable: what form the restatement took
	got  string
	want string
	// floating marks a job's container image, whose `want` is the literal `latest`
	// rather than a version. It changes only the remediation text — the equality
	// check is the same one. image is set on floating sites so the vacuity check
	// can key on (file, image); see expectedFallbacks.
	floating bool
	// forbidden marks a fallback in the delivered tree, where the answer is not
	// a different tag but no fallback at all. Its own class because `floating`
	// would file it under the "must name :latest" remediation, which is exactly
	// the advice that must not be given for an instance.
	forbidden bool
	image     string
	// ambiguous marks a floating site whose container value names MORE THAN ONE
	// of our images. Such a value is still judged — every tag of ours in it must
	// float — but it cannot satisfy a declared expectedFallbacks entry, because
	// nothing here can say which image that job actually runs.
	ambiguous bool
	// unplaced marks a floating site found by pattern rather than by YAML
	// position — it must float, but it cannot vouch for a declared container
	// image, because nothing says a job runs it.
	unplaced bool
}

func (s pinSite) ok() bool { return s.got == s.want }

// shown renders a captured value for a human. An EMPTY capture is the case that
// earns this: `ci-tofu:` (no tag at all) printed "tag is  but this fallback must
// float" — a sentence with a hole in it, the same blank-value defect the
// templated-tag capture was widened to remove.
func shown(v string) string {
	if v == "" {
		return "(empty)"
	}
	return v
}

func Run(root string, verbose bool, out, errOut io.Writer) error {
	repo := capability.RepoForGate(Extension(), root)
	args, err := loadVersionAuthority(repo)
	if err != nil {
		return err
	}
	files, err := versionScanFiles(repo)
	if err != nil {
		return err
	}
	sites, parseFails, err := collectPinSites(repo, files, args)
	if err != nil {
		return err
	}
	var bad []pinSite
	for _, s := range sites {
		if !s.ok() {
			bad = append(bad, s)
		}
	}
	if verbose {
		for _, s := range sites {
			// Per class. "DRIFT" on a floating fallback says its tag disagrees
			// with the Dockerfile — which invites exactly the bump-to-match edit
			// the split reporting below exists to prevent.
			mark := "ok"
			switch {
			case s.ok():
			case s.forbidden:
				mark = "FORBID"
			case s.floating:
				mark = "FLOAT"
			default:
				mark = "DRIFT"
			}
			fmt.Fprintf(out, "  %-6s %s:%d  %s = %s\n", mark, s.file, s.line, s.what, shown(s.got))
		}
	}
	// The pip-pin scan is a SEPARATE authority problem (see pippins.go) and runs
	// whether or not the ARG restatements agree, so one red does not hide the other.
	nPip, pipErr := runPipPins(repo, verbose, out, errOut)

	// Collected, not returned early. Returning on the first vacuity problem
	// suppressed every drift the same run had already found — and CI runs this
	// without --verbose, so that drift was emitted NOWHERE and cost a second red
	// round-trip to discover. A gate should report everything it knows.
	vac := append(parseFails, vacuities(sites, args)...)
	if len(bad) == 0 && len(vac) == 0 {
		// Both counts, because they are enforced by opposite rules and a reader
		// who sees only a total cannot tell that two sites are exempt from the
		// pin — the thing the two-rule split exists to make visible.
		floating := 0
		for _, s := range sites {
			if s.floating {
				floating++
			}
		}
		fmt.Fprintf(out, "version-pins: OK — %d restatement(s) agree with %s, %d container image(s) float on :%s\n",
			len(sites)-floating, versionAuthorityFile, floating, floatingTag)
		if pipErr != nil {
			return pipErr
		}
		fmt.Fprint(out, pipPinSummary(nPip))
		return nil
	}
	// Two classes, reported separately: they fail for opposite reasons and a
	// single "disagrees with the Dockerfile" verdict would send someone bumping a
	// fallback that is supposed to name no version at all.
	var drifted, floated, forbidden []pinSite
	for _, s := range bad {
		if s.forbidden {
			forbidden = append(forbidden, s)
			fmt.Fprintf(errOut, "::error file=%s,line=%d::%s — the delivered tree must not carry one\n",
				s.file, s.line, s.what)
			continue
		}
		if s.floating {
			floated = append(floated, s)
			fmt.Fprintf(errOut, "::error file=%s,line=%d::%s is %s but this container image must float on :%s\n",
				s.file, s.line, s.what, shown(s.got), floatingTag)
			continue
		}
		drifted = append(drifted, s)
		fmt.Fprintf(errOut, "::error file=%s,line=%d::%s is %s but %s declares %s\n",
			s.file, s.line, s.what, shown(s.got), versionAuthorityFile, s.want)
	}
	if len(drifted) > 0 {
		fmt.Fprintf(errOut, "\n%s %d version pin(s) disagree with %s:\n", color.Red("✗"), len(drifted), versionAuthorityFile)
		for _, s := range drifted {
			fmt.Fprintf(errOut, "    %s:%d  %s = %s (want %s)\n", s.file, s.line, s.what, shown(s.got), s.want)
		}
		fmt.Fprintf(errOut, "\nThe Dockerfile ARG block is the authority. Either bump these to match it, or —\n"+
			"if the ARG is what is stale — bump the ARG and re-run. Every restatement of a\n"+
			"tool version must move together; that is the whole point of this gate.\n")
	}
	if len(floated) > 0 {
		fmt.Fprintf(errOut, "\n%s %d container image(s) do not float on :%s:\n",
			color.Red("✗"), len(floated), floatingTag)
		for _, s := range floated {
			fmt.Fprintf(errOut, "    %s:%d  %s = %s (want %s)\n", s.file, s.line, s.what, shown(s.got), s.want)
		}
		fmt.Fprintf(errOut, "\nA job's container image must name a tag that is already published when the change\n"+
			"lands AND stays correct as versions move. :%s is the only one: build-images.yml\n"+
			"republishes it on every push to main.\n\n"+
			"  a version tag  — not published yet. build-images builds on pushes to main while\n"+
			"                   lint runs on the bump's own push, so the bump red-lights Lint\n"+
			"                   with `manifest unknown` until the image lands, reading as a\n"+
			"                   broken image reference rather than an ordering artefact.\n"+
			"  a sha- tag     — published, but frozen to one commit, so the image silently\n"+
			"                   stops tracking the toolchain the tree declares. (The sha that\n"+
			"                   would not rot — this commit's — is the one not yet published.)\n"+
			"  a templated tag — resolved per run; nothing here can tell what it will name.\n"+
			"  a digest       — immutable and published, and frozen for exactly that reason.\n"+
			"                   Docker resolves `name:tag@digest` BY DIGEST, so `:%s@sha256:…`\n"+
			"                   reads as floating and is not.\n\n"+
			"If you need a reproducible image, that decision belongs in the repo variable this\n"+
			"job reads — set vars.TF_IMAGE / vars.KUBE_IMAGE to a :sha-<commit> or digest\n"+
			"image, which overrides whatever is written here.\n",
			floatingTag, floatingTag)
	}
	if len(forbidden) > 0 {
		fmt.Fprintf(errOut, "\n%s %d container image(s) in the delivered tree:\n", color.Red("✗"), len(forbidden))
		for _, s := range forbidden {
			fmt.Fprintf(errOut, "    %s:%d  %s\n", s.file, s.line, s.what)
		}
		fmt.Fprintf(errOut, "\ninstance-template/ is scaffold an INSTANCE runs, and its image is pinned through\n"+
			"vars.TF_IMAGE / vars.KUBE_IMAGE to the template ref that instance is on — which is\n"+
			"what `llz ci assert-image-fresh` enforces. Neither answer this gate knows is right\n"+
			"there: :latest unpins the image from the ref, and a version tag is not what an\n"+
			"instance resolves. Drop the fallback, or decide what it should resolve to and\n"+
			"teach deliveredTree about it.\n")
	}
	for _, v := range vac {
		if v.file != "" {
			fmt.Fprintf(errOut, "::error file=%s::%s\n", v.file, v.msg)
		} else {
			fmt.Fprintf(errOut, "::error::%s\n", v.msg)
		}
	}
	if len(vac) > 0 {
		fmt.Fprintf(errOut, "\n%s %d rule(s) matched nothing:\n", color.Red("✗"), len(vac))
		for _, v := range vac {
			fmt.Fprintf(errOut, "    %s\n", v.msg)
		}
		fmt.Fprintf(errOut, "\nA rule that examines nothing reports OK on a tree it never read, which looks\n"+
			"exactly like the drift this gate exists to catch. Restore what it names, or move\n"+
			"the declaration to wherever the thing lives now.\n")
	}

	var why []string
	if len(drifted) > 0 {
		why = append(why, fmt.Sprintf("%d pin(s) drifted", len(drifted)))
	}
	if len(floated) > 0 {
		why = append(why, fmt.Sprintf("%d container image(s) do not float on :%s", len(floated), floatingTag))
	}
	if len(forbidden) > 0 {
		why = append(why, fmt.Sprintf("%d container image(s) in the delivered tree", len(forbidden)))
	}
	if len(vac) > 0 {
		why = append(why, fmt.Sprintf("%d rule(s) matched nothing", len(vac)))
	}
	// The pip scan's verdict rides along rather than replacing ours: they are
	// separate authority problems and either can be the reason this is red.
	if pipErr != nil {
		return fmt.Errorf("version-pins: %s (and %w)", strings.Join(why, ", "), pipErr)
	}
	return fmt.Errorf("version-pins: %s", strings.Join(why, ", "))
}

// loadVersionAuthority reads the Dockerfile ARG block into name -> version.
func loadVersionAuthority(repo capability.Repo) (map[string]string, error) {
	data, err := repo.ReadFile(filepath.FromSlash(versionAuthorityFile))
	if err != nil {
		return nil, fmt.Errorf("version-pins: read %s: %w", versionAuthorityFile, err)
	}
	args := map[string]string{}
	for _, m := range reDockerfileArg.FindAllStringSubmatch(string(data), -1) {
		args[m[1]] = m[2]
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("version-pins: %s declares no ARG versions — refusing to pass vacuously", versionAuthorityFile)
	}
	return args, nil
}

// versionScanFiles walks scanRoots for text files that may restate a version.
// Test files are excluded: a fixture legitimately pins a made-up version.
func versionScanFiles(repo capability.Repo) ([]string, error) {
	var files []string
	for _, r := range scanRoots {
		start := filepath.FromSlash(r)
		err := repo.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil // an optional root (Makefile, template-scripts) may be absent
				}
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", ".terraform", "testdata", "node_modules":
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if strings.HasSuffix(name, "_test.go") {
				return nil
			}
			switch {
			case strings.HasSuffix(name, ".yml"), strings.HasSuffix(name, ".yaml"),
				strings.HasSuffix(name, ".go"), strings.HasSuffix(name, ".sh"),
				strings.HasPrefix(name, "Dockerfile"), name == "Makefile":
				// Already repo-relative: the reader expresses everything under
				// its own root.
				files = append(files, filepath.ToSlash(path))
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("version-pins: walk %s: %w", r, err)
		}
	}
	sort.Strings(files)
	return files, nil
}

func collectPinSites(repo capability.Repo, files []string, args map[string]string) ([]pinSite, []vacuity, error) {
	var sites []pinSite
	var parseFails []vacuity
	for _, rel := range files {
		data, err := repo.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			return nil, nil, fmt.Errorf("version-pins: read %s: %w", rel, err)
		}
		raw := string(data)
		body := maskComments(rel, raw)
		isAuthority := rel == versionAuthorityFile

		// 1a. Container images, located by YAML position and required to float.
		//
		// They are collected first so the pin scan below can skip what they
		// already judged. Without that, a container image pinned to the current
		// ARG matched both rules at once: the pin rule said "ok" (it does equal
		// the ARG) while this one said "must be :latest", and the report listed
		// the same line twice with opposite verdicts.
		// THE RAW TEXT, NOT THE MASKED BODY. Masking exists for the regex
		// classes, where a version inside a comment is prose; a YAML parser
		// already ignores comments, so handing it the masked copy buys nothing
		// and asks a mutated document to behave like the real one.
		//
		// The stakes are why this is worth being careful about rather than
		// convenient: if a file fails to parse it has no container images at all,
		// so its image falls through to the PIN rule and a version-tagged one is
		// REQUIRED rather than forbidden — the gate mandating the trap.
		//
		// HONESTLY SCOPED: this was raised in review as a live break (an indented
		// comment inside a `run: |` block scalar becoming an all-space line that
		// yaml.v3 rejects), and it did NOT reproduce — seven constructed cases,
		// including that one, parse identically masked and unmasked, because a
		// spaces-only line reads as empty. One case diverges and it is the other
		// direction: a tab-indented comment makes an INVALID document parse once
		// masked. So this is not a fixed outage; it is removing a needless
		// difference between what the parser reads and what the file says.
		cis, parsed := containerImages(raw)
		if isYAML(rel) && !parsed && namesOneOfOurs(body) {
			// See containerImages: the file going quiet flips this rule's verdict
			// for whatever image it names, so it fails closed HERE rather than
			// leaning on a gate that covers other files.
			parseFails = append(parseFails, vacuity{file: rel, msg: fmt.Sprintf(
				"%s names one of our CI images but does not parse as YAML, so its container images "+
					"were not read — a version-tagged one is then ACCEPTED by the pin rule instead of "+
					"refused. actionlint will say what is wrong with the file", rel)})
		}
		for _, ci := range cis {
			// How many of our images this one value names. More than one and it
			// is a conditional whose branch we cannot resolve — see pinSite.ambiguous.
			// DISTINCT IMAGES, not published names. Counting names, a value
			// naming ONE image under both `ci-tofu` and its `ci-terraform` alias
			// counted as two and was called ambiguous — so both its sites judged
			// `ok` while the gate failed with a "give it its own job" remediation
			// that cannot be satisfied, because there is only one image there.
			oursNamed := 0
			for _, p := range imagePins {
				for _, n := range p.names() {
					if _, ok := judgeContainerImage(ci.value, n); ok {
						oursNamed++
						break
					}
				}
			}
			for _, p := range imagePins {
				for _, name := range p.names() {
					// The verdict is the returned tag itself: pinSite.ok()
					// compares it against floatingTag, and every non-floating
					// path returns something that is not "latest".
					tag, isNamed := judgeContainerImage(ci.value, name)
					if !isNamed {
						continue
					}
					// ONE VERDICT PER SITE. In the delivered tree the finding is
					// that the image is hardcoded at all, so this stops instead of
					// also judging the tag.
					if deliveredTree(rel) {
						sites = append(sites, pinSite{
							file: rel, line: ci.line,
							what: name + " container image in the delivered tree",
							got:  "hardcoded", want: "resolved from vars.<X>_IMAGE",
							// No image: it is read only for the
							// expectedFallbacks key, which covers floating sites
							// in THIS repo. Set here it would be a field whose
							// value cannot matter — a thing to notice, not carry.
							forbidden: true,
						})
						continue
					}
					sites = append(sites, pinSite{
						file: rel, line: ci.line,
						// Labelled AND KEYED with the name as written. Keying on
						// the primary let a fallback rewritten to the ci-terraform
						// alias satisfy the declared {lint.yml, ci-tofu} entry —
						// the same rename-satisfies-the-check hole that (file,
						// image) keying was added to close, and it bites exactly
						// when the deprecation window closes and the alias stops
						// being republished.
						what: name + " container image tag",
						got:  tag, want: floatingTag,
						floating: true, image: name,
						// A value naming several of our images answers for none
						// of them; see pinSite.ambiguous.
						ambiguous: oursNamed > 1,
					})
				}
			}
		}

		// 1b. Published image tags anywhere else: ghcr.io/<org>/ci-tofu:1.12.5.
		// A version-tagged reference outside a container image is an ordinary pin
		// and has to agree with the ARG, exactly as it always did.
		for _, p := range imagePins {
			want, ok := args[p.arg]
			if !ok {
				continue
			}
			for _, name := range p.names() {
				// IN THIS REPO'S OWN YAML, A VERSION-TAGGED REFERENCE TO ONE OF
				// OUR IMAGES IS NEVER A PIN. It is the ordering trap wherever it
				// sits: build-images publishes on pushes to main, a workflow runs
				// on the bump's own push, so the tag is not there yet — the
				// position only decides how precisely we can explain it.
				//
				// THIS ENDS A CLASS RATHER THAN A CASE. Requiring the ARG here
				// made every position the positional rule did not recognise
				// degrade to the WORST possible verdict: eight review rounds each
				// found another spelling — the `container:` shorthand, service
				// containers, matrix and matrix.include, env, inputs, a second
				// document, an alias, `uses: docker://`, a composite action's
				// steps — and every one of them failed the same way, by falling
				// through to here and being told to restate the version. An
				// unrecognised position now degrades to "must float", which is
				// the right advice even when the guard cannot say which position
				// it is looking at.
				//
				// It costs nothing measurable: this class matches NOTHING in the
				// real tree, and never did. The pins that exist are ARG
				// restatements, the build matrix and the Go constants.
				//
				// Not in the delivered tree, where floating is the wrong answer
				// (an instance resolves its image from the repo variable, pinned
				// to the template ref), and not outside YAML, where a script
				// pulling an image does not run on the bump's push.
				floatInstead := isYAML(rel) && !deliveredTree(rel)
				for _, m := range reImageTag(name).FindAllStringSubmatchIndex(body, -1) {
					// m[2] here TOO. The site two lines down uses it because
					// imageLeftBoundary consumes a byte that is the preceding
					// newline at column 0; a lookup keyed on m[0] disagreed with
					// the site it guards by one line, which is either a wrongly
					// suppressed reference or a second contradictory verdict.
					if judgedAsContainer(cis, lineOf(body, m[2]), name+":"+body[m[2]:m[3]]) {
						continue // the other rule already has a verdict on it
					}
					site := pinSite{
						// m[2], NOT m[0] — imageLeftBoundary consumes a byte, and
						// for a reference at column 0 that byte is the preceding
						// NEWLINE, so the annotation landed on the line above.
						// Identical to the off-by-one already fixed for the
						// bare-ARG class, arriving through the anchor added since.
						file: rel, line: lineOf(body, m[2]),
						what: name + " image reference",
						got:  body[m[2]:m[3]], want: want,
					}
					if floatInstead {
						site.want, site.floating, site.image = floatingTag, true, name
						// NOT ambiguous, and not able to vouch either: this is a
						// reference of unknown position, so it says nothing about
						// whether a job RUNS the image. expectedFallbacks is
						// satisfied by container positions only.
						site.unplaced = true
					}
					sites = append(sites, site)
				}
			}
			// 3. Go constant restating the tag.
			if p.goConst != "" {
				for _, m := range reGoConst(p.goConst).FindAllStringSubmatchIndex(body, -1) {
					sites = append(sites, pinSite{
						// m[2] for the same reason as the image tag above: this
						// one is anchored too. gofmt keeps a const off column 0,
						// so it is unreachable today — which is exactly why it
						// would be found late.
						file: rel, line: lineOf(body, m[2]),
						what: p.goConst,
						got:  body[m[2]:m[3]], want: want,
					})
				}
			}
		}

		// 2. build-images.yml matrix: {"image":"ci-tofu", … "version":"1.12.5"}.
		for _, m := range reMatrixEntry.FindAllStringSubmatchIndex(body, -1) {
			image, ver := body[m[2]:m[3]], body[m[4]:m[5]]
			if ver == "" {
				continue // devcontainer/llz carry no version tag
			}
			for _, p := range imagePins {
				if p.image != image {
					continue
				}
				if want, ok := args[p.arg]; ok {
					sites = append(sites, pinSite{
						file: rel, line: lineOf(body, m[0]),
						what: matrixWhat(image),
						got:  ver, want: want,
					})
				}
			}
		}

		// 4. Bare ARG restatements: KUBECTL_VERSION: "<version>".
		if isAuthority {
			continue // the ARG lines themselves are the authority, not restatements
		}
		for _, name := range sortedKeys(args) {
			for _, m := range reArgRestatement(name).FindAllStringSubmatchIndex(body, -1) {
				sites = append(sites, pinSite{
					// m[2], NOT m[0]. This class's leading boundary is
					// `(?:^|[^A-Z0-9_])`, which for an assignment at the start of
					// a line consumes the PRECEDING NEWLINE — so m[0] sits on the
					// line above and every annotation for this class was off by
					// one (the real tree reported Makefile:12 and :19 for
					// assignments on 13 and 20). The value capture cannot be
					// anywhere but the right line.
					file: rel, line: lineOf(body, m[2]),
					what: name,
					got:  body[m[2]:m[3]], want: args[name],
				})
			}
		}
	}
	return sites, parseFails, nil
}

// isYAML gates the parse-failure check to files that are SUPPOSED to be YAML.
// Without it a Makefile and this package's own .go files — which name our images
// in table entries — were reported as unparseable workflows, which they are not.
func isYAML(rel string) bool {
	return strings.HasSuffix(rel, ".yml") || strings.HasSuffix(rel, ".yaml")
}

// namesOneOfOurs reports whether raw text mentions any published image name.
func namesOneOfOurs(raw string) bool {
	for _, p := range imagePins {
		for _, n := range p.names() {
			if reImageBare(n).MatchString(raw) || reImageRef(n).MatchString(raw) {
				return true
			}
		}
	}
	return false
}

// siteKey identifies a site by file as well as label, for the checks that care
// WHERE a class was satisfied and not merely that it was.
type siteKey struct{ file, what string }

// shortDigest trims a digest for the report — the whole hex tells the reader
// nothing the first few characters do not, and wraps the line.
func shortDigest(d string) string {
	const keep = len("@sha256:") + 12
	if len(d) > keep {
		return d[:keep] + "…"
	}
	return d
}

// vacuity is one rule that matched nothing. file is set when the rule names a
// specific file, so the caller can annotate it; empty when the whole point is
// that nothing was found anywhere.
type vacuity struct{ file, msg string }

// vacuities fails closed on a rule that examined nothing.
//
// The authority-file check already refuses to pass when the Dockerfile declares
// no ARGs; these are the same refusal one level down, and each was earned:
//
//   - A declared fallback that is not found means the exemption subtracts a site
//     from the pinned set while enforcing nothing on it — and the next person to
//     add a fallback there inherits a decision nobody re-made.
//   - The goConst class had been silently vacuous for real: the constants moved
//     out of tools/cmd/llz into this package's own citags.go and were renamed
//     CITofuTag/CIKubernetesTag, so the pattern matched nothing on the real tree
//     while the gate's own help text still advertised the check. Setting
//     CITofuTag = "1.9.8" — the founding scar, verbatim — passed.
//   - The build-matrix pattern reads TWO adjacent JSON fields in one regex, so
//     it is broken by a purely cosmetic edit: writing "version" before "image"
//     in a matrix row stops it matching and the row silently stops being a site.
//
// ALL of them are collected, not the first. These are reported next to whatever
// drift the same run found, so one red says everything that is wrong.
//
// WHICH CLASSES ARE NOT ASSERTED, AND WHY. The image-tag class (a version-tagged
// `ci-tofu:1.12.5` reference) legitimately matches nothing on the current tree —
// the only two such references are lint.yml's container images, and they are
// judged by the positional rule now. It is a catch-all for a version-tagged reference someone adds
// later, not a class with known members, so requiring it to match would fail on
// a correct tree. The bare-ARG class is likewise open-ended by design: it exists
// so "a restatement nobody knew about still gets checked".
func vacuities(sites []pinSite, args map[string]string) []vacuity {
	foundFallback := map[floatingFallback]bool{}
	// Both keyed on pinSite.what, which for these two classes is the constant
	// name and the matrixWhat() label verbatim — set at exactly one place each
	// in collectPinSites.
	//
	// TREE-WIDE vs PER-FILE, and the difference is not an oversight. The matrix
	// has exactly one authoritative home, so a matching row somewhere else must
	// not satisfy it. A Go constant does not: these two already moved packages
	// once, that move is what made the class vacuous, and pinning the check to a
	// path would fire on the next legitimate move while the constant was still
	// being scanned. For a constant the invariant really is "this class has a
	// member anywhere in the scanned tree".
	found := map[string]bool{}
	foundIn := map[siteKey]bool{}
	for _, s := range sites {
		if s.floating && !s.ambiguous && !s.unplaced {
			foundFallback[floatingFallback{file: s.file, image: s.image}] = true
		}
		found[s.what] = true
		foundIn[siteKey{file: s.file, what: s.what}] = true
	}
	var out []vacuity
	// UNAMBIGUOUS container images only. Keyed on mere existence, one value
	// naming both of our images registered a site for each and satisfied BOTH
	// declared entries — so deleting the entire Kubernetes lint job passed with
	// "2 container image(s) float".
	//
	// A DISTINCTNESS COUNT AND THEN A MATCHING WERE BOTH TRIED, and both were the
	// wrong shape. Two ci-tofu jobs — one whose value merely MENTIONS
	// ci-kubernetes — with the ci-kubernetes job deleted gives a perfectly valid
	// assignment (ci-tofu → job one, ci-kubernetes → job two) while no job runs
	// ci-kubernetes at all. Distinctness was never the property: the question is
	// whether the tree contains a job that unambiguously runs THIS image, and a
	// value naming two of them answers it for neither. Ambiguous sites are still
	// JUDGED — every tag of ours in them must float — they just cannot vouch for
	// a declared entry.
	ambiguousFor := map[floatingFallback]bool{}
	for _, s := range sites {
		if s.floating && s.ambiguous {
			ambiguousFor[floatingFallback{file: s.file, image: s.image}] = true
		}
	}
	for _, f := range expectedFallbacks {
		if foundFallback[f] {
			continue
		}
		if ambiguousFor[f] {
			out = append(out, vacuity{file: f.file, msg: fmt.Sprintf(
				"no job in %s unambiguously runs %s — it is named only by a value that also names "+
					"another of our images, so nothing here can say which one that job runs; give it "+
					"its own job, or point expectedFallbacks at the one that has it", f.file, f.image)})
			continue
		}
		out = append(out, vacuity{file: f.file, msg: fmt.Sprintf(
			"no %s container image found in %s — if the job moved, point expectedFallbacks at "+
				"its new home; only drop the entry if the job is genuinely gone, because dropping "+
				"it while the job is still there leaves that image ungated", f.image, f.file)})
	}
	for _, p := range imagePins {
		// THE ARG FIRST, because it is upstream of both checks below and its
		// absence explains them. Renaming ARG TOFU_VERSION used to be reported as
		// "CITofuTag was not found under any scanRoot" AND "the matrix row was
		// removed or its fields reordered" — two accusations against two correct
		// files, with the real cause named nowhere. Both classes gate on
		// args[p.arg] resolving, so when it does not, that is the one thing to
		// say.
		if _, ok := args[p.arg]; !ok {
			out = append(out, vacuity{file: versionAuthorityFile, msg: fmt.Sprintf(
				"%s declares no %s — every rule keyed on it (the %s image tag, its build-matrix row, "+
					"and the %s constant) silently stops being a site; restore the ARG or update "+
					"imagePins to the name it has now", versionAuthorityFile, p.arg, p.image, p.goConst)})
			continue
		}
		if p.goConst != "" && !found[p.goConst] {
			out = append(out, vacuity{msg: fmt.Sprintf(
				"the constant %s was not found under any scanRoot — it was renamed or moved out of the "+
					"scanned tree, so this gate stopped checking the class it was written for; update "+
					"imagePins/scanRoots to follow it", p.goConst)})
		}
		if !foundIn[siteKey{file: buildMatrixFile, what: matrixWhat(p.image)}] {
			out = append(out, vacuity{file: buildMatrixFile, msg: fmt.Sprintf(
				"no build-matrix version was found for %s in %s — the row was removed, or its "+
					"\"image\" and \"version\" fields were reordered, which is enough to stop "+
					"reMatrixEntry matching and silently drop the site", p.image, buildMatrixFile)})
		}
	}
	return out
}

// matrixWhat is the label a build-matrix site carries. A function, not two
// string concatenations, because vacuities() looks the label up and a
// silent mismatch between the two spellings would re-open the hole it closes.
func matrixWhat(image string) string { return "build matrix version for " + image }

func lineOf(body string, off int) int {
	return 1 + strings.Count(body[:off], "\n")
}

// maskComments blanks out whole-line comments, preserving length so byte offsets
// and line numbers still line up with the original.
//
// A version inside a comment is prose, not a pin — the same reasoning that keeps
// docs/ out of scanRoots. Without this the gate reports its own header (which
// documents the `ci-tofu:<ver>` form it matches) and any workflow comment that
// mentions a version in passing.
func maskComments(rel, body string) string {
	marker := "#"
	if strings.HasSuffix(rel, ".go") {
		marker = "//"
	}
	lines := strings.Split(body, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), marker) {
			lines[i] = strings.Repeat(" ", len(ln))
		}
	}
	return strings.Join(lines, "\n")
}
