package upgrade

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
//  1. `llz upgrade` re-prompted every copier question, because copier.UpdateArgv
//     omitted --defaults. With no TTY that is not a prompt, it is an unhandled
//     OSError out of prompt_toolkit — so the command was unusable in CI, in a
//     wrapper script, over `ssh host 'llz upgrade'`. Check `update-is-
//     noninteractive` is that bug, and it is why this gate closes stdin rather
//     than inheriting it.
//  2. An answer the CURRENT template's validator rejects is silently replaced by
//     the template DEFAULT, exit 0, no warning — copier falls back to the
//     default when it cannot prompt, and to the default in the prompt when it
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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/gitcmd"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/llzver"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/manifest"
)

// probeUpgradeAnswers are the answers the scaffold is built with. Every value is
// deliberately NOT the template's default, so `answers-preserved` is testing
// preservation rather than coincidence — see the file header.
var probeUpgradeAnswers = map[string]string{
	"instance_repo": "probe-org/probe-instance",
	"openbao_team":  "probe-team",
}

func UpgradeTestCmd() *cobra.Command {
	var from, to, template, dir, llzBin string
	var keep bool
	var depth int
	c := &cobra.Command{
		Use:   "upgrade-test",
		Short: "scaffold at each of the last N releases and `llz upgrade` to HEAD — the day-2 gate instance-test does not cover",
		Long: "Stands up the path an ADOPTER takes on day 2, which nothing else runs:\n" +
			"`copier copy` at an older release, then `llz upgrade` to the commit under\n" +
			"test. Asserts the upgrade is non-interactive, that it preserves every answer\n" +
			"it is not supposed to move, that the pin advanced, that it left no conflict\n" +
			"markers or .rej files behind, and that the result MATCHES a fresh scaffold at\n" +
			"the same ref.\n\n" +
			"Runs once per release in --depth, so the instance that is three releases behind\n" +
			"is covered and not just the one that upgraded last week. Drives copier and the\n" +
			"llz under test against this repo at two git refs, so it is offline, cloud-free,\n" +
			"and works on a branch or a fork. instance-test.sh is the `copier copy` half;\n" +
			"this is the upgrade half.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunUpgradeTest(upgradeTestOpts{
				from: from, to: to, template: template, dir: dir, llzBin: llzBin, keep: keep, depth: depth,
			})
		},
	}
	f := c.Flags()
	f.StringVar(&from, "from", "", "release tag to scaffold at (default: the last --depth releases; setting this pins the run to one)")
	f.StringVar(&to, "to", "", "ref to upgrade to (default: HEAD)")
	f.IntVar(&depth, "depth", DefaultUpgradeDepth, "how many of the most recent releases to upgrade FROM, newest first")
	f.StringVar(&template, "template", "", "template repo path (default: this checkout's root)")
	f.StringVar(&dir, "dir", ".Upgrade-test", "build directory (gitignored)")
	f.StringVar(&llzBin, "llz", "", "llz binary to upgrade WITH (default: the one running this gate)")
	f.BoolVar(&keep, "keep", false, "leave the built instances in place for inspection")
	return c
}

// DefaultUpgradeDepth is how many releases back the gate covers by default.
//
// THREE, NOT ONE. One hop only ever proved that the release cut last week can be
// upgraded from, and that is the instance least likely to be broken — an adopter
// who upgrades on every release is exercising a diff the maintainers just looked
// at. The instance that hurts is the one that skipped two or three releases, where
// the file whose manifest class changed in vN-2 meets the file whose content moved
// in vN-1, and copier resolves both against a base neither maintainer had in mind.
// That instance had no gate at all.
//
// Three is where the cost curve turns, not a round number: each hop is one
// scaffold plus one upgrade plus one fresh render (~45s measured), so three fits
// the instantiate job's remaining budget with room, while every release further
// back adds the same cost for a population that shrinks fast.
const DefaultUpgradeDepth = 3

type upgradeTestOpts struct {
	from, to, template, dir, llzBin string
	keep                            bool
	depth                           int
}

