package bootstrapcluster

// c08_review_test.go — the gates for the C08 findings of the 2026-08-13 review.
//
// One class again: the bootstrap acting on an answer it never got. A kubeconfig
// it could not read, a Secret it could not see, a Job it had not finished
// watching, and a remote it could not reach.

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ── ResolveKubeconfig ────────────────────────────────────────────────────────

// TestResolveKubeconfigRefusesAnUnusableExplicitPath. An explicit --kubeconfig
// that was missing or zero-byte used to fall through silently to KUBECONFIG_RAW
// and then to the ambient ~/.kube/config, so an operator who named the wrong file
// — or a workflow whose fetch step produced an empty one — ran the whole bootstrap
// against WHATEVER CLUSTER THE MACHINE WAS ALREADY POINTED AT, and it reported
// success. This command deletes and recreates StorageClasses.
func TestResolveKubeconfigRefusesAnUnusableExplicitPath(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A resolvable ambient kubeconfig, so the OLD behaviour would have succeeded
	// here — silently, against this file, which is the whole defect.
	ambient := filepath.Join(dir, "ambient")
	if err := os.WriteFile(ambient, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", ambient)
	t.Setenv("KUBECONFIG_RAW", "apiVersion: v1\n")

	for name, path := range map[string]string{
		"missing":   filepath.Join(dir, "nope"),
		"zero-byte": empty,
	} {
		t.Run(name, func(t *testing.T) {
			got, _, err := ResolveKubeconfig(path)
			if err == nil {
				t.Fatalf("an explicit --kubeconfig that cannot be used must not fall back — resolved %q, "+
					"which is a destructive bootstrap pointed at a cluster the operator did not name", got)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("the error must name the path it was given, got: %v", err)
			}
		})
	}
}

// TestResolveKubeconfigStillHonoursAUsableExplicitPath pins the exclusion.
func TestResolveKubeconfigStillHonoursAUsableExplicitPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(p, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, err := ResolveKubeconfig(p)
	if err != nil || got != p {
		t.Fatalf("ResolveKubeconfig(%q) = (%q, %v), want it honoured", p, got, err)
	}
}

// ── waitAplGitConfig ─────────────────────────────────────────────────────────

// TestWaitAplGitConfigRetriesAnAbsentSecret is the fresh-managed-cluster case the
// ten-minute wait exists for, and the case it could not reach.
//
// waitAplGitConfig returns immediately on any error that is NOT
// errAplNotBYOGitReady. An ABSENT Secret — which is exactly what a fresh managed
// cluster has, because Linode installs apl-core after this runs — produced a plain
// error, so the wait gave up on attempt 1 and failed the bootstrap terminally. It
// only ever worked for the "Secret exists, repoUrl empty" case, which is the only
// case its tests exercised.
func TestWaitAplGitConfigRetriesAnAbsentSecret(t *testing.T) {
	const url = "https://gitea.example/repo.git"
	enc := base64.StdEncoding.EncodeToString([]byte(url))

	var polls int
	d := bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			line := strings.Join(args, " ")
			if !strings.Contains(line, "get secret") {
				return "", true
			}
			if strings.Contains(line, "repoUrl") {
				polls++
			}
			if polls <= 2 {
				// What a fresh managed cluster actually returns: Linode installs
				// apl-core AFTER this runs, so the Secret does not exist yet.
				return `Error from server (NotFound): secrets "apl-git-config" not found`, false
			}
			if strings.Contains(line, "repoUrl") {
				return enc, true
			}
			return "", true
		},
		now:   time.Now,
		sleep: func(time.Duration) {},
	}

	cfg, err := waitAplGitConfig(d)
	if err != nil {
		t.Fatalf("an absent apl-secrets/apl-git-config is the state this ten-minute wait EXISTS for, and "+
			"it gave up instead: %v", err)
	}
	if cfg.repoURL != url {
		t.Errorf("repoURL = %q, want %q once apl-core publishes", cfg.repoURL, url)
	}
	if polls < 3 {
		t.Errorf("polled %d time(s); the wait must keep asking while the Secret is absent", polls)
	}
}

