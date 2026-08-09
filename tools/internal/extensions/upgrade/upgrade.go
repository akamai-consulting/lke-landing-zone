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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/buildpreflight"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/onboard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/selfupgrade"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/sustain"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/templatemanifest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/copier"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/proc"
	"sigs.k8s.io/yaml"
)

// SustainDeps is injected by package main: assembling sustain.Deps needs
// lockableScaffoldFiles and the global --yes, both of which are main's. Same
// caller-side-assembly rule promote.Deps established.
var SustainDeps func() sustain.Deps

func Run(dryRun bool, ref string, commit, noRender bool) error {
	// Same prerequisite as `llz new`, and worse to discover late: this one runs
	// snapshot/restore around copier, so failing at the exec is failing mid-flight.
	if err := copier.Require(dryRun, "`llz upgrade`"); err != nil {
		return err
	}
	oldRef := currentTemplateRef() // "" if this is not an instance checkout yet
	// Snapshot before copier runs — it rewrites this file in place, so afterwards
	// there is nothing left to compare against (see Lever 0).
	answersBefore := currentAnswerMap()

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
	policy, policyErr := templatemanifest.Load("")
	var owned selfupgrade.UpgradeSnapshot
	if policyErr != nil {
		fmt.Fprintf(os.Stderr, "%s no usable .template-manifest (%v) — upgrading without manifest-class enforcement\n", color.Yellow("!"), policyErr)
	} else {
		if owned, err = selfupgrade.SnapshotUpgradeOwned(policy); err != nil {
			return fmt.Errorf("snapshot owned files: %w", err)
		}
		defer owned.Cleanup()
	}
	if err := proc.RunEcho(dryRun, copier.UpdateArgv(ref)...); err != nil {
		return fmt.Errorf("copier update: %w", err)
	}
	if policyErr == nil {
		if err := selfupgrade.ApplyManifestPolicy(dryRun, ref, owned); err != nil {
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
	if dryRun {
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
	if !noRender {
		if err := renderAfter(dryRun); err != nil {
			return err
		}
	}

	// ── Lever 3: name what the new pin invalidates that this command cannot fix ──
	// Same class as lever 2, different owner. TF_IMAGE/KUBE_IMAGE are derived FROM
	// the pin copier just rewrote, so they go stale on every upgrade with no
	// operator judgment involved — but the value CI reads is a GitHub repo
	// VARIABLE, and `llz upgrade` is a local command that pushes nothing.
	// Re-rendering cannot reach it. Left unsaid, the skew surfaces as a failed
	// `llz ci assert-image-fresh` on the first pipeline run after every upgrade.
	reportCIImageSkew(newRef)

	// One place to see what the upgrade touched, so a big managed-file churn is a
	// single reviewable summary rather than a scattered surprise at commit time.
	// Runs after the re-render so the diffstat is the WHOLE upgrade — scaffold plus
	// the apl-values churn the new pin implies — not the half an operator would
	// then have to re-review.
	printSummary(oldRef, newRef)

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
	for _, f := range strings.Split(buildpreflight.GitOut("diff", "--name-only"), "\n") {
		if f = strings.TrimSpace(f); f != "" {
			changed[f] = true
		}
	}
	for _, f := range strings.Split(buildpreflight.GitOut("ls-files", "--others", "--exclude-standard"), "\n") {
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
	if stat := buildpreflight.GitOut("diff", "--stat"); stat != "" {
		fmt.Println(stat)
	} else if untracked := buildpreflight.GitOut("ls-files", "--others", "--exclude-standard"); untracked != "" {
		fmt.Printf("new files:\n%s\n", untracked)
	} else {
		fmt.Println(color.Dim("  no file changes — already up to date."))
	}
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
var reportCIImageSkew = func(ref string) {
	local := onboard.ReadEnvFile(".llz/vars.env")
	skew := templatecommit.StaleCIImageVars(ref, func(k string) string { return local[k] })
	if len(skew) == 0 {
		return
	}
	repo := "<owner>/<instance>"
	if a, _ := answers.Read("."); a != nil && strings.Contains(a.InstanceRepo, "/") {
		repo = a.InstanceRepo
	}
	fmt.Fprintf(os.Stderr, "\n%s the new pin moved the ci images this instance should run — %d variable(s) still name the old commit:\n",
		color.Yellow("!"), len(skew))
	for _, s := range skew {
		fmt.Fprintf(os.Stderr, "    %s\n      have %s\n      want %s\n", color.Bold(s.Name), color.Dim(s.Have), color.Cyan(s.Want))
	}
	fmt.Fprintf(os.Stderr, "  CI reads these as %s repo variables, which this command does not push. Re-pin with either:\n", repo)
	fmt.Fprintf(os.Stderr, "    %s\n", color.Cyan("llz tokens --env <env> --yes"))
	for _, s := range skew {
		fmt.Fprintf(os.Stderr, "    %s\n", color.Cyan(fmt.Sprintf("gh variable set %s --repo %s --body %s", s.Name, repo, s.Want)))
	}
	fmt.Fprintln(os.Stderr, color.Dim("  Until then the first pipeline run fails `llz ci assert-image-fresh`."))
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

func commitUpgrade(dryRun bool, oldRef, newRef string) error {
	if strings.TrimSpace(buildpreflight.GitOut("status", "--porcelain")) == "" {
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
