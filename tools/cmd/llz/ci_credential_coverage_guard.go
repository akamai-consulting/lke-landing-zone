package main

// ci_credential_coverage_guard.go implements `llz ci credential-coverage-guard` —
// the static gate on credentials the platform holds but nothing measures.
//
// THE PROBLEM IT SOLVES IS THE SAME ONE plaintext-guard SOLVES, one subsystem
// over: drift, not any single credential. The credential single pane answers "is
// anything overdue for rotation?" from three feeds, and every feed is driven by a
// LITERAL LIST in Go — ghPATTargets, ghSecretTargets, reconcilelanes.CredPaths. A list is only
// as good as the last person who revisited it, and nobody did: ADR 0009 went
// looking for credentials with no expiry, found the state backend, wrote three
// entries, and left OPENBAO_SEAL_KEY — the at-rest key for every other credential
// in the platform — off the pane entirely. Not by decision. By omission.
//
// Adding a `secrets.FOO` to a workflow is a one-line edit that no gate looked at.
// This makes it a reviewed one: every secret an instance workflow consumes must
// be MEASURED by one of the feeds, or REGISTERED here with a reason and an owner.
//
// COVERAGE IS DERIVED, NOT RESTATED. The guard reads ghPATTargets and
// ghSecretTargets directly rather than keeping its own copy of them. A second
// list would drift from the first, and a coverage gate that disagrees with the
// thing it gates is worse than none — it would vouch for credentials nobody
// measures. So the registry below holds ONLY the exemptions, and the way to
// satisfy this guard for a real credential is to measure it.
//
// UNUSED ENTRIES FAIL, for the reason plaintextAllowed's do: a registry that
// keeps entries for secrets no workflow uses stops being reviewable, because the
// next reader cannot tell which lines are load-bearing.
//
// SCOPE. instance-template/.github/workflows — the credential surface an INSTANCE
// runs on, which is what the single pane is about. The template repo's own
// harness workflows are out of scope: they run on a throwaway e2e instance, and
// pulling them in would put maintainer-only credentials on every adopter's
// dashboard as permanent `unknown` rows.
//
// WHAT IT DOES NOT DO. It cannot see a credential that never appears in a
// workflow — one seeded by hand straight into OpenBao, say. That residue belongs
// to reconcilelanes.CredPaths and its own gate (`assert-rotation-health`), and is stated here so
// nobody reads a color.Green run as "every credential is covered".

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/guardkit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/tokeninv"
)

// credExemptKind names WHY a secret needs no write-time or expiry entry of its
// own. It is a closed vocabulary on purpose: "accepted" is not a reason, and a
// free-text field would collect one.
const (
	// credExemptLinodeAccount — a Linode PAT. gatherLinodeTokens enumerates every
	// PAT on the account and classifies its expiry, so these are measured BY VALUE
	// rather than by name; adding them to ghSecretTargets would double-count them
	// under a second `cred` label.
	credExemptLinodeAccount = "linode-account-enumeration"
	// credExemptEphemeral — minted per job by GitHub and expired when the run
	// ends. There is nothing to rotate and no age that outlives the measurement.
	credExemptEphemeral = "ephemeral"
	// credExemptNotACredential — carries no secret material (a robot's NAME, a
	// git revision). Held as a secret for convenience of plumbing, not for
	// confidentiality.
	credExemptNotACredential = "not-a-credential"
	// credExemptRotationWindow — set only DURING a rotation and deleted when it
	// completes. Steady state is absent, so measuring it would report
	// `unconfigured` on every healthy instance — the false-page shape that
	// `expect` exists to avoid, except here the honest answer is not to measure
	// it at all.
	credExemptRotationWindow = "rotation-window"
)

