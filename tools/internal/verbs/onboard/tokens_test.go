package onboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/statepassphrase"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
)

// TestNothingToProvisionNote gates the exit line of the most-run path through
// `llz tokens`: the idempotent re-run, where the command provisions nothing and
// what it PRINTS is the entire deliverable.
//
// It is a real gate rather than a spelling check because the branch it guards
// returns before pushToRepo. An operator who rotates a credential by editing
// .llz/secrets.env lands here with everything envreq.Satisfied (presence, not
// value) and their edit unpushed — so a message that does not name
// `llz secrets push` sends them away believing the repo has what they just
// changed. Each assertion below corresponds to a way the old string actually
// went wrong: no route out, no statement of what the check does not cover, and
// an env name hardcoded to the template's own lane.
func TestNothingToProvisionNote(t *testing.T) {
	got := NothingToProvisionNote("prod", "acme/instance")

	// The route out. This is the whole point of the message: `llz secrets push`
	// is a sibling verb, and nothing else in a `llz tokens` run mentions it.
	if !strings.Contains(got, "llz secrets push prod --yes") {
		t.Errorf("re-run message never names the command that pushes a hand-edited credential:\n%s", got)
	}
	// The advice is only safe from a checkout of the repo this run targeted:
	// PushSecrets goes through ghcli.SecretSetArgv, which passes no --repo, so it
	// pushes wherever `gh` infers the repo from the working directory. An --admin
	// or --repo run targets one repo and stands in another, and the difference
	// between the two is a full set of credentials in the wrong place.
	if !strings.Contains(got, "acme/instance") {
		t.Errorf("re-run message recommends a CWD-relative push without naming the repo it must run against:\n%s", got)
	}
	// PRESENCE-not-VALUE has to be said, not implied. "Everything is set" is true
	// and still leaves the operator with the wrong conclusion.
	if !strings.Contains(got, ".llz/secrets.env") {
		t.Errorf("re-run message claims everything is set without saying what is NOT checked:\n%s", got)
	}
	// vars.env rides the same early return — pushToRepo is skipped for both, so a
	// hand-edited variable is dropped exactly like a hand-edited secret.
	if !strings.Contains(got, ".llz/vars.env") {
		t.Errorf("re-run message covers only secrets, but this branch drops hand-edited variables too:\n%s", got)
	}
	// The deployment the operator named, not `e2e`. The old constant reported on
	// the template's throwaway lane for every adopter who has no such env — the
	// misdirection DefaultDoctorEnv() removes one verb over.
	if !strings.Contains(got, "infra-prod") {
		t.Errorf("re-run message does not name the deployment it reported on:\n%s", got)
	}
	if strings.Contains(got, "e2e") {
		t.Errorf("re-run message names `e2e` for an --env prod run:\n%s", got)
	}
}

