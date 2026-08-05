package main

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

// withStubbedInfraEnvs pins the environment listing so a test exercising the
// SECRET probes is not decided by the (real, networked) listing call.
func withStubbedInfraEnvs(t *testing.T) {
	t.Helper()
	orig := instanceInfraEnvs
	t.Cleanup(func() { instanceInfraEnvs = orig })
	instanceInfraEnvs = func(_, env string) ([]string, bool) { return []string{"infra-" + env}, true }
}

// tfEncryptionAlphabet is the character class the tf-encryption-env action
// enforces before interpolating the passphrase into an HCL string. Duplicated
// here on purpose: if the generator ever drifts outside it, CI would reject the
// value at the FIRST terraform init of a brand-new instance — exactly the failure
// this whole file exists to remove — and a local test is the only thing that
// catches that before an adopter does.
var tfEncryptionAlphabet = regexp.MustCompile(`^[A-Za-z0-9+/=_-]+$`)

func TestGeneratedPassphraseSatisfiesTheActionsGuards(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		got, err := generateStatePassphrase()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !tfEncryptionAlphabet.MatchString(got) {
			t.Fatalf("passphrase %q leaves the alphabet tf-encryption-env accepts", got)
		}
		// pbkdf2's floor. The action checks this itself and fails init below it.
		if len(got) < 16 {
			t.Fatalf("passphrase %q is %d chars, under pbkdf2's 16-char minimum", got, len(got))
		}
		if seen[got] {
			t.Fatalf("generated a duplicate passphrase on iteration %d — not random", i)
		}
		seen[got] = true
	}
}

func TestGHSecretPresentOnlyTrustsADefiniteAnswer(t *testing.T) {
	cases := []struct {
		name             string
		err              error
		present, answerd bool
	}{
		{"exists", nil, true, true},
		{"404 is the only answer meaning absent", errors.New("gh: Not Found (HTTP 404)"), false, true},
		{"401 says nothing", errors.New("gh: Bad credentials (HTTP 401)"), false, false},
		{"5xx says nothing", errors.New("gh: Server Error (HTTP 502)"), false, false},
		{"offline says nothing", errors.New("dial tcp: no such host"), false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, c.err })
			present, answered := ghSecretPresent("repos/o/r/actions/secrets/" + statePassphraseSecret)
			if present != c.present || answered != c.answerd {
				t.Errorf("got (present=%v, answered=%v), want (%v, %v)", present, answered, c.present, c.answerd)
			}
		})
	}
}

