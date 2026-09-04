// Package upgrade re-renders an instance from a newer template release and tells
// the operator what it could not do for them.
//
// THE ANSWER-MAP HELPERS CAME WITH IT. answerRegressions, currentAnswerMap,
// readAnswerMap and VolatileAnswers were in ci_upgrade_test_gate.go — a
// file about a CI gate — and were used by NOTHING except this command. Four
// symbols, one caller, two files.
//
// WHAT answerRegressions IS FOR is the part worth keeping. `copier update` can
// silently change an answer, and most of those changes are fine
// (VolatileAnswers names the ones that legitimately move). The rest are
// reported, because an answer that drifted during an upgrade is a decision the
// operator made once and the template quietly un-made.
package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/sustain"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/promote"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/upstreamupdates"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/copier"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/gitcmd"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/manifest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/proc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/onboard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/selfupgrade"
	"sigs.k8s.io/yaml"
)

// SustainDeps is injected by package main: assembling sustain.Deps needs
// lockableScaffoldFiles and the global --yes, both of which are main's. Same
// caller-side-assembly rule promote.Deps established.
var SustainDeps func() sustain.Deps

func Run(dryRun bool, ref string, commit, noRender, noDoctor bool) error {
	// Same prerequisite as `llz new`, and worse to discover late: this one runs
	// snapshot/restore around copier, so failing at the exec is failing mid-flight.
	if err := copier.Require(dryRun, "`llz upgrade`"); err != nil {
		return err
	}
	oldRef := currentTemplateRef() // "" if this is not an instance checkout yet
	// Snapshot before copier runs — it rewrites this file in place, so afterwards
	// there is nothing left to compare against (see Lever 0).
	answersBefore := currentAnswerMap()
	// Same reason as the line above, for a different loss. The manifest policy
	// overwrites every `managed` file from a clean render, so a local edit to one
	// is discarded — and AFTER copier has run there is no way to tell an edit the
	// operator made from a hunk copier merged. This is the last moment the
	// question can be asked, which is why it is asked here and reported later.
	managedEdits := managedEditsBefore()

	// Always resolve to a concrete ref so the instance's llz_version pins update in
	// lockstep with the template code (a bare `copier update` would float the code
	// to the latest tag but leave the recorded llz_version stale). selfupgrade.UpdateRepo()
	// names the template this instance tracks (its .copier-answers upstream_org).
	ref, err := copier.Ref(ref, selfupgrade.UpdateRepo())
	if err != nil {
		return err
	}
	// Copier has exactly one update strategy — re-render + 3-way merge — but the
	// manifest declares three (managed / merge / owned). Capture the `owned` files
	// BEFORE copier runs so we can put them back after; `managed` files are then
	// overwritten from a clean render of the target ref. Without this the classes
	// are documentation only and copier merges everything alike.
	//
	// Degrade gracefully: a pre-manifest instance (or one whose manifest failed to
	// parse) still upgrades exactly as it did before, just without class enforcement.
	policy, policyErr := manifest.Load("")
	var owned selfupgrade.UpgradeSnapshot
	if policyErr != nil {
		fmt.Fprintf(os.Stderr, "%s no usable .template-manifest (%v) — upgrading without manifest-class enforcement\n", color.Yellow("!"), policyErr)
	} else {
		if owned, err = selfupgrade.SnapshotUpgradeOwned(policy); err != nil {
			return fmt.Errorf("snapshot owned files: %w", err)
		}
		defer owned.Cleanup()
	}
	// This render runs copier's `_tasks` too — it is the one that delivers docs/
	// and repoints the root Markdown links in the INSTANCE — and they invoke `llz`
	// by name. An operator running an llz that is not on PATH (by absolute path,
	// straight out of a build directory) would otherwise get the tasks' no-llz
	// fallback and an unpruned docs tree, from the command whose whole job is
	// delivering the template correctly. The clean render below arms this
	// independently; SelfOnPATH no-ops the second time (os.SameFile).
	restorePATH, err := proc.SelfOnPATH("llz")
	if err != nil {
		return err
	}
	defer restorePATH()
	// RENDER BEFORE MUTATING. The clean render shells out to copier, which runs the
	// template's `_tasks` — arbitrary shell that can fail for reasons this instance
	// has no control over. Doing it after `copier update` meant such a failure left
	// the instance half-upgraded: merged by copier, missing the `managed` overwrite
	// and the declared removals, with nothing to roll back to. Now the fragile step
	// runs while the instance is still untouched, so a failure here costs the
	// operator a re-run rather than a wedged tree.
	var policyRender *selfupgrade.ManifestPolicy
	if policyErr == nil {
		// Not wrapped: renderUpgradeScaffold already says "render target scaffold for
		// manifest policy", and wrapping produced that phrase twice in one message.
		if policyRender, err = selfupgrade.PrepareManifestPolicy(dryRun, ref); err != nil {
			return err
		}
		defer policyRender.Cleanup()
	}
	if err := proc.RunEcho(dryRun, copier.UpdateArgv(ref)...); err != nil {
		return fmt.Errorf("copier update: %w", err)
	}
	if policyRender != nil {
		if err := policyRender.Apply(owned); err != nil {
			return fmt.Errorf("apply manifest policy: %w", err)
		}
	}
	// copier update never deletes a file the template dropped between versions, so
	// apply the template's declared removals (.template-removals) ourselves — now
	// up to date from the copier update above. Honors --dry-run internally.
	// Runs AFTER the manifest policy: a removed file is absent from the clean render,
	// so the overwrite pass can't resurrect it, but the ordering makes that explicit.
	if err := sustain.ApplyTemplateRemovals(SustainDeps()); err != nil {
		return fmt.Errorf("apply template removals: %w", err)
	}
	// BEFORE the dry-run return, deliberately: a dry run is the one where the
	// operator can still save the edit, so it is the run that most needs to hear
	// about it.
	reportClobberedManaged(managedEdits, dryRun)

	// THE APL-CORE PIN, and it is ABOVE THE DRY-RUN RETURN for the same reason
	// reportClobberedManaged is: a dry run is the one where the operator can still
	// change their mind, so it is the run that most needs to see this. Sited below,
	// the whole lever was unreachable on `--dry-run` — the removal was never
	// previewed, and the test asserting a dry run writes nothing was covering a path
	// production never took.
	//
	// `spec.cluster.bootstrap.aplChartVersion` is optional and an omitted pin
	// resolves to clusterspec.BaselineAplChartVersion, which is a value THIS release
	// moved — so an environment that wrote the old baseline into the field now names
	// a version its own llz does not target, deterministically, on every bump.
	//
	// The write half is sited further down, below the two gates that can abort and
	// ahead of printSummary — see the comment there. Both orderings are pinned by
	// TestUpgradeRunRetracksPinsBeforeRendering.
	if dryRun {
		// PRINTED, NOT DISCARDED. The return value was dropped on the floor here, so a
		// dry run showed the per-file lines on stderr and then returned without the
		// numbered checklist — including the "will BLOCK validation" entry that
		// reportAplPinsLeftAlone argues must not be left to scrolling stderr. A dry
		// run is the one where the operator is still deciding, so it is the run that
		// most needs the summary.
		printNextSteps(retrackAplPins(true))
		return nil
	}
	// No provenance re-stamp: copier's `update` already rewrote .copier-answers.yml
	// (_commit + llz_version), which IS the record — see stamp.go. That also retires
	// the stamp/rollback dance this used to need around the conflict gate below.
	newRef := currentTemplateRef()

	// ── Lever 0: the answers must be the ones we came in with ────────────────
	// copier does not error on an answer it cannot keep — it substitutes the
	// template DEFAULT and exits 0. The reachable case is an answer the CURRENT
	// template's validator rejects (instance_repo grew one in #405), which is
	// exactly the malformed value an instance scaffolded before that validator can
	// be holding. instance_repo is the ArgoCD repoURL and every `gh` target, so the
	// silent outcome is an instance re-rendered against `your-org/your-instance-repo`
	// — a repository that does not exist — with a clean exit and a normal-looking
	// diffstat.
	//
	// Checked rather than trusted: --defaults on the update argv preserves answers
	// in the normal case, but it does not close this one, and a flag that is right
	// 99% of the time is not a substitute for verifying the 1%.
	if regressions := AnswerRegressions(answersBefore, currentAnswerMap()); len(regressions) > 0 {
		fmt.Fprintf(os.Stderr, "\n%s copier update rewrote %d answer(s) it does not own:\n", color.Red("✗"), len(regressions))
		for _, r := range regressions {
			fmt.Fprintf(os.Stderr, "    %s\n", r)
		}
		return fmt.Errorf("the tree is at %s but its identity changed — restore the answer(s) above in "+
			".copier-answers.yml and re-run (a value the template's validator now rejects is replaced by "+
			"the DEFAULT, silently; fix the value, don't accept the default)", newRef)
	}

	// ── Lever 1: make the upgrade reviewable + safe ──────────────────────────
	// A botched copier 3-way merge can leave <<<<<<< markers that otherwise ship
	// invalid YAML far downstream (the gsap-apl incident). Fail loudly here BEFORE
	// the operator commits, instead of relying on `llz lint` to catch it later.
	if bad := conflictFiles(); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "\n%s copier update left merge-conflict markers in %d file(s):\n", color.Red("✗"), len(bad))
		for _, f := range bad {
			fmt.Fprintf(os.Stderr, "    %s\n", f)
		}
		return fmt.Errorf("resolve the conflict marker(s) above, then commit — the working tree is at %s but is NOT yet valid", newRef)
	}

	// ── Lever 2: re-render what the new pin invalidates ──────────────────────
	// The pin copier just rewrote (.copier-answers.yml llz_version) is the value
	// resolveTemplateRef feeds into every apl-values remote ref, so EVERY committed
	// kustomization goes stale the moment copier finishes — deterministically, on
	// every upgrade, with no operator judgment involved. Leaving it manual is not a
	// nuisance but a correctness gap: `--commit` recorded a knowingly-stale render,
	// and a live instance ran three releases behind on what ArgoCD DEPLOYS (see
	// stepRenderFresh). Since that guard landed, the omission also makes `--commit`
	// fail outright — the pre-commit hook runs `llz lint`, which now refuses the
	// very commit this command is trying to make.
	//
	// AFTER the conflict gate on purpose: rendering over merge markers would bury
	// the real failure under a parse error from a file the operator has to fix anyway.
	// THE WRITE HALF, and it is down here rather than beside the preview because
	// everything between them can `return err`: the answers-regression gate and the
	// conflict-marker gate both abort the upgrade. Writing to the operator's owned
	// spec files above them meant an aborted upgrade left a mutation its own failure
	// message never mentioned. The dry run still previews at the earlier point,
	// where the operator can still act on it.
	//
	// AND IT MUST PRECEDE printSummary, which is the ordering that actually binds
	// below. An earlier version of this comment said the RENDER had to see the edit
	// — that was simply false: EffectiveAplChartVersion has no non-test callers,
	// `apl_chart_version` is no longer a tfvar, and nothing in the render path reads
	// the pin. printSummary is different: it is `git diff --stat` over the whole
	// tree, the one place an operator sees what the upgrade touched, and a write
	// after it is a change to their spec that the summary silently omits. --commit
	// below then records it either way.
	pinSteps := retrackAplPins(false)

	if !noRender {
		if err := renderAfter(dryRun); err != nil {
			// THE SPEC IS ALREADY EDITED BY THE TIME THIS CAN FAIL. Returning bare
			// left the operator with a render error, silently modified spec files and
			// none of the checklist — including a "will BLOCK validation" entry about
			// a file this command had just rewritten. Same shape as the dry run that
			// computed its steps and discarded them: the run that goes WRONG is the
			// one that most needs to say what it already did.
			printNextSteps(pinSteps)
			return err
		}
	}

	// ── Levers 3 and 5: what this command cannot do, said out loud ───────────
	steps := reportWhatTheUpgradeCouldNotDo(newRef)
	for _, st := range pinSteps {
		steps = appendStep(steps, st)
	}

	// One place to see what the upgrade touched, so a big managed-file churn is a
	// single reviewable summary rather than a scattered surprise at commit time.
	// Runs after the re-render so the diffstat is the WHOLE upgrade — scaffold plus
	// the apl-values churn the new pin implies — not the half an operator would
	// then have to re-review.
	printSummary(oldRef, newRef)

	return finishUpgrade(dryRun, commit, noDoctor, oldRef, newRef, steps)
}

