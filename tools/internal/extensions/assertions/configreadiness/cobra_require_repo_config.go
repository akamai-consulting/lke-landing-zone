package configreadiness

// cobra_require_repo_config.go implements `llz ci require-repo-config` — the
// REPO-LEVEL half of the readiness check, asked at PR time by the instance's own
// pipeline instead of fifteen minutes into an apply.
//
// THE INCIDENT. v0.0.42 made TF_STATE_ENCRYPTION_PASSPHRASE required and nothing
// in the upgrade path said so. A live adopter took the upgrade, merged it, and
// learned about the new requirement from a failed `Terraform init` on the next
// pull request — against a secret that had never existed on their repo. `llz
// upgrade` now runs the readiness check as an advisory (see the post-upgrade
// doctor), which closes the case where the operator uses `llz upgrade`. This
// verb closes the case where they do not: a `copier update` driven by hand, a
// Renovate-style bot PR, a hand-edited pin. Nothing about being told at upgrade
// time should depend on which command performed the upgrade.
//
// WHY ONLY THE REPO-LEVEL REQUIREMENTS, which is the whole design constraint. A
// delivered CI job runs with GITHUB_TOKEN, which cannot list repository secrets
// (that needs admin) — so presence can only be observed the way Actions itself
// observes it: by mapping the secret into `env:` and looking at whether the value
// arrived. A job with no `environment:` resolves REPO-level secrets only, so an
// infra-<env>-scoped requirement would read as missing on a correctly configured
// instance. Checking those would make this gate cry wolf on every PR, and a gate
// that cries wolf is removed. The env-scoped half is already covered where it can
// be: apply-vpc's pre-flight, inside the environment.
//
// THE LIST IS THE REQUIREMENT TABLE, NOT A COPY OF IT. envreq.E2ERequirements is
// the single source of truth for what an instance needs; this verb filters it
// rather than restating it, so a requirement added there is checked here the day
// it lands. What the workflow still has to name by hand is the `env:` mapping —
// GitHub has no way to splat secrets — and that is exactly the drift
// TestDeliveredJobCoversRepoLevelRequirements pins.

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
	"github.com/spf13/cobra"
)