// TestInvalidCredentialsError gates the rule `llz tokens` and `llz doctor` now
// share: a present-but-dead credential is a non-zero exit, not a green run.
//
// The coupling is the point. Both commands probe validity, and they used to
// restate the verdict separately — doctor erroring, tokens printing a dim
// warning and returning nil, which TokensCmd answers with "Next steps: llz
// build". Calling the real constructor from a test is what keeps a later edit to
// one command from re-opening that gap silently.
func TestInvalidCredentialsError(t *testing.T) {
	if err := InvalidCredentialsError(0, "acme/instance"); err != nil {
		t.Errorf("a clean probe must not be an error, got %v", err)
	}
	// Defensive: ProbeTokenValidities cannot return a negative, but a nil here is
	// the difference between a refusal and a green run.
	if err := InvalidCredentialsError(-1, "acme/instance"); err != nil {
		t.Errorf("negative count must not be an error, got %v", err)
	}
	err := InvalidCredentialsError(2, "acme/instance")
	if err == nil {
		t.Fatal("2 dead credentials reported no error — this is the exit-0-on-a-revoked-token bug")
	}
	// The operator has to learn WHICH repo and WHAT to do; "invalid" alone sends
	// them back to the readiness table with no verb.
	for _, want := range []string{"2", "acme/instance", "rotate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

// TestPushSecretsRefusesUnverifiablePassphrase gates the fail-CLOSED half of the
// passphrase guard: with no way to resolve the repo, a push carrying a
// passphrase must stop, and must say so in terms of the passphrase.
//
// WHAT IT PINS IS THE REFUSAL, NOT MERELY THAT ONE HAPPENS. Before the fix this
// input already failed — branchpolicy.Lock resolves the repo from the same
// answers file and refused first, so nothing was ever pushed — but with "cannot
// lock branch policy: instance repo unknown", which names neither the passphrase
// nor the hazard. The assertion on the message is therefore the whole gate: drop
// the guard and this test fails on the wrong-diagnostic arm, which is exactly the
// regression, and it keeps failing if Lock later gains a fallback resolver and
// stops refusing at all.
func TestPushSecretsRefusesUnverifiablePassphrase(t *testing.T) {
	writeEnv := func(t *testing.T, body string) {
		t.Helper()
		if err := os.MkdirAll(".llz", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(".llz", "secrets.env"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Carrying a passphrase with no way to check it against the repo: refuse.
	t.Run("refuses", func(t *testing.T) {
		chdirTempDir(t)
		writeEnv(t, statepassphrase.SecretName+"=locally-edited\nLINODE_API_TOKEN=x\n")
		// --yes deliberately: the guard must fire before the mutating path, not be
		// masked by the plan-only early return that a no-yes run takes anyway.
		err := PushSecrets(Opts{Yes: true}, "prod")
		if err == nil {
			t.Fatal("pushed a passphrase it could not check against the live one")
		}
		// The diagnostic has to be about the passphrase. "cannot lock branch policy"
		// is technically a stop, but it sends the operator to fix the wrong thing.
		if !strings.Contains(err.Error(), statepassphrase.SecretName) {
			t.Errorf("refusal does not name the secret it is protecting: %v", err)
		}
	})

	// A push carrying no passphrase has nothing to clobber; a missing answers file
	// must not block it, or the guard becomes an outage of its own.
	t.Run("lets an unrelated push through", func(t *testing.T) {
		chdirTempDir(t)
		writeEnv(t, "LINODE_API_TOKEN=x\n")
		// No --yes: prints the plan and returns before any cloud mutation, so this
		// exercises the guard without needing gh or the network.
		if err := PushSecrets(Opts{}, "prod"); err != nil {
			t.Errorf("a push with no passphrase must not be blocked by an unresolvable repo: %v", err)
		}
	})
}

func TestRegionFromCluster(t *testing.T) {
	cases := map[string]string{
		"us-ord-1":     "us-ord",
		"us-ord-10":    "us-ord",
		"us-iad-18":    "us-iad",
		"eu-central-1": "eu-central",
		"single":       "single", // no hyphen → returned unchanged
	}
	for in, want := range cases {
		if got := regionFromCluster(in); got != want {
			t.Errorf("regionFromCluster(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoSlug(t *testing.T) {
	cases := map[string]string{
		"akamai-consulting/lke-landing-zone-example": "lke-landing-zone-example",
		"Org/My-Repo": "my-repo",
		"bare":        "bare",
	}
	for in, want := range cases {
		if got := repoSlug(in); got != want {
			t.Errorf("repoSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveInstanceRepo(t *testing.T) {
	// Explicit flag always wins.
	if r, err := answers.ResolveInstanceRepo("owner/explicit", false); err != nil || r != "owner/explicit" {
		t.Fatalf("flag: got (%q,%v), want owner/explicit", r, err)
	}
	// Admin with no flag and no answers file falls back to the example repo.
	if r, err := answers.ResolveInstanceRepo("", true); err != nil || r != templateid.DefaultOrg+"/"+templateid.Name+"-example" {
		t.Fatalf("admin default: got (%q,%v)", r, err)
	}
	// Non-admin with no flag and (presumably) no .copier-answers.yml here errors.
	if _, err := answers.ResolveInstanceRepo("", false); err == nil {
		t.Errorf("expected error when no repo can be determined")
	}
}
