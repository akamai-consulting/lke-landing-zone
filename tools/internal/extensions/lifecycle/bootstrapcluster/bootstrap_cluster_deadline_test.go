package bootstrapcluster

// Deadline coverage for the managed-bootstrap wait loops.
//
// waitManagedArgoReady and migrateAplValuesToGitHub are both bounded by the
// bootstrapDeps clock seam (`now` vs a deadline, paced by `sleep`). Every test
// here drives one of them to EXPIRY and asserts the error it hands back — the
// branch a CI operator actually reads when the gate gives up, and the one the
// runbooks quote. It was previously untested for a mechanical reason worth
// naming: a `sleep` stub that does not advance the fake clock makes the
// deadline unreachable, so a loop that fails to exit early spins forever and
// the SUITE HANGS instead of failing. Use advancingClock (below) in every deps
// literal that fakes the clock.

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// advancingClock returns a now/sleep pair for bootstrapDeps whose sleep MOVES
// the fake clock forward. Pairing a fake `now` with `sleep: func(time.Duration) {}`
// freezes time: the deadline never arrives and the loop never terminates.
func advancingClock() (func() time.Time, func(time.Duration)) {
	now := time.Unix(0, 0).UTC()
	return func() time.Time { return now }, func(d time.Duration) { now = now.Add(d) }
}

// ── waitManagedArgoReady ─────────────────────────────────────────────────────

// The give-up error is the operator's whole diagnosis on a fresh managed
// cluster: which half is missing (the Application CRD, or argocd-server's
// replicas) decides whether apl_enabled is still converging or is wedged.
func TestWaitManagedArgoReady_DeadlineErrorIsDiagnostic(t *testing.T) {
	now, sleep := advancingClock()
	start := now()
	var polls int
	d := bootstrapDeps{
		kubectl: func(...string) (string, bool) { polls++; return "", false },
		now:     now,
		sleep:   sleep,
	}
	err := waitManagedArgoReady(d)
	if err == nil {
		t.Fatal("ArgoCD never became ready — want the budget error, got nil (the bridge would then apply against a cluster with no Application CRD and hard-fail)")
	}
	for _, want := range []string{
		"not ready within " + managedArgoReadyBudget.String(),
		"Application CRD present=false",
		`argocd-server availableReplicas=""`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("deadline error missing %q: %v", want, err)
		}
	}
	if elapsed := now().Sub(start); elapsed < managedArgoReadyBudget {
		t.Errorf("gave up after %s of fake time, before the %s budget", elapsed, managedArgoReadyBudget)
	}
	// Two reads (CRD + deployment) per tick, for the whole budget.
	if want := 2 * int(managedArgoReadyBudget/managedArgoReadyInterval); polls < want {
		t.Errorf("polled %d times over the budget, want at least %d — the loop is not actually re-reading the cluster", polls, want)
	}
}

// The four halves of the readiness condition, each on its own: none of these
// shapes is ready, so each must run out the budget rather than admit the bridge.
func TestWaitManagedArgoReady_NotReadyShapesRunOutTheBudget(t *testing.T) {
	cases := []struct {
		name    string
		crdOK   bool
		avail   string
		availOK bool
	}{
		{"Application CRD not registered yet", false, "1", true},
		{"argocd-server unreadable", true, "", false},
		{"availableReplicas not reported", true, "", true},
		{"availableReplicas is 0", true, "0", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now, sleep := advancingClock()
			d := bootstrapDeps{
				kubectl: func(args ...string) (string, bool) {
					if strings.Contains(strings.Join(args, " "), "deploy argocd-server") {
						return c.avail, c.availOK
					}
					return "applications.argoproj.io", c.crdOK
				},
				now:   now,
				sleep: sleep,
			}
			if err := waitManagedArgoReady(d); err == nil {
				t.Fatalf("%s is NOT ready — waitManagedArgoReady returned nil and would let the bridge apply", c.name)
			}
		})
	}
}

// The ready shape returns on the FIRST poll: no sleep, no clock movement. If
// this ever starts consuming the budget the gate has stopped recognising a
// healthy managed ArgoCD and every bootstrap pays 15 minutes before failing.
func TestWaitManagedArgoReady_ReturnsOnTheFirstReadyPoll(t *testing.T) {
	now, sleep := advancingClock()
	start := now()
	var polls int
	d := bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			polls++
			if strings.Contains(strings.Join(args, " "), "deploy argocd-server") {
				return "1\n", true
			}
			return "applications.argoproj.io", true
		},
		now:   now,
		sleep: sleep,
	}
	if err := waitManagedArgoReady(d); err != nil {
		t.Fatalf("Application CRD registered + 1 available argocd-server replica IS ready: %v", err)
	}
	if polls != 2 {
		t.Errorf("made %d kubectl reads, want exactly 2 (one CRD + one deployment)", polls)
	}
	if now() != start {
		t.Errorf("a ready cluster must not sleep; clock moved %s", now().Sub(start))
	}
}

