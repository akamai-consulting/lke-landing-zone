package onboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/statepassphrase"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
)

// TestNothingToProvisionNote gates the exit line of the most-run path through
// `llz tokens`: the idempotent re-run, where the command provisions nothing and
// what it PRINTS is the entire deliverable.
//
// It is a real gate rather than a spelling check because the branch it guards
// returns before pushToRepo. An operator who rotates a credential by editing
// .llz/secrets.env lands here with everything envreq.Satisfied (presence, not
// value) and their edit unpushed.
//
// WHAT MOVED, AND WHY THE OLD ASSERTIONS ARE NOT SIMPLY DELETED. This message
// used to name `llz secrets push` unconditionally, because it could not tell the
// in-sync operator from the about-to-fail one. It now can — envreq.DetectDrift
// dates each pushed secret against the local file — so the route out moved into
// envreq.Drift.Note, which prints it WITH the names of the secrets that need it.
// The guarantee is unchanged and the assertion for it now lives in
// TestDriftNote_NamesTheRouteOut; what is asserted here is the other half, which
// the old unconditional text could not deliver: that the in-sync case makes a
// STRONGER claim rather than repeating a caveat that applies to everyone.
func TestNothingToProvisionNote(t *testing.T) {
	// ── In sync: the comparison ran and found nothing behind ──────────────────
	inSync := NothingToProvisionNote("prod", "acme/instance", envreq.Drift{LocalMod: time.Now()})
	// The deployment the operator named, not `e2e`. The old constant reported on
	// the template's throwaway lane for every adopter who has no such env — the
	// misdirection DefaultDoctorEnv() removes one verb over.
	if !strings.Contains(inSync, "infra-prod") {
		t.Errorf("re-run message does not name the deployment it reported on:\n%s", inSync)
	}
	if strings.Contains(inSync, "e2e") {
		t.Errorf("re-run message names `e2e` for an --env prod run:\n%s", inSync)
	}
	// PRESENCE-not-VALUE still has to be said. Being in sync by timestamp is not
	// the same as being equal by value, and the message must not imply it is.
	// Whitespace-normalised: the caveat wraps across lines, and a test that
	// breaks on rewrapping tests the line width, not the claim.
	if !strings.Contains(strings.Join(strings.Fields(inSync), " "), "never reads a secret back") {
		t.Errorf("in-sync message overclaims — it must still say values are not compared:\n%s", inSync)
	}
	// ...but it must NOT tell someone with nothing to push to go and push. That
	// is the noise that made the old text unreadable.
	if strings.Contains(inSync, "llz secrets push") {
		t.Errorf("in-sync message sends the operator to push anyway — this is the wallpaper the drift check exists to remove:\n%s", inSync)
	}

	// ── Undecidable: no local file to date anything against ───────────────────
	// The one case that still needs the old blanket caveat, because nothing was
	// compared. Here the route out MUST be named: an operator holding credentials
	// somewhere else has no other prompt to push them.
	noFile := NothingToProvisionNote("prod", "acme/instance", envreq.Drift{})
	if !strings.Contains(noFile, "llz secrets push prod --yes") {
		t.Errorf("with nothing to compare, the message must still name the push command:\n%s", noFile)
	}
	if !strings.Contains(noFile, "acme/instance") {
		t.Errorf("recommends a CWD-relative push without naming the repo it must run against:\n%s", noFile)
	}

	// ── Drift found: the head line must not bury the warning that follows ─────
	behind := NothingToProvisionNote("prod", "acme/instance", envreq.Drift{
		Behind:   []envreq.DriftEntry{{Name: "OPENBAO_SECRETS_WRITE_TOKEN", PushedAt: time.Now()}},
		LocalMod: time.Now(),
	})
	if strings.Contains(behind, "never reads a secret back") {
		t.Errorf("head line repeats the generic caveat ahead of the specific warning, diluting it:\n%s", behind)
	}
}

// TestDriftNote_NamesTheRouteOut is the assertion that moved out of
// TestNothingToProvisionNote: the operator who edited .llz/secrets.env and did
// not push must be told the command that pushes it. `llz secrets push` is a
// sibling verb and nothing else in a `llz tokens` run mentions it.
func TestDriftNote_NamesTheRouteOut(t *testing.T) {
	d := envreq.Drift{
		Behind:   []envreq.DriftEntry{{Name: "OPENBAO_SECRETS_WRITE_TOKEN", PushedAt: time.Unix(0, 0)}},
		LocalMod: time.Unix(1, 0),
	}
	got := d.Note("prod", "acme/instance")
	for _, want := range []string{"llz secrets push prod --yes", "acme/instance", "OPENBAO_SECRETS_WRITE_TOKEN"} {
		if !strings.Contains(got, want) {
			t.Errorf("drift note does not mention %q:\n%s", want, got)
		}
	}
}

// TestDriftNote_AheadDoesNotSayPush is the direction a naive implementation gets
// backwards, and the reason `llz tokens` does not simply auto-sync.
//
// `llz ci rotate-broad-pat` publishes a fresh LINODE_API_TOKEN to every
// infra-<deployment> and REVOKES the one it replaced; llz-secret-rotation.yml
// does the same for the TF_STATE_* pair. For those three, a pushed copy newer
// than the local file means the LOCAL one is stale and already dead. Telling an
// operator to push it back is telling them to break the pipeline.
func TestDriftNote_AheadDoesNotSayPush(t *testing.T) {
	d := envreq.Drift{
		Ahead:    []envreq.DriftEntry{{Name: "LINODE_API_TOKEN", PushedAt: time.Unix(2, 0)}},
		LocalMod: time.Unix(1, 0),
	}
	got := d.Note("prod", "acme/instance")
	if strings.Contains(got, "llz secrets push") {
		t.Errorf("a CI-rotated secret must never be recommended for re-push — that overwrites a live credential with a revoked one:\n%s", got)
	}
	if !strings.Contains(got, "LINODE_API_TOKEN") {
		t.Errorf("note does not name the rotated secret:\n%s", got)
	}
	// The operator has to learn which copy is the stale one, or they will "fix"
	// it in the wrong direction by hand.
	if !strings.Contains(got, "stale") {
		t.Errorf("note does not tell the operator their own copy is the stale one:\n%s", got)
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