// finishUpgrade is the tail of Run: the advisory readiness check, then the
// optional commit.
//
// SPLIT OUT SO IT CAN BE TESTED, and the property under test is an ordering one
// that no assertion about the seam alone can reach — that a readiness check which
// REPORTS PROBLEMS still leaves the upgrade successful, and still lets --commit
// commit. Everything above it in Run shells out to copier and the network, so the
// only way to exercise this behavior was to stop making it part of that.
func finishUpgrade(dryRun, commit, noDoctor bool, oldRef, newRef string, steps []string) error {
	// ── Lever 4: what the new release now REQUIRES that the old one did not ──
	// Lever 3 covers values the upgrade invalidates; this covers values it makes
	// NEWLY MANDATORY, which nothing here can compute — the required set lives in
	// the readiness check, and knowing which of them are actually set means asking
	// GitHub. v0.0.42 made TF_STATE_ENCRYPTION_PASSPHRASE required and said
	// nothing, so the first an operator heard of it was a failed pipeline run
	// against a secret that had never existed.
	//
	// ADVISORY, NEVER FATAL. `llz upgrade` is otherwise local, and an operator
	// upgrading offline or without gh auth must still get their upgrade — so the
	// readiness check's error is REPORTED and discarded, not returned. --no-doctor
	// skips it outright. Skipped on --dry-run too: nothing has been rendered, so
	// the readiness it would report is the tree's current one, not the upgrade's.
	if !noDoctor && !dryRun {
		runPostUpgradeDoctor()
	}

	// THE CHECKLIST GOES LAST, and that is the whole reason it exists separately
	// from the reporters that produced it. Each of them prints where it is
	// relevant — mid-upgrade, next to the thing it is about — and then the copier
	// churn, the diffstat and a full readiness report scroll past on top. An
	// operator whose next action is three screens up has been told and not
	// informed. This is the same content, numbered, on the last screen.
	printNextSteps(steps)

	// A single labeled commit so the operator reviews ONE diff and history reads
	// "template vX → vY", not N unrelated file changes. Opt-in (--commit) — we
	// never silently commit someone's working tree.
	if commit {
		return commitUpgrade(dryRun, oldRef, newRef)
	}
	fmt.Fprintln(os.Stderr, color.Dim("  (re-run with --commit to stage + commit this upgrade as one labeled commit)"))
	return nil
}

