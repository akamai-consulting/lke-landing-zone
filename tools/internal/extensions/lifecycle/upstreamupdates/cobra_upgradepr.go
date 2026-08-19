package upstreamupdates

// cobra_upgradepr.go — CLI surface for Decide: read git state, act on the
// verdict. Transport and wiring only; the judgement is in upgradepr.go and is
// tested without git or GitHub.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/gitcmd"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/spf13/cobra"
)

// execSeam is the single process hop this file makes. ONE seam rather than a
// kubectlprobe.Exec at each call site: it lets a test read the ARGV the real
// helpers build — `--draft` is load-bearing (it keeps the bot's PR off the
// state-writing plan job), and a test that restated the flag list instead of
// reading it would pass while the flag was absent.
//
// It delegates through a CLOSURE rather than taking kubectlprobe.Exec's value:
// `var execSeam = kubectlprobe.Exec` would snapshot whatever that var pointed at
// when this package initialised, so a test swapping kubectlprobe.Exec would stub
// gitOut's side of the package and leave the real `gh pr create` and `git push`
// live underneath it. seedspecial, reachability and identityconfig each carry the
// same note; that bug has cost this campaign three times.
var execSeam = func(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }

// upgradePR is one of this repo's upgrade pull requests: its head branch and the
// state gh reports for it.
type upgradePR struct {
	Head  string
	State string // OPEN | CLOSED | MERGED — read, never inferred from the query
}

// prListLimit bounds the pull-request page the two guards read. Large enough that
// truncation is implausible on a real instance repo, and truncation is an ERROR
// rather than a shrug (see upgradeBranches).
const (
	prListLimit  = "300"
	prListLimitN = 300
)