// PreviousReleaseTag picks the release an adopter would most plausibly be
// upgrading FROM: the highest bare vX.Y.Z tag that is not on the commit under
// test. It delegates the "highest release" rule to llzver.LatestLLZTag — the SAME rule
// `llz self-update` and `llz new` apply — so the gate scaffolds onto exactly the
// release an adopter would have installed, rather than a second opinion about
// what "latest" means that could drift from the one that ships.
//
// Excluding the tag on HEAD is the whole point. Cutting a release puts a tag on
// the commit this gate is checking, and "upgrade v0.0.40 → v0.0.40" is a no-op
// that passes while testing nothing — the failure mode where a color.Green gate means
// least, on the one run that matters most.
func PreviousReleaseTag(tags []string, headTags map[string]bool) (string, bool) {
	var candidates []string
	for _, t := range tags {
		if t = strings.TrimSpace(t); releaseTagRe.MatchString(t) && !headTags[t] {
			candidates = append(candidates, t)
		}
	}
	return llzver.LatestLLZTag(candidates)
}

// PreviousReleaseTags is PreviousReleaseTag n times over: the n most recent
// releases an adopter could be sitting on, newest first.
//
// It is built by REPEATED APPLICATION of PreviousReleaseTag rather than by
// sorting the tag list itself, so "which release is next" is answered in exactly
// one place. A second ordering here could disagree with the first — and the way
// it would disagree is by admitting a pre-release or a legacy `llz/v*` tag that
// `llz self-update` skips, i.e. by testing an upgrade FROM a release no adopter
// can be running.
//
// Returns fewer than n when the repo has fewer releases; the caller decides
// whether a short list is acceptable (see assertDepthCovered — silently covering
// one release while claiming three is the failure mode this splits out).
func PreviousReleaseTags(tags []string, headTags map[string]bool, n int) []string {
	excluded := map[string]bool{}
	for t, v := range headTags {
		excluded[t] = v
	}
	var out []string
	for len(out) < n {
		t, ok := PreviousReleaseTag(tags, excluded)
		if !ok {
			break
		}
		out = append(out, t)
		excluded[t] = true
	}
	return out
}

// releaseTagRe keeps ONLY a full release tag. llzver.LatestLLZTag cannot do this on its
// own: llzver.Semver() deliberately tolerates a `-pre`/`+build` suffix, and its normal
// callers hand it a list the GitHub releases API already filtered by isDraft /
// isPrerelease. This gate reads `git tag`, where that metadata does not exist —
// and the release convention here is to cut a PRE-RELEASE first, so `v0.0.41-rc1`
// is routinely the highest tag in the repo. Scaffolding onto one would test a
// release no adopter can install: `llz self-update` and `llz new` both skip
// pre-releases, so nobody is ever upgrading FROM one.
var releaseTagRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// UpgradeUnderTestArgv is what the gate runs to perform the upgrade: the real
// `llz upgrade`, in the binary under test.
//
// NOT copier.UpdateArgv, which is what this gate used to run. `copier update` is
// step one of three — the manifest-class policy pass and the .template-removals
// pass follow it, both implemented in Go in this repo, and stopping at copier left
// both of them ungated while the gate's name said the upgrade path was covered.
// The AGENTS.md link regression lived in step two: copier produced the right file
// and the policy pass put an older one back over it.
//
// --no-render and --no-doctor are what keep the gate offline; neither is part of
// what an upgrade DELIVERS. Nothing else may be added here lightly — every flag is
// a way in which the thing under test stops being the command an adopter runs.
func UpgradeUnderTestArgv(llzBin, ref string) []string {
	return []string{llzBin, "upgrade", "--ref", ref, "--no-render", "--no-doctor"}
}