// renderAfter reconciles the spec into the artifacts the new pin
// invalidated — the gitignored <env>.tfvars and the COMMITTED apl-values
// kustomizations whose `?ref=` is the pin itself.
//
// No-op for a pre-spec instance, using the same InstancePresent guard
// stepRenderFresh uses: there is no spec to render from, and an upgrade must
// still work for an instance that has not adopted one.
//
// A render failure here leaves a tree that is genuinely half-upgraded — the
// scaffold is at the new ref while apl-values still points at the old one — so it
// says exactly that rather than surfacing the bare render error.
func renderAfter(dryRun bool) error {
	tfDir, _, _ := instancelayout.Detect()
	if !clusterspec.InstancePresent(filepath.Dir(tfDir)) {
		return nil
	}
	if err := render.Run(dryRun, "", false, false, false); err != nil {
		return fmt.Errorf("the copier update applied cleanly, but re-rendering the spec failed — the scaffold is "+
			"at the new ref while the committed apl-values still reference the OLD one, which is what ArgoCD would "+
			"sync. Fix the problem below and run `llz render` before committing:\n%w", err)
	}
	return nil
}

// currentTemplateRef reads the ref this checkout is pinned to (copier's answers,
// see stamp.go), falling back to the short SHA. "" outside an instance.
func currentTemplateRef() string {
	if ref := answers.PinnedTemplateRef(); ref != "" {
		return ref
	}
	tv := sustain.ResolveTemplateVersion(SustainDeps())
	if tv.TemplateRef != "" {
		return tv.TemplateRef
	}
	if len(tv.TemplateSHA) >= 8 {
		return tv.TemplateSHA[:8]
	}
	return tv.TemplateSHA
}