// Both scopes, because either placement is legal and GitHub resolves the
// env-scoped copy FIRST inside an `environment: infra-<env>` job. Asking only the
// repo scope returned a definite 404 for an instance holding it on infra-<env>,
// so llz minted a SECOND passphrase and printed a banner claiming it protected
// state it could not decrypt.
func TestStatePassphraseExistsChecksBothScopes(t *testing.T) {
	// Pin the environment set so these subtests exercise the secret probing alone;
	// enumeration itself is covered by TestStatePassphraseExistsSeesAPeerDeploymentsCopy.
	origEnvs := instanceInfraEnvs
	t.Cleanup(func() { instanceInfraEnvs = origEnvs })
	instanceInfraEnvs = func(_, env string) ([]string, bool) { return []string{"infra-" + env}, true }

	// Which api paths were asked, so the env scope cannot silently stop being
	// consulted.
	t.Run("asks the repo scope and the env scope", func(t *testing.T) {
		var asked []string
		withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
			asked = append(asked, args[1])
			return nil, errors.New("gh: Not Found (HTTP 404)")
		})
		present, answered := statePassphraseExists("o/r", "lab")
		if present || !answered {
			t.Fatalf("two definite 404s mean definitely absent, got (%v, %v)", present, answered)
		}
		if len(asked) != 2 {
			t.Fatalf("expected both scopes queried, got %v", asked)
		}
		if !strings.Contains(asked[0], "/actions/secrets/") {
			t.Errorf("first lookup is not the repo scope: %s", asked[0])
		}
		if !strings.Contains(asked[1], "/environments/infra-lab/secrets/") {
			t.Errorf("second lookup is not the infra-lab env scope: %s", asked[1])
		}
	})

	t.Run("env-scoped copy counts as present", func(t *testing.T) {
		withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
			if strings.Contains(args[1], "/environments/") {
				return []byte(""), nil // it lives here
			}
			return nil, errors.New("gh: Not Found (HTTP 404)")
		})
		present, answered := statePassphraseExists("o/r", "lab")
		if !present || !answered {
			t.Fatalf("an env-scoped passphrase must count as present, got (%v, %v)", present, answered)
		}
	})

	t.Run("repo-scoped copy short-circuits", func(t *testing.T) {
		withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
			if strings.Contains(args[1], "/actions/secrets/") {
				return []byte(""), nil
			}
			t.Error("kept looking after a definite hit")
			return nil, nil
		})
		if present, answered := statePassphraseExists("o/r", "lab"); !present || !answered {
			t.Fatalf("got (%v, %v), want present", present, answered)
		}
	})

	// A 403 on the env scope cannot rule out a passphrase living there, so the
	// whole question is unanswered — not "absent".
	t.Run("an indefinite answer on either scope means unknown", func(t *testing.T) {
		withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
			if strings.Contains(args[1], "/environments/") {
				return nil, errors.New("gh: Forbidden (HTTP 403)")
			}
			return nil, errors.New("gh: Not Found (HTTP 404)")
		})
		if present, answered := statePassphraseExists("o/r", "lab"); present || answered {
			t.Fatalf("got (%v, %v), want unanswered", present, answered)
		}
	})
}

// The reachable form of the clobber bug: the early-return in runTokens only fires
// when NOTHING is missing, so an instance holding the passphrase on infra-primary
// still reaches ensureStatePassphrase while provisioning a second deployment —
// which is exactly the HA flow the quickstart teaches.
func TestEnsureStatePassphraseHonoursAnEnvScopedCopy(t *testing.T) {
	withStubbedInfraEnvs(t)
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		if strings.Contains(args[1], "/environments/") {
			return []byte(""), nil
		}
		return nil, errors.New("gh: Not Found (HTTP 404)")
	})
	p, err := planStatePassphrase("o/r", "lab", map[string]string{})
	if err != nil {
		t.Fatalf("an env-scoped passphrase is present, not absent: %v", err)
	}
	if p.generate {
		t.Fatal("would mint a second passphrase alongside the env-scoped one")
	}
	if p.push {
		t.Fatal("would push a repo-level copy that shadows nothing and confuses rotation")
	}
}