// TestWaitAplGitConfigStillFailsOnADecodeError pins the exclusion: a Secret whose
// contents are broken will not fix itself, and burning the full ten minutes on it
// is worse than saying so immediately.
func TestWaitAplGitConfigStillFailsOnADecodeError(t *testing.T) {
	d := bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			if strings.Contains(strings.Join(args, " "), "get secret") {
				return "!!! not base64 !!!", true
			}
			return "", true
		},
		now:   time.Now,
		sleep: func(time.Duration) { t.Fatal("a decode failure must not be retried") },
	}
	if _, err := waitAplGitConfig(d); err == nil {
		t.Fatal("a Secret that does not decode is terminal, not transient")
	}
}

// ── the migration Job's branch probe, executed ───────────────────────────────

// TestAplBranchStateSeparatesAbsentFromUnreachable RUNS the shell the Job runs.
//
// `git ls-remote --heads URL BRANCH | grep -q refs/heads/BRANCH` collapses two
// questions into one: ls-remote's EXIT STATUS answers "could I ask?", its OUTPUT
// answers "is it there?". An ls-remote that failed on auth, DNS, a proxy or a rate
// limit prints nothing, grep finds nothing, and the script concludes the branch is
// GONE — which makes this Job force-push apl-core's abandoned in-cluster tree over
// a healthy apl-<env> branch and revert every values change made since the switch.
//
// And the grep was a SUBSTRING test: `main` matches refs/heads/maintenance,
// `apl-primary` matches refs/heads/apl-primary-backup.
//
// Asserting on the rendered TEXT would be asserting on a copy. This executes
// AplBranchStateScript itself, against a fake `git`.
func TestAplBranchStateSeparatesAbsentFromUnreachable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh")
	}
	for name, tc := range map[string]struct {
		gitExit  int
		gitOut   string
		branch   string
		wantCode int
	}{
		"present":              {0, "abc123\trefs/heads/apl-primary\n", "apl-primary", 0},
		"absent":               {0, "", "apl-primary", 1},
		"unreachable":          {128, "fatal: Authentication failed", "apl-primary", 2},
		"rate limited":         {128, "fatal: unable to access: The requested URL returned error: 429", "apl-primary", 2},
		"similar name only":    {0, "abc123\trefs/heads/apl-primary-backup\n", "apl-primary", 1},
		"prefix name only":     {0, "abc123\trefs/heads/maintenance\n", "main", 1},
		"present among others": {0, "a\trefs/heads/maintenance\nb\trefs/heads/main\n", "main", 0},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			git := filepath.Join(dir, "git")
			script := "#!/bin/sh\nprintf '%s' " + shQuote(tc.gitOut) + "\nexit " + itoa(tc.gitExit) + "\n"
			if err := os.WriteFile(git, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "-c", AplBranchStateScript+"\nbranch_state \"$1\" \"$2\"\n", "sh",
				"https://example.invalid/repo.git", tc.branch)
			// PREPEND, not replace: branch_state also uses awk and printf, which the
			// Job's alpine/git image provides via busybox. Replacing PATH outright
			// tested a shell that had no awk, which is not the shell the Job runs.
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, _ := cmd.CombinedOutput()
			code := cmd.ProcessState.ExitCode()
			if code != tc.wantCode {
				t.Errorf("branch_state = %d, want %d (0 present, 1 absent, 2 could-not-ask)\n%s",
					code, tc.wantCode, out)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// TestAplMigrateJobRefusesToRebuildOnAnUnreachableRemote asserts the Job WIRES
// that distinction rather than dropping it on the floor — the exit-2 arm has to
// stop the rebuild, not fall through to it.
func TestAplMigrateJobRefusesToRebuildOnAnUnreachableRemote(t *testing.T) {
	y := aplMigrateJobManifest("main", "apl-e2e", []string{"harbor"}, true)
	if !strings.Contains(y, "branch_state()") {
		t.Fatal("the Job no longer defines branch_state — the ls-remote guard is back to conflating " +
			"'could not ask' with 'not there'")
	}
	for _, want := range []string{
		`branch_state "$DST_URL" "$DST_BRANCH"`,
		`"$dst" -eq 2`,
		"refusing to rebuild",
	} {
		if !strings.Contains(y, want) {
			t.Errorf("the destination guard must handle could-not-ask; missing %q", want)
		}
	}
}

// ── the migration Job's retry ────────────────────────────────────────────────

// TestMigrateWaitDoesNotAbortWhileTheJobIsStillRetrying.
//
// The Job sets `backoffLimit: 1`, so Kubernetes runs the pod TWICE — and the
// moment the first pod fails, `.status.failed` becomes 1 while the retry is still
// pending. The wait read that as terminal and returned an error; the caller's
// deferred del() then DELETED the Job, killing the retry Kubernetes was about to
// run. The one extra attempt the backoffLimit exists to provide could never
// happen.
//
// Here the Job reports failed=1 with no Failed condition (mid-retry) and then
// succeeds, which is the sequence a transient clone failure produces.
func TestMigrateWaitDoesNotAbortWhileTheJobIsStillRetrying(t *testing.T) {
	var polls int
	d := bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			line := strings.Join(args, " ")
			switch {
			case strings.Contains(line, "status.succeeded"):
				polls++
				if polls >= 3 {
					return "1", true // the retry pod completed
				}
				return "", true
			case strings.Contains(line, "conditions"):
				return "", true // NOT terminally failed: the retry is still to come
			case strings.Contains(line, "status.failed"):
				return "1", true // one pod HAS failed — the old, wrong terminal signal
			case strings.Contains(line, "logs"):
				return "transient clone failure", true
			}
			return "", true
		},
		apply: func(string, string, bool) (string, bool) { return "", true },
		now:   time.Now,
		sleep: func(time.Duration) {},
	}
	err := migrateAplValuesToGitHub(d, migrateFixture(),
		"https://github.com/acme/instance.git", "apl-primary", []string{"harbor"}, "tok")
	if err != nil {
		t.Fatalf("a first-pod failure with the retry still pending is not terminal — aborting here "+
			"deletes the Job and kills the retry backoffLimit: 1 exists to provide: %v", err)
	}
}

