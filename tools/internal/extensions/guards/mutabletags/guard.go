package mutabletags

// guard.go implements `llz ci mutable-tag-guard` — the gate that keeps a MUTABLE
// image tag publishable only from the default branch's HEAD.
//
// WHY (issue #451): build-images.yml publishes `:latest`, `:sha-<commit>` and an
// optional `:<version>` from one build, and its `workflow_dispatch` is not gated
// on the ref — deliberately, because release-e2e and e2e-instantiate must be able
// to drive it on a feature branch. So the ref gate has to live on the PUBLISH.
// Until it did, a branch build repointed `:latest` and `:<version>` for everyone:
//
//   - lint.yml's container fallback resolves `ci-kubernetes:latest` on every run
//     of this repo (neither KUBE_IMAGE nor TF_IMAGE is set here), so every open
//     PR's Lint job moved to a branch's toolchain image;
//   - `llz ci assert-image-fresh` reads the baked sha of the image an instance
//     resolves and expects the template ref's commit — `:latest` stamped with a
//     branch sha is exactly the skew it exists to catch, manufactured by us;
//   - the no-path-filter design at the top of build-images.yml exists solely to
//     keep `:latest` == main's HEAD, and a branch dispatch falsified it until the
//     next main push.
//
// It was not hypothetical, and not deliberate either: e2e-instantiate.yml
// dispatches the build ON THE BRANCH automatically (`pin-instance-images --ref
// "${GITHUB_REF_NAME}" --build-if-missing`), so every branch that ran an e2e did
// this. Nothing could see it — each `--tag` was individually well-formed, and the
// tag that moved looked identical afterwards.
//
// WHAT IT CHECKS, and each arm is one way the fix can be undone:
//
//  1. every mutable `--tag` sits inside the `PUBLISH_MUTABLE` gate;
//  2. the gate expression still constrains `github.ref` — `PUBLISH_MUTABLE: true`
//     satisfies arm 1 and publishes from everywhere;
//  3. at least one mutable tag is still published, because `:latest` is what
//     version-pins REQUIRES this repo's container jobs to float on. Dropping it
//     would satisfy arms 1 and 2 vacuously and break Lint on the next main push;
//  4. at least one `sha-` tag is published OUTSIDE the gate, because that is the
//     tag release-e2e and pin-instance-images wait on. Moving it inside would
//     satisfy every other arm and hang a branch e2e on an image that never
//     publishes.
//
// SCOPED TO THE ONE PUBLISHER, not to every workflow, and that is a choice with a
// reason. llz-release.yml also pushes a version tag (`llz:${VER}`) — from a
// release, where "tags are immutable, never move one" is the rule that governs it
// instead. A tree-wide scan would have to encode that exception and would report
// on files whose publish policy this gate does not own. A rename of the publisher
// is caught: the file is required to exist.
//
// The extraction is pure and unit-tested; the filesystem is reached only by the
// single ReadFile in Run.

