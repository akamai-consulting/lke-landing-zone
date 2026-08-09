// Package copier wraps the `copier` CLI — the tool that renders and updates an
// instance from this template.
//
// IT IS THE LAYER `llz new` AND `llz upgrade` SHARE. Both were in commands.go and
// both reached for the same five things: which ref to scaffold from, how to build
// the argv, and whether copier is even installed. Splitting the two commands
// without naming that layer first would have duplicated it.
//
// THE REF RESOLUTION IS THE PART WITH A RULE IN IT. A scaffold must never float on
// `main`: the template's own tflint gate rejects an unpinned module source,
// Renovate cannot bump it, and copier's llz_version validator refuses it. So Ref
// falls back from "this binary's version" to "the newest published vX.Y.Z of the
// template" and errors rather than guessing.
package copier

import (
	"fmt"
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/selfupgrade"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
)

// Version is the llz build stamp, injected by package main at init — the same
// one-source rule internal/reconciler and internal/selfupgrade follow. It decides
// whether this binary can anchor a scaffold to its own release, so a stale "dev"
// here silently changes what gets pinned.
var Version = "dev"

// ResolveRef picks the template ref to scaffold/upgrade from: an explicit
// --ref verbatim (tag, branch, or SHA), else this llz binary's own Version when it
// is a real release (the CLI is the Version anchor), else "" — signalling the
// caller (Ref) to resolve the latest published release tag, since a dev
// build has no Version to anchor to. The chosen value is rendered into the
// instance's pins as copier's llz_version, so the scaffold references exactly the
// release it was cut from.
func ResolveRef(ref string) string {
	if ref != "" {
		return ref
	}
	if _, _, _, ok := selfupgrade.Semver(Version); ok {
		return selfupgrade.NormalizeLLZTag(Version)
	}
	return ""
}

// LatestReleaseFn resolves the newest published vX.Y.Z release of a template repo;
// seamed for tests. It reuses self-update's release picker, which drops drafts /
// pre-releases and ignores the llz/v* CLI tag track (selfupgrade.LatestLLZTag).
var LatestReleaseFn = selfupgrade.LatestRelease

// Ref resolves the concrete ref to scaffold/pin to. It falls back from a
// dev build (no anchor Version) to the latest published vX.Y.Z release of repo, so
// a scaffold never floats on `main` — which the template's own tflint gate
// (terraform_module_pinned_source) rejects, Renovate can't bump, and copier now
// refuses (the llz_version validator). repo is the template's <org>/<name>.
func Ref(ref, repo string) (string, error) {
	if r := ResolveRef(ref); r != "" {
		return r, nil
	}
	tag, err := LatestReleaseFn(repo)
	if err != nil {
		return "", fmt.Errorf("this is a dev build of llz (no anchor Version) and the latest %s release could not be resolved to pin to: %w\n"+
			"  pass --ref vX.Y.Z to pin to a release explicitly", repo, err)
	}
	return tag, nil
}
func CopyArgv(org, ref, dir string) []string {
	return []string{"copier", "copy", "--trust", "--vcs-ref", ref,
		"--data", "llz_version=" + ref,
		"gh:" + org + "/" + templateid.Name, dir}
}

// UpdateArgv is the update invocation, and --defaults is load-bearing.
//
// Without it `copier update` RE-ASKS every question — upstream_org,
// instance_repo, openbao_team — using the stored answers as onboard.Prompt defaults. Two
// costs, and the second is the one that bit:
//
//   - With no terminal that is not a onboard.Prompt, it is an unhandled OSError out of
//     prompt_toolkit. `llz upgrade` inherits the operator's stdin, so it worked
//     by hand and died in CI, in a wrapper script, and over `ssh host 'llz
//     upgrade'` — with a Python traceback, not a message.
//   - Interactively it is three unexplained prompts mid-upgrade, on answers that
//     are rendered INTO managed files. instance_repo becomes the ArgoCD repoURL
//     and every `gh` target, so one stray keystroke re-renders the instance
//     against a repo that does not exist.
//
// --defaults keeps the stored answers (verified: a non-default instance_repo and
// openbao_team both survive v0.0.39 → v0.0.40 untouched). It does NOT make the
// answers safe on its own — copier still falls back to the template DEFAULT for
// an answer it cannot keep, which is why runUpgrade verifies them afterwards
// rather than trusting this flag. `llz ci upgrade-test` runs this exact argv.
func UpdateArgv(ref string) []string {
	a := []string{"copier", "update", "--trust", "--defaults"}
	if ref != "" {
		a = append(a, "--vcs-ref", ref, "--data", "llz_version="+ref)
	}
	return a
}

// Require fails with an install route when the copier CLI is absent.
//
// `llz new` and `llz upgrade` are thin wrappers around `copier copy` / `copier
// update`, and copier is a Python tool that is on no machine by default. Without
// this, the scaffold died on exec's own words — `copier copy: exec: "copier":
// executable file not found in $PATH` — as the SECOND command of the quickstart,
// naming a tool the operator has never heard of and no way to get it. The
// installer only checks `gh`, and `llz doctor` (which does list copier) reports
// rather than gates AND is two steps further down the quickstart, so nothing
// between install and scaffold said this out loud.
//
// action names the command, because the two have different recovery paths: a
// failed `llz new` leaves nothing behind, a failed `llz upgrade` is mid-flight.
// g so a --dry-run, which never execs copier (run() returns before exec), reports
// the gap without failing on it — the flag's contract is "print what would run,
// change nothing", and a dry-run that hard-errors on a tool it was not going to
// invoke breaks it. Warn rather than stay silent: "this would work" is the wrong
// answer too.
func Require(dryRun bool, action string) error {
	if kubectlprobe.Lookable("copier") {
		return nil
	}
	if dryRun {
		fmt.Fprintf(os.Stderr, "%s `copier` is not on PATH — %s would fail here (dry-run: not executing it).\n",
			color.Yellow("!"), action)
		fmt.Fprintf(os.Stderr, "  install it first: %s\n", color.Cyan("pipx install copier"))
		return nil
	}
	//lint:ignore ST1005 multi-line operator diagnostic: the trailing period closes an embedded install-route block, not a sentence fragment
	return fmt.Errorf("the `copier` CLI is not on PATH — %s renders the scaffold with it.\n"+
		"  copier is a Python tool and is not installed by default (the llz installer does not add it):\n"+
		"  • pipx install copier      (what this repo's own CI uses)\n"+
		"  • uv tool install copier\n"+
		"  • brew install copier\n"+
		"  Then re-run %s. `llz doctor` lists the rest of the toolchain it expects.", action, action)
}