// conflictFiles returns the working-tree files copier update just changed
// (or added) that carry a merge-conflict marker — reusing the Phase 0
// conflictMarkerLines predicate so this gate and `llz lint` agree.
func conflictFiles() []string {
	changed := map[string]bool{}
	for _, f := range strings.Split(gitcmd.Out("diff", "--name-only"), "\n") {
		if f = strings.TrimSpace(f); f != "" {
			changed[f] = true
		}
	}
	for _, f := range strings.Split(gitcmd.Out("ls-files", "--others", "--exclude-standard"), "\n") {
		if f = strings.TrimSpace(f); f != "" {
			changed[f] = true
		}
	}
	var bad []string
	for f := range changed {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if len(conflictMarkerLines(string(data))) > 0 {
			bad = append(bad, f)
		}
	}
	sort.Strings(bad)
	return bad
}

// printSummary shows the version delta + a diffstat of the churn.
func printSummary(oldRef, newRef string) {
	from := oldRef
	if from == "" {
		from = "(unknown)"
	}
	fmt.Printf("\n%s template %s → %s\n", color.Bold("upgrade"), from, newRef)
	if stat := gitcmd.Out("diff", "--stat"); stat != "" {
		fmt.Println(stat)
	} else if untracked := gitcmd.Out("ls-files", "--others", "--exclude-standard"); untracked != "" {
		fmt.Printf("new files:\n%s\n", untracked)
	} else {
		fmt.Println(color.Dim("  no file changes — already up to date."))
	}
}

// runPostUpgradeDoctor runs the readiness check and reports it as ADVICE.
//
// It is the same check `llz doctor` runs with no arguments — same repo, same
// default env — so an operator sees here exactly what they would see there, and
// the two can never disagree about what "ready" means.
//
// Its error is deliberately swallowed. A missing secret means the INSTANCE is not
// ready; it does not mean the upgrade failed, and returning it would fail a
// command whose work is already committed to the tree — worse, it would break
// `--commit` after the copier update had landed. So it says the thing loudly and
// lets the operator act.
//
// Package var so tests substitute it: the real one shells out to gh and the
// Linode API, which a unit test has no business doing.
//
// THE DEPLOYMENT IT ASKS ABOUT IS THE INSTANCE'S OWN, resolved by the same
// function `llz doctor` resolves its --env default with — so an operator told to
// "re-check with llz doctor" gets the identical answer. This was a hardcoded
// "e2e", which is the TEMPLATE's throwaway lane and a deployment no adopter has.
// Run against a live adopter mid-upgrade it produced one correct finding (a
// missing TF_STATE_ENCRYPTION_PASSPHRASE, newly required by that very release)
// wrapped in three wrong instructions: a report headed infra-e2e, a fix reading
// `llz tokens --env e2e --yes`, and a closing "run `llz env add e2e` first".
// An advisory nobody trusts is an advisory nobody reads.
var runPostUpgradeDoctor = func() {
	env := onboard.DefaultDoctorEnv()
	fmt.Fprintf(os.Stderr, "\n%s readiness after the upgrade (a new release can require secrets the old one did not):\n\n",
		color.Bold("Checking"))
	if err := onboard.RunDoctor("", env, false, false, "", ""); err != nil {
		fmt.Fprintf(os.Stderr, "\n%s the upgrade itself succeeded — the readiness gaps above are the instance's, not the upgrade's.\n", color.Yellow("!"))
		fmt.Fprintf(os.Stderr, "  Fix them, then re-check with %s\n", color.Cyan("llz doctor --env "+env))
	}
}

// instanceDeployments names this instance's deployments, for the remedies that
// would otherwise print `<env>` and make the operator resolve it.
//
// A PLACEHOLDER IN A COPY-PASTEABLE COMMAND IS A DEFECT, not a style choice — the
// provider-lock remedy carried `<root>` for exactly as long as nobody read it as
// an instruction. Empty when there is no deployment to name, and the callers fall
// back to `<env>` rather than printing a command with a hole in it.
func instanceDeployments() []string {
	tfDir, _, _ := instancelayout.Detect()
	return upstreamupdates.DeploymentsToApply(filepath.Dir(tfDir))
}

// oneOrPlaceholder renders the single deployment this instance has, or `<env>`
// when there is none or several — with two, naming one of them would be a guess
// about which the operator meant.
func oneOrPlaceholder(envs []string) string {
	if len(envs) == 1 {
		return envs[0]
	}
	return "<env>"
}

// printNextSteps closes the upgrade with what the operator still has to do, in
// the order they have to do it.
//
// SILENT WHEN THERE IS NOTHING, because an upgrade that needs no follow-up is the
// common case and a checklist that always prints teaches people to skip it.
func printNextSteps(steps []string) {
	if len(steps) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\n%s before this upgrade reaches a cluster:\n", color.Bold("NEXT STEPS"))
	for i, s := range steps {
		fmt.Fprintf(os.Stderr, "  %d. %s\n", i+1, s)
	}
	fmt.Fprintln(os.Stderr, color.Dim("  Each is explained in full above."))
}

