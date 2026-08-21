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
//  2. the gate expression is still the sanctioned one, and nothing sets that
//     variable anywhere else — `PUBLISH_MUTABLE: true` satisfies arm 1 and
//     publishes from everywhere, and so does an assignment inside the script;
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
// WHAT IT DOES NOT CATCH, stated rather than implied. This is a static reader of
// one file's shell, and a determined author can always put the publish somewhere it
// cannot see — a multi-line `{ … } >> "$GITHUB_ENV"` group, a value assembled
// across three variables, a script invoked from another file. The arms above are
// aimed at the EDIT SOMEONE MAKES BY ACCIDENT, which is what actually happened in
// #451: nobody was evading a guard, the publish simply was not gated. `llz ci
// assert-image-fresh` is the live half of the same question and answers it from the
// registry, where evasion has nowhere to hide.
//
// The extraction is pure and unit-tested; the filesystem is reached only by the
// single ReadFile in Run.

import (
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

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

// sanctionedGate is the one correct value of the gate expression, and it is
// COMPARED rather than read.
//
// PATTERN-READING THIS EXPRESSION WAS TRIED THREE TIMES AND LET AN INVERSION
// THROUGH EVERY TIME. `Contains(expr, "github.ref")` passed `github.ref != …` and
// `github.ref_name == 'main'`. Requiring `github.ref ==` with a ref literal passed
// `!(github.ref == 'refs/heads/main')`, `github.ref == github.ref`, and a
// feature-branch literal. Requiring a DEFAULT-branch literal still passed
// `'refs/heads/main' || 'refs/heads/my-feature'` and `|| true` — adding a
// disjunct rather than replacing one. Each fix was a narrower pattern and each
// had another spelling behind it, because "does this expression evaluate to
// `only on the default branch's HEAD`" is a question about SEMANTICS that a
// regular expression cannot ask.
//
// The expression is one line with one correct value, so the guard holds it to
// that value and prints it on failure. That is a stronger property than any
// reading of it, and the only one with no next spelling behind it. Whitespace is
// normalised; nothing else is.
const sanctionedGate = "${{ (github.ref == 'refs/heads/main' || github.ref == 'refs/heads/master') " +
	"&& (inputs.sha == '' || inputs.sha == github.sha) }}"

// `master` IS IN IT ON PURPOSE, and the reason is the `push:` filter, which is
// `branches: [main, master]`. The two have to agree: a push to a branch named
// `master` already triggers this workflow, so gating the publish more narrowly
// than the trigger would leave a build that runs, publishes only `sha-` tags, and
// disagrees with the file above it about which branch is the default. Fork
// portability is the other half — a fork whose default branch is `master` needs
// its own `:latest` republished. Removing it is a two-file change, not one.

// normalizeExpr collapses runs of whitespace so a folded scalar or a re-indent
// compares equal to the same expression on one line.
func normalizeExpr(e string) string { return strings.Join(strings.Fields(e), " ") }

// reTag captures the image reference of a `--tag` argument. BOTH SPELLINGS: the
// space-separated form and `--tag=<ref>`. Reading only the first was a bypass of
// exactly the class this guard refuses `-t` for — one `=` and the publish is
// invisible.
var reTag = regexp.MustCompile(`--tag[=\s"']["'\s]*([^"'\s]+)`)

// reShortTag finds docker's SHORT tag flag applied to something IMAGE-SHAPED. It
// is not parsed — it is REFUSED, because `-t` would let a mutable publish back in
// past a guard that only reads `--tag`.
//
// THE BUILD LINE IS THE WHOLE NARROWING, and it used to be joined by a second one
// that was worse than useless. Requiring a `:` in the argument was meant to spare
// `mktemp -t llz` and `sort -t ,`; what it actually did was exempt the most mutable
// publish there is — `-t "${REPO}/${IMAGE}"`, no tag at all, which docker publishes
// as `:latest`. (The comment justifying it also claimed the `:` excluded `docker
// run -t <image>:<tag>`, which is false: every image reference has a colon.) Scoped
// to a line that BUILDS, the separator and tty spellings are already out of reach —
// `mktemp -t` is not a build — so the flag is refused whatever follows it.
// The `(` and the quotes in the leading class are not decoration: an array append
// writes the flag as `TAGS+=(-t "…")` with no space in front of it, and the quoted
// spelling `TAGS+=("-t" "…")` puts a quote there. Since round thirteen nothing
// downstream cares which of the two it is — this arm refuses the SPELLING rather
// than parsing it — but the pattern still has to admit both, or the quoted one is
// simply not seen.
var reShortTag = regexp.MustCompile(`(?:^|[\s("'])(-t)(?:[=\s"']|$)`)

// reDockerPush finds a publish that carries no flag at all. `--tag` is not the
// only way to move a tag — `docker push <ref>:<tag>` moves one with nothing this
// guard reads on the line — so the same bypass family gets the same answer: it is
// refused rather than half-parsed, because the tags this workflow publishes are
// the ones assembled into the buildx invocation and nothing else.
var reDockerPush = regexp.MustCompile(`\b(docker)\s+(?:image\s+)?push\s+["']?\S`)

// reOutputPublish finds buildx's `--output`/`-o` EXPORTER, which publishes with no
// `--tag` anywhere on the line (`--output type=image,name=<ref>,push=true`). Same
// bypass family as `-t` and `docker push`, same answer: refused, not half-parsed.
//
// MATCHED ON THE VALUE, NOT THE FLAG, because `-o` collides with far more commands
// than `-t` does: `set -o pipefail` opens every script in this workflow, and
// `curl -o`, `kubectl -o json` and `sort -o` are all ordinary. An exporter says so
// in its argument, so that is what is looked for rather than a list of the
// commands it is not.
//
// AND "CONTAINS type=image" IS NOT "IS AN EXPORTER ARGUMENT" — `grep -o type=image
// manifest.txt` carries the substring and publishes nothing. What makes an
// exporter a PUBLISH is a destination: `name=` (the reference) or `push=true`.
// Without one it is a local export, which is not this gate's business either.
// The trailing group is everything up to the next space, so `--output-dir` can be
// told from `--output` with an argument: a flag this arm does not own is followed
// by more flag, not by a separator.
var reOutputPublish = regexp.MustCompile(`(?:^|[\s("'])(--output|-o)([^\s"'=]*)(?:[=\s"']+["']?([^"'\s\\]+))?`)

// exporterPublish reports whether an `--output` value is a publishing exporter.
func exporterPublish(v string) bool {
	if !strings.Contains(v, "type=image") && !strings.Contains(v, "type=registry") {
		return false
	}
	return strings.Contains(v, "name=") || strings.Contains(v, "push=true")
}

// unreadableValue reports a value this arm cannot judge: absent (the flag's
// argument is on the next line) or WHOLLY an expansion (it is in a variable). An
// expansion INSIDE an otherwise readable exporter — `name=${REPO}/x:latest` — is
// not unreadable; the attributes that make it a publish are right there. Both are
// failures ON A BUILD LINE — `--tag "$LATEST"` already fails closed as an
// unreadable reference, and the exporter has to be held to the same rule or it is
// the way round it. Off a build line there is no reason to think an unreadable
// `-o` is an exporter at all, which is what keeps `curl -fsSL -o \` and a trailing
// `jq … -o` out of it.
func unreadableValue(v string) bool {
	return v == "" || strings.HasPrefix(v, "$") || strings.HasPrefix(v, "`")
}

// reGateAssigned finds the gate variable being SET inside a script — either as a
// shell variable (`PUBLISH_MUTABLE=true`, with or without `export`), which
// overrides what the expression produced for the rest of that script, or written
// into `$GITHUB_ENV`, which does the same for every later step. The expression is
// held to one exact value; an assignment makes that value irrelevant, and nothing
// in the YAML says so.
// ANCHORED AT A COMMAND POSITION. `PUBLISH_MUTABLE=` after another word is an
// ARGUMENT to that word — `echo PUBLISH_MUTABLE="${PUBLISH_MUTABLE}"` prints the
// value and `env | grep PUBLISH_MUTABLE=` looks for it, and neither changes the
// gate. An assignment starts a command: at the beginning of the line, or after a
// separator, optionally behind `export`.
// `export` IS ONE OF FIVE SPELLINGS, and the other four set the variable just as
// well. The quote in the leading class is for the sixth, which lives inside
// `eval "…"` — see the eval handling in scanFile for why that one is read from the
// original line rather than the blanked one.
var reGateAssigned = regexp.MustCompile(`(?:^|[;&|({"']|\bthen\b|\bdo\b|\belse\b)\s*` +
	`(?:(?:export|readonly|declare|typeset|local)\s+)?` + gateVar + `\s*=`)

// reGateRead is `read` assigning the gate — an assignment with no `=` in it.
var reGateRead = regexp.MustCompile(`\bread\b[^;|&]*\b` + gateVar + `\b`)

// reEvalContext names the commands that run a STRING as shell. Inside one, the
// quoted span is code rather than a message, so the assignment scan reads the
// original line there instead of the blanked one — `eval "PUBLISH_MUTABLE=true"`
// sets the gate, while `echo "PUBLISH_MUTABLE=$X"` prints it.
var reEvalContext = regexp.MustCompile(`\b(eval|(?:sh|bash|zsh)\s+-c)\b`)

// reGithubEnvWrite is a redirect INTO $GITHUB_ENV. Two unanchored Contains stood
// here while the comment beside them claimed the clause was "pinned to the
// redirect" — so `echo "see GITHUB_ENV docs before touching PUBLISH_MUTABLE"` read
// as setting the gate. That is the comment-asserts-an-exclusion-the-pattern-lacks
// defect this file has now named three times.
// `>>` ALONE WAS NARROWER THAN THE TWO Contains IT REPLACED: `> "$GITHUB_ENV"`
// truncates and writes, and `| tee -a` writes without a redirect at all. Pinning
// to the redirect was the right idea and `>>` was the wrong pin.
var reGithubEnvWrite = regexp.MustCompile(`(?:>>?|\btee\b(?:\s+-\S+)*)\s*["']?\$\{?GITHUB_ENV`)

// reHeredoc finds a heredoc opening. `<<<` (a herestring) is deliberately not
// matched: it is one word, not a body this scanner would go on to read as code.
//
// THE LEADING `[^<]` IS WHAT MAKES THAT TRUE. Without it the scan simply started on
// the SECOND `<` of `<<<word` and matched anyway — a comment asserting an exclusion
// the pattern did not implement, which is the same class of defect as the `-t`
// comment above.
var reHeredoc = regexp.MustCompile(`(?:^|[^<])<<-?\s*["']?[A-Za-z_]`)

// reArithmetic matches an arithmetic expansion, which is removed before the
// heredoc scan: `$((1 << attempt))` is a left shift, and this workflow's retry
// backoff is one edit from being written that way.
var reArithmetic = regexp.MustCompile(`\$\(\([^)]*\)\)`)

// reGateOpen matches THE canonical gate: an `if` whose condition is the POSITIVE
// test of gateVar.
//
// POSITIVE, AND THAT IS THE POINT. "Is this tag lexically inside the gate?" is not
// the question — `if [ "${PUBLISH_MUTABLE}" != "true" ]` puts the mutable publish
// inside a block that runs on every ref EXCEPT the default branch, which is #451
// exactly inverted, and a scanner that only locates the block passes it. `!=`
// cannot match here because the pattern requires `=` where the `!` sits.
var reGateOpen = regexp.MustCompile(`^if\s+\[\[?\s*"?\$\{?` + gateVar + `\}?"?\s*=\s*"?true"?\s*\]\]?`)

// reBuildCommand names a container build invocation: the command whose own `-t`
// is a tag flag by definition, and the only place an unreadable `-o` is assumed to
// be an exporter.
var reBuildCommand = regexp.MustCompile(`\b(buildx|docker\s+build)\b`)

// reBlockToken finds the shell keywords that open, switch and close a block, as
// WORDS. `elif` cannot match the `if` alternative — the `l` before it leaves no
// word boundary — and Actions' own `if:` is not a word match either.
var reBlockToken = regexp.MustCompile(`\b(if|elif|else|fi|then)\b`)

// shortTagCommands are the commands that take a `-t` meaning something other than a
// container tag. Named rather than inferred: the list is short because this guard
// reads ONE file, and a line here is a line somebody is already looking at.
// Matched as WORDS, not substrings, and not as shell tokens either: `$(mktemp -t
// …)` puts the command inside a token that starts `TMP=$(`, so a token comparison
// misses it — while a plain substring test would exempt any line containing
// `UPDATE_DATE` on the strength of "date".
var reShortTagCommands = regexp.MustCompile(`\b(mktemp|sort|join|column|tar|date|timeout|ps)\b`)

// reDockerRun exempts `docker run -t`, which allocates a tty rather than naming a
// tag. The version-stamp step in this workflow runs the image it just built.
var reDockerRun = regexp.MustCompile(`\bdocker\s+run\b`)

// shortTagExempt reports whether this line's `-t` belongs to something that is not
// a container tag.
func shortTagExempt(bare string) bool {
	// A BUILD'S OWN `-t` IS NEVER EXEMPT, whatever else the line says. The
	// exemptions are for lines whose COMMAND is mktemp, sort or `docker run`; on a
	// build invocation `-t` is the tag flag by definition, and reading the
	// exemptions line-wide let `--build-arg BUILD_DATE="$(date …)"` excuse the tag
	// beside it.
	if buildLine(bare) {
		return false
	}
	return reDockerRun.MatchString(bare) || reShortTagCommands.MatchString(bare)
}

// quoteSpan is one quoted run: `open` is the index of the opening quote (-1 when
// the run began on an earlier line) and `close` the index just past its content.
type quoteSpan struct {
	open, close int
}

// contains reports whether off falls inside the span's CONTENT.
func (q quoteSpan) contains(off int) bool { return off > q.open && off < q.close }

// spansOf reads the quoted runs of a line, continuing a run left open by the
// previous one. It returns the spans and the quote still open at end of line.
//
// A STRING CAN OUTLIVE ITS LINE, and resetting the state per line meant the second
// half of a multi-line string was read as code — where one `fi` in prose pops the
// gate stack and every tag after it stops being gated. Backslash escapes are
// honoured inside double quotes and not inside single ones, which is the shell's
// own rule and the difference between tracking the string and losing it at the
// first `\"`.
func spansOf(line string, open byte) ([]quoteSpan, byte) {
	var spans []quoteSpan
	quote, start := open, -1
	sub := 0 // open `$( … )` levels inside the current double-quoted run
	for i := 0; i < len(line); i++ {
		b := line[i]
		// A COMMAND SUBSTITUTION INSIDE A STRING IS CODE. `TMP="$(mktemp -t llz)"` is
		// quoted, and its contents are a command — the shell says so, and blanking them
		// lost every word the scanners read. So the span CLOSES at `$(` and the loop
		// carries on in CODE state until the matching `)`, at which point the quoted
		// run reopens.
		//
		// CARRIES ON, rather than skipping ahead to the paren: a skip that counted
		// parens with no notion of quoting never balanced `tr -d '('`, ran to end of
		// line, and left every later line marked as string content. Tracking it means
		// the inner quotes are spans of their own, which is also what stops a `#`
		// inside one from reading as a comment.
		if quote == '"' && b == '$' && i+1 < len(line) && line[i+1] == '(' {
			spans = append(spans, quoteSpan{open: start, close: i})
			quote, start = 0, -1
			sub, i = sub+1, i+1
			continue
		}
		if sub > 0 && quote == 0 && b == '(' {
			sub++
			continue
		}
		if sub > 0 && quote == 0 && b == ')' {
			if sub--; sub == 0 {
				quote, start = '"', i // the quoted run resumes at the closing paren
			}
			continue
		}
		switch {
		case b == '\\' && quote != '\'':
			// THE ESCAPE WORKS OUTSIDE QUOTES TOO, and honouring it only inside them
			// meant `echo Say \"hello` opened a phantom run that swallowed the rest of
			// the script — after which an ungated `:latest` read as prose. Single
			// quotes are the one place a backslash is literal, which is the shell's
			// rule and the reason that case is excluded rather than special-cased.
			i++ // the escaped byte is content, whatever it is
		case quote != 0:
			if b == quote {
				spans = append(spans, quoteSpan{open: start, close: i})
				quote, start = 0, -1
			}
		case b == '\'' || b == '"':
			quote, start = b, i
		}
	}
	if quote != 0 {
		spans = append(spans, quoteSpan{open: start, close: len(line)})
	}
	return spans, quote
}

// spanAt returns the quoted span containing off, if any.
func spanAt(spans []quoteSpan, off int) (quoteSpan, bool) {
	for _, q := range spans {
		if q.contains(off) {
			return q, true
		}
	}
	return quoteSpan{}, false
}

// quotedFlag reports whether a flag found at off is inside a string.
//
// FOUR ROUNDS TRIED TO TELL A QUOTED ARGUMENT LIST FROM A SENTENCE, and this is
// the function that stopped trying. The rules went: the span is exactly the flag;
// the span BEGINS with the flag; begins-with and every later word is
// "value-shaped"; the same plus a list of container subcommands. Each widening let
// a real publish through — `EXTRA="--tag …:latest --target runtime"`,
// `eval "docker push … && echo ok"` — or turned an `::error::` message into one,
// and the two failures alternated round after round.
//
// They alternate because the question is undecidable at this altitude: `"--tag
// x:latest"` and `"--tag is assembled into TAGS"` differ in ENGLISH, not in shell.
// So the guard asks a decidable question instead — is the flag inside a string? —
// and REFUSES when it is, which is what it already does with `-t`, `docker push`,
// `--output` and heredocs. Refusing is satisfiable in one edit (unquote the flag,
// or reword the sentence) and it has no next spelling. A rule that fails closed on
// prose costs a reworded message; a rule that guesses wrong costs `:latest`
// republished from every branch, and that one nobody notices.
func quotedFlag(spans []quoteSpan, off int) bool {
	_, inside := spanAt(spans, off)
	return inside
}

// tagSite is one `--tag` argument: where it is, what tag it names, and whether it
// is inside the gate.
type tagSite struct {
	line  int    // 1-based, in publisherFile
	ref   string // the whole image reference as written
	tag   string // the tag portion — `latest`, `sha-${SHA}`, `${VERSION}`
	gated bool
	// namesGate marks a site whose own line tests gateVar without being the
	// canonical block. Such a line is a real attempt at the rule spelled a way this
	// guard cannot verify, and telling its author to "move it inside the gate" —
	// which is what the plain finding says — would be advice about a gate they are
	// already looking at.
	namesGate bool
}

// mutable reports whether this tag can be repointed by a later build. Everything
// that is not a per-commit tag is mutable, INCLUDING a version tag: it is
// republished unchanged on every main build, so a branch moves it exactly as it
// moves `:latest`. Deriving the answer the other way round — an allowlist of
// mutable names — would let the next tag anyone adds default to "immutable".
func (s tagSite) mutable() bool { return !strings.HasPrefix(s.tag, immutablePrefix) }

// scan is everything reading the file yields: the publish sites, and the two ways
// the read itself can fail.
type scan struct {
	sites []tagSite
	// unbalanced holds the line of the outermost `if` left open in each `run:`
	// script. A scanner that lost the block structure cannot say what is gated, and
	// "could not tell" is not "nothing wrong".
	unbalanced []int
	// shortFlag is the first line publishing through docker's `-t`, or 0.
	shortFlag int
	// push is the first line publishing through a bare `docker push <ref>:<tag>`, or 0.
	push int
	// output is the first line publishing through buildx's `--output` exporter, or 0.
	output int
	// quoted is the first line naming a publish flag inside a string, or 0.
	quoted int
	// heredoc is the first line opening a heredoc, or 0 — a quoting form this
	// scanner does not model, so it refuses rather than reads the body as code.
	heredoc int
	// runtimeEnv is the first line writing the gate variable to $GITHUB_ENV, or 0.
	runtimeEnv int
	// parseErr is set when the file is not YAML this gate can walk.
	parseErr error
}

// note records the first line an arm fired on.
func (s *scan) note(field *int, line int) {
	if *field == 0 && line != 0 {
		*field = line
	}
}

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

	sc := scanFile(body)
	problems := judge(body, sc)

	if len(problems) == 0 {
		gated, ungated := 0, 0
		for _, s := range sc.sites {
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
func judge(body string, sc scan) []problem {
	var out []problem

	// SEPARATE "COULD NOT TELL" FROM "NOTHING THERE", at the outermost level: a file
	// this gate cannot parse is a file it has not read, and every arm below would
	// then be judging an empty set.
	if sc.parseErr != nil {
		return []problem{{msg: fmt.Sprintf("could not be parsed as YAML (%v) — every check below would "+
			"otherwise run over zero scripts and report clean", sc.parseErr)}}
	}
	sites := sc.sites

	// FAIL CLOSED ON AN EMPTY CORPUS. No `--tag` at all is what a rewritten build
	// step, a moved publish, or a wrong file looks like; "0 tags, all gated" is a
	// green check over a policy nobody read.
	if len(sites) == 0 {
		out = append(out, problem{msg: "no `--tag` argument found — either the publish moved out of this file " +
			"or the build step was rewritten, and this gate would otherwise pass having judged nothing"})
	}
	if sc.heredoc > 0 {
		// A HEREDOC IS A THIRD QUOTING FORM, and this scanner models two. Reading a
		// heredoc body as code turns a `fi` or a `--tag` in a template into a verdict,
		// so it refuses. Same rule as everywhere else here: what it cannot read, it
		// does not pass.
		out = append(out, problem{line: sc.heredoc, msg: "this `run:` script opens a heredoc — its body is not shell " +
			"this guard can tell from code, so it refuses rather than reading a template as a publish. Keep the " +
			"tag assembly out of heredocs, or teach the scanner this form"})
	}
	if sc.runtimeEnv > 0 {
		// THE THIRD DOOR ONTO THE SAME VARIABLE. Workflow env is checked from the
		// YAML, step env with it — but a step that writes `PUBLISH_MUTABLE=true` to
		// $GITHUB_ENV sets it for every LATER step, on any ref, with the correct
		// expression still sitting in the file above. Nothing in the YAML says so.
		out = append(out, problem{line: sc.runtimeEnv, msg: fmt.Sprintf("this script SETS `%s` — as a shell variable "+
			"for the rest of the script, or into $GITHUB_ENV for every later step. Either way the gate is decided at "+
			"RUNTIME, on any ref, while the expression in the env block still reads correctly and nothing in the "+
			"YAML says otherwise. The gate has exactly one source, and it is that expression", gateVar)})
	}
	if sc.quoted > 0 {
		out = append(out, problem{line: sc.quoted, msg: "a `--tag` inside a string is refused here — quoted, it is a " +
			"publish when it is an argument (`EXTRA=\"--tag …\"`, `TAGS+=(\"--tag\" …)`) and prose when it is a " +
			"sentence, and those two differ in English rather than in shell. Four rules tried to tell them apart and " +
			"each one either let a real publish through or reddened a message; this one is decidable. Write the flag " +
			"unquoted so the gate can read it, or reword the sentence"})
	}
	if sc.output > 0 {
		out = append(out, problem{line: sc.output, msg: "buildx's `--output` exporter is refused here — " +
			"`type=image,name=<ref>,push=true` publishes a tag with no `--tag` on the line for this gate to read. " +
			"Assemble the tags into TAGS and let `--push` publish them"})
	}
	if sc.push > 0 {
		out = append(out, problem{line: sc.push, msg: "`docker push` of an explicit tag is refused here — a publish " +
			"outside the `--tag` list this gate reads is a publish it cannot hold to the ref. Add the tag to TAGS " +
			"and let the build push it"})
	}
	if sc.shortFlag > 0 {
		out = append(out, problem{line: sc.shortFlag, msg: "docker's short `-t` tag flag is refused here — write `--tag`, " +
			"which is the form this gate reads (a tag it cannot see is a tag it cannot hold)"})
	}
	for _, line := range sc.unbalanced {
		// A SCANNER THAT LOST THE BLOCK STRUCTURE CANNOT ANSWER THE QUESTION. Whatever
		// it says about what is gated after an unclosed `if` is a guess, and a guess
		// that resolves to "gated" is this gate passing over the defect itself.
		out = append(out, problem{line: line, msg: "this `run:` script's `if` blocks did not close — the gate scope " +
			"after that point is unknowable, so this fails rather than guessing (if the shell is correct and " +
			"this guard mis-read it, that is a bug in the guard, not a reason to edit around it)"})
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
		if s.gated {
			continue
		}
		if s.namesGate {
			// It IS an attempt at the rule, spelled a way this guard cannot verify.
			// "Move it inside the gate" would be advice about a gate its author is
			// looking at, so say what is actually wrong instead.
			out = append(out, problem{line: s.line, msg: fmt.Sprintf("`:%s` is a MUTABLE tag inside a test of `%s` that "+
				"is not the canonical gate — this guard recognises only `if [ \"${%s}\" = \"true\" ]; then … fi`, "+
				"because a condition it cannot read is a condition it cannot confirm points the right way. `!=` "+
				"publishes from every ref EXCEPT the default branch, which is this bug inverted rather than fixed",
				s.tag, gateVar, gateVar)})
			continue
		}
		out = append(out, problem{line: s.line, msg: fmt.Sprintf("`:%s` is a MUTABLE tag published from any ref — "+
			"move it inside the `%s` gate", s.tag, gateVar)})
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

	entries := gateExpressions(body)
	if len(entries) == 0 {
		out = append(out, problem{msg: fmt.Sprintf("no `%s:` env entry — the shell gate reads a variable this workflow "+
			"never sets, which expands empty and publishes nothing anywhere", gateVar)})
	}
	for _, e := range entries {
		if normalizeExpr(e.expr) == normalizeExpr(sanctionedGate) {
			continue
		}
		out = append(out, problem{line: e.line, msg: fmt.Sprintf("`%s` is not the sanctioned gate expression. "+
			"It is `%s`; it must be `%s`. Held to the exact value because every attempt to READ it let an "+
			"inversion through — `%s !=`, `%s_name` (which is `main`, not `refs/heads/main`, so the gate is false "+
			"everywhere and `:latest` silently stops being republished), `!( … )`, `%s == %s`, a feature-branch "+
			"literal, and `|| true`. The `inputs.sha` half is not decoration either: `workflow_dispatch` takes a "+
			"`sha` input, so without it a dispatch on the default branch stamps `:latest` with any commit it is "+
			"given. Env layers and the INNERMOST wins, so every setting of this variable has to be this one",
			gateVar, e.expr, sanctionedGate, refContext, refContext, refContext, refContext)})
	}
	return out
}

// scanFile extracts every `--tag` argument in every `run:` script, with its gate
// scope.
//
// IT READS THE `run:` SCRIPTS AS SHELL, AND NOTHING ELSE AS ANYTHING. Both halves
// are scars. Scanning the whole file for `if`/`fi` counted an input's
// `description:` prose ("…matches even if the branch HEAD has since moved") as an
// open block that never closed; scanning a script without regard to quoting
// counted `echo "… only if this is true"` the same way. Either one leaves every
// later tag looking enclosed — and if the same line happens to name PUBLISH_MUTABLE,
// looking GATED, which is the guard reporting OK over the defect it exists for.
//
// So: YAML says which text is shell (a `run:` value), and each script is scanned
// with its own block stack, on text with quoted spans blanked out. The gate's own
// condition survives that blanking because it is TESTED on the unblanked line —
// the shell keywords are what must not come out of a string, not the variable name.
func scanFile(body string) scan {
	var out scan
	scripts, err := runScripts(body)
	if err != nil {
		out.parseErr = err
		return out
	}
	for _, sc := range scripts {
		var stack []block // one entry per open `if`
		pending := -1     // index in stack of the block whose condition is still open
		// building tracks a container build across its continuation lines. It is NOT
		// what decides whether a flag is refused — that was tried and missed the tag
		// array — only whether an UNREADABLE `-o` value is treated as an exporter.
		building, continued := false, false
		var open byte // the quote still open from the previous line, if any
		for i, raw := range strings.Split(sc.body, "\n") {
			// ONE RULE FOR WHAT IS QUOTED, used by both halves. stripComment had its
			// own quote scanner, which did not honour the backslash escape spansOf
			// does — so on `echo Say \"hello   # publish only if this is main` the two
			// disagreed about where the string ended, the comment survived, and its
			// `if` was read as shell. The spans are computed once, on the RAW line, and
			// the comment is whatever follows the first `#` outside them.
			fileLine := sc.startLine + i
			rawSpans, _ := spansOf(raw, open)
			line := raw
			if cut := commentStart(raw, rawSpans, open); cut >= 0 {
				line = raw[:cut]
			}
			var spans []quoteSpan
			spans, open = spansOf(line, open)
			bare := blank(line, spans) // shell structure only — no keyword may come out of a string
			if out.heredoc == 0 && reHeredoc.MatchString(reArithmetic.ReplaceAllString(bare, "")) {
				out.heredoc = fileLine
			}
			// TWO TEXTS, DELIBERATELY. A shell assignment is CODE, so it is read from
			// the blanked line — `echo "PUBLISH_MUTABLE=$X"` prints the value and
			// assigns nothing. A `$GITHUB_ENV` write is the opposite: the assignment it
			// carries lives INSIDE the string being echoed, so that one is read from the
			// original and pinned to the redirect, which is what makes it a write rather
			// than a mention.
			assignScan := bare
			if reEvalContext.MatchString(bare) {
				assignScan = line // the quoted span is a script, not a message
			}
			if out.runtimeEnv == 0 && (reGateAssigned.MatchString(assignScan) ||
				reGateRead.MatchString(assignScan) ||
				(strings.Contains(line, gateVar) && reGithubEnvWrite.MatchString(line))) {
				out.runtimeEnv = fileLine
			}
			// EVERY MATCH, NOT THE FIRST: a line can name a flag and then use it.
			if !shortTagExempt(bare) {
				out.note(&out.shortFlag, publishFlag(line, reShortTag, fileLine))
			}
			building = buildLine(bare) || (building && continued)
			continued = strings.HasSuffix(strings.TrimRight(bare, " \t"), `\`)
			out.note(&out.output, outputPublish(line, bare, fileLine, building))
			out.note(&out.push, publishFlag(line, reDockerPush, fileLine))
			// WITHIN A LINE, ORDER DECIDES. `…; then echo x; else TAGS+=(--tag …:latest);
			// fi` publishes that tag when the gate is FALSE, and a tag written after a
			// `fi` is not inside anything — but a scanner that judges the line's tags
			// first and its keywords afterwards records both as gated. So the keyword
			// events and the tag sites are replayed in the order they appear.
			events := blockEvents(line, bare, fileLine)
			opened, names := false, false
			for _, e := range events {
				opened = opened || (e.kind == "if" && e.gate)
				names = names || (e.kind == "if" && e.namesVar)
			}
			// A CONDITION CAN WRAP, and reading only the `if`'s own line made a
			// PUBLISH_MUTABLE test on the continuation invisible — so every tag in the
			// body was told to "move it inside the gate" it was already inside. While a
			// condition is still open, a mention of the variable belongs to it.
			if pending >= 0 && pending < len(stack) && strings.Contains(line, gateVar) && !stack[pending].gate {
				stack[pending].namesVar = true
			}
			next := 0
			for _, m := range reTag.FindAllStringSubmatchIndex(line, -1) {
				if quotedFlag(spans, m[0]) {
					out.note(&out.quoted, fileLine)
					continue // judged by the quoted-flag arm, which refuses the spelling
				}
				for next < len(events) && events[next].off < m[0] {
					stack = apply(stack, events[next])
					next++
				}
				gated, unverifiable := false, names
				for _, b := range stack {
					gated = gated || b.gate
					unverifiable = unverifiable || (b.namesVar && !b.gate)
				}
				ref := line[m[2]:m[3]]
				out.sites = append(out.sites, tagSite{
					line: fileLine, ref: ref, tag: tagOf(ref), gated: gated,
					namesGate: !opened && (unverifiable || strings.Contains(line, gateVar)),
				})
			}
			for ; next < len(events); next++ {
				stack = apply(stack, events[next])
			}
			for _, e := range events {
				switch e.kind {
				case "if":
					pending = len(stack) - 1 // this block's condition is open until `then`
				case "then":
					pending = -1
				}
			}
		}
		if len(stack) > 0 {
			out.unbalanced = append(out.unbalanced, stack[0].line)
		}
	}
	return out
}

// segmentAt is the command segment containing off — the text back to the nearest
// separator. Command substitutions are unblanked by the time this runs, so a build
// inside one governs its own flags rather than the outer line's.
// It also reports whether a separator preceded off ON THIS LINE. When none did,
// the flag belongs to whatever command the previous line was still writing — the
// one-flag-per-line layout the real build command uses, where the continuation
// line names no command at all.
func segmentAt(bare string, off int) (string, bool) {
	if off > len(bare) {
		off = len(bare)
	}
	start := 0
	for i := 0; i < off; i++ {
		switch bare[i] {
		case ';', '&', '|', '(', ')':
			start = i + 1
		}
	}
	return bare[start:off], start > 0
}

// buildLine reports whether this line (or segment) invokes a container build.
func buildLine(bare string) bool {
	return reBuildCommand.MatchString(bare)
}

// outputPublish reports where this line publishes through buildx's exporter, or 0.
func outputPublish(line, bare string, fileLine int, building bool) int {
	for _, m := range reOutputPublish.FindAllStringSubmatchIndex(line, -1) {
		if m[4] >= 0 && line[m[4]:m[5]] != "" {
			continue // `--output-dir` and friends: a different flag, not this one
		}
		v := ""
		if m[6] >= 0 {
			v = line[m[6]:m[7]]
		}
		if unreadableValue(v) {
			// FAILING CLOSED IS FOR THE BUILD'S OWN FLAGS. The command after `&&` is a
			// different command, and `sort -o "$OUT" f` is not an exporter however the
			// line began — so the question is asked of the SEGMENT the flag sits in
			// rather than of the whole line. A continued build that has not reached a
			// separator yet is still the build, which is what `building` carries.
			seg, separated := segmentAt(bare, m[2])
			if building && (!separated || buildLine(seg)) {
				return fileLine
			}
			continue
		}
		if exporterPublish(v) {
			return fileLine
		}
	}
	return 0
}

// publishFlag reports where this line uses one of the refused publish spellings,
// or 0. Quoted or not: these arms REFUSE a spelling rather than parse it, so there
// is nothing for quoting to change about the answer.
func publishFlag(line string, re *regexp.Regexp, fileLine int) int {
	if re.MatchString(line) {
		return fileLine
	}
	return 0
}

// blockEvent is one shell block keyword and where it sits on the line. `gate`
// marks an `if` whose condition is the canonical positive test.
type blockEvent struct {
	off  int
	line int
	kind string
	gate bool
	// namesVar marks an `if` whose CONDITION mentions the gate variable without
	// being the canonical form. It is carried on the block rather than judged per
	// line because a gate spans lines: `if [[ "${PUBLISH_MUTABLE}" == "true" ]]` is
	// a real gate this guard cannot confirm, and every tag inside it used to be told
	// to "move it inside the gate" it was already in.
	namesVar bool
}

// block is one open `if`: whether it is the canonical gate, and whether it is an
// unverifiable attempt at one.
type block struct {
	gate, namesVar bool
	// line is where this `if` opened, so an unclosed one can be NAMED. "Somewhere in
	// this script" is the least actionable thing a scanner can say about a block it
	// lost track of, and it always knows which one.
	line int
}

// blockEvents reads the line's block keywords in order. Offsets come from the
// blanked text — so no keyword is read out of a string — and the CONDITION is read
// from the original at the same offset, which is why blankQuoted must preserve
// byte positions exactly.
func blockEvents(line, bare string, fileLine int) []blockEvent {
	var out []blockEvent
	for _, m := range reBlockToken.FindAllStringSubmatchIndex(bare, -1) {
		kind := bare[m[2]:m[3]]
		e := blockEvent{off: m[0], line: fileLine, kind: kind}
		if kind == "if" {
			e.gate = reGateOpen.MatchString(line[m[0]:])
			e.namesVar = !e.gate && strings.Contains(condition(line[m[0]:]), gateVar)
		}
		out = append(out, e)
	}
	return out
}

// condition is the text between `if` and the `then` that closes it, on this line.
func condition(rest string) string {
	if i := strings.Index(rest, "then"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// apply advances the block stack by one keyword.
func apply(stack []block, e blockEvent) []block {
	switch e.kind {
	case "if":
		return append(stack, block{gate: e.gate, namesVar: e.namesVar, line: e.line})
	case "then": // structure-free: it only closes a condition, which scanFile tracks
	case "else", "elif":
		// THE ELSE ARM RUNS WHEN THE GATE IS FALSE. Leaving the entry set would mark
		// a mutable tag there as gated when it publishes from precisely the refs the
		// gate exists to exclude.
		if len(stack) > 0 {
			stack[len(stack)-1].gate = false
		}
	case "fi":
		if len(stack) > 0 {
			return stack[:len(stack)-1]
		}
	}
	return stack
}

// script is one `run:` value with the file line its first line sits on.
type script struct {
	startLine int
	body      string
}

// runScripts returns every `run:` scalar in the workflow, in document order.
func runScripts(body string) ([]script, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(body), &root); err != nil {
		return nil, err
	}
	var out []script
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				if k.Value == "run" && v.Kind == yaml.ScalarNode {
					// A block scalar's node line is the `run: |` line and its content
					// starts on the next one; an inline scalar starts on its own line.
					start := v.Line
					if v.Style == yaml.LiteralStyle || v.Style == yaml.FoldedStyle {
						start++
					}
					out = append(out, script{startLine: start, body: v.Value})
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&root)
	return out, nil
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

// gateEntry is one place the workflow sets the gate variable.
type gateEntry struct {
	line int
	expr string
}

// gateExpressions returns EVERY setting of the gate variable, in file order.
//
// EVERY, BECAUSE GITHUB RESOLVES THE LAST ONE THAT APPLIES. Env is layered
// workflow → job → step, and the innermost wins, so a step-level
// `PUBLISH_MUTABLE: 'true'` defeats the gate at runtime while the workflow-level
// expression above it still reads perfectly. Checking the first match is checking
// the entry that LOSES — the shape this arm exists to catch, sneaking past the arm
// itself. All of them must consult the ref.
func gateExpressions(body string) []gateEntry {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(body), &root); err != nil {
		return nil // judge has already failed the file on this
	}
	var out []gateEntry
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if k, v := n.Content[i], n.Content[i+1]; k.Value == gateVar && v.Kind == yaml.ScalarNode {
					out = append(out, gateEntry{line: v.Line, expr: v.Value})
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&root)
	return out
}

// blankQuoted replaces the CONTENTS of quoted spans with spaces, keeping every
// other character in place so column positions and word boundaries survive.
//
// It is applied only where shell STRUCTURE is read. `echo "publishing :latest only
// if this is true"` contains no keyword — the shell sees one word — and a scanner
// that reads `if` out of it opens a block that never closes, after which every tag
// in the script looks enclosed. The gate's own condition is matched on the
// unblanked line, so blanking cannot hide the thing being looked for.
// BYTE-WISE, NOT RUNE-WISE, and that is load-bearing rather than a style choice:
// the keyword offsets found in the blanked text are used to index the ORIGINAL
// line. Blanking a multi-byte rune (this file's comments and echoes carry em
// dashes) to a single space would shorten the string and slide every later offset,
// pointing the condition match at the middle of a word. Quote characters are
// ASCII, and no UTF-8 continuation byte can be mistaken for one, so a byte scan is
// safe as well as position-preserving.
func blank(line string, spans []quoteSpan) string {
	out := []byte(line)
	for _, q := range spans {
		for i := q.open + 1; i < q.close; i++ {
			out[i] = ' '
		}
	}
	return string(out)
}

// commentStart is the index of the `#` that begins a comment, or -1.
//
// Both YAML and shell use `#`, and both are present in this file. A `#` inside a
// quoted run is content, and a `#` that is not preceded by whitespace is part of a
// word (`sha-1#2`), so neither starts a comment. When the line begins INSIDE a
// string carried over from the previous one, nothing on it can start a comment
// until that string closes — which is why the spans are consulted rather than
// re-derived here.
func commentStart(line string, spans []quoteSpan, open byte) int {
	for i := 0; i < len(line); i++ {
		if line[i] != '#' {
			continue
		}
		if _, inside := spanAt(spans, i); inside {
			continue
		}
		if i == 0 && open == 0 {
			return 0
		}
		if i > 0 && isSpace(rune(line[i-1])) {
			return i
		}
	}
	return -1
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' }