// ── migrateAplValuesToGitHub ─────────────────────────────────────────────────

func migrateFixture() aplGitConfig {
	return aplGitConfig{
		repoURL:  "http://git-server.git-server.svc.cluster.local/otomi/values.git",
		branch:   "main",
		username: "otomi-admin",
		password: "gitea-pw",
	}
}

// A Job that never reports succeeded/failed (apl-core still initializing its
// values repo, or the pod unschedulable) must hit the budget and say so —
// configureManagedApl's caller only warns, so this string is the sole record
// that the BYO-Git repoint never happened.
func TestMigrateAplValuesToGitHub_DeadlineError(t *testing.T) {
	now, sleep := advancingClock()
	start := now()
	var jobDeletes int
	d := bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			if strings.Contains(strings.Join(args, " "), "delete job") {
				jobDeletes++
			}
			return "", true // neither .status.succeeded nor .status.failed set
		},
		apply: func(string, string, bool) (string, bool) { return "", true },
		now:   now,
		sleep: sleep,
	}
	err := migrateAplValuesToGitHub(d, migrateFixture(),
		"https://github.com/acme/instance.git", "apl-primary", []string{"harbor"}, "s3cr3t-pat")
	if err == nil {
		t.Fatal("the migration Job never completed — want the budget error, got nil (the caller would then repoint apl-core at an EMPTY branch and the operator's `git reset --hard` would wipe its values tree)")
	}
	if want := "did not complete within " + aplMigrateBudget.String(); !strings.Contains(err.Error(), want) {
		t.Errorf("deadline error = %v, want it to mention %q", err, want)
	}
	// The loop is attempt-counted (attempts := aplMigrateBudget / aplMigrateTick)
	// and sleeps AFTER each check, so the final attempt does not sleep: the clock
	// advances (attempts-1) ticks, not the whole budget. Derived from the
	// constants rather than hardcoded, so a budget or tick change moves this with
	// it instead of breaking it — an earlier version asserted the full budget and
	// broke when the deadline loop became an attempt loop upstream.
	wantAdvance := time.Duration(int(aplMigrateBudget/aplMigrateTick)-1) * aplMigrateTick
	if elapsed := now().Sub(start); elapsed < wantAdvance {
		t.Errorf("gave up after %s of fake time, want at least %s (%d attempts x %s, the last without a sleep)",
			elapsed, wantAdvance, int(aplMigrateBudget/aplMigrateTick), aplMigrateTick)
	}
	// Once up front (a prior run would make the Job template immutable-conflicted)
	// and once on the way out via the deferred cleanup.
	if jobDeletes != 2 {
		t.Errorf("deleted the Job %d times, want 2 (pre-clean + deferred cleanup)", jobDeletes)
	}
}

// A Job that reports .status.failed short-circuits the wait with its (redacted)
// logs — the credential-bearing clone/push URLs must never reach the CI log.
func TestMigrateAplValuesToGitHub_FailedJobSurfacesRedactedLogs(t *testing.T) {
	now, sleep := advancingClock()
	d := bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			line := strings.Join(args, " ")
			switch {
			case strings.Contains(line, "status.failed"):
				return "1", true
			case strings.Contains(line, "logs"):
				return "fatal: could not read from https://x-access-token:s3cr3t-pat@github.com/acme/instance.git (password gitea-pw)", true
			}
			return "", true
		},
		apply: func(string, string, bool) (string, bool) { return "", true },
		now:   now,
		sleep: sleep,
	}
	err := migrateAplValuesToGitHub(d, migrateFixture(),
		"https://github.com/acme/instance.git", "apl-primary", []string{"harbor"}, "s3cr3t-pat")
	if err == nil {
		t.Fatal("a failed migration Job must abort the wait, not run out the budget")
	}
	if !strings.Contains(err.Error(), "migration Job failed") {
		t.Errorf("error = %v, want it to report the Job failure", err)
	}
	for _, secret := range []string{"s3cr3t-pat", "gitea-pw"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("migration Job logs leaked %q into the error: %v", secret, err)
		}
	}
}

// ── readAplGitConfig (the migration's input) ─────────────────────────────────