// credCoverageExempt registers every secret an instance workflow uses that is
// deliberately NOT on the credential single pane, keyed by secret name.
//
// The bar is the same as plaintextAllowed's: the reason must say why measuring it
// would be meaningless or misleading, not merely that it is allowed.
var credCoverageExempt = map[string]struct {
	kind, reason string
}{
	"LINODE_API_TOKEN": {
		kind: credExemptLinodeAccount,
		reason: "the broad account PAT. Its expiry IS measured — gatherLinodeTokens lists every " +
			"PAT on the account and classifies each one (no-expiry / expired / over-policy), and " +
			"its ROTATION AGE rides secret/linode/broad-pat in reconcilelanes.CredPaths, class automated. " +
			"Measuring the GitHub secret's write time as well would publish a third series for " +
			"one credential under a third cred label",
	},
	"LINODE_DNS_TOKEN": {
		kind: credExemptLinodeAccount,
		reason: "the Domains-scoped PAT cert-manager's DNS-01 solver uses. Same account, so the " +
			"same enumeration measures its expiry. Optional — DNS-01 certs are opt-in — which is " +
			"the second reason not to give it a write-time entry: it would report `unconfigured` " +
			"on every instance that uses HTTP-01",
	},
	"GITHUB_TOKEN": {
		kind: credExemptEphemeral,
		reason: "the Actions-minted job token. Scoped to one run, expires with it, and is not " +
			"stored anywhere an operator could rotate. There is no age to measure",
	},
	"HARBOR_ROBOT_NAME": {
		kind:   credExemptNotACredential,
		reason: "the robot's ACCOUNT NAME, published beside HARBOR_PASSWORD so a rebuilt or standby cluster can adopt the existing robot. Not secret material; the password half is measured",
	},
	"HARBOR_PULL_ROBOT_NAME": {
		kind:   credExemptNotACredential,
		reason: "as HARBOR_ROBOT_NAME, for the pull-only robot. HARBOR_PULL_PASSWORD is the measured half",
	},
	"APPS_REPO_REVISION": {
		kind: credExemptNotACredential,
		reason: "a git revision (branch/tag/SHA) for the app-delivery repo. Held as a secret only " +
			"because an instance may not want its private repo's branch names in a public " +
			"workflow file — it authorizes nothing",
	},
	"TF_STATE_ENCRYPTION_PASSPHRASE_OLD": {
		kind: credExemptRotationWindow,
		reason: "the PREVIOUS state-encryption passphrase, set ONLY while secret-rotation.yml " +
			"scope=state-passphrase is rolling over and deleted once every root verifies under " +
			"the new key alone (ADR 0009). Absent is the healthy steady state, so a write-time " +
			"entry would fire LLZCredentialUnconfigured on every instance that is not " +
			"mid-rotation. Its rotation is measured on the CURRENT passphrase, which is the " +
			"credential that actually has to be young",
	},
}

// uncommented drops WHOLE-LINE YAML comments before the scan. A `secrets.FOO`
// that appears only in prose is not a usage, and counting it defeats the rule
// this guard leans on hardest: unused registry entries fail, so a dead exemption
// stays reviewable. A comment naming a retired secret would have kept its
// exemption alive forever — the registry rot the staleness check exists to catch,
// re-entering through the door the check was watching.
//
// WHOLE-LINE ONLY, deliberately. Stripping from any `#` would also cut
// `run: echo "# ${{ secrets.FOO }}"`, dropping a REAL usage from the set — and an
// unmeasured credential going unnoticed is the failure this guard exists to
// prevent, so the two error directions are not symmetric. A trailing comment that
// names a secret is rare; a block of prose that does is the realistic rot vector.
func uncommented(body string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// credSecretRef matches a `secrets.NAME` reference in a workflow expression. It
// deliberately does NOT try to read `secrets:` declaration blocks: a secret that
// is declared and never referenced is not consumed by anything, and the point of
// this guard is what an instance actually holds and uses.
//
// `secrets: inherit` cannot match — the dot is required.
var credSecretRef = regexp.MustCompile(`\bsecrets\.([A-Z][A-Z0-9_]*)\b`)

func ciCredentialCoverageGuardCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "credential-coverage-guard",
		Short: "fail when an instance workflow uses a credential nothing measures",
		Long: "Static gate on credential-observability drift. Every `secrets.NAME` an\n" +
			"instance workflow consumes must either be MEASURED — by expiry (ghPATTargets,\n" +
			"or the Linode account enumeration) or by GitHub write time (ghSecretTargets) —\n" +
			"or be REGISTERED in credCoverageExempt with a kind and a reason.\n\n" +
			"Coverage is read from those lists directly rather than restated here, so the\n" +
			"way to satisfy this guard for a real credential is to measure it. Unregistered\n" +
			"secrets fail, and so do registry entries no workflow uses any more.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCICredentialCoverageGuard(root) },
	}
	c.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return c
}

// credCoverage is how one secret is accounted for, for the report line.
type credCoverage struct {
	how    string // expiry | write-time | exempt:<kind>
	detail string
}

