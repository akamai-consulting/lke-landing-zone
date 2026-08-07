package main

// ci_upgrade_test_gate.go implements `llz ci upgrade-test` — the day-2 twin of
// template-scripts/ci/instance-test.sh.
//
// THE GAP THIS CLOSES. `copier update` did not appear in a single workflow,
// Makefile target, or script — only in comments. instance-test runs `copier
// copy` and stops. So the SCAFFOLD path was gated and the UPGRADE path, which
// every adopter takes on day 2 and which touches the same answers, tasks and
// merge machinery, was exercised by nobody.
//
// That is the shape `llz ci assert-adopter-pin` exists for, one altitude up: a
// harness that covers the configuration we build and not the one operators run.
// It cost two bugs, both found by hand rather than by CI:
//
//  1. `llz upgrade` re-prompted every copier question, because copierUpdateArgv
//     omitted --defaults. With no TTY that is not a onboard.Prompt, it is an unhandled
//     OSError out of prompt_toolkit — so the command was unusable in CI, in a
//     wrapper script, over `ssh host 'llz upgrade'`. Check `update-is-
//     noninteractive` is that bug, and it is why this gate closes stdin rather
//     than inheriting it.
//  2. An answer the CURRENT template's validator rejects is silently replaced by
//     the template DEFAULT, exit 0, no warning — copier falls back to the
//     default when it cannot onboard.Prompt, and to the default in the onboard.Prompt when it
//     can. For instance_repo that repoints the ArgoCD repoURL and every `gh`
//     target at a repository that does not exist. Check `answers-preserved`.
//
// WHY THE PROBE ANSWERS MATTER. Both are asserted with deliberately non-default
// values (probeUpgradeAnswers). The default instance_repo is
// `your-org/your-instance-repo`, so an instance scaffolded with defaults cannot
// tell "your answer was preserved" from "your answer was reset to the default" —
// they are the same string. That ambiguity hid bug 2 during its first
// investigation; the gate must not be able to make the same mistake.
//
// Local and cloud-free: it drives copier against THIS repo at two git refs, so
// it runs offline, on a PR branch, and in a fork. It stands up no cluster and
// touches no registry.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/onboard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/selfupgrade"
)

// probeUpgradeAnswers are the answers the scaffold is built with. Every value is
// deliberately NOT the template's default, so `answers-preserved` is testing
// preservation rather than coincidence — see the file header.
var probeUpgradeAnswers = map[string]string{
	"instance_repo": "probe-org/probe-instance",
	"openbao_team":  "probe-team",
}

// upgradeVolatileAnswers are the keys an upgrade is SUPPOSED to rewrite: the
// provenance copier maintains and the version pin the upgrade exists to move.
// Everything else must survive untouched.
var upgradeVolatileAnswers = map[string]bool{
	"_commit": true, "_src_path": true, "llz_version": true,
}

func ciUpgradeTestCmd() *cobra.Command {
	var from, to, template, dir string
	var keep bool
	c := &cobra.Command{
		Use:   "upgrade-test",
		Short: "scaffold at the previous release and `copier update` to HEAD — the day-2 gate instance-test does not cover",
		Long: "Stands up the path an ADOPTER takes on day 2, which nothing else runs:\n" +
			"`copier copy` at the previous release, then `copier update` to the commit\n" +
			"under test. Asserts the update is non-interactive, that it preserves every\n" +
			"answer it is not supposed to move, that the pin advanced, and that it left no\n" +
			"conflict markers or .rej files behind.\n\n" +
			"Drives copier against this repo at two git refs, so it is offline, cloud-free,\n" +
			"and works on a branch or a fork. instance-test.sh is the `copier copy` half;\n" +
			"this is the `copier update` half.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runUpgradeTest(upgradeTestOpts{from: from, to: to, template: template, dir: dir, keep: keep})
		},
	}
	f := c.Flags()
	f.StringVar(&from, "from", "", "release tag to scaffold at (default: the highest vX.Y.Z tag that is not the commit under test)")
	f.StringVar(&to, "to", "", "ref to upgrade to (default: HEAD)")
	f.StringVar(&template, "template", "", "template repo path (default: this checkout's root)")
	f.StringVar(&dir, "dir", ".Upgrade-test", "build directory (gitignored)")
	f.BoolVar(&keep, "keep", false, "leave the built instance in place for inspection")
	return c
}

type upgradeTestOpts struct {
	from, to, template, dir string
	keep                    bool
}

