package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMiniInstance lays down the smallest tree buildPreflight recognizes: a
// landingzone.yaml (so InstancePresent is true) and one environments/<env>.yaml.
func writeMiniInstance(t *testing.T, dir string, envs ...string) {
	t.Helper()
	lz := "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: mini\nspec:\n  instance:\n    repo: my-org/mini\n"
	if err := os.WriteFile(filepath.Join(dir, "landingzone.yaml"), []byte(lz), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "environments"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range envs {
		body := fmt.Sprintf("apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: ClusterDefinition\nmetadata:\n  name: %s\nspec:\n  cluster:\n    region: us-sea\n", e)
		if err := os.WriteFile(filepath.Join(dir, "environments", e+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "terraform-iac-bootstrap", "cluster"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// stubGitHub makes every gh API call return the given JSON bodies, keyed by a
// substring of the request path. A path with no entry 404s (returns an error).
func stubGitHub(t *testing.T, bodies map[string]any) {
	t.Helper()
	orig := ghAPIJSON
	t.Cleanup(func() { ghAPIJSON = orig })
	ghAPIJSON = func(path string, out any) error {
		// Longest match wins: "repos/<r>" is a prefix of "repos/<r>/contents/…",
		// and map iteration order would otherwise pick between them at random.
		best, found := "", false
		for frag := range bodies {
			if strings.Contains(path, frag) && len(frag) > len(best) {
				best, found = frag, true
			}
		}
		if !found {
			return fmt.Errorf("gh api %s: HTTP 404", path)
		}
		b, _ := json.Marshal(bodies[best])
		return json.Unmarshal(b, out)
	}
}

func TestBuildPreflightUnknownDeployment(t *testing.T) {
	// `llz build labb` — a name no spec declares. validateEnvName can't catch it
	// (deployment names are free-form), and GitHub accepts any `region` input.
	dir := t.TempDir()
	writeMiniInstance(t, dir, "lab")
	chdir(t, dir)
	stubGitHub(t, nil)

	err := buildPreflight("labb")
	if err == nil {
		t.Fatal("expected a refusal for a deployment that does not exist")
	}
	// "this instance has: lab" — not a bare "lab", which "labb" itself satisfies.
	for _, want := range []string{`no deployment "labb"`, "this instance has: lab)", "llz env add labb"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestBuildPreflightUnpushedDeployment(t *testing.T) {
	// The default first-timer state: `llz env add` committed the spec, the push
	// never happened, so the branch the workflow checks out has no such env.
	dir := t.TempDir()
	writeMiniInstance(t, dir, "lab")
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"), []byte("instance_repo: my-org/mini\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	// Repo resolves and has a default branch, but the file is not on it.
	stubGitHub(t, map[string]any{"repos/my-org/mini": map[string]any{"default_branch": "main"}})
	origLook := execLookPath
	t.Cleanup(func() { execLookPath = origLook })
	execLookPath = func(string) (string, error) { return "/usr/bin/gh", nil }

	err := buildPreflight("lab")
	if err == nil {
		t.Fatal("expected a refusal when the deployment is not on the build branch")
	}
	for _, want := range []string{"not on my-org/mini's main branch", "git push", "environments/lab.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestBuildPreflightPassesWhenPushed(t *testing.T) {
	dir := t.TempDir()
	writeMiniInstance(t, dir, "lab")
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"), []byte("instance_repo: my-org/mini\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	stubGitHub(t, map[string]any{
		"repos/my-org/mini/contents/": map[string]any{"sha": "deadbeef"},
		"repos/my-org/mini":           map[string]any{"default_branch": "main"},
	})
	origLook := execLookPath
	t.Cleanup(func() { execLookPath = origLook })
	execLookPath = func(string) (string, error) { return "/usr/bin/gh", nil }

	// Pushed, but the local file has since been edited (its blob differs from the
	// one on the branch): that is advisory, not a refusal — building an older
	// revision on purpose is legitimate — but it must SAY so, because the build
	// will run the pushed spec and not the edits in front of you.
	var err error
	warn := captureStderr(t, func() { err = buildPreflight("lab") })
	if err != nil {
		t.Fatalf("a pushed deployment must pass: %v", err)
	}
	if !strings.Contains(warn, "differs from your working copy") {
		t.Errorf("a stale pushed spec must be flagged, got: %q", warn)
	}
}

func TestBuildPreflightSilentWithoutASpec(t *testing.T) {
	// Legacy (pre-spec) instances and non-instance directories keep working: the
	// gate can only ever fail a build that was already going to fail.
	chdir(t, t.TempDir())
	if err := buildPreflight("lab"); err != nil {
		t.Fatalf("no spec ⇒ no opinion, got %v", err)
	}
}

func TestBuildPreflightUnreachableGitHubDoesNotBlock(t *testing.T) {
	// The gate's whole licence to exist is that an unknown answer costs nothing.
	// An unauthenticated gh, a 5xx, secondary rate limiting, or a plane with no
	// wifi must NOT be reported as "this deployment was never pushed" — that
	// diagnosis would be wrong AND would block a build that is perfectly fine.
	dir := t.TempDir()
	writeMiniInstance(t, dir, "lab")
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"), []byte("instance_repo: my-org/mini\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	origLook := execLookPath
	t.Cleanup(func() { execLookPath = origLook })
	execLookPath = func(string) (string, error) { return "/usr/bin/gh", nil }

	for _, transient := range []string{
		"dial tcp: lookup api.github.com: no such host",
		"gh: You have exceeded a secondary rate limit (HTTP 403)",
		"gh: Internal Server Error (HTTP 500)",
	} {
		orig := ghAPIJSON
		ghAPIJSON = func(path string, out any) error {
			if strings.Contains(path, "/contents/") {
				return errors.New(transient)
			}
			return json.Unmarshal([]byte(`{"default_branch":"main"}`), out)
		}
		var err error
		warn := captureStderr(t, func() { err = buildPreflight("lab") })
		ghAPIJSON = orig
		if err != nil {
			t.Errorf("%q must not block the dispatch, got: %v", transient, err)
		}
		if !strings.Contains(warn, "could not check") {
			t.Errorf("%q should say the check was skipped, got: %q", transient, warn)
		}
	}
}

func TestGhFileSHASeparatesAbsenceFromIgnorance(t *testing.T) {
	orig := ghAPIJSON
	t.Cleanup(func() { ghAPIJSON = orig })

	// A real 404 is an ANSWER: the file is not on that ref.
	ghAPIJSON = func(string, any) error { return errors.New("gh: Not Found (HTTP 404)") }
	if _, found, ok := ghFileSHA("o/r", "environments/lab.yaml", "main"); !ok || found {
		t.Errorf("404 → found=false ok=true; got found=%v ok=%v", found, ok)
	}
	// Anything else is not an answer at all.
	ghAPIJSON = func(string, any) error { return errors.New("HTTP 503 Service Unavailable") }
	if _, _, ok := ghFileSHA("o/r", "environments/lab.yaml", "main"); ok {
		t.Error("a 503 must not be reported as a usable answer")
	}
}

func TestBuildPreflightChecksTheInstanceSpecToo(t *testing.T) {
	// The apply renders the tfvars from the ASSEMBLED spec, so landingzone.yaml
	// is as load-bearing as the deployment's own file: an unpushed `llz spec set`
	// (acmeEmail, teams, shared networks) would build the old instance-level
	// values with nothing said.
	dir := t.TempDir()
	writeMiniInstance(t, dir, "lab")
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"), []byte("instance_repo: my-org/mini\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	origLook := execLookPath
	t.Cleanup(func() { execLookPath = origLook })
	execLookPath = func(string) (string, error) { return "/usr/bin/gh", nil }

	// The deployment is on the branch; the instance spec is not.
	orig := ghAPIJSON
	t.Cleanup(func() { ghAPIJSON = orig })
	ghAPIJSON = func(path string, out any) error {
		switch {
		case strings.Contains(path, "contents/landingzone.yaml"):
			return errors.New("gh: Not Found (HTTP 404)")
		case strings.Contains(path, "contents/"):
			return json.Unmarshal([]byte(`{"sha":"deadbeef"}`), out)
		default:
			return json.Unmarshal([]byte(`{"default_branch":"main"}`), out)
		}
	}

	err := buildPreflight("lab")
	if err == nil {
		t.Fatal("an unpushed landingzone.yaml must block the dispatch")
	}
	if !strings.Contains(err.Error(), "the instance spec (landingzone.yaml)") {
		t.Errorf("error %q should name the instance spec, not the deployment", err)
	}
}

func TestPublishHintNamesTheBranchYouAreOn(t *testing.T) {
	// `git push` is the RIGHT advice only on the default branch. A dispatch always
	// runs terraform.yml from the default branch, so an operator on a feature
	// branch can push all day and never move the tree the build reads — telling
	// them "git push" would send them round the loop a second time.
	orig := execOutput
	t.Cleanup(func() { execOutput = orig })

	execOutput = func(_ string, _ ...string) ([]byte, error) { return []byte("main\n"), nil }
	if got := publishHint("main"); got != cyan("git push") {
		t.Errorf("on the default branch the hint is a plain push, got %q", got)
	}

	execOutput = func(_ string, _ ...string) ([]byte, error) { return []byte("feat/new-env\n"), nil }
	got := publishHint("main")
	for _, want := range []string{"git push -u origin feat/new-env", "merge it into main", "not from feat/new-env"} {
		if !strings.Contains(got, want) {
			t.Errorf("feature-branch hint %q missing %q", got, want)
		}
	}

	// Detached HEAD / no git: fall back to the plain form rather than inventing a
	// branch name.
	execOutput = func(_ string, _ ...string) ([]byte, error) { return []byte("HEAD\n"), nil }
	if got := publishHint("main"); got != cyan("git push") {
		t.Errorf("detached HEAD should fall back to a plain push, got %q", got)
	}
}

func TestBuildPreflightUnparseableSpecSaysSo(t *testing.T) {
	// A spec that does not parse yields an EMPTY deployment list, and reporting
	// "no deployment lab" would blame the argument for a broken file.
	dir := t.TempDir()
	writeMiniInstance(t, dir, "lab")
	if err := os.WriteFile(filepath.Join(dir, "landingzone.yaml"), []byte("apiVersion: [broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	stubGitHub(t, nil)

	err := buildPreflight("lab")
	if err == nil {
		t.Fatal("a broken spec must not pass the preflight")
	}
	if !strings.Contains(err.Error(), "does not load") {
		t.Errorf("error %q should blame the spec, not the deployment name", err)
	}
}

func TestCmdBuildSkipPreflightBypassesTheCheck(t *testing.T) {
	// The escape hatch has to actually work: a spec that deliberately lives
	// elsewhere (another branch, another checkout) must still be dispatchable.
	dir := t.TempDir()
	writeMiniInstance(t, dir) // no deployments at all — the preflight would refuse
	chdir(t, dir)
	stubGitHub(t, nil)

	if err := cmdBuild([]string{"lab"}, globalOpts{}, false); err == nil {
		t.Fatal("without --skip-preflight an unknown deployment must be refused")
	}
	if err := cmdBuild([]string{"lab"}, globalOpts{}, true); err != nil {
		t.Errorf("--skip-preflight must bypass the check, got %v", err)
	}
}

func TestBuildPreflightStaleCheckoutIsNotAnError(t *testing.T) {
	// A colleague added `prod`, pushed it, and you have not pulled. The build
	// reads the PUSHED tree, so the dispatch works — refusing it on the evidence
	// of your own checkout would block a good build, and the old advice ("run
	// llz env add prod") would have authored a duplicate deployment.
	dir := t.TempDir()
	writeMiniInstance(t, dir, "lab") // local spec knows only "lab"
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"), []byte("instance_repo: my-org/mini\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	origLook := execLookPath
	t.Cleanup(func() { execLookPath = origLook })
	execLookPath = func(string) (string, error) { return "/usr/bin/gh", nil }
	stubGitHub(t, map[string]any{
		"contents/environments/prod.yaml": map[string]any{"sha": "cafe1234"},
		"repos/my-org/mini":               map[string]any{"default_branch": "main"},
	})

	var err error
	warn := captureStderr(t, func() { err = buildPreflight("prod") })
	if err != nil {
		t.Fatalf("a deployment present on the build branch must dispatch: %v", err)
	}
	for _, want := range []string{"not in your checkout", "IS on my-org/mini's main branch", "git pull"} {
		if !strings.Contains(warn, want) {
			t.Errorf("warning %q missing %q", warn, want)
		}
	}
}

func TestBuildPreflightUnknownEverywhereStillRefuses(t *testing.T) {
	// The typo case must survive the stale-checkout allowance: unknown locally AND
	// absent from the branch is still a refusal, and it now says both.
	dir := t.TempDir()
	writeMiniInstance(t, dir, "lab")
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"), []byte("instance_repo: my-org/mini\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	origLook := execLookPath
	t.Cleanup(func() { execLookPath = origLook })
	execLookPath = func(string) (string, error) { return "/usr/bin/gh", nil }
	stubGitHub(t, map[string]any{"repos/my-org/mini": map[string]any{"default_branch": "main"}})

	err := buildPreflight("labb")
	if err == nil {
		t.Fatal("a name that exists nowhere must still be refused")
	}
	for _, want := range []string{`no deployment "labb"`, "none on the branch the build reads", "git pull"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestBuildPreflightMissingRepoNamesTheRealProblem(t *testing.T) {
	// `llz new` without --push leaves an instance whose repo does not exist. The
	// build cannot dispatch to it at all, and "could not resolve a default branch"
	// reads as a transient — say what is actually wrong.
	dir := t.TempDir()
	writeMiniInstance(t, dir, "lab")
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"), []byte("instance_repo: my-org/mini\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	origLook, origOut := execLookPath, execOutput
	t.Cleanup(func() { execLookPath, execOutput = origLook, origOut })
	execLookPath = func(string) (string, error) { return "/usr/bin/gh", nil }
	// repoExists shells out to `gh api repos/...` through execOutput; fail it.
	execOutput = func(string, ...string) ([]byte, error) { return nil, errors.New("gh: Not Found (HTTP 404)") }
	stubGitHub(t, nil) // ghDefaultBranch also 404s

	var err error
	warn := captureStderr(t, func() { err = buildPreflight("lab") })
	if err != nil {
		t.Fatalf("an absent repo must not block the dispatch (gh reports it): %v", err)
	}
	// It must NOT assert the repo is absent — a 404 also covers "your token
	// cannot see it", and "already exists" is a miserable way to find that out.
	if !strings.Contains(warn, "does not exist, or your `gh` login cannot see it") {
		t.Errorf("warning should allow for both, got: %q", warn)
	}
	if !strings.Contains(warn, "gh repo create") {
		t.Errorf("warning should still offer the create command, got: %q", warn)
	}
}

func TestIsNotFoundErrMatchesTheStatusNotAnyStray404(t *testing.T) {
	// gh writes "gh: Not Found (HTTP 404)". Matching a bare "404" (or a bare "Not
	// Found") reads a status out of text that carries none — a port, a request
	// id, a proxy's own words — and a false "answered: absent" here BLOCKS a
	// build, which is the outcome this split exists to prevent.
	for _, real := range []string{"gh: Not Found (HTTP 404)", "exit status 1: gh: Not Found (HTTP 404)"} {
		if !isNotFoundErr(errors.New(real)) {
			t.Errorf("%q is a genuine 404", real)
		}
	}
	for _, notAnswer := range []string{
		"dial tcp 10.0.0.1:4040: connect: connection refused",
		"request id 404abc failed: HTTP 500",
		"gh: Forbidden (HTTP 403)",
		"Not Found in cache; retrying",
	} {
		if isNotFoundErr(errors.New(notAnswer)) {
			t.Errorf("%q is not an answer about the file", notAnswer)
		}
	}
}

func TestGhFileSHAEscapesTheRef(t *testing.T) {
	// A branch name may legally contain '#', which a query string treats as a
	// fragment delimiter: "?ref=feat/#123" asks about the DEFAULT ref instead,
	// answering a different question than the caller asked.
	orig := ghAPIJSON
	t.Cleanup(func() { ghAPIJSON = orig })
	var seen string
	ghAPIJSON = func(path string, out any) error {
		seen = path
		return json.Unmarshal([]byte(`{"sha":"abc"}`), out)
	}
	if _, _, ok := ghFileSHA("o/r", "environments/lab.yaml", "feat/#123 fix"); !ok {
		t.Fatal("expected an answer")
	}
	if strings.Contains(seen, "#") || strings.Contains(seen, " ") {
		t.Errorf("ref was not escaped: %q", seen)
	}
	if !strings.Contains(seen, "ref=feat%2F%23123+fix") && !strings.Contains(seen, "ref=feat%2F%23123%20fix") {
		t.Errorf("unexpected escaping: %q", seen)
	}
}

func TestRepoMissingOnlyOn404(t *testing.T) {
	// tokens.go's repoExists returns false for ANY gh error, which would put
	// "create the repo" in front of an operator whose repo exists and whose token
	// merely cannot see it — advice that fails with "already exists".
	orig := execOutput
	t.Cleanup(func() { execOutput = orig })

	execOutput = func(string, ...string) ([]byte, error) { return nil, errors.New("gh: Not Found (HTTP 404)") }
	if !repoMissing("o/r") {
		t.Error("a 404 means the repo is absent (or invisible)")
	}
	execOutput = func(string, ...string) ([]byte, error) { return nil, errors.New("gh: Bad credentials (HTTP 401)") }
	if repoMissing("o/r") {
		t.Error("a 401 says nothing about whether the repo exists")
	}
	execOutput = func(string, ...string) ([]byte, error) { return []byte("{}"), nil }
	if repoMissing("o/r") {
		t.Error("a success means it exists")
	}
}

// ── unpublished spec/overlay edits ───────────────────────────────────────────
//
// The two-file remote check above catches "committed but never pushed". These
// cover the state it structurally cannot see: edits that were never committed at
// all, which is where the quickstart's own step order lands an operator (`llz env
// add` commits up front, then doctor tells them to fill placeholders, and nothing
// commits THAT).

func TestWarnUnpublishedEditsNamesWhatTheBuildWillMiss(t *testing.T) {
	// The suggested `git add --` is filtered through existingPaths, so the scanned
	// paths have to actually exist for the publish line to appear (an absent
	// pathspec makes `git add` fatal — see the comment there). Lay them down.
	dir := t.TempDir()
	chdir(t, dir)
	for _, d := range []string{"environments", filepath.Join("apl-values", "lab"), filepath.Join("apl-values", "_shared")} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"landingzone.yaml", filepath.Join("environments", "lab.yaml")} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var gotArgs []string
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "git" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		gotArgs = args
		return []byte(" M apl-values/lab/values.yaml\n?? environments/lab.yaml\n"), nil
	})

	out := captureStderr(t, func() { warnUnpublishedEdits(".", "apl-values", "lab") })

	// The operator has to be able to act on this: which files, and the one thing
	// that makes the build see them.
	for _, want := range []string{
		"apl-values/lab/values.yaml",
		"environments/lab.yaml",
		"NOT in it",
		"git push",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing %q, got:\n%s", want, out)
		}
	}

	// Scoped to the spec + this deployment's overlay, never a bare `git status`.
	// A whole-repo scan would flag every unrelated edit in the checkout — noise
	// that trains the operator to skip the one line that matters here.
	if len(gotArgs) < 3 || gotArgs[0] != "status" || gotArgs[1] != "--porcelain" || gotArgs[2] != "--" {
		t.Fatalf("expected a pathspec-scoped `git status --porcelain --`, got %v", gotArgs)
	}
	paths := strings.Join(gotArgs[3:], " ")
	for _, want := range []string{"landingzone.yaml", "environments", filepath.Join("apl-values", "lab")} {
		if !strings.Contains(paths, want) {
			t.Errorf("pathspec %q missing %q", paths, want)
		}
	}
}

func TestWarnUnpublishedEditsSilentWhenClean(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte("\n"), nil })
	if out := captureStderr(t, func() { warnUnpublishedEdits(".", "apl-values", "lab") }); out != "" {
		t.Errorf("a clean tree must say nothing, got:\n%s", out)
	}
}

func TestWarnUnpublishedEditsSilentWithoutGit(t *testing.T) {
	// Outside a git repo, or with no git at all: no evidence of an unpublished
	// edit is not evidence of one. Same degrade-to-quiet rule as the rest of this
	// gate — it can only ever warn about a build that was already going to be wrong.
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return nil, errors.New("fatal: not a git repository")
	})
	if out := captureStderr(t, func() { warnUnpublishedEdits(".", "apl-values", "lab") }); out != "" {
		t.Errorf("an unanswerable question must stay quiet, got:\n%s", out)
	}
}