// reportCIImageSkew warns when the tree's ci image variables still name the
// commit the PREVIOUS pin resolved to, and prints both routes back.
//
// Deliberately a warning, not a fix and not an error. The authoritative copy of
// these two values is a GitHub repo variable; `llz upgrade` neither reads nor
// writes repo config, and quietly growing a network mutation here would make an
// otherwise-local command need credentials. So it says the thing at the moment
// the skew is created — the #405 lens, one lever up: fail (or here, warn) at the
// mistake rather than 20 minutes into the run that inherits it.
//
// The `gh variable set` pair is worded to match `llz ci assert-image-fresh`'s
// remediation verbatim, because an operator who ignores this will meet that one
// next and the two must read as the same instruction.
var reportCIImageSkew = func(ref string) string {
	local := cli.ReadEnvFile(".llz/vars.env")
	skew, unchecked := templatecommit.CIImageSkewReport(ref, func(k string) string { return local[k] })
	repo := "<owner>/<instance>"
	if a, _ := answers.Read("."); a != nil && strings.Contains(a.InstanceRepo, "/") {
		repo = a.InstanceRepo
	}
	// ── COULD NOT TELL IS NOT THE SAME AS NOTHING TO DO ───────────────────────
	//
	// This used to return "" here, which put the upgrade's most time-critical
	// instruction behind a silent abstention. computeCIImageVars has no
	// commit-pinned answer when the new ref does not resolve or when the images
	// for the commit it names are not published yet — and the second is the NORMAL
	// state in the minutes after a release, while build-images.yml is still
	// running. That is precisely when someone runs `llz upgrade`.
	//
	// Measured on akamai/gsap-apl's v0.0.47 → v0.0.48: NEXT STEPS listed three
	// items and none was the re-pin; `llz tokens --env prod --yes` then re-pinned
	// BOTH variables off the previous commit. The upgrade had the pin, had the
	// recorded values, and reported nothing.
	//
	// IT STILL ADVISES NO RE-PIN, and that restraint is the pre-existing one this
	// keeps: with no commit-pinned answer the only thing to re-pin ONTO is the
	// floating tags, and #407 exists to keep instances off those. `llz tokens`
	// refuses to write them for the same reason, so the step says re-run it once
	// the images exist rather than naming a value.
	if unchecked != "" {
		repin := color.Cyan("llz tokens --env " + oneOrPlaceholder(instanceDeployments()) + " --yes")
		fmt.Fprintf(os.Stderr, "\n%s could not check the ci images this instance should run: %s\n",
			color.Yellow("!"), unchecked)
		fmt.Fprintf(os.Stderr, "  TF_IMAGE / KUBE_IMAGE are computed FROM the pin this upgrade just moved, so they are\n"+
			"  stale by construction until something re-pins them. CI reads them as %s repo\n"+
			"  variables, which this command reads no more than it writes. Re-run this once the\n"+
			"  release's images are published:\n", repo)
		fmt.Fprintf(os.Stderr, "    %s\n", repin)
		fmt.Fprintln(os.Stderr, color.Dim("  It re-pins only a commit-pinned answer, so running it early is safe: it writes\n"+
			"  nothing until the images exist. Until then the first pipeline run fails `llz ci assert-image-fresh`."))
		return "Re-pin the CI image variables once the release images publish: " + repin + " (unverified this run)"
	}
	if len(skew) == 0 {
		return ""
	}
	fmt.Fprintf(os.Stderr, "\n%s the new pin moved the ci images this instance should run — %d variable(s) still name the old commit:\n",
		color.Yellow("!"), len(skew))
	for _, s := range skew {
		fmt.Fprintf(os.Stderr, "    %s\n      have %s\n      want %s\n", color.Bold(s.Name), color.Dim(s.Have), color.Cyan(s.Want))
	}
	envs := instanceDeployments()
	fmt.Fprintf(os.Stderr, "  CI reads these as %s repo variables, which this command does not push. Re-pin with either:\n", repo)
	if len(envs) == 0 {
		fmt.Fprintf(os.Stderr, "    %s\n", color.Cyan("llz tokens --env <env> --yes"))
	}
	for _, e := range envs {
		fmt.Fprintf(os.Stderr, "    %s\n", color.Cyan("llz tokens --env "+e+" --yes"))
	}
	for _, s := range skew {
		fmt.Fprintf(os.Stderr, "    %s\n", color.Cyan(fmt.Sprintf("gh variable set %s --repo %s --body %s", s.Name, repo, s.Want)))
	}
	fmt.Fprintln(os.Stderr, color.Dim("  Until then the first pipeline run fails `llz ci assert-image-fresh`."))
	return "Re-pin the CI image variables: " + color.Cyan("llz tokens --env "+oneOrPlaceholder(envs)+" --yes")
}