// previousReleaseTag picks the release an adopter would most plausibly be
// upgrading FROM: the highest bare vX.Y.Z tag that is not on the commit under
// test. It delegates the "highest release" rule to selfupgrade.LatestLLZTag — the SAME rule
// `llz self-update` and `llz new` apply — so the gate scaffolds onto exactly the
// release an adopter would have installed, rather than a second opinion about
// what "latest" means that could drift from the one that ships.
//
// Excluding the tag on HEAD is the whole point. Cutting a release puts a tag on
// the commit this gate is checking, and "upgrade v0.0.40 → v0.0.40" is a no-op
// that passes while testing nothing — the failure mode where a color.Green gate means
// least, on the one run that matters most.
func previousReleaseTag(tags []string, headTags map[string]bool) (string, bool) {
	var candidates []string
	for _, t := range tags {
		if t = strings.TrimSpace(t); releaseTagRe.MatchString(t) && !headTags[t] {
			candidates = append(candidates, t)
		}
	}
	return selfupgrade.LatestLLZTag(candidates)
}

// releaseTagRe keeps ONLY a full release tag. selfupgrade.LatestLLZTag cannot do this on its
// own: selfupgrade.Semver() deliberately tolerates a `-pre`/`+build` suffix, and its normal
// callers hand it a list the GitHub releases API already filtered by isDraft /
// isPrerelease. This gate reads `git tag`, where that metadata does not exist —
// and the release convention here is to cut a PRE-RELEASE first, so `v0.0.41-rc1`
// is routinely the highest tag in the repo. Scaffolding onto one would test a
// release no adopter can install: `llz self-update` and `llz new` both skip
// pre-releases, so nobody is ever upgrading FROM one.
var releaseTagRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// answerRegressions lists every answer the upgrade changed that it had no
// business changing. Pure so the comparison — not the copier run — is what the
// unit tests exercise.
//
// A DISAPPEARED key counts: copier dropping an answer is the same loss as
// rewriting it, and the value it renders next is the template default either way.
func answerRegressions(before, after map[string]string) []string {
	var out []string
	for k, was := range before {
		if upgradeVolatileAnswers[k] {
			continue
		}
		now, ok := after[k]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s: %q → (dropped)", k, was))
		case now != was:
			out = append(out, fmt.Sprintf("%s: %q → %q", k, was, now))
		}
	}
	sort.Strings(out)
	return out
}

// copierScaffoldArgv builds the SCAFFOLD invocation. It cannot reuse
// copierCopyArgv: that one addresses the template as `gh:<org>/<name>`, and this
// gate must point copier at a local path so it works offline, on a branch, and
// in a fork. --defaults here is a harness choice — `llz new` legitimately
// prompts for its three answers, and they are supplied below as --data.
//
// The UPGRADE invocation is deliberately NOT built here. It is
// copierUpdateArgv — the exact argv `llz upgrade` runs — because a gate that
// composed its own would be testing copier rather than testing us, and would
// have passed cleanly while `llz upgrade` was unusable in every unattended
// context. That is the blind spot this whole file exists to remove; re-creating
// it one level down would be the same mistake in a smaller box.
func copierScaffoldArgv(template, ref, dest string, answers map[string]string) []string {
	a := []string{"copier", "copy", "--trust", "--defaults", "--vcs-ref", ref, "--data", "llz_version=" + ref}
	for _, k := range onboard.SortedKeys(answers) {
		a = append(a, "--data", k+"="+answers[k])
	}
	return append(a, template, dest)
}

// runCopier runs one copier invocation in dir with stdin CLOSED.
//
// Closing stdin is the assertion, not a convenience. `llz upgrade` inherits the
// operator's terminal, so a re-prompting `copier update` looks fine by hand and
// dies in every unattended context — which is exactly how the bug survived. A
// gate that inherited a TTY would reproduce the blind spot it exists to remove.
var runCopier = func(dir string, argv []string) ([]byte, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin = nil // == /dev/null
	return cmd.CombinedOutput()
}

// currentAnswerMap is the working directory instance's answers, or nil when
// there is no readable answers file. nil is the pre-copier / not-an-instance
// case, and answerRegressions over a nil `before` reports nothing — an upgrade
// cannot be said to have lost an answer that was never recorded.
func currentAnswerMap() map[string]string {
	m, err := readAnswerMap(".copier-answers.yml")
	if err != nil {
		return nil
	}
	return m
}

// readAnswerMap loads a .copier-answers.yml as a flat string map. Non-scalar
// values are rendered with %v; the answers this template asks are all scalars,
// and a structural change there should show up as a diff rather than a panic.
func readAnswerMap(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out, nil
}