// credMeasuredByName maps every secret the inventory measures BY NAME to how.
// Derived from the two target lists — never a copy of them.
func credMeasuredByName() map[string]credCoverage {
	out := map[string]credCoverage{}
	for _, t := range tokeninv.GHPATTargets {
		d := "expiry probed via the token-expiration header"
		if t.Optional {
			d += " (optional — skipped when unset)"
		}
		out[t.Name] = credCoverage{how: "expiry", detail: d}
	}
	for _, t := range tokeninv.GHSecretTargets {
		out[t.Name] = credCoverage{
			how:    "write-time",
			detail: fmt.Sprintf("GitHub secret write time, class %s, expect %s", t.Class, t.Expect),
		}
	}
	return out
}

func runCICredentialCoverageGuard(root string) error {
	dir := guardkit.RepoPath(root, filepath.Join("instance-template", ".github", "workflows"))
	// An instance IS the scaffold's contents, so the same trees sit at the root
	// there. esRepoPath resolves the template layout; this is the instance one.
	if _, err := os.Stat(dir); err != nil {
		dir = guardkit.RepoPath(root, filepath.Join(".github", "workflows"))
	}

	used, examined, err := collectWorkflowSecretRefs(dir)
	if err != nil {
		return err
	}
	// Same rationale as the sibling guards: a guard that read no workflows prints
	// the same color.Green as one that read all of them.
	if err := guardkit.RequireCorpus("credential-coverage-guard", examined, []string{dir}); err != nil {
		return err
	}

	unmeasured, stale := classifyCredentialCoverage(used, os.Stdout)

	failed := false
	for _, name := range unmeasured {
		failed = true
		fmt.Printf("::error::%s is used by an instance workflow and nothing measures it. "+
			"A credential nobody measures cannot be reported overdue, cannot be reported missing, "+
			"and is invisible on the credential single pane — which is how OPENBAO_SEAL_KEY, the "+
			"at-rest key for every other credential in the platform, stayed off it. Either add it "+
			"to ghSecretTargets (write time — works for anything with no expiry, needs no new "+
			"credential or job) or ghPATTargets (expiry), or register it in credCoverageExempt "+
			"(ci_credential_coverage_guard.go) with a kind and a reason saying why measuring it "+
			"would be meaningless. See docs/adr/0009-unmeasurable-credential-coverage.md.\n", name)
	}

	for _, k := range stale {
		failed = true
		fmt.Printf("::error::credCoverageExempt entry %q matches no `secrets.%s` in any instance "+
			"workflow. The secret was retired or renamed — delete the entry. A registry that keeps "+
			"dead entries stops being reviewable, because a reader cannot tell which lines are "+
			"load-bearing.\n", k, k)
	}

	if failed {
		return fmt.Errorf("credential-coverage-guard: unmeasured credential(s) and/or stale registry entries")
	}
	fmt.Printf("credential-coverage-guard: %d secret(s) across %d workflow(s) — every one measured or registered.\n",
		len(used), examined)
	return nil
}

// classifyCredentialCoverage splits the used secrets into the two failure sets —
// consumed but unmeasured, and registered but unused — writing the accounted-for
// ones to w as it goes. Pure apart from w, so a test can assert on ONE of the two
// verdicts: a caller checking only the error string cannot tell "this credential
// is unmeasured" from "the other registry entries went unused", and every table
// test with a two-file corpus would have collided with the second.
func classifyCredentialCoverage(used []string, w io.Writer) (unmeasured, stale []string) {
	measured := credMeasuredByName()
	seenExempt := map[string]bool{}
	for _, name := range used {
		if c, ok := measured[name]; ok {
			fmt.Fprintf(w, "  ok: %-36s %s — %s\n", name, c.how, c.detail)
			continue
		}
		if e, ok := credCoverageExempt[name]; ok {
			seenExempt[name] = true
			fmt.Fprintf(w, "  ok: %-36s exempt:%s — %s\n", name, e.kind, e.reason)
			continue
		}
		unmeasured = append(unmeasured, name)
	}
	for k := range credCoverageExempt {
		if !seenExempt[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	return unmeasured, stale
}

// collectWorkflowSecretRefs returns the sorted, de-duplicated set of secret names
// referenced across the workflows in dir, plus the number of files read.
func collectWorkflowSecretRefs(dir string) ([]string, int, error) {
	set := map[string]bool{}
	examined := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // a missing tree is requireCorpus's problem, not a walk error
		}
		if info.IsDir() {
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(path)); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		examined++
		for _, m := range credSecretRef.FindAllStringSubmatch(uncommented(string(b)), -1) {
			set[m[1]] = true
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, examined, nil
}