// The one that matters. ghAPI/ghSecretNames fold every failure into "not
// configured", which for this secret would mean generate-and-clobber: overwriting
// a live passphrase makes every existing state file permanently unreadable. An
// unanswerable lookup must stop the run, not proceed on an assumption.
func TestPlanStatePassphraseRefusesToGuessWhenGitHubCannotAnswer(t *testing.T) {
	withStubbedInfraEnvs(t)
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return nil, errors.New("gh: Bad credentials (HTTP 401)")
	})

	_, err := planStatePassphrase("o/r", "lab", map[string]string{})

	if err == nil {
		t.Fatal("an unknown answer must stop the run, not mint a passphrase")
	}
	for _, want := range []string{"permanently unreadable", "gh auth status", "Refusing to act", "repo\n  admin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// The plan is what decides both "mint?" and "push?", and it must be reachable
// before the wizard prompts for anything — a refusal after the interactive
// section discards three pasted PATs and orphans a freshly created OBJ key.
func TestPlanStatePassphrase(t *testing.T) {
	withStubbedInfraEnvs(t)
	absent := func(string, ...string) ([]byte, error) { return nil, errors.New("gh: Not Found (HTTP 404)") }
	presentAnywhere := func(string, ...string) ([]byte, error) { return []byte(""), nil }

	t.Run("nothing anywhere: mint it and push it", func(t *testing.T) {
		withExecOutput(t, absent)
		p, err := planStatePassphrase("o/r", "lab", map[string]string{})
		if err != nil || !p.generate || !p.push {
			t.Fatalf("got (%+v, %v), want generate+push", p, err)
		}
	})

	// A previous run minted it and the push did not land. The repo has none, so
	// there is nothing to clobber — publish the copy we hold.
	t.Run("cached but repo has none: push, do not re-mint", func(t *testing.T) {
		withExecOutput(t, absent)
		p, err := planStatePassphrase("o/r", "lab", map[string]string{statePassphraseSecret: "cached"})
		if err != nil || p.generate || !p.push {
			t.Fatalf("got (%+v, %v), want push without generate", p, err)
		}
	})

	// THE CLOBBER GUARD. pushToRepo re-pushes every secret in the map, so a stale
	// cache (rotation moved the repo on; TF_STATE_ENCRYPTION_PASSPHRASE_OLD is
	// already deleted) would push the pre-rotation value back and leave every
	// state file unreadable with no fallback.
	t.Run("repo already has one: never push a cached value over it", func(t *testing.T) {
		withExecOutput(t, presentAnywhere)
		p, err := planStatePassphrase("o/r", "lab", map[string]string{statePassphraseSecret: "stale-local-copy"})
		if err != nil {
			t.Fatalf("present is not an error: %v", err)
		}
		if p.generate {
			t.Error("re-generated over an existing passphrase — this strands all state")
		}
		if p.push {
			t.Error("would push a possibly-stale cached value over the live one")
		}
	})

	t.Run("repo has one and nothing cached: do nothing", func(t *testing.T) {
		withExecOutput(t, presentAnywhere)
		p, err := planStatePassphrase("o/r", "lab", map[string]string{})
		if err != nil || p.generate || p.push {
			t.Fatalf("got (%+v, %v), want a no-op plan", p, err)
		}
	})
}

func TestEnsureStatePassphraseGeneratesWhenThePlanSaysSo(t *testing.T) {
	secrets := map[string]string{}
	plan := statePassphrasePlan{generate: true, push: true}

	var err error
	out := captureStderr(t, func() { err = ensureStatePassphrase(globalOpts{yes: true}, plan, "o/r", secrets) })
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := secrets[statePassphraseSecret]
	if got == "" {
		t.Fatal("no passphrase was added to the push set")
	}
	if !tfEncryptionAlphabet.MatchString(got) {
		t.Errorf("generated %q, outside the accepted alphabet", got)
	}
	// Shown once, with the consequence stated where the operator can still act —
	// and saying plainly that the local cache is not escrow.
	if !strings.Contains(out, got) {
		t.Error("the passphrase must be printed: this is the only time it is on screen")
	}
	for _, want := range []string{"COPY IT OFFLINE", "permanently unreadable", "Neither is escrow"} {
		if !strings.Contains(out, want) {
			t.Errorf("escrow banner missing %q, got:\n%s", want, out)
		}
	}
}

func TestEnsureStatePassphraseMintsNothingWhenThePlanSaysNot(t *testing.T) {
	secrets := map[string]string{}
	if err := ensureStatePassphrase(globalOpts{yes: true}, statePassphrasePlan{}, "o/r", secrets); err != nil {
		t.Fatalf("a no-op plan must not fail: %v", err)
	}
	if _, minted := secrets[statePassphraseSecret]; minted {
		t.Fatal("minted a passphrase the plan did not ask for")
	}
}

func TestEnsureStatePassphraseMintsNothingWithoutYes(t *testing.T) {
	// `llz tokens` without --yes prints a plan and creates nothing. A passphrase
	// minted here would be shown once and then discarded when the operator re-runs
	// with --yes and got a DIFFERENT one — while the first may already have been
	// escrowed as if it were real.
	for _, g := range []globalOpts{{}, {dryRun: true}} {
		secrets := map[string]string{}
		plan := statePassphrasePlan{generate: true, push: true}
		if err := ensureStatePassphrase(g, plan, "o/r", secrets); err != nil {
			t.Fatalf("planning must not fail: %v", err)
		}
		if _, minted := secrets[statePassphraseSecret]; minted {
			t.Errorf("minted a passphrase with %+v — nothing may be created without --yes", g)
		}
	}
}

func TestStatePassphraseIsPushedRepoLevel(t *testing.T) {
	// Repo-level, unlike every other instance secret: one instance has ONE
	// passphrase (a single TF_STATE_ENCRYPTION_KEY_NAME, rotated across every root
	// of every deployment), and GitHub resolves a repo-level secret inside an
	// infra-<env> job. An env-scoped copy would let a second deployment be
	// provisioned with a different passphrase.
	if secretIsEnvScoped(statePassphraseSecret) {
		t.Error("the state-encryption passphrase must be repo-level")
	}
	// Everything else is unchanged — this generalization must not move any
	// existing secret out of infra-<env>.
	for _, n := range []string{
		"LINODE_API_TOKEN", "TF_STATE_ACCESS_KEY", "TF_STATE_SECRET_KEY",
		"OPENBAO_SECRETS_WRITE_TOKEN", "APL_VALUES_REPO_TOKEN", "LINODE_DNS_TOKEN",
		"GHCR_READ_TOKEN",
	} {
		if !secretIsEnvScoped(n) {
			t.Errorf("%s must stay env-scoped (infra-<env>)", n)
		}
	}
	// An unknown name keeps the old default rather than silently going repo-level.
	if !secretIsEnvScoped("SOMETHING_NOT_IN_THE_TABLE") {
		t.Error("unknown secrets must default to env-scoped")
	}
}

func TestStatePassphraseIsRequiredForReadiness(t *testing.T) {
	// The gap this closes: the secret every Terraform root needs was absent from
	// the table, so `llz doctor` reported a green instance whose first build could
	// not init.
	var found bool
	for _, r := range e2eRequirements(false) {
		if r.Name != statePassphraseSecret {
			continue
		}
		found = true
		if !r.Required {
			t.Error("terraform-init exits 1 without it — it is not optional")
		}
		if !r.Secret {
			t.Error("it is secret material, not a variable")
		}
	}
	if !found {
		t.Fatalf("%s missing from e2eRequirements — doctor cannot report it", statePassphraseSecret)
	}
}

// ── the last-word push guard ─────────────────────────────────────────────────

// The reachable clobber the plan alone could not stop: `llz secrets gather`
// re-prompts every catalog entry on every run and its own text says
// `openssl rand -base64 32`, so an operator adding one missing token can paste a
// NEW passphrase, and `llz secrets push` would write it over the live one.
func TestDropStatePassphraseIfLiveRefusesToOverwriteALiveValue(t *testing.T) {
	withStubbedInfraEnvs(t)
	withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte(""), nil }) // present
	secrets := map[string]string{statePassphraseSecret: "freshly-pasted", "OTHER": "keep"}

	var err error
	out := captureStderr(t, func() { err = dropStatePassphraseIfLive("o/r", "lab", secrets, false) })
	if err != nil {
		t.Fatalf("present is a no-op, not an error: %v", err)
	}
	if _, still := secrets[statePassphraseSecret]; still {
		t.Fatal("left the passphrase in the push set — this overwrites live state encryption")
	}
	if secrets["OTHER"] != "keep" {
		t.Error("dropped an unrelated secret")
	}
	for _, want := range []string{"already exists", "NOT overwriting", "secret-rotation.yml"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing %q, got:\n%s", want, out)
		}
	}
}