// mergeConflictArtifacts walks the built instance for the two ways a botched
// 3-way merge shows up: markers left inside a file, and copier's .rej/.orig
// siblings. `llz upgrade` gates on the markers already; the .rej files it does
// not see, and they are how a merge reports it gave up on a hunk entirely.
func mergeConflictArtifacts(root string) (markers, rejects []string, err error) {
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if ext := filepath.Ext(p); ext == ".rej" || ext == ".orig" {
			rejects = append(rejects, rel)
			return nil
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil // unreadable (symlink, mode) is not a merge artifact
		}
		if lines := conflictMarkerLines(string(b)); len(lines) > 0 {
			markers = append(markers, fmt.Sprintf("%s:%d", rel, lines[0]))
		}
		return nil
	})
	sort.Strings(markers)
	sort.Strings(rejects)
	return markers, rejects, err
}

func runUpgradeTest(o upgradeTestOpts) error {
	root := o.template
	if root == "" {
		out, err := gitOutput(".", "rev-parse", "--show-toplevel")
		if err != nil {
			return fmt.Errorf("not in a git checkout of the template (pass --template): %w", err)
		}
		root = out
	}
	// ABSOLUTE, always. copier records the template as `_src_path` and re-resolves
	// it from the INSTANCE directory on update, so a relative --template silently
	// points somewhere else by then: `make upgrade-test` passes `--template ..`
	// (LLZ_CI runs from tools/), which the update step resolved against the built
	// instance and rejected with "Updating is only supported in git-tracked
	// templates" — a message about the template that was really about the path.
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve template path %q: %w", root, err)
	}
	root = abs
	if _, statErr := os.Stat(filepath.Join(root, "copier.yml")); statErr != nil {
		return fmt.Errorf("%s is not a copier template (no copier.yml) — pass --template <template checkout>", root)
	}
	if _, err := execLookPath("copier"); err != nil {
		fmt.Println("upgrade-test: SKIPPED — copier not installed (pipx install copier)")
		return nil
	}

	to := o.to
	if to == "" {
		sha, err := gitOutput(root, "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("resolve HEAD: %w", err)
		}
		to = sha
	}
	from := o.from
	if from == "" {
		tagsOut, err := gitOutput(root, "tag", "--list")
		if err != nil {
			return fmt.Errorf("list tags: %w", err)
		}
		headOut, _ := gitOutput(root, "tag", "--points-at", to)
		headTags := map[string]bool{}
		for _, t := range strings.Fields(headOut) {
			headTags[t] = true
		}
		var ok bool
		if from, ok = previousReleaseTag(strings.Split(tagsOut, "\n"), headTags); !ok {
			// A shallow clone has no tags. Skipping is right — this gate cannot
			// invent a prior release, and failing would make every shallow checkout
			// color.Red for a reason that is not about the change under test.
			fmt.Println("upgrade-test: SKIPPED — no vX.Y.Z tag to upgrade from (shallow clone? fetch tags, or pass --from)")
			return nil
		}
	}

	build := o.dir
	if !filepath.IsAbs(build) {
		build = filepath.Join(root, build)
	}
	if err := os.RemoveAll(build); err != nil {
		return fmt.Errorf("clean %s: %w", build, err)
	}
	if !o.keep {
		defer func() { _ = os.RemoveAll(build) }()
	}
	if err := os.MkdirAll(build, 0o755); err != nil {
		return err
	}
	inst := filepath.Join(build, "instance")

	fmt.Printf("upgrade-test: %s → %s\n", from, shortRef(to))

	// 1. Scaffold at the previous release.
	if out, err := runCopier(build, copierScaffoldArgv(root, from, inst, probeUpgradeAnswers)); err != nil {
		return fmt.Errorf("scaffold at %s failed:\n%s", from, indentedTail(string(out), 20))
	}
	answersPath := filepath.Join(inst, ".copier-answers.yml")
	before, err := readAnswerMap(answersPath)
	if err != nil {
		return fmt.Errorf("read scaffolded answers: %w", err)
	}
	fmt.Printf("  ✓ scaffolded at %s\n", from)

	// copier update diffs against a committed tree, so the scaffold has to be one.
	// --no-verify: the scaffold arms a pre-commit hook that runs the full `llz
	// lint`, which is instance-test's job and would triple this gate's runtime.
	for _, argv := range [][]string{
		{"git", "init", "-q"},
		{"git", "add", "-A"},
		{"git", "-c", "user.email=upgrade-test@llz", "-c", "user.name=upgrade-test",
			"commit", "-q", "--no-verify", "-m", "scaffold at " + from},
	} {
		if out, err := runCopier(inst, argv); err != nil {
			return fmt.Errorf("%s: %w\n%s", argv[1], err, indentedTail(string(out), 10))
		}
	}

	// 2. The upgrade, with stdin closed. THE check — see runCopier.
	out, upErr := runCopier(inst, copierUpdateArgv(to))
	var failures []string
	if upErr != nil {
		detail := indentedTail(string(out), 25)
		hint := ""
		if strings.Contains(string(out), "prompt_toolkit") || strings.Contains(string(out), "Traceback") {
			hint = "\n    This is copier PROMPTING. `copier update` re-asks every question unless it is\n" +
				"    passed --defaults, and with no terminal that is an unhandled exception rather\n" +
				"    than a onboard.Prompt — so the command works by hand and dies in CI, in a script, and\n" +
				"    over ssh. Fix: add --defaults to the update argv (copierUpdateArgv)."
		}
		failures = append(failures, fmt.Sprintf("update-is-noninteractive: `copier update` to %s failed:\n%s%s",
			shortRef(to), detail, hint))
	} else {
		fmt.Printf("  ✓ update-is-noninteractive — `copier update` ran with stdin closed\n")
	}

	// Everything below inspects the RESULT, so it only means anything if the
	// update produced one. Reporting "answers were not preserved" about a tree the
	// update never wrote would blame the wrong bug.
	if upErr != nil {
		return upgradeTestFailure(failures)
	}

	after, err := readAnswerMap(answersPath)
	if err != nil {
		return fmt.Errorf("read upgraded answers: %w", err)
	}
	if regressions := answerRegressions(before, after); len(regressions) > 0 {
		failures = append(failures, "answers-preserved: the upgrade rewrote answers it does not own:\n      "+
			strings.Join(regressions, "\n      ")+
			"\n    copier falls back to the template DEFAULT for an answer it cannot keep — including\n"+
			"    one the CURRENT template's validator rejects. instance_repo is the ArgoCD repoURL\n"+
			"    and every `gh` target, so this silently repoints the instance at a repo that does\n"+
			"    not exist, exit 0.")
	} else {
		fmt.Printf("  ✓ answers-preserved — %d answer(s) survived unchanged\n", len(before)-len(upgradeVolatileAnswers))
	}

	if got := after["llz_version"]; got != to {
		failures = append(failures, fmt.Sprintf("pin-advanced: llz_version is %q, want %q — the upgrade did not re-pin, so "+
			"every rendered `?ref=` still resolves to the old release", got, to))
	} else {
		fmt.Printf("  ✓ pin-advanced — llz_version is now %s\n", shortRef(to))
	}

	markers, rejects, err := mergeConflictArtifacts(inst)
	if err != nil {
		return fmt.Errorf("scan the upgraded instance: %w", err)
	}
	switch {
	case len(markers) > 0 || len(rejects) > 0:
		var b strings.Builder
		b.WriteString("clean-merge: the 3-way merge left artifacts behind:")
		for _, m := range markers {
			b.WriteString("\n      conflict marker  " + m)
		}
		for _, r := range rejects {
			b.WriteString("\n      rejected hunk    " + r)
		}
		b.WriteString("\n    A file in this state ships invalid YAML/HCL to an instance. If the template legitimately\n" +
			"    rewrote a line the scaffold also owns, the file's class in .template-manifest is wrong.")
		failures = append(failures, b.String())
	default:
		fmt.Println("  ✓ clean-merge — no conflict markers, no .rej/.orig files")
	}

	if len(failures) == 0 {
		fmt.Printf("upgrade-test: OK — an instance at %s upgrades to %s cleanly and unattended.\n", from, shortRef(to))
		return nil
	}
	return upgradeTestFailure(failures)
}

func upgradeTestFailure(failures []string) error {
	for _, f := range failures {
		fmt.Fprintf(os.Stderr, "  %s %s\n", color.Red("✗"), f)
	}
	return fmt.Errorf("upgrade-test: %d check(s) failed — the day-2 path an adopter takes is broken", len(failures))
}

func shortRef(r string) string {
	if len(r) == 40 && hexSHARe.MatchString(r) {
		return r[:12]
	}
	return r
}

// indentedTail is the END of a copier failure, indented into this gate's report.
// The tail, because a traceback's exception line and copier's own message are
// last while the head is a wall of file-creation noise.
func indentedTail(s string, n int) string {
	t := cigate.TailLines(s, n)
	if t == "" {
		return "      (no output)"
	}
	return "      " + strings.ReplaceAll(t, "\n", "\n      ")
}