// RepoLevelRequirements are the requirements a job with no `environment:` can
// honestly observe: required, repo-scoped, and the instance's rather than the
// template harness's.
func RepoLevelRequirements() []envreq.Requirement {
	var out []envreq.Requirement
	for _, r := range envreq.E2ERequirements(false) {
		if r.Required && !r.EnvScope && !r.Template {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RepoLevelRequirementNames is the same set as bare names, for the delivered-YAML
// coverage test.
func RepoLevelRequirementNames() []string {
	reqs := RepoLevelRequirements()
	names := make([]string, 0, len(reqs))
	for _, r := range reqs {
		names = append(names, r.Name)
	}
	return names
}

// MissingRepoConfig returns the requirements whose value did not arrive. Pure —
// lookup is injected — so every verdict is tested without an environment.
func MissingRepoConfig(reqs []envreq.Requirement, getenv func(string) string) []envreq.Requirement {
	var missing []envreq.Requirement
	for _, r := range reqs {
		if strings.TrimSpace(getenv(r.Name)) == "" {
			missing = append(missing, r)
		}
	}
	return missing
}

// ReportRepoConfig writes the ::error:: annotations and returns the failure. The
// annotations are WRITTEN rather than folded into the error for the reason
// require-secret records: GitHub parses an annotation only at the start of a
// line, and a returned error reaches stderr behind main's "llz: " prefix.
func ReportRepoConfig(reqs []envreq.Requirement, getenv func(string) string, out, errOut io.Writer) error {
	// FAIL CLOSED ON VACUITY, like every other gate on this branch. An empty
	// requirement set means the table moved or the filter stopped matching — not
	// that the instance is configured — and reporting success for having checked
	// nothing is the exact shape this whole change set exists to remove.
	if len(reqs) == 0 {
		fmt.Fprintf(errOut, "::error::no repo-level required values resolved — the requirement table moved "+
			"or the filter no longer matches it. This check examined NOTHING; treating that as a pass is "+
			"how a gate goes quiet.\n")
		return fmt.Errorf("no repo-level requirements resolved — refusing to pass having checked nothing")
	}
	missing := MissingRepoConfig(reqs, getenv)
	for _, r := range reqs {
		if !contains(missing, r.Name) {
			fmt.Fprintf(out, "  %s: present.\n", r.Name)
		}
	}
	if len(missing) == 0 {
		fmt.Fprintf(out, "%d required repo-level value(s) present.\n", len(reqs))
		return nil
	}
	for _, r := range missing {
		kind := "variable"
		if r.Secret {
			kind = "secret"
		}
		fmt.Fprintf(errOut, "::error::%s is not set. This repository %s is REQUIRED — %s\n", r.Name, kind, r.How)
	}
	// One remediation line, not one per item: `llz tokens` provisions the whole
	// set, and an operator who reads N copies of the same command reads none.
	fmt.Fprintf(errOut, "::error::Provision them with `llz tokens --env <deployment> --yes`, "+
		"or set them by hand (Settings → Secrets and variables → Actions). "+
		"A release can make a value newly required — this check is why you are hearing it now "+
		"rather than from a failed apply.\n")
	// THE ONE FALSE POSITIVE THIS CHECK CAN PRODUCE, named so nobody has to
	// rediscover it. These values are repo-level BY DESIGN (envreq's table, and
	// `llz tokens` pushes them there) — one instance has one state-encryption
	// passphrase, one state bucket, one image pin. An instance that instead placed
	// them in an infra-<deployment> Environment by hand is configured against that
	// design, and this job — which has no `environment:`, because it cannot have
	// one and still be honest about what it can see — will report them missing
	// while `llz doctor` reports them present, since doctor falls back env->repo.
	// Saying so here is the difference between a two-minute fix and an argument
	// with the tool.
	fmt.Fprintf(errOut, "::error::If a value above IS set but in an infra-<deployment> Environment: these "+
		"five are REPO-level by design, and this job has no `environment:` so it cannot see an env-scoped "+
		"copy. Move it to repo scope (the tokens wizard does this) — `llz doctor` will disagree with this check "+
		"until you do, because it falls back from environment to repo scope and this cannot.\n")
	return fmt.Errorf("%d required repo-level value(s) missing: %s", len(missing), strings.Join(namesOf(missing), ", "))
}

func contains(reqs []envreq.Requirement, name string) bool {
	for _, r := range reqs {
		if r.Name == name {
			return true
		}
	}
	return false
}

func namesOf(reqs []envreq.Requirement) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Name)
	}
	return out
}

// RequireRepoConfigCmd is `llz ci require-repo-config`.
func RequireRepoConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "require-repo-config",
		Short: "fail when a REQUIRED repo-level secret or variable is missing (upgrade-prerequisite gate)",
		Long: "Checks the repo-level half of the readiness requirements — the part a\n" +
			"delivered CI job can observe without admin rights, by reading the values the\n" +
			"calling step mapped into env:. The set comes from the requirement table, so a\n" +
			"newly required value is checked the release it lands, which is the failure this\n" +
			"exists for: v0.0.42 made TF_STATE_ENCRYPTION_PASSPHRASE required and an\n" +
			"adopter first heard of it from a failed `terraform init`.\n\n" +
			"Env-scoped (infra-<deployment>) requirements are deliberately NOT checked here:\n" +
			"a job without an `environment:` cannot see them, so demanding them would fail\n" +
			"every correctly configured instance. apply-vpc's pre-flight covers those.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return ReportRepoConfig(RepoLevelRequirements(), os.Getenv, os.Stdout, os.Stderr)
		},
	}
}