import (
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// publisherFile is the workflow whose publish policy this gate owns. Slash-form.
const publisherFile = ".github/workflows/build-images.yml"

// gateVar is the workflow env var carrying the ref decision, named in both halves
// of the contract this gate holds together: the `${{ }}` expression that computes
// it, and the shell `if` that reads it.
const gateVar = "PUBLISH_MUTABLE"

// immutablePrefix marks the one tag that names a single commit and can therefore
// never be repointed. `sha-${SHA}` is written with the workflow's own env var, so
// the check is on the PREFIX rather than on a resolved sha.
const immutablePrefix = "sha-"

// refContext is what the gate expression must consult. An expression that does
// not mention it is not gating on the ref, whatever else it says.
const refContext = "github.ref"

// reTag captures the image reference of a `--tag` argument, quoted or not.
var reTag = regexp.MustCompile(`--tag\s+"?([^"\s]+)"?`)

// reShortTag finds docker's SHORT tag flag. It is not parsed — it is REFUSED,
// because `-t` would let a mutable publish back in past a guard that only reads
// `--tag`. A bypass this gate cannot see is worse than one it rejects.
var reShortTag = regexp.MustCompile(`(?:^|\s)-t\s+\S`)

// reGateExpr captures the value of the workflow env key that computes the gate.
// Anchored on the key at any indentation: it is one mapping entry in one file,
// and a YAML walk to find it would still have to be told the key's name.
var reGateExpr = regexp.MustCompile(`(?m)^\s*` + gateVar + `:\s*(\S.*?)\s*$`)

// tagSite is one `--tag` argument: where it is, what tag it names, and whether it
// is inside the gate.
type tagSite struct {
	line  int    // 1-based, in publisherFile
	ref   string // the whole image reference as written
	tag   string // the tag portion — `latest`, `sha-${SHA}`, `${VERSION}`
	gated bool
}

// mutable reports whether this tag can be repointed by a later build. Everything
// that is not a per-commit tag is mutable, INCLUDING a version tag: it is
// republished unchanged on every main build, so a branch moves it exactly as it
// moves `:latest`. Deriving the answer the other way round — an allowlist of
// mutable names — would let the next tag anyone adds default to "immutable".
func (s tagSite) mutable() bool { return !strings.HasPrefix(s.tag, immutablePrefix) }

// Run fails when publisherFile can publish a mutable tag from a non-default ref.
func Run(root string, out, errOut io.Writer) error {
	repo := capability.RepoForGate(Extension(), root)

	data, err := repo.ReadFile(filepath.FromSlash(publisherFile))
	if err != nil {
		// FAIL CLOSED ON A VANISHED SUBJECT. A rename would otherwise make this
		// gate report success over a publish policy it never read — the "examined
		// nothing" pass that looks exactly like the bug.
		return fmt.Errorf("mutable-tag-guard: read %s: %w\n"+
			"  That workflow is the image publisher this gate holds. If it moved, update publisherFile\n"+
			"  here and llz ci cosign-subject-guard's pinned subject together — the Kyverno signature\n"+
			"  policy names the same path", publisherFile, err)
	}
	body := string(data)

	sites := tagSites(body)
	problems := judge(body, sites)

	if len(problems) == 0 {
		gated, ungated := 0, 0
		for _, s := range sites {
			if s.gated {
				gated++
				continue
			}
			ungated++
		}
		fmt.Fprintf(out, "mutable-tag-guard: OK — %s: %d `--tag` site(s) publish from any ref, %d only behind %s\n",
			publisherFile, ungated, gated, gateVar)
		return nil
	}
	for _, p := range problems {
		if p.line > 0 {
			fmt.Fprintf(errOut, "::error file=%s,line=%d::%s\n", publisherFile, p.line, p.msg)
			continue
		}
		fmt.Fprintf(errOut, "::error file=%s::%s\n", publisherFile, p.msg)
	}
	fmt.Fprintf(errOut, "\n%s %d problem(s) with %s's publish policy:\n", color.Red("✗"), len(problems), publisherFile)
	for _, p := range problems {
		if p.line > 0 {
			fmt.Fprintf(errOut, "    %s:%d  %s\n", publisherFile, p.line, p.msg)
			continue
		}
		fmt.Fprintf(errOut, "    %s\n", p.msg)
	}
	fmt.Fprint(errOut, "\n"+remedy)
	return fmt.Errorf("mutable-tag-guard: %d problem(s) in %s", len(problems), publisherFile)
}

// remedy is the shape the workflow has to keep. It is printed rather than
// described because every failure of this gate has the same answer, and the
// answer is four lines of shell.
const remedy = `A mutable tag is a name main and every open PR resolve. Publishing one from a
branch repoints Lint's container fallback, the sha that ` + "`llz ci assert-image-fresh`" + `
compares against, and any instance that never pinned TF_IMAGE — all at once, and
invisibly, until the next push to main puts them back.

The shape this gate holds:

    NAMES=("${IMAGE}"); [ -z "${ALIAS}" ] || NAMES+=("${ALIAS}"); TAGS=()
    for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}"); done
    if [ "${` + gateVar + `}" = "true" ]; then
      for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:latest"); …; done
    fi

with ` + gateVar + ` computed from ` + refContext + ` in the workflow env block. The sha- tag
stays OUTSIDE the gate on purpose: release-e2e and ` + "`llz ci pin-instance-images`" + `
wait on it, so a branch build must still publish it.
`

// problem is one finding, with the file line it sits on (0 when the finding is
// about the file as a whole).
type problem struct {
	line int
	msg  string
}

// judge applies every arm to an already-parsed file. Pure, and the whole of the
// decision — Run does IO and printing and nothing else.
//
// IT COLLECTS RATHER THAN RETURNING ON THE FIRST. CI runs this without --verbose,
// so a finding suppressed by an early return is emitted nowhere and costs a
// second red round-trip to discover.
func judge(body string, sites []tagSite) []problem {
	var out []problem

	// FAIL CLOSED ON AN EMPTY CORPUS. No `--tag` at all is what a rewritten build
	// step, a moved publish, or a wrong file looks like; "0 tags, all gated" is a
	// green check over a policy nobody read.
	if len(sites) == 0 {
		out = append(out, problem{msg: "no `--tag` argument found — either the publish moved out of this file " +
			"or the build step was rewritten, and this gate would otherwise pass having judged nothing"})
	}
	if loc := shortFlagLine(body); loc > 0 {
		out = append(out, problem{line: loc, msg: "docker's short `-t` tag flag is refused here — write `--tag`, " +
			"which is the form this gate reads (a tag it cannot see is a tag it cannot hold)"})
	}

	var mutable, immutableUngated int
	for _, s := range sites {
		if !strings.Contains(s.ref, ":") {
			// SEPARATE "COULD NOT TELL" FROM "NOTHING THERE". A reference with no
			// tag portion is unparseable, not immutable.
			out = append(out, problem{line: s.line, msg: fmt.Sprintf("cannot read a tag out of `--tag %s` — "+
				"this gate has to know whether it is mutable, and a reference it cannot classify is not a pass", s.ref)})
			continue
		}
		if !s.mutable() {
			if !s.gated {
				immutableUngated++
			}
			continue
		}
		mutable++
		if !s.gated {
			out = append(out, problem{line: s.line, msg: fmt.Sprintf("`:%s` is a MUTABLE tag published from any ref — "+
				"move it inside the `%s` gate", s.tag, gateVar)})
		}
	}

	// NEVER DERIVE THE EXPECTED SET FROM THE THING UNDER TEST. If deleting the
	// `:latest` publish emptied the mutable set, arms 1 and 2 would both pass on a
	// workflow that had stopped publishing the tag version-pins requires every
	// container job in this repo to float on.
	if len(sites) > 0 && mutable == 0 {
		out = append(out, problem{msg: "no mutable tag is published at all. `:latest` is the tag `llz ci version-pins` " +
			"REQUIRES this repo's container images to name (a version tag is not yet published when the bump lands), " +
			"and `llz ci assert-image-fresh` reads its baked sha — so it has to keep being republished on main"})
	}
	// Same trap from the other side: a `sha-` tag inside the gate publishes
	// nothing on a branch, and release-e2e then waits out its budget on an image
	// that was never going to appear.
	if len(sites) > 0 && immutableUngated == 0 {
		out = append(out, problem{msg: fmt.Sprintf("no `%s…` tag is published outside the `%s` gate. That tag is what "+
			"`llz ci pin-instance-images --build-if-missing` triggers this build FOR and then blocks on, so a branch "+
			"build has to publish it", immutablePrefix, gateVar)})
	}

	expr, ok := gateExpression(body)
	switch {
	case !ok:
		out = append(out, problem{msg: fmt.Sprintf("no `%s:` env entry — the shell gate reads a variable this workflow "+
			"never sets, which expands empty and publishes nothing anywhere", gateVar)})
	case !strings.Contains(expr, refContext):
		out = append(out, problem{line: lineOf(body, gateVar+":"), msg: fmt.Sprintf("`%s` does not consult `%s` "+
			"(it is %s) — the tags are gated on something that is not the ref, which is the defect this gate exists for",
			gateVar, refContext, expr)})
	}
	return out
}

// tagSites extracts every `--tag` argument with its gate scope.
//
// IT READS THE FILE AS SHELL, WHICH IS THE HONEST LEVEL. The property is "this
// tag is inside that `if`", and a YAML walk to the `run:` scalar would still have
// to answer it by scanning the script. Actions YAML cannot confuse the scan: a
// job-level condition is `if:` WITH the colon, which is not the shell keyword,
// and comments are stripped before any token is read.
func tagSites(body string) []tagSite {
	var out []tagSite
	var stack []bool // one entry per open `if`; true when its condition names gateVar
	for i, raw := range strings.Split(body, "\n") {
		line := stripComment(raw)
		opensGate := hasToken(line, "if") && strings.Contains(line, gateVar)
		gated := opensGate
		for _, g := range stack {
			gated = gated || g
		}
		for _, m := range reTag.FindAllStringSubmatch(line, -1) {
			ref := m[1]
			out = append(out, tagSite{line: i + 1, ref: ref, tag: tagOf(ref), gated: gated})
		}
		// The stack is updated AFTER the line's own tags are judged, so a one-line
		// `if …; then TAGS+=(--tag …); fi` counts as gated rather than as whatever
		// encloses it.
		for _, tok := range strings.Fields(line) {
			switch strings.TrimRight(tok, ";") {
			case "if":
				stack = append(stack, opensGate)
			case "fi":
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
		}
	}
	return out
}

// tagOf is the tag portion of an image reference — everything after the last
// colon. Returns "" for a reference with no tag at all, which judge reports
// rather than classifying.
func tagOf(ref string) string {
	i := strings.LastIndex(ref, ":")
	if i < 0 {
		return ""
	}
	return ref[i+1:]
}

// gateExpression returns the `${{ … }}` expression the workflow computes the gate
// from.
func gateExpression(body string) (string, bool) {
	m := reGateExpr.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// shortFlagLine reports the first line using docker's `-t` short tag flag, or 0.
// Comments are stripped first — the prose explaining why `-t` is refused must not
// be read as a use of it, the same distinction setup-go-sole-site draws.
func shortFlagLine(body string) int {
	for i, raw := range strings.Split(body, "\n") {
		if reShortTag.MatchString(stripComment(raw)) {
			return i + 1
		}
	}
	return 0
}

// lineOf is the 1-based line of the first occurrence of needle, or 0.
func lineOf(body, needle string) int {
	for i, line := range strings.Split(body, "\n") {
		if strings.Contains(line, needle) {
			return i + 1
		}
	}
	return 0
}

// hasToken reports whether line contains word as a shell WORD — `if`, not `if:`
// (a job condition), `elif`, or `notify`.
func hasToken(line, word string) bool {
	for _, tok := range strings.Fields(line) {
		if strings.TrimRight(tok, ";") == word {
			return true
		}
	}
	return false
}

// stripComment removes a `#` comment, respecting quotes. Both YAML and shell use
// `#`, and both are present in this file — a trailing comment saying "if the
// version is empty" must not open an if-block in the scan.
func stripComment(line string) string {
	var quote rune
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '#' && (i == 0 || isSpace(rune(line[i-1]))):
			return line[:i]
		}
	}
	return line
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' }