// When the repo acquires a passphrase between the early plan and the push (a peer
// deployment provisioned concurrently), the operator has just been shown an escrow
// banner for a value that is now going nowhere. Saying so is the difference
// between a confusing no-op and an escrowed string they believe is live.
func TestDropStatePassphraseIfLiveSaysWhenAMintedValueIsDiscarded(t *testing.T) {
	withStubbedInfraEnvs(t)
	withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte(""), nil })
	secrets := map[string]string{statePassphraseSecret: "just-minted"}
	out := captureStderr(t, func() {
		if err := dropStatePassphraseIfLive("o/r", "lab", secrets, true); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	if !strings.Contains(out, "was NOT pushed") {
		t.Errorf("a discarded freshly-minted passphrase must be called out, got:\n%s", out)
	}
}

func TestDropStatePassphraseIfLiveKeepsItWhenTheRepoHasNone(t *testing.T) {
	withStubbedInfraEnvs(t)
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return nil, errors.New("gh: Not Found (HTTP 404)")
	})
	secrets := map[string]string{statePassphraseSecret: "ours"}
	if err := dropStatePassphraseIfLive("o/r", "lab", secrets, true); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if secrets[statePassphraseSecret] != "ours" {
		t.Fatal("dropped the passphrase the repo definitively lacks — nothing would ever be pushed")
	}
}