// Seams: every side effect the command has, so a test drives the whole decision
// path without a repo or a network.
var (
	gitOut = func(args ...string) (string, error) { return gitcmd.Output(".", args...) }

	// upgradePRs lists this repo's upgrade pull requests, SCOPED BY HEAD BRANCH and
	// carrying their state.
	//
	// TWO EMPIRICAL FACTS SHAPE THIS, and I had the first one backwards:
	//
	//  1. `gh pr list --state closed` INCLUDES MERGED. Measured, not assumed —
	//     `--state closed --json state` returns MERGED rows. An earlier cut relied
	//     on the opposite, so a merged upgrade at some version made the
	//     rejected-version guard fire, and a legitimate re-commit at an unchanged
	//     pin (a drifted `managed` file restored) was discarded as "a reviewer
	//     rejected this" — exit 0, green run. State is therefore read, not inferred
	//     from the query.
	//
	//  2. An UNSCOPED list is filled by unrelated pull requests. `--limit 300` on
	//     this repo's closed PRs returns exactly 300 today, so a truncation guard
	//     counting every row fired on every run. `--search head:<stem>` scopes the
	//     page to upgrade branches (verified: it prefix-matches, and returns 0 for
	//     a stem nothing uses), which makes both the guard and the count mean what
	//     they say.
	upgradePRs = func(state, stem string) ([]upgradePR, error) {
		out, err := execSeam("gh", "pr", "list", "--state", state, "--search", "head:"+stem,
			"--limit", prListLimit, "--json", "headRefName,state",
			"--jq", `.[] | .headRefName + " " + .state`)
		if err != nil {
			return nil, fmt.Errorf("could not list %s pull requests for %s: %w\n"+
				"    refusing to act on a guess: this decides whether an upgrade is already awaiting review "+
				"and whether this version was already rejected", state, stem, err)
		}
		var prs []upgradePR
		rows := 0
		for _, l := range strings.Split(string(out), "\n") {
			f := strings.Fields(strings.TrimSpace(l))
			if len(f) != 2 {
				continue
			}
			rows++
			if !strings.HasPrefix(f[0], stem) {
				continue // search is fuzzy at the edges; the stem is the authority
			}
			prs = append(prs, upgradePR{Head: f[0], State: strings.ToUpper(f[1])})
		}
		// THE RAW PAGE IS WHAT IS COUNTED, not the rows that survived the filter.
		// Truncation is a property of the RESPONSE: `gh` stops at --limit before this
		// code sees anything, so one fuzzy non-match on a full page drops the
		// filtered count to 299 and the guard waves through a page that is missing
		// rows — which is the one thing it exists to refuse, since a truncated page
		// reads exactly like 'none'.
		if rows >= prListLimitN {
			return nil, fmt.Errorf("`gh pr list` returned a full page of %d %s pull requests under %s — the page "+
				"may be truncated, and a truncated page reads exactly like 'none'", rows, state, stem)
		}
		return prs, nil
	}

	pushBranch = func(branch string) error {
		if _, err := execSeam("git", "switch", "-c", branch); err != nil {
			return fmt.Errorf("create branch %s: %w", branch, err)
		}
		// A PLAIN push, never forced: BranchName carries the commit SHA, so the name
		// cannot already exist. If it somehow does, failing is the right answer.
		if _, err := execSeam("git", "push", "origin", branch); err != nil {
			return fmt.Errorf("push %s: %w", branch, err)
		}
		return nil
	}

	// --draft IS LOAD-BEARING, not a courtesy to the reviewer.
	//
	// A genuine upgrade rewrites the vendored .github/workflows/llz-*.yml bodies,
	// which are in terraform.yml's pull_request paths: filter. That selects
	// plan-cluster-pr — whose `llz ci tf-import` step WRITES
	// cluster/<deployment>/terraform.tfstate with nothing serializing it against a
	// concurrent apply. plan-cluster-pr skips DRAFT pull requests, and that skip
	// exists precisely so an automated one cannot take that write; terraform.yml
	// lists `ready_for_review` in its trigger types so a human can promote the PR
	// and get the plan when they want it.
	//
	// repo-readiness does NOT skip drafts, so the check that actually matters — a
	// newly mandatory secret this release needs — still runs on arrival.
	createPR = func(title, base, head string) error {
		// TWO DECORATIONS ARE DROPPABLE, AND THE DROPS HAVE TO COMPOSE — which is
		// the bug this shape exists to prevent rather than a generalisation for its
		// own sake. Each refusal used to retry with the OTHER decoration still set
		// and return the error if that second call failed. A private repo on a Free
		// plan that has never created the label — i.e. every fresh instance — refuses
		// BOTH, the label complaint is what `gh` reports, and so the draft fallback
		// was unreachable on exactly the repo shape it was written for: the run
		// errored with a branch pushed, no pull request, and no summary.
		//
		// --label is dropped rather than required: nothing keys off it (both guards
		// match the branch STEM), so losing it costs only the filter in the GitHub
		// UI. --draft is dropped last and never silently — see the warning below.
		// THE BODY IS COMPOSED PER ATTEMPT, not passed in: it makes a claim about
		// --draft ("this PR opens as a DRAFT on purpose, so plan-cluster-pr is
		// skipped"), and on the fallback that claim is false about the very pull
		// request carrying it. The reviewer reads the body; the ::warning below goes
		// to a run log they have no reason to open.
		attempt := func(draft, label bool) error {
			args := []string{"pr", "create"}
			if draft {
				args = append(args, "--draft")
			}
			args = append(args, "--title", title, "--body", prBody(draft), "--base", base, "--head", head)
			if label {
				args = append(args, "--label", "template-upgrade")
			}
			_, err := execSeam("gh", args...)
			return err
		}

		// ONLY the two named refusals retry. Retrying on ANY error is how a create
		// that SUCCEEDED and then errored late (a timeout reading the response, say)
		// becomes a second attempt: the retry fails on the pull request the first
		// call already made, the run goes red, and no summary is written for a PR
		// that exists. Anything else is returned as-is.
		//
		// The `draft &&` / `label &&` guards are what bound the loop: a decoration is
		// only ever dropped once, so this runs at most three times and cannot spin on
		// an error that keeps naming something already gone.
		draft, label := true, true
		first := error(nil)
		for {
			err := attempt(draft, label)
			if err == nil {
				if !draft {
					// Draft pull requests are unavailable on a private repo on a Free plan.
					// The branch is already pushed by now, so refusing would strand it — but a
					// NON-draft PR selects the state-writing plan job, which is the one thing
					// --draft exists to prevent. So: open it, and say plainly that the
					// protection is absent, rather than either stranding the work or letting
					// the write happen silently.
					fmt.Fprintf(os.Stderr, "::warning title=Draft unavailable::this repository cannot open DRAFT pull "+
						"requests (private repo on a Free plan), so the upgrade PR is open and WILL select "+
						"plan-cluster-pr — whose tf-import step writes Terraform state. Convert it to a draft, or merge "+
						"it promptly, and do not leave it open across an apply.\n")
				}
				return nil
			}
			if first == nil {
				first = err
			}
			low := strings.ToLower(err.Error())
			switch {
			case label && strings.Contains(low, "label"):
				label = false
			case draft && strings.Contains(low, "draft"):
				draft = false
			default:
				// The FIRST error is what gets wrapped: it is the one that describes the
				// repo, while a later one only says the fallback did not help either.
				if !errors.Is(err, first) {
					return fmt.Errorf("%w (retry without %s also failed: %v)", first, dropped(draft, label), err)
				}
				return err
			}
		}
	}
)