// reportWhatTheUpgradeCouldNotDo prints every "you still have to do this
// yourself" note, in one place.
//
// GATHERED INTO A FUNCTION SO THE WIRING CAN BE PINNED. Each reporter had its own
// test and none of them asserted it was REACHED — delete the call from Run and the
// suite stayed green, which is the same shape as the reporters' own subject:
// something silently not happening. TestEveryUpgradeReporterIsReached holds the
// set, so a fourth added later has to be wired in to pass.
//
// Nothing here may fail the upgrade. Its work is already in the operator's tree by
// the time these run, and a note that aborts the command it annotates would take
// the tree with it.
func reportWhatTheUpgradeCouldNotDo(newRef string) []string {
	// Lever 3: what the new pin invalidates that this command cannot fix. Same
	// class as lever 2, different owner. TF_IMAGE/KUBE_IMAGE are derived FROM the
	// pin copier just rewrote, so they go stale on every upgrade with no operator
	// judgment involved — but the value CI reads is a GitHub repo VARIABLE, and
	// `llz upgrade` is a local command that pushes nothing. Re-rendering cannot
	// reach it. Left unsaid, the skew surfaces as a failed `llz ci
	// assert-image-fresh` on the first pipeline run after every upgrade.
	steps := appendStep(nil, reportCIImageSkew(newRef))

	// The bucket prefix is the same family again, and the one that costs the most
	// to learn late: it is invisible until an APPLY plans a rename, which is
	// twenty minutes into a run, after the PR has already merged.
	steps = appendStep(steps, reportUnpinnedObjLabelPrefix())

	// promote.yml is `owned`, so copier leaves it alone — but what renders it moved
	// with the pin, and a release that changes the generator leaves every instance
	// in the field carrying a pipeline its own `--check` rejects. Nothing produces a
	// diff to notice: the file is unchanged, which is the problem. Doctor below says
	// so and its error is discarded by design, so without this the finding never
	// reaches the checklist an operator actually reads.
	steps = appendStep(steps, reportUnrunnablePromotionPipeline())

	// Lever 5: what the upgrade has NOT done, which reads like something it did.
	// The easiest of the three to miss, because nothing about the tree looks
	// unfinished. The roots are gitignored and generated from the pin at every
	// terraform op, so an upgrade that changes what Terraform DOES to a cluster
	// produces no .tf diff; and llz-terraform.yml neither plans nor applies on push
	// to main. So a merged upgrade can sit unapplied indefinitely with every check
	// green. `llz drift` cannot see it — it compares the repo's pin to the template
	// head and never asks a cluster.
	steps = appendStep(steps, reportDeploymentsToApply())
	return steps
}

// appendStep collects a reporter's one-line next step, dropping the empty string
// a reporter with nothing to say returns.
func appendStep(steps []string, step string) []string {
	if step == "" {
		return steps
	}
	return append(steps, step)
}

// reportUnpinnedObjLabelPrefix warns when this instance leaves its Object Storage
// bucket prefix to the default.
//
// WHY IT MATTERS ONLY WHEN UNSET. `spec.instance.objLabelPrefix` defaults to the
// INSTANCE NAME. Bucket labels are create-time only, so for an instance whose
// buckets were provisioned under a different prefix — every instance created
// before that field existed, when the module hardcoded `platform` — the default is
// a RENAME, and Terraform's only way to honour a rename is to delete the bucket
// and create an empty one. That is where Loki's chunks and the Harbor registry
// live.
//
// SAID HERE BECAUSE OF WHEN THE ALTERNATIVE SAYS IT. Nothing else notices until an
// apply plans the rename, and by then the upgrade PR has merged and the operator
// is twenty minutes into a run that is about to be refused. This costs one read of
// a file already on disk.
//
// IT DOES NOT GUESS A VALUE, and that restraint is the point. Naming a prefix
// would mean deciding which of the account's buckets belong to this instance from
// their labels alone, and the wrong guess sends someone to pin a prefix that
// renames the buckets holding their data — the precise failure this whole area has
// been fixed for twice. `llz ci assert-upgrade-plan` answers it exactly, from a
// real plan plus the live object counts, and that is what this points at.
//
// Package var for the same reason its neighbours are: the real one reads the tree.
var reportUnpinnedObjLabelPrefix = func() string {
	tfDir, _, _ := instancelayout.Detect()
	root := filepath.Dir(tfDir)
	lz, err := clusterspec.LoadInstance(root)
	if err != nil || lz == nil {
		return "" // no spec to read; a legacy instance renders no tfvars from one either
	}
	if strings.TrimSpace(lz.Spec.Instance.ObjLabelPrefix) != "" {
		return "" // pinned deliberately; nothing to warn about
	}
	env := onboard.DefaultDoctorEnv()
	fmt.Fprintf(os.Stderr, "\n%s this instance does not pin %s, so its Object Storage buckets are labelled %s.\n",
		color.Yellow("!"), color.Bold("spec.instance.objLabelPrefix"), color.Bold(lz.ObjLabelPrefix()+"-*"))
	fmt.Fprintf(os.Stderr, "  If your buckets were created under a DIFFERENT prefix — instances predating that field used\n"+
		"  `platform` — the next apply will plan to REPLACE them, and a bucket label is create-time only,\n"+
		"  so that means deleting the one holding your Loki chunks and Harbor registry.\n")
	fmt.Fprintf(os.Stderr, "  Check it BEFORE you merge, from a real plan and the live object counts:\n\n")
	for _, line := range objPrefixCheck(env) {
		fmt.Fprintf(os.Stderr, "    %s\n", color.Cyan(line))
	}
	fmt.Fprintln(os.Stderr, color.Dim("\n  It exits 0 if nothing is at risk, and names the prefix to pin if something is.\n"+
		"  Step by step: docs/runbooks/bucket-prefix-rename.md"))
	return "Check whether your bucket prefix would rename live buckets (" + color.Cyan("docs/runbooks/bucket-prefix-rename.md") + ")"
}