func TestDropStatePassphraseIfLiveRefusesOnAnUnansweredLookup(t *testing.T) {
	withStubbedInfraEnvs(t)
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return nil, errors.New("gh: Bad credentials (HTTP 401)")
	})
	secrets := map[string]string{statePassphraseSecret: "ours"}
	err := dropStatePassphraseIfLive("o/r", "lab", secrets, false)
	if err == nil {
		t.Fatal("must not push on an unanswered lookup — it could be overwriting a live value")
	}
	if !strings.Contains(err.Error(), "permanently unreadable") {
		t.Errorf("error should state the stake: %v", err)
	}
}

func TestDropStatePassphraseIfLiveIgnoresAnAbsentEntry(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		t.Error("asked GitHub about a secret that is not in the push set")
		return nil, nil
	})
	if err := dropStatePassphraseIfLive("o/r", "lab", map[string]string{"OTHER": "x"}, false); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

// A passphrase scoped to a DIFFERENT deployment's environment must count. An
// instance whose first deployment holds it on infra-primary would otherwise look
// empty while provisioning `dr`, and the repo-level copy llz then minted would
// shadow nothing while breaking primary's environment-less plan job.
func TestStatePassphraseExistsSeesAPeerDeploymentsCopy(t *testing.T) {
	origEnvs := instanceInfraEnvs
	t.Cleanup(func() { instanceInfraEnvs = origEnvs })
	instanceInfraEnvs = func(_, env string) ([]string, bool) { return []string{"infra-" + env, "infra-primary"}, true }

	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		if strings.Contains(args[1], "/environments/infra-primary/") {
			return []byte(""), nil
		}
		return nil, errors.New("gh: Not Found (HTTP 404)")
	})

	present, answered := statePassphraseExists("o/r", "dr")
	if !present || !answered {
		t.Fatalf("got (%v, %v) — a peer deployment's passphrase must count as present", present, answered)
	}
}

// An unreadable environments LISTING is not evidence that no peer deployment
// holds the passphrase. Folding it into "only infra-<env> exists" turned an
// indefinite answer into a definite absent — the same class the per-scope rule
// above forbids, and the route by which a second passphrase gets minted over a
// live one. A token with Secrets:read but not Environments:read hits exactly this.
func TestStatePassphraseExistsTreatsAnUnreadableListingAsUnknown(t *testing.T) {
	origEnvs := instanceInfraEnvs
	t.Cleanup(func() { instanceInfraEnvs = origEnvs })
	instanceInfraEnvs = func(string, string) ([]string, bool) { return nil, false }

	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return nil, errors.New("gh: Not Found (HTTP 404)") // repo scope: definitely absent
	})

	present, answered := statePassphraseExists("o/r", "dr")
	if present || answered {
		t.Fatalf("got (present=%v, answered=%v), want unanswered — the listing could not be read", present, answered)
	}
}