// dropped names the decorations createPR had already given up when a retry
// failed, so the wrapped error says which fallback was being attempted rather
// than leaving the reader to infer it from the message it wraps.
func dropped(draft, label bool) string {
	switch {
	case !draft && !label:
		return "--draft or the label"
	case !draft:
		return "--draft"
	case !label:
		return "the label"
	}
	return "any fallback"
}

// UpgradePRCmd is `llz ci upgrade-pr`.
func UpgradePRCmd() *cobra.Command {
	var beforeSHA, base string
	var force bool
	c := &cobra.Command{
		Use:   "upgrade-pr",
		Short: "open a pull request for an `llz upgrade` that CI has just committed",
		Long: "Reads the repo state left by `llz upgrade --commit` and, if it produced a\n" +
			"commit, pushes a branch and opens a DRAFT pull request for it.\n\n" +
			"`llz upgrade` exits 0 both when it upgraded and when the instance was already\n" +
			"current, so --before is how this tells those apart: HEAD moving is the only\n" +
			"honest signal that there is anything to push.\n\n" +
			"The branch carries the commit SHA, so two runs can never compute the same\n" +
			"name. What a run may PROPOSE is decided by two questions about pull requests\n" +
			"rather than about refs: is an upgrade already awaiting review, and was this\n" +
			"version already closed unmerged. Either answer stops the run; --force overrides\n" +
			"both, for an operator who dispatched with an explicit --ref.\n\n" +
			"Draft on purpose: plan-cluster-pr writes Terraform state and skips drafts, so\n" +
			"marking the pull request ready is what asks for the plan.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if beforeSHA == "" {
				return fmt.Errorf("--before is required: without the pre-upgrade HEAD there is no way to tell " +
					"an upgrade from an instance that was already current")
			}
			// Named before anything else so the error is the missing secret rather
			// than whatever `gh pr create` says about authentication forty seconds
			// later, by which time a branch has already been pushed.
			// GH_TOKEN ONLY. Accepting GITHUB_TOKEN here would contradict the very
			// sentence this error prints: a pull request opened with it runs no
			// checks, so the run would report a successful upgrade and hand back a PR
			// nothing had verified — the silent success this verb exists to prevent,
			// arrived at through its own guard.
			if os.Getenv("GH_TOKEN") == "" {
				hint := "It needs Contents: write, Pull requests: write and Workflows: write. Full detail " +
					"is in the TEMPLATE repo at docs/secrets.md, which is not delivered into an instance"
				if os.Getenv("GITHUB_TOKEN") != "" {
					hint = "GITHUB_TOKEN is set and is deliberately NOT accepted: a pull request opened with it " +
						"runs none of the checks that make an upgrade reviewable. Set GH_TOKEN from the " +
						"LLZ_AUTOMATION_TOKEN secret instead"
				}
				return fmt.Errorf("no GH_TOKEN in the environment: the automated template upgrade needs the "+
					"LLZ_AUTOMATION_TOKEN repository secret to push a branch and open its pull request. %s", hint)
			}
			after, err := gitOut("rev-parse", "HEAD")
			if err != nil {
				return fmt.Errorf("read HEAD: %w", err)
			}
			// THE PIN, AND ONLY THE PIN — no `git describe`. An instance repo carries
			// no llz tags but may well carry the ADOPTER's, and with fetch-depth: 0
			// one of those would win and name the branch after their release.
			version := pinnedVersion()
			if version == "" {
				return fmt.Errorf("could not read the template pin from .copier-answers.yml after the upgrade: " +
					"llz_version and _commit are both empty.\n" +
					"    refusing to continue: the pull-request title and the rejected-version guard both key " +
					"off the pin, and a placeholder would make every release look like the same one")
			}
			s := State{
				BeforeSHA: strings.TrimSpace(beforeSHA),
				AfterSHA:  strings.TrimSpace(after),
				Version:   strings.TrimSpace(version),
				Forced:    force,
			}

			// Asked only when there is something to propose. Two questions, both
			// about pull requests: is one already awaiting review, and was THIS
			// version already rejected.
			// Asked even when forced, so the notice can say what was overridden rather
			// than the run looking like there was nothing in its way.
			if s.AfterSHA != s.BeforeSHA {
				// A QUERY FAILURE IS FATAL ONLY WHEN THE ANSWER IS LOAD-BEARING. On an
				// unforced run it decides whether to propose, so refusing to guess is
				// right. On a --force run both guards are overridden and the answer only
				// decorates an annotation — failing there throws away an upgrade an
				// operator explicitly asked for, after `llz upgrade --commit` has already
				// done the work, over a question whose answer changes nothing.
				guardQuery := func(err error) error {
					if !force {
						return err
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "::warning title=Guard state unknown::%v\n"+
						"    --force was given, so this does not change what happens: proposing anyway.\n", err)
					return nil
				}
				open, oerr := upgradePRs("open", branchStem)
				if oerr != nil {
					if err := guardQuery(oerr); err != nil {
						return err
					}
				}
				s.OpenUpgradePR = len(open) > 0

				// Scoped to THIS version's stem, because the question is about the
				// version rather than about upgrades in general.
				closed, cerr := upgradePRs("closed", VersionStem(s.Version))
				if cerr != nil {
					if err := guardQuery(cerr); err != nil {
						return err
					}
				}
				// MERGED WINS OVER CLOSED, so the whole page is read rather than stopping
				// at the first closed row.
				//
				// `--state closed` returns both, and they mean opposite things: CLOSED is
				// a reviewer rejecting the upgrade, MERGED is that version having SHIPPED.
				// A version can carry both — a first attempt closed, a later one (say a
				// --force run) merged — and then the version is in the tree. Breaking on
				// the first CLOSED row refused every later legitimate re-commit at that
				// pin, which is what a `managed` file restored from drift is, forever and
				// at exit 0. That is the same permanent-stop class the comment above the
				// state check claims to have closed.
				rejected, shipped := false, false
				for _, pr := range closed {
					if !ProposesVersion(pr.Head, s.Version) {
						// ProposesVersion rather than the stem prefix the query was scoped by:
						// the stem for v1.2.3 also prefixes every v1.2.3-rc1 branch, so one
						// closed pre-release would block the GA release of the same number
						// forever, green and at exit 0.
						continue
					}
					switch pr.State {
					case "CLOSED":
						rejected = true
					case "MERGED":
						shipped = true
					}
				}
				s.RejectedThisVersion = rejected && !shipped
			}

			d := Decide(s)
			d.Report(cmd.ErrOrStderr(), s)

			// THE SUMMARY IS WRITTEN AFTER THE WORK, NOT AFTER THE DECISION. It used
			// to go out here, so a failed push or a refused `gh pr create` left
			// "opened a pull request: `true`" and a branch name standing in the run
			// summary for a branch that does not exist — the run reads as a
			// successful upgrade and the operator goes looking for a PR nobody can
			// find. Decide() says what SHOULD happen; only the calls below know what
			// DID.
			summarise := func() {
				if err := ghaout.Append("GITHUB_STEP_SUMMARY", d.Summary(s)); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "::warning::could not write the step summary: %v\n", err)
				}
			}
			if !d.OpenPR {
				summarise()
				return nil
			}
			if base == "" {
				base = os.Getenv("GITHUB_REF_NAME")
			}
			if base == "" {
				return fmt.Errorf("no base branch: pass --base or set GITHUB_REF_NAME")
			}
			if err := pushBranch(d.Branch); err != nil {
				return err
			}
			if err := createPR("chore(template): upgrade to "+s.Version, base, d.Branch); err != nil {
				return err
			}
			d.Opened(cmd.ErrOrStderr(), s)
			summarise()
			return nil
		},
	}
	c.Flags().StringVar(&beforeSHA, "before", "", "HEAD as recorded before `llz upgrade` ran (required)")
	c.Flags().StringVar(&base, "base", "", "base branch for the pull request (default: $GITHUB_REF_NAME)")
	c.Flags().BoolVar(&force, "force", false,
		"propose even if an upgrade pull request is already open or this version was previously rejected — for an operator-dispatched run that named a version explicitly")
	return c
}

// pinnedVersion is the pin the upgrade just wrote, via the package that already
// owns that fallback chain (llz_version, then _commit, then the stamp).
//
// IT RETURNS "" RATHER THAN "unknown", AND THE CALLER REFUSES ON IT. The first cut
// had a hand-rolled reader that degraded to the literal "unknown", which is a
// version-INDEPENDENT branch name — and one run on that name wedges the workflow
// permanently: every later month computes the same branch, finds the pull request
// it opened, reports "already has a pull request" and exits 0. Upgrades stop
// arriving and the job stays green.
//
// That is the identical wedge this file rejects `git describe` for a few lines
// up: any branch name that does not move with the release turns the
// already-open check from a safety into a permanent stop.
var pinnedVersion = func() string { return strings.TrimSpace(answers.PinnedTemplateRef()) }