// objPrefixCheck is the local plan-and-assert an operator runs to find out whether
// their next apply would rename a bucket.
//
// THREE REAL COMMANDS, NOT ONE CONVENIENT ONE. The first cut of this warning said
// `llz build <env> --dry-run`, which cannot answer the question: `llz build`
// DISPATCHES the pipeline with action=apply, and --dry-run is llz's global
// "print the command and change nothing" — so it prints a dispatch, runs no plan,
// and tells the operator nothing. A warning whose remedy does not work is worse
// than no warning, and this is the fifth time that shape has appeared in this
// change.
//
// EVERY OpenTofu LINE GOES THROUGH `llz tofu`, INCLUDING `show`. The first cut of
// this list ended with a bare `tofu show -json tfplan.bin`, which fails: the PLAN
// FILE IS ENCRYPTED TOO (encryption.tf configures `plan` as well as `state`), so
// reading it needs $TF_ENCRYPTION exactly as writing it did. It failed with the
// same "Invalid expression" this release exists to stop people meeting.
//
// It works in the WORKFLOW because the composite action exports TF_ENCRYPTION for
// the whole job — which is precisely why copying a line out of CI into an
// operator's terminal is not a safe way to write a remedy.
//
// `assert-upgrade-plan` is the same binary on the same bytes the pipeline runs, so
// a clean answer here is the answer CI will give.
func objPrefixCheck(env string) []string {
	return []string{
		"cd terraform-iac-bootstrap/object-storage",
		"llz tofu --region " + env + " -- init -upgrade",
		"llz tofu -- plan -var-file=" + env + ".tfvars -out=tfplan.bin",
		"llz tofu -- show -json tfplan.bin | llz ci assert-upgrade-plan",
	}
}

// reportUnrunnablePromotionPipeline warns when promote.yml can no longer run —
// most often because THIS UPGRADE is what stopped it running.
//
// WHY IT BELONGS IN THIS FAMILY. promote.yml is `owned` in .template-manifest, so
// copier will not touch it; what renders it is the GENERATOR inside the llz that
// just moved. v0.0.45 rendered the empty placeholder for a one-deployment
// instance and v0.0.47 renders a real single stage from the identical spec, so an
// upgrade across that boundary leaves a promote.yml that is stale by definition
// and produces no diff of its own to notice. Same shape as reportCIImageSkew: a
// value derived from the pin, left behind by a command that cannot reach it. (The
// third of that family reported a stale provider lock; it is gone, because its
// subject is rendered now — see tools/internal/shared/tfroots.)
//
// SAID HERE BECAUSE OF WHERE THE ALTERNATIVE SAYS IT. `llz doctor` already fails
// on this (doctor_build_preflights.go, PR #511) and runs a few lines below —
// but finishUpgrade discards doctor's error by design, so a fatal ✗ arrives as a
// yellow advisory with the copier churn, the diffstat and a full readiness report
// scrolling past on top. That is the exact gap printNextSteps exists to close, and
// the checklist carried the other four reporters and not this one. Found on
// akamai/gsap-apl's v0.0.45 → v0.0.47 upgrade: doctor said it, NEXT STEPS did not,
// and the stale pipeline was caught by hand.
//
// THE VERDICT IS RunnableErr, NOT p.Changed, and the difference is not
// hypothetical. Reporting only regenerable drift would list the WEAKER problem and
// stay silent on the stronger one — a promote.yml naming a deployment that does not
// exist dispatches stages that each die inside `llz render`, and it is not always
// regenerable. Both fail `llz env pipeline --check` on the PR the operator is about
// to open, so both belong on the checklist.
//
// ADVISORY, LIKE ITS NEIGHBOURS: the upgrade's work is already in the tree.
// NOT --require-pipeline, for the reason doctor gives — "no pipeline yet" is the
// state every fresh instance is in and is not something an upgrade broke.
//
// Package var for the same reason its neighbours are: the real one reads the tree.
var reportUnrunnablePromotionPipeline = func() string {
	tfDir, _, relPrefix := instancelayout.Detect()
	plan, err := promote.PlanWorkflow(promote.DefaultDeps(), tfDir, relPrefix)
	if err != nil {
		// SAID, NOT SWALLOWED. A spec or a
		// promote.yml this cannot read yields no verdict at all, and silence here
		// reads as "my pipeline is fine" for the one file that is about to be
		// dispatched.
		fmt.Fprintf(os.Stderr, "\n%s could not check this instance's promotion pipeline: %v\n", color.Yellow("!"), err)
		fmt.Fprintln(os.Stderr, color.Dim("  `llz env pipeline --check` reports the same thing, and fails the upgrade PR."))
		return ""
	}
	rerr := plan.RunnableErr(false)
	if rerr == nil {
		return ""
	}
	// THE HEADLINE HAS TO MATCH THE CAUSE. "This release generates a different
	// promote.yml" is true of drift and false of a stage naming a deployment that
	// does not exist — the release generated nothing there, and a sentence that
	// blames the upgrade for the operator's edit sends them looking for a
	// regression that is not in the diff.
	if plan.Changed {
		fmt.Fprintf(os.Stderr, "\n%s this release generates a different %s than the one this instance carries:\n",
			color.Yellow("!"), color.Bold("promote.yml"))
	} else {
		fmt.Fprintf(os.Stderr, "\n%s this instance's %s cannot run as written:\n",
			color.Yellow("!"), color.Bold("promote.yml"))
	}
	// The message is multi-line by design — it names each bad stage, the
	// deployments that DO exist, and which remedy applies — so it is indented
	// whole rather than flattened, exactly as doctor prints it.
	for _, l := range strings.Split(rerr.Error(), "\n") {
		fmt.Fprintf(os.Stderr, "    %s\n", l)
	}
	fmt.Fprintln(os.Stderr, color.Dim("  Until then `llz env pipeline --check` fails on the upgrade PR, and promote.yml's\n"+
		"  own dispatch preflight refuses the run."))
	// THE STEP NAMES THE COMMAND THAT ACTUALLY FIXES THIS TREE. Regenerable drift
	// is one command; a pipeline llz cannot regenerate from is the operator's edit,
	// and pointing that operator at a generator that will not write is the
	// remedy-that-does-not-work shape objPrefixCheck above was rewritten for.
	if plan.Changed {
		return "Run " + color.Cyan("llz env pipeline") + " and commit promote.yml (this one blocks the PR)"
	}
	return "Fix promote.yml's stages by hand — see the detail above (this one blocks the PR)"
}