// ── from the code review of this PR ─────────────────────────────────────────

// TestTheInterpolatedScriptKeepsItsOwnLine. indentScript emits no trailing
// newline, and the `%[8]s` placeholder shared a source line with the comment
// after it — so the rendered Job ran `}          # RE-BOOTSTRAP REPAIR ONLY. …`
// as one line, making the repair rationale read as a comment on branch_state's
// closing brace. It parses in both sh and YAML, which is exactly why it would
// have survived: the only symptom is a reader being told the wrong thing.
func TestTheInterpolatedScriptKeepsItsOwnLine(t *testing.T) {
	y := aplMigrateJobManifest("main", "apl-e2e", []string{"harbor"}, true)
	if !strings.Contains(y, "}\n          # RE-BOOTSTRAP REPAIR ONLY.") {
		t.Errorf("the interpolated branch_state block must end its own line before the next comment;\n"+
			"rendered:\n%s", excerptAround(y, "RE-BOOTSTRAP REPAIR ONLY"))
	}
}

// excerptAround returns a few lines of context around needle, for a failure
// message that shows the reader what actually rendered.
func excerptAround(body, needle string) string {
	lines := strings.Split(body, "\n")
	for i, ln := range lines {
		if strings.Contains(ln, needle) {
			lo, hi := i-2, i+2
			if lo < 0 {
				lo = 0
			}
			if hi > len(lines) {
				hi = len(lines)
			}
			return strings.Join(lines[lo:hi], "\n")
		}
	}
	return "(not found)"
}