// CopierScaffoldArgv builds the SCAFFOLD invocation. It cannot reuse
// copier.CopyArgv: that one addresses the template as `gh:<org>/<name>`, and this
// gate must point copier at a local path so it works offline, on a branch, and
// in a fork. --defaults here is a harness choice — `llz new` legitimately
// prompts for its three answers, and they are supplied below as --data.
//
// The UPGRADE invocation is deliberately NOT built here. It is
// copier.UpdateArgv — the exact argv `llz upgrade` runs — because a gate that
// composed its own would be testing copier rather than testing us, and would
// have passed cleanly while `llz upgrade` was unusable in every unattended
// context. That is the blind spot this whole file exists to remove; re-creating
// it one level down would be the same mistake in a smaller box.
func CopierScaffoldArgv(template, ref, dest string, answers map[string]string) []string {
	a := []string{"copier", "copy", "--trust", "--defaults", "--vcs-ref", ref, "--data", "llz_version=" + ref}
	for _, k := range cli.SortedKeys(answers) {
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

// osExecutable is os.Executable behind a seam, so a test can pin the binary the
// gate would upgrade with without being run as that binary.
var osExecutable = os.Executable

// putOnPATH prepends dir to this process's PATH so every child inherits it.
//
// The children are copier, and copier's `_tasks`, which invoke `llz` BY NAME and
// degrade to a warning when `command -v llz` comes up empty. Under `make
// upgrade-test` the binary is ./bin/llz — a path, not something on PATH — so
// without this the tasks took their fallback on both sides of the comparison and
// the convergence check compared two instances that had each skipped the same
// delivery. Mutating this process's own environment is the right scope: it is a
// short-lived CI gate whose entire purpose is running those children, and setting
// it per-exec would mean threading an env through the seam every caller shares.
func putOnPATH(dir string) error {
	if dir == "" {
		return nil
	}
	path := os.Getenv("PATH")
	for _, existing := range filepath.SplitList(path) {
		if existing == dir {
			return nil
		}
	}
	if path == "" {
		return os.Setenv("PATH", dir)
	}
	return os.Setenv("PATH", dir+string(os.PathListSeparator)+path)
}

// MergeConflictArtifacts walks the built instance for the two ways a botched
// 3-way merge shows up: markers left inside a file, and copier's .rej/.orig
// siblings. `llz upgrade` gates on the markers already; the .rej files it does
// not see, and they are how a merge reports it gave up on a hunk entirely.
func MergeConflictArtifacts(root string) (markers, rejects []string, err error) {
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

func RunUpgradeTest(o upgradeTestOpts) error {
	root := o.template
	if root == "" {
		out, err := gitcmd.Output(".", "rev-parse", "--show-toplevel")
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
		sha, err := gitcmd.Output(root, "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("resolve HEAD: %w", err)
		}
		to = sha
	}

	// The binary that performs the upgrade is THE ONE RUNNING THIS GATE unless a
	// caller says otherwise. Anything else answers for code that is not under test:
	// `llz upgrade` is three steps, two of them (the manifest-class policy pass and
	// the declared removals) implemented right here in Go, so a gate that reached
	// for whatever `llz` happened to be installed would be checking the last
	// release's upgrade logic against this commit's template.
	llzBin := o.llzBin
	if llzBin == "" {
		self, err := osExecutable()
		if err != nil {
			return fmt.Errorf("resolve the llz binary to upgrade with (pass --llz): %w", err)
		}
		llzBin = self
	}
	if abs, err := filepath.Abs(llzBin); err == nil {
		llzBin = abs // the upgrade runs with cwd inside the built instance
	}
	if _, err := os.Stat(llzBin); err != nil {
		return fmt.Errorf("the llz binary to upgrade with is not there: %w", err)
	}
	// copier's _tasks call `llz` by name — see putOnPATH. Done before any render,
	// because the reference scaffold is rendered by those same tasks.
	if err := putOnPATH(filepath.Dir(llzBin)); err != nil {
		return fmt.Errorf("put %s on PATH for copier's tasks: %w", filepath.Dir(llzBin), err)
	}

	depth := o.depth
	if depth < 1 {
		depth = 1
	}
	var froms []string
	switch {
	case o.from != "":
		// An explicit --from pins the run to that one release: the caller is
		// reproducing a specific upgrade, and quietly adding two more hops around it
		// would bury the one they asked about.
		froms = []string{o.from}
	default:
		tagsOut, err := gitcmd.Output(root, "tag", "--list")
		if err != nil {
			return fmt.Errorf("list tags: %w", err)
		}
		headOut, _ := gitcmd.Output(root, "tag", "--points-at", to)
		headTags := map[string]bool{}
		for _, t := range strings.Fields(headOut) {
			headTags[t] = true
		}
		if froms = PreviousReleaseTags(strings.Split(tagsOut, "\n"), headTags, depth); len(froms) == 0 {
			// A shallow clone has no tags. Skipping is right — this gate cannot
			// invent a prior release, and failing would make every shallow checkout
			// color.Red for a reason that is not about the change under test.
			fmt.Println("upgrade-test: SKIPPED — no vX.Y.Z tag to upgrade from (shallow clone? fetch tags, or pass --from)")
			return nil
		}
		// SAY SO when the repo cannot supply the depth asked for. A young repo (or a
		// clone fetched with --depth) legitimately has fewer releases, so this is not
		// a failure — but it IS the gate covering less than its name claims, and the
		// one thing it must not do is print a green summary that reads as if three
		// releases were exercised when one was.
		if len(froms) < depth {
			fmt.Printf("upgrade-test: NOTE — asked for the last %d releases, this checkout has %d (%s). "+
				"Coverage is that much narrower.\n", depth, len(froms), strings.Join(froms, ", "))
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

	// ONE fresh scaffold for the whole run, not one per hop. It is the comparison
	// target for every release — the same instance a brand-new adopter gets at
	// `to` — and it does not depend on where the upgrade started, so rendering it
	// per hop would triple the copier work to produce identical trees.
	fresh := filepath.Join(build, "fresh")
	freshOut, err := runCopier(build, CopierScaffoldArgv(root, to, fresh, probeUpgradeAnswers))
	if err != nil {
		return fmt.Errorf("scaffold the comparison instance at %s failed:\n%s", ShortRef(to), IndentedTail(string(freshOut), 20))
	}
	if err := assertTasksRan(root, fresh); err != nil {
		return err
	}
	freshFiles, err := DigestTree(fresh)
	if err != nil {
		return fmt.Errorf("digest the comparison instance: %w", err)
	}
	// FAIL CLOSED ON VACUITY. Every convergence gap is measured against this tree,
	// so an empty one turns the strongest check in the gate into a loop over
	// nothing that reports success — the exact shape of a green check that examined
	// no evidence.
	if len(freshFiles) == 0 {
		return fmt.Errorf("the comparison scaffold at %s is EMPTY — convergence would be asserted against nothing", ShortRef(to))
	}
	policy, err := manifest.Load(fresh)
	if err != nil {
		return fmt.Errorf("load .template-manifest from the comparison instance: %w", err)
	}

	fmt.Printf("upgrade-test: %s → %s (%d file(s) in the reference scaffold)\n",
		strings.Join(froms, ", "), ShortRef(to), len(freshFiles))

	// A HARNESS FAILURE CANCELS ITS HOP, NOT THE RUN — which is what runUpgradeHop's
	// doc comment already promised and the loop did not do. Returning here threw
	// away every check failure the earlier hops had already found: a convergence gap
	// at v0.0.42 vanished because the v0.0.40 scaffold later failed to render, so the
	// run reported the harness problem and stayed silent about the real defect it
	// had already measured. The two are also different verdicts — "the upgrade is
	// broken" versus "could not tell" — and both have to reach the summary.
	var failures, harnessErrs []string
	for _, from := range froms {
		fmt.Printf("\n── from %s ──\n", from)
		hopFailures, err := runUpgradeHop(hopOpts{
			root: root, build: build, from: from, to: to, llzBin: llzBin,
			freshFiles: freshFiles, policy: policy,
		})
		failures = append(failures, hopFailures...)
		if err != nil {
			// One line here, the full text in the summary: these carry a 20-line
			// copier tail, and printing it twice buries the check failures between
			// two copies of the same wall of output.
			fmt.Fprintf(os.Stderr, "  %s harness: could not measure this hop — see the summary\n", color.Yellow("!"))
			harnessErrs = append(harnessErrs, fmt.Sprintf("from %s: %v", from, err))
		}
	}

	if len(failures) == 0 && len(harnessErrs) == 0 {
		fmt.Printf("\nupgrade-test: OK — an instance at %s upgrades to %s cleanly, unattended, "+
			"and ends up identical to a fresh scaffold.\n", strings.Join(froms, "/"), ShortRef(to))
		return nil
	}
	// A hop that could not be measured must not be summarised as OK either: the
	// releases it covered went unchecked, and saying nothing about that is how a
	// gate reports coverage it does not have.
	return UpgradeTestFailure(failures, harnessErrs)
}

type hopOpts struct {
	root, build, from, to, llzBin string
	freshFiles                    map[string]string
	policy                        manifest.Manifest
}

// runUpgradeHop is one release's worth of the gate: scaffold at `from`, upgrade
// to `to` with the llz under test, and check the result five ways.
//
// It returns CHECK failures in the slice and only errors out on harness failures
// (a scaffold that would not build, an unreadable tree). The distinction matters
// at depth: one release failing to scaffold must not cancel the other two, and
// "could not tell" must not be reported as "the upgrade is broken".
func runUpgradeHop(o hopOpts) ([]string, error) {
	inst := filepath.Join(o.build, "instance-"+o.from)

	// 1. Scaffold at the older release.
	if out, err := runCopier(o.build, CopierScaffoldArgv(o.root, o.from, inst, probeUpgradeAnswers)); err != nil {
		return nil, fmt.Errorf("scaffold at %s failed:\n%s", o.from, IndentedTail(string(out), 20))
	}
	answersPath := filepath.Join(inst, ".copier-answers.yml")
	before, err := ReadAnswerMap(answersPath)
	if err != nil {
		return nil, fmt.Errorf("read scaffolded answers: %w", err)
	}
	fmt.Printf("  ✓ scaffolded at %s\n", o.from)

	// copier update diffs against a committed tree, so the scaffold has to be one.
	// --no-verify: the scaffold arms a pre-commit hook that runs the full `llz
	// lint`, which is instance-test's job and would triple this gate's runtime.
	for _, argv := range [][]string{
		{"git", "init", "-q"},
		{"git", "add", "-A"},
		{"git", "-c", "user.email=upgrade-test@llz", "-c", "user.name=upgrade-test",
			"commit", "-q", "--no-verify", "-m", "scaffold at " + o.from},
	} {
		if out, err := runCopier(inst, argv); err != nil {
			return nil, fmt.Errorf("%s: %w\n%s", argv[1], err, IndentedTail(string(out), 10))
		}
	}

	// 2. The upgrade, with stdin closed.
	//
	// `llz upgrade`, NOT `copier update`. This gate used to run copier's argv
	// directly, on the reasoning that copier.UpdateArgv is the exact argv `llz
	// upgrade` runs — which is true, and covers the FIRST of the three things the
	// command does. The other two are the manifest-class policy pass (restore
	// `owned`, overwrite `managed` from a clean render of the target) and the
	// declared removals from .template-removals, both of them Go in this repo, both
	// of them unreached by any test that stopped at copier. That is where the
	// AGENTS.md link regression lived: `copier update` produced the right file and
	// the policy pass put the wrong one back.
	//
	// --no-render / --no-doctor keep it offline: render wants a spec and terraform,
	// the readiness check wants gh and the Linode API. Neither is part of what an
	// upgrade DELIVERS, which is what this gate measures.
	out, upErr := runCopier(inst, UpgradeUnderTestArgv(o.llzBin, o.to))
	var failures []string
	if upErr != nil {
		detail := IndentedTail(string(out), 25)
		hint := ""
		if strings.Contains(string(out), "prompt_toolkit") || strings.Contains(string(out), "Traceback") {
			hint = "\n    This is copier PROMPTING. `copier update` re-asks every question unless it is\n" +
				"    passed --defaults, and with no terminal that is an unhandled exception rather\n" +
				"    than a cli.Prompt — so the command works by hand and dies in CI, in a script, and\n" +
				"    over ssh. Fix: add --defaults to the update argv (copier.UpdateArgv)."
		}
		failures = append(failures, fmt.Sprintf("upgrade-is-noninteractive [from %s]: `llz upgrade --ref %s` failed:\n%s%s",
			o.from, ShortRef(o.to), detail, hint))
	} else {
		fmt.Printf("  ✓ upgrade-is-noninteractive — `llz upgrade` ran with stdin closed\n")
	}

	// Everything below inspects the RESULT, so it only means anything if the
	// upgrade produced one. Reporting "answers were not preserved" about a tree the
	// upgrade never wrote would blame the wrong bug.
	if upErr != nil {
		return failures, nil
	}
	// The upgrade's own subprocesses degrade to a warning when they cannot find
	// llz; that path renders a DIFFERENT instance, and comparing two equally
	// degraded trees is how a convergence check reports success having exercised
	// none of the delivery it exists to measure.
	// A DEGRADED RENDER **HERE** IS A FINDING, NOT A HARNESS PROBLEM. The identical
	// call on the fresh scaffold before the loop already covers "this machine cannot
	// render at all"; by the time we reach this line that render has succeeded, so
	// tasks that degraded during the UPGRADE mean the upgrade did not deliver — the
	// exact class this gate exists for, and the one `llz upgrade` publishes itself
	// on PATH to prevent. Bucketing it as a harness error reported a real regression
	// as "could not be measured" and sent the operator to inspect their --llz binary
	// for a defect in the product.
	if err := assertTasksRan(o.root, inst); err != nil {
		failures = append(failures, fmt.Sprintf("tasks-delivered [from %s]: %v\n"+
			"    The upgraded instance did not receive the delivery copier's `_tasks` perform.\n"+
			"    `llz upgrade` puts its own binary on PATH for them (proc.SelfOnPATH); when that\n"+
			"    stops working the tasks fall back to a warning and the instance keeps an unpruned\n"+
			"    docs/ tree plus un-repointed root links.", o.from, err))
		return failures, nil
	}

	after, err := ReadAnswerMap(answersPath)
	if err != nil {
		return failures, fmt.Errorf("read upgraded answers: %w", err)
	}
	if regressions := AnswerRegressions(before, after); len(regressions) > 0 {
		failures = append(failures, "answers-preserved [from "+o.from+"]: the upgrade rewrote answers it does not own:\n      "+
			strings.Join(regressions, "\n      ")+
			"\n    copier falls back to the template DEFAULT for an answer it cannot keep — including\n"+
			"    one the CURRENT template's validator rejects. instance_repo is the ArgoCD repoURL\n"+
			"    and every `gh` target, so this silently repoints the instance at a repo that does\n"+
			"    not exist, exit 0.")
	} else {
		fmt.Printf("  ✓ answers-preserved — %d answer(s) survived unchanged\n", len(before)-len(VolatileAnswers))
	}

	if got := after["llz_version"]; got != o.to {
		failures = append(failures, fmt.Sprintf("pin-advanced [from %s]: llz_version is %q, want %q — the upgrade did not re-pin, so "+
			"every rendered `?ref=` still resolves to the old release", o.from, got, o.to))
	} else {
		fmt.Printf("  ✓ pin-advanced — llz_version is now %s\n", ShortRef(o.to))
	}

	markers, rejects, err := MergeConflictArtifacts(inst)
	if err != nil {
		return failures, fmt.Errorf("scan the upgraded instance: %w", err)
	}
	switch {
	case len(markers) > 0 || len(rejects) > 0:
		var b strings.Builder
		b.WriteString("clean-merge [from " + o.from + "]: the 3-way merge left artifacts behind:")
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

	// 3. THE delivery check: is this now the same instance a new adopter gets?
	upgradedFiles, err := DigestTree(inst)
	if err != nil {
		return failures, fmt.Errorf("digest the upgraded instance: %w", err)
	}
	gaps := ConvergenceGaps(o.freshFiles, upgradedFiles, o.policy.Classify)
	if len(gaps) > 0 {
		failures = append(failures, FormatConvergenceGaps(o.from, gaps))
	} else {
		fmt.Printf("  ✓ converges-with-fresh — identical to a fresh scaffold across %d template-owned file(s)\n",
			countAsserted(o.freshFiles, o.policy.Classify))
	}
	return failures, nil
}

// countAsserted is how many files the convergence check actually compared, for
// the success line. Printing the number is the point: "converges" over 4 files
// and over 400 are the same words and very different claims, and the first is how
// a scaffold that half-failed reports in.
func countAsserted(files map[string]string, classOf func(string) string) int {
	n := 0
	for p := range files {
		if convergenceAsserted(classOf(p)) {
			n++
		}
	}
	return n
}

// assertTasksRan fails when copier's `_tasks` took their no-llz fallback.
//
// WHY IT IS HERE AT ALL. Those tasks are what deliver docs/, prune it to the
// operator set, and repoint the root-Markdown links that target template-only
// paths. When `command -v llz` comes up empty they degrade to a warning and an
// unpruned tree — and they degrade IDENTICALLY on both sides of the comparison, so
// two equally undelivered instances match each other perfectly and
// converges-with-fresh reports success having measured none of the delivery it
// exists to measure. "The upgrade delivered everything" and "neither side
// delivered anything" are the same green check without this.
//
// IT READS THE TREE, NOT THE LOG. The first cut scanned copier's output for the
// fallback message — which copier also prints while ECHOING the task it is about
// to run, so the gate failed on every single render including the successful ones.
// A log line saying a fallback exists is not evidence that it was taken.
//
// The signal is that docs/ came out SMALLER than the template's: pruning is
// exactly what the fallback cannot do. Deriving it from the two trees means no
// copy of deliver-docs' keep-set lives here to drift out of date — a hardcoded
// "adopter-guide.md must be gone" would go stale the day that file is renamed, and
// go stale silently, in the check whose job is noticing silence.
func assertTasksRan(templateRoot, instRoot string) error {
	tmplDocs, err := countFiles(filepath.Join(templateRoot, "docs"))
	if err != nil {
		return fmt.Errorf("count the template's docs/: %w", err)
	}
	instDocs, err := countFiles(filepath.Join(instRoot, "docs"))
	if err != nil {
		return fmt.Errorf("count the rendered instance's docs/: %w", err)
	}
	switch {
	case tmplDocs == 0:
		return fmt.Errorf("the template at %s has no docs/ — the delivery this gate measures does not exist", templateRoot)
	case instDocs == 0:
		return fmt.Errorf("%s received no docs/ at all — copier's docs task did not run", instRoot)
	case instDocs >= tmplDocs:
		return fmt.Errorf("copier's _tasks degraded: %s carries %d docs file(s) against the template's %d, so "+
			"`llz ci deliver-docs` never pruned it.\n"+
			"  This is a harness failure, not a finding. The tasks invoke `llz` BY NAME and fall back to a\n"+
			"  warning when it is not on PATH; the gate prepends the binary under test for exactly this\n"+
			"  reason (putOnPATH). An instance rendered by the fallback path also skips the root-link\n"+
			"  repoint, and comparing two such instances proves nothing about what an upgrade delivers.\n"+
			"  Check that the binary passed as --llz exists and is executable", instRoot, instDocs, tmplDocs)
	}
	return nil
}

func countFiles(root string) (int, error) {
	n := 0
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return n, err
}

// UpgradeTestFailure reports both verdicts a run can produce, because they are
// different claims. A CHECK failure says the upgrade is broken. A HARNESS failure
// says that hop could not be measured — the releases it covered went unchecked,
// which is not the same as passing and must not be summarised as OK.
func UpgradeTestFailure(failures, harnessErrs []string) error {
	for _, f := range failures {
		fmt.Fprintf(os.Stderr, "  %s %s\n", color.Red("✗"), f)
	}
	for _, h := range harnessErrs {
		fmt.Fprintf(os.Stderr, "  %s harness: %s\n", color.Yellow("!"), h)
	}
	switch {
	case len(failures) == 0 && len(harnessErrs) == 0:
		// Not reachable through RunUpgradeTest, which returns early on a clean run —
		// but a helper that turns "nothing went wrong" into an error is a trap for
		// the next caller, and the arm costs one line.
		return nil
	case len(failures) == 0:
		return fmt.Errorf("upgrade-test: %d hop(s) could not be measured — the gate reached no verdict on them", len(harnessErrs))
	case len(harnessErrs) == 0:
		return fmt.Errorf("upgrade-test: %d check(s) failed — the day-2 path an adopter takes is broken", len(failures))
	default:
		return fmt.Errorf("upgrade-test: %d check(s) failed — the day-2 path an adopter takes is broken; "+
			"a further %d hop(s) could not be measured at all", len(failures), len(harnessErrs))
	}
}

func ShortRef(r string) string {
	if len(r) == 40 && hexSHARe.MatchString(r) {
		return r[:12]
	}
	return r
}

// IndentedTail is the END of a copier failure, indented into this gate's report.
// The tail, because a traceback's exception line and copier's own message are
// last while the head is a wall of file-creation noise.
func IndentedTail(s string, n int) string {
	t := cigate.TailLines(s, n)
	if t == "" {
		return "      (no output)"
	}
	return "      " + strings.ReplaceAll(t, "\n", "\n      ")
}