// reportDeploymentsToApply names the deployments the operator still has to
// dispatch, because merging or committing an upgrade applies nothing.
//
// ADVISORY, and phrased as a LIST OF DEPLOYMENTS rather than a list of stale
// ones. Whether any of them has been applied since is not recorded anywhere this
// command can read, and a reminder that overclaims is one people learn to skip.
//
// Silent outside an instance and silent for an instance with no deployment yet:
// both are states where there is nothing useful to say, and neither is worth a
// paragraph on every upgrade.
//
// Package var for the same reason its neighbours are: the real one reads the
// spec off disk.
var reportDeploymentsToApply = func() string {
	tfDir, _, _ := instancelayout.Detect()
	envs := upstreamupdates.DeploymentsToApply(filepath.Dir(tfDir))
	if len(envs) == 0 {
		return ""
	}
	fmt.Fprintf(os.Stderr, "\n%s the upgrade is in your working tree; it has not reached any cluster.\n",
		color.Yellow("!"))
	fmt.Fprintf(os.Stderr, "  Terraform runs on dispatch, not on push, and the roots are generated from the pin\n"+
		"  at every op — so a change to what Terraform does shows no diff here. Apply each deployment:\n")
	for _, e := range envs {
		fmt.Fprintf(os.Stderr, "    %s\n", color.Cyan("llz build "+e+" --yes"))
	}
	return "After the PR merges, apply each deployment: " + color.Cyan("llz build "+oneOrPlaceholder(envs)+" --yes")
}

// commit stages the whole tree and records the upgrade as one labeled
// commit, so history reads "template vX → vY". No-op (not an error) when the
// update produced no changes.
// conflictMarkerLines is copied from checks.go, not shared: eleven lines with a
// caller on each side of the boundary, and the thing it recognises (a git conflict
// marker) is not going to change.
func conflictMarkerLines(content string) []int {
	var lines []int
	for i, ln := range strings.Split(content, "\n") {
		ln = strings.TrimRight(ln, "\r")
		for _, m := range []string{"<<<<<<<", ">>>>>>>"} {
			if ln == m || strings.HasPrefix(ln, m+" ") {
				lines = append(lines, i+1)
			}
		}
	}
	return lines
}

// Package var for the same reason runPostUpgradeDoctor is one: the real
// implementation runs `git add -A` + `git commit` in the caller's working tree,
// which a unit test must not do — and the ordering test needs to observe THAT it
// was reached, not perform it.
var commitUpgrade = func(dryRun bool, oldRef, newRef string) error {
	if strings.TrimSpace(gitcmd.Out("status", "--porcelain")) == "" {
		fmt.Fprintln(os.Stderr, color.Green("✓")+" already up to date — nothing to commit.")
		return nil
	}
	from := oldRef
	if from == "" {
		from = "(initial)"
	}
	msg := fmt.Sprintf("chore(template): upgrade %s → %s\n\nApplied via `llz upgrade`.", from, newRef)
	if err := proc.RunEcho(dryRun, "git", "add", "-A"); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	if err := proc.RunEcho(dryRun, "git", "commit", "-m", msg); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	fmt.Fprintf(os.Stderr, "%s committed template upgrade %s → %s\n", color.Green("✓"), from, newRef)
	return nil
}

// VolatileAnswers are the keys an upgrade is SUPPOSED to rewrite: the
// provenance copier maintains and the version pin the upgrade exists to move.
// Everything else must survive untouched.
var VolatileAnswers = map[string]bool{
	"_commit": true, "_src_path": true, "llz_version": true,
}

// AnswerRegressions lists every answer the upgrade changed that it had no
// business changing. Pure so the comparison — not the copier run — is what the
// unit tests exercise.
//
// A DISAPPEARED key counts: copier dropping an answer is the same loss as
// rewriting it, and the value it renders next is the template default either way.
func AnswerRegressions(before, after map[string]string) []string {
	var out []string
	for k, was := range before {
		if VolatileAnswers[k] {
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

// currentAnswerMap is the working directory instance's answers, or nil when
// there is no readable answers file. nil is the pre-copier / not-an-instance
// case, and AnswerRegressions over a nil `before` reports nothing — an upgrade
// cannot be said to have lost an answer that was never recorded.
func currentAnswerMap() map[string]string {
	m, err := ReadAnswerMap(".copier-answers.yml")
	if err != nil {
		return nil
	}
	return m
}

// ReadAnswerMap loads a .copier-answers.yml as a flat string map. Non-scalar
// values are rendered with %v; the answers this template asks are all scalars,
// and a structural change there should show up as a diff rather than a panic.
func ReadAnswerMap(path string) (map[string]string, error) {
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