// aplGitSecretDeps stubs the apl-secrets/apl-git-config read the way kubectl
// answers it: base64 per `-o jsonpath={.data.<key>}`. failKey makes that ONE
// read exit non-zero (an RBAC denial or an apiserver blip on a single call).
func aplGitSecretDeps(data map[string]string, failKey string) bootstrapDeps {
	return bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			line := strings.Join(args, " ")
			for k, v := range data {
				if !strings.Contains(line, "jsonpath={.data."+k+"}") {
					continue
				}
				if k == failKey {
					return "Error from server (Forbidden): secrets \"apl-git-config\" is forbidden", false
				}
				if v == "" {
					return "", true
				}
				return base64.StdEncoding.EncodeToString([]byte(v)), true
			}
			return "", true
		},
	}
}

func fullAplGitSecret() map[string]string {
	return map[string]string{
		"repoUrl":  "http://git-server.git-server.svc.cluster.local/otomi/values.git",
		"branch":   "otomi-values", // deliberately NOT "main": the default must not mask a dropped read
		"username": "otomi-admin",
		"password": "gitea-pw",
	}
}

func TestReadAplGitConfig_DecodesEveryField(t *testing.T) {
	c, err := readAplGitConfig(aplGitSecretDeps(fullAplGitSecret(), ""))
	if err != nil {
		t.Fatalf("readAplGitConfig: %v", err)
	}
	for _, f := range []struct{ name, got, want string }{
		{"repoURL", c.repoURL, "http://git-server.git-server.svc.cluster.local/otomi/values.git"},
		{"branch", c.branch, "otomi-values"},
		{"username", c.username, "otomi-admin"},
		{"password", c.password, "gitea-pw"},
	} {
		if f.got != f.want {
			t.Errorf("%s = %q, want %q — the migration Job clones with a silently EMPTY credential", f.name, f.got, f.want)
		}
	}
}

// Every one of the four reads is load-bearing: a dropped error hands the
// migration a half-built config, which clones/pushes anonymously and then runs
// out its 13-minute budget instead of failing here with the real reason.
func TestReadAplGitConfig_PropagatesEveryFailedRead(t *testing.T) {
	for _, key := range []string{"repoUrl", "branch", "username", "password"} {
		c, err := readAplGitConfig(aplGitSecretDeps(fullAplGitSecret(), key))
		if err == nil {
			t.Errorf("the %s read failed but readAplGitConfig returned %+v with no error", key, c)
			continue
		}
		if want := "read " + aplGitSecretNS + "/" + aplGitSecretName; !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want it to name the unreadable Secret (%q)", key, err, want)
		}
	}
}

// An apl-core that has not yet written repoUrl is "not BYO-Git-configured", not
// "migrate from nowhere": pushing a tree built from an empty source would wipe
// the values repo the operator hard-resets to.
func TestReadAplGitConfig_EmptyRepoURLIsAnError(t *testing.T) {
	empty := fullAplGitSecret()
	empty["repoUrl"] = ""
	if c, err := readAplGitConfig(aplGitSecretDeps(empty, "")); err == nil {
		t.Fatalf("no repoUrl must be an error, got config %+v", c)
	} else if !strings.Contains(err.Error(), "no repoUrl") {
		t.Errorf("error = %v, want it to say the Secret has no repoUrl", err)
	}

	// An empty branch is the only field that defaults (apl-core's own default).
	noBranch := fullAplGitSecret()
	noBranch["branch"] = ""
	c, err := readAplGitConfig(aplGitSecretDeps(noBranch, ""))
	if err != nil {
		t.Fatalf("a configured repoUrl with no branch is valid: %v", err)
	}
	if c.branch != "main" {
		t.Errorf("branch = %q, want the main default (the migration Job would otherwise clone branch \"\")", c.branch)
	}
}

// configureManagedApl must ABORT on an unreadable/unconfigured BYO-Git Secret.
// Continuing would apply the migration Job against an unknown source repo and
// burn the full 13-minute budget before failing with a useless message.
func TestConfigureManagedApl_AbortsWhenTheBYOGitSecretIsUnusable(t *testing.T) {
	empty := fullAplGitSecret()
	empty["repoUrl"] = ""
	now, sleep := advancingClock()
	var applied []string
	d := aplGitSecretDeps(empty, "")
	d.apply = func(y, _ string, _ bool) (string, bool) { applied = append(applied, y); return "", true }
	d.now, d.sleep = now, sleep

	o := bootstrapClusterOpts{env: "primary", instanceRepo: "acme/instance", instanceRepoToken: "s3cr3t-pat"}
	err := configureManagedApl(o, d)
	if err == nil {
		t.Fatal("configureManagedApl must propagate the readAplGitConfig failure")
	}
	if !strings.Contains(err.Error(), "no repoUrl") {
		t.Errorf("error = %v, want the readAplGitConfig reason", err)
	}
	if len(applied) != 0 {
		t.Errorf("configureManagedApl applied %d manifest(s) after failing to read the source repo coordinates:\n%s",
			len(applied), strings.Join(applied, "\n---\n"))
	}
}
