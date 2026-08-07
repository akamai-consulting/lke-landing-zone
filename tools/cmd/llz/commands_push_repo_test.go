package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghcli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/templateid"
)

// withGhOwnerKind stubs the instance_repo owner classifier.
func withGhOwnerKind(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	orig := ghcli.OwnerKindFn
	t.Cleanup(func() { ghcli.OwnerKindFn = orig })
	ghcli.OwnerKindFn = fn
}

// withTemplateSourceStatus stubs the --org template-source preflight.
func withTemplateSourceStatus(t *testing.T, fn func(string) (bool, error)) {
	t.Helper()
	orig := templateSourceStatusFn
	t.Cleanup(func() { templateSourceStatusFn = orig })
	templateSourceStatusFn = fn
}

// withInstanceRepoExists stubs the "is the instance repo already there?" probe.
func withInstanceRepoExists(t *testing.T, fn func(string) bool) {
	t.Helper()
	orig := instanceRepoExistsFn
	t.Cleanup(func() { instanceRepoExistsFn = orig })
	instanceRepoExistsFn = fn
}

// scaffoldDir writes a minimal instance checkout (just the copier answers
// pushInstanceRepo reads) and returns its path.
func scaffoldDir(t *testing.T, instanceRepo string) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".copier-answers.yml"), "instance_repo: "+instanceRepo+"\n")
	return dir
}

// TestPushInstanceRepoMissingOwner is the adopter bug: instance_repo named an org
// that did not exist yet, so `gh repo create` failed with a bare CreateRepository
// permissions error and no next step. The owner must now be classified FIRST, and
// the error has to name both fixes (create the org / use an owner you have).
func TestPushInstanceRepoMissingOwner(t *testing.T) {
	dir := scaffoldDir(t, "ch-org/ch-instance-repo")
	// git rev-parse HEAD succeeds → no commit is seeded; nothing else shells out.
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name == "gh" {
			t.Errorf("unexpected gh call %v — the owner check is seamed", args)
		}
		return nil, nil
	})
	withGhOwnerKind(t, func(owner string) (string, error) {
		if owner != "ch-org" {
			t.Errorf("classified %q, want the <owner> half of instance_repo", owner)
		}
		return "", nil // definitively 404
	})
	orig := ghLoginFn
	t.Cleanup(func() { ghLoginFn = orig })
	ghLoginFn = func() string { return "chandraS" }

	pushed, err := pushInstanceRepo(globalOpts{yes: true}, dir)
	if err == nil {
		t.Fatal("expected an error when the instance_repo owner does not exist")
	}
	if pushed {
		t.Error("pushed = true although the repo was never created")
	}
	for _, want := range []string{
		`owner "ch-org" does not exist`,
		"creates a REPOSITORY, never its owner",
		// Both branches, because the fixes do not overlap: an uncreated org
		// resumes from the scaffold, a misspelled owner has to be re-scaffolded
		// (the wrong name is already rendered into the workflows).
		`Is "ch-org" the owner you meant?`,
		"https://github.com/organizations/new",
		"gh repo create ch-org/ch-instance-repo --private --source " + dir,
		"misspelled",
		"instance_repo chandraS/ch-instance-repo",
		"re-scaffolding",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}
}

// A misspelled owner must not be told to create an org under the typo'd name —
// an owner you can log in as always exists, so "absent" is more often a typo.
// Both branches must carry their OWN command: they do not overlap (create-then-
// push resumes from the scaffold, a corrected name has to be re-scaffolded).
func TestMissingRepoOwnerErrOffersBothBranches(t *testing.T) {
	msg := missingRepoOwnerErr("acme/inst", "acme", "my-instance", "someone").Error()
	if !strings.Contains(msg, `Is "acme" the owner you meant?`) {
		t.Errorf("the two branches are not posed as a question:\n%s", msg)
	}
	org := strings.Index(msg, "organizations/new")
	spell := strings.Index(msg, "misspelled")
	if org < 0 || spell < 0 {
		t.Fatalf("both branches must be offered:\n%s", msg)
	}
	// The org branch is the one that resumes from the scaffold, so it leads; the
	// name-is-wrong branch follows with the re-scaffold. Order is the claim.
	if org > spell {
		t.Errorf("the create-then-push branch must lead (it resumes from the scaffold):\n%s", msg)
	}
	if !strings.Contains(msg[spell:], "llz new <new-dir>") {
		t.Errorf("the misspelled branch has no command of its own:\n%s", msg)
	}
}

// An owner that is a DIFFERENT user's account can never be created in, at any
// token scope — so say that, instead of offering permission checks that cannot
// help. Only claimed when llz knows which account it is authenticated as.
func TestPushInstanceRepoForeignUserOwner(t *testing.T) {
	dir := scaffoldDir(t, "someone-else/inst")
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, nil })
	withGhOwnerKind(t, func(string) (string, error) { return "User", nil })
	withInstanceRepoExists(t, func(string) bool { return false })
	orig := ghLoginFn
	t.Cleanup(func() { ghLoginFn = orig })

	ghLoginFn = func() string { return "me" }
	_, err := pushInstanceRepo(globalOpts{yes: true}, dir)
	if err == nil {
		t.Fatal("expected an error for a repo owned by another user")
	}
	for _, want := range []string{
		`owner "someone-else" is another GitHub USER's account`,
		"never in another",
		"llz new <new-dir> --push --yes",
		"gh auth switch --hostname github.com --user someone-else",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}

	// Your OWN account is the normal case — case-insensitively, since GitHub
	// logins are.
	ghLoginFn = func() string { return "SomeOne-Else" }
	if _, err := pushInstanceRepo(globalOpts{}, dir); err != nil {
		t.Errorf("blocked a repo under the caller's own account: %v", err)
	}
	// An unresolvable login proves nothing — do not claim the owner is foreign.
	ghLoginFn = func() string { return "" }
	if _, err := pushInstanceRepo(globalOpts{}, dir); err != nil {
		t.Errorf("claimed a foreign owner without knowing the login: %v", err)
	}
}

// The org-membership probe is meaningless for a user owner (`gh api
// user/memberships/orgs/<user>` just 404s), and read:org is only needed for orgs.
func TestCreateRepoErrTailorsChecksToOwnerKind(t *testing.T) {
	user := createRepoErr("me/inst", "my-instance", "User", errors.New("exit status 1")).Error()
	if strings.Contains(user, "user/memberships/orgs") {
		t.Errorf("offered an org-membership check for a user owner:\n%s", user)
	}
	if strings.Contains(user, "read:org") {
		t.Errorf("asked a user owner for the read:org scope:\n%s", user)
	}
	for _, kind := range []string{"Organization", ""} {
		org := createRepoErr("acme/inst", "my-instance", kind, errors.New("exit status 1")).Error()
		if !strings.Contains(org, "gh api user/memberships/orgs/acme") {
			t.Errorf("kind %q dropped the org-membership check:\n%s", kind, org)
		}
	}
}

// Every `gh auth` remediation must name the host llz actually uses (GH_HOST):
// `llz doctor` scopes its own auth check that way, so pointing a GHE operator at
// github.com would have them authenticate where doctor is not looking.
func TestRemediationHonorsGHHost(t *testing.T) {
	t.Setenv("GH_HOST", "ghe.example.com")
	msgs := map[string]string{
		"unreachable": ghcli.UnreachableErr("acme/inst", errors.New("boom"), "tail").Error(),
		"create/org":  createRepoErr("acme/inst", "d", "Organization", errors.New("boom")).Error(),
		"create/user": createRepoErr("me/inst", "d", "User", errors.New("boom")).Error(),
		"foreign":     foreignUserOwnerErr("them/inst", "them", "d", "me").Error(),
		// GHE creates orgs on the customer's OWN host, so the org-creation link
		// is as host-dependent as the `gh auth` commands.
		"missing owner": missingRepoOwnerErr("acme/inst", "acme", "d", "me").Error(),
	}
	for name, msg := range msgs {
		// Only the HOST ARGUMENTS must follow GH_HOST — https://cli.github.com is
		// the install page and stays put wherever you authenticate.
		for _, bad := range []string{"--hostname github.com", "-h github.com", "https://github.com/organizations"} {
			if strings.Contains(msg, bad) {
				t.Errorf("%s passes %q under GH_HOST=ghe.example.com:\n%s", name, bad, msg)
			}
		}
		if !strings.Contains(msg, "ghe.example.com") {
			t.Errorf("%s never names the configured host:\n%s", name, msg)
		}
	}
}

// The unreachable-GitHub message must not claim a repo is public when --org
// points somewhere that could be a private fork.
func TestTemplateUnreachableTail(t *testing.T) {
	up := templateUnreachableTail(templateid.DefaultOrg, templateid.DefaultOrg+"/"+templateid.Name)
	if !strings.Contains(up, "is public") {
		t.Errorf("upstream tail lost the public-repo fact:\n%s", up)
	}
	fork := templateUnreachableTail("acme", "acme/"+templateid.Name)
	if strings.Contains(fork, "is public") {
		t.Errorf("claimed a --org fork is public:\n%s", fork)
	}
	if !strings.Contains(fork, "private fork") {
		t.Errorf("fork tail does not explain what else it could be:\n%s", fork)
	}
}

// `llz tokens` / `llz doctor` must not report an unanswerable gh as a missing
// instance repo — the same collapse the `llz new` preflight was fixed for.
func TestRequireInstanceRepo(t *testing.T) {
	withLookPath(t, func(f string) (string, error) { return "/usr/bin/" + f, nil })

	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, nil })
	if err := requireInstanceRepo("acme/inst"); err != nil {
		t.Errorf("reachable repo rejected: %v", err)
	}

	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("gh auth login") })
	err := requireInstanceRepo("acme/inst")
	if err == nil {
		t.Fatal("expected an error when gh cannot answer")
	}
	if strings.Contains(err.Error(), "not visible to your") {
		t.Errorf("reported an unanswerable gh as a missing repo:\n%s", err)
	}
	for _, want := range []string{"gh auth status", "does NOT mean the repo is missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}

	// A real 404 still is a missing repo.
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		_, e := exec.Command("sh", "-c", "echo 'gh: Not Found (HTTP 404)' >&2; exit 1").Output()
		return nil, e
	})
	withGhOwnerKind(t, func(string) (string, error) { return "Organization", nil })
	var missing error
	out := captureStderr(t, func() { missing = requireInstanceRepo("acme/inst") })
	// A 404 is "absent OR private to another account" — GitHub hides the two
	// behind the same status, so the message must not claim it is definitely gone.
	if missing == nil || !strings.Contains(missing.Error(), "not visible to your `gh` login") {
		t.Errorf("a 404 must report an unreachable repo, got %v", missing)
	}
	for _, want := range []string{"gh repo create acme/inst", "If it DOES exist, you are authed as an account that cannot see it"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing-repo remediation missing %q:\n%s", want, out)
		}
	}
}

// An unresolvable `gh` login must not leave a dangling suggestion.
func TestMissingRepoOwnerErrWithoutLogin(t *testing.T) {
	err := missingRepoOwnerErr("acme/inst", "acme", "my-instance", "")
	if !strings.Contains(err.Error(), "<your-login>/inst") {
		t.Errorf("no-login fallback missing the placeholder owner:\n%s", err)
	}
}

// An owner llz could not classify (no gh auth, offline, rate limit) must NOT be
// reported as absent — the create still runs and speaks for itself.
func TestPushInstanceRepoIndeterminateOwnerStillCreates(t *testing.T) {
	dir := scaffoldDir(t, "acme/inst")
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, nil })
	withGhOwnerKind(t, func(string) (string, error) { return "", errors.New("gh auth required") })
	withInstanceRepoExists(t, func(string) bool { return false })

	var pushed bool
	var err error
	// --yes withheld: runGated prints the plan instead of reaching GitHub.
	out := captureStderr(t, func() { pushed, err = pushInstanceRepo(globalOpts{}, dir) })
	if err != nil {
		t.Fatalf("indeterminate owner must not fail the push: %v", err)
	}
	if pushed {
		t.Error("pushed = true without --yes")
	}
	if !strings.Contains(out, "gh repo create acme/inst") {
		t.Errorf("create was not attempted:\n%s", out)
	}
}

// The other half of the story: once the adopter creates the repo by hand, a
// re-run must adopt it (remote + push) rather than dead-ending on `gh repo
// create`'s "Name already exists on this account".
func TestPushInstanceRepoAdoptsExistingRepo(t *testing.T) {
	dir := scaffoldDir(t, "acme/inst")
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if len(args) > 3 && args[3] == "get-url" {
			return nil, errors.New("no origin yet")
		}
		return nil, nil
	})
	withGhOwnerKind(t, func(string) (string, error) { return "Organization", nil })
	withInstanceRepoExists(t, func(repo string) bool { return repo == "acme/inst" })

	var err error
	out := captureStderr(t, func() { _, err = pushInstanceRepo(globalOpts{}, dir) })
	if err != nil {
		t.Fatalf("adopting an existing repo failed: %v", err)
	}
	if strings.Contains(out, "gh repo create") {
		t.Errorf("re-created a repo that already exists:\n%s", out)
	}
	for _, want := range []string{
		"already exists on GitHub",
		"git -C " + dir + " remote add origin https://github.com/acme/inst.git",
		"git -C " + dir + " push -u origin HEAD",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("adopt path missing %q:\n%s", want, out)
		}
	}
	// Without --yes runGated only PRINTS, so the announcement must not claim a
	// push that did not happen.
	if !strings.Contains(out, "would wire it") {
		t.Errorf("announced a push that runGated only planned:\n%s", out)
	}
}

// With --yes the same announcement states what it is actually doing.
func TestAdoptExistingRepoAnnouncesWhatItDoes(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("no origin") })
	out := captureStderr(t, func() {
		_ = adoptExistingRepo(globalOpts{dryRun: true}, "d", "acme/inst")
	})
	if !strings.Contains(out, "would wire it") {
		t.Errorf("dry-run announced a real push:\n%s", out)
	}
}

// The adopt path hand-builds origin's URL (`gh repo create` would have resolved
// the host itself), so it has to follow GH_HOST or it points at a host the repo
// does not live on and the push fails.
func TestAdoptExistingRepoHonorsGHHost(t *testing.T) {
	t.Setenv("GH_HOST", "ghe.example.com")
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("no origin yet") })
	out := captureStderr(t, func() {
		if err := adoptExistingRepo(globalOpts{}, "my-instance", "acme/inst"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "https://ghe.example.com/acme/inst.git") {
		t.Errorf("origin does not follow GH_HOST:\n%s", out)
	}
}

// An existing origin is re-pointed, not `remote add`ed (which would fail).
func TestAdoptExistingRepoResetsOrigin(t *testing.T) {
	dir := t.TempDir()
	withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte("git@github.com:old/repo.git\n"), nil })
	out := captureStderr(t, func() {
		if err := adoptExistingRepo(globalOpts{}, dir, "acme/inst"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "remote set-url origin") {
		t.Errorf("existing origin was not re-pointed:\n%s", out)
	}
}

// --dry-run must stay a pure plan: no classification, no existence probe.
func TestPushInstanceRepoDryRunSkipsProbes(t *testing.T) {
	dir := scaffoldDir(t, "ch-org/ch-instance-repo")
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, nil })
	withGhOwnerKind(t, func(string) (string, error) {
		t.Error("dry-run classified the owner over the network")
		return "", nil
	})

	var err error
	out := captureStderr(t, func() { _, err = pushInstanceRepo(globalOpts{dryRun: true}, dir) })
	if err != nil {
		t.Fatalf("dry-run push: %v", err)
	}
	if !strings.Contains(out, "(dry-run) gh repo create ch-org/ch-instance-repo") {
		t.Errorf("dry-run plan missing the create:\n%s", out)
	}
}

// ghcli.NotFound separates "GitHub says this account does not exist" from every other
// gh failure — the distinction the preflight is gated on. Built from a real
// ExitError so the stderr capture path is the one (*exec.Cmd).Output produces.
func TestGhNotFound(t *testing.T) {
	notFound := func(stderr string) error {
		_, err := exec.Command("sh", "-c", "echo '"+stderr+"' >&2; exit 1").Output()
		return err
	}
	if !ghcli.NotFound(notFound("gh: Not Found (HTTP 404)")) {
		t.Error("a 404 was not recognised as an absent account")
	}
	if ghcli.NotFound(notFound("gh: To use GitHub CLI in a GitHub Actions workflow, set the GH_TOKEN environment variable")) {
		t.Error("an auth failure was misread as an absent account")
	}
	if ghcli.NotFound(errors.New("exec: \"gh\": executable file not found in $PATH")) {
		t.Error("a missing binary was misread as an absent account")
	}
}

func TestGhOwnerKind(t *testing.T) {
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "gh" || args[1] != "users/acme" {
			t.Errorf("classified via %q %v, want `gh api users/acme`", name, args)
		}
		return []byte("Organization\n"), nil
	})
	if kind, err := ghcli.OwnerKind("acme"); kind != "Organization" || err != nil {
		t.Errorf("ghcli.OwnerKind = (%q, %v), want (Organization, nil)", kind, err)
	}
	// An indeterminate failure must surface as an error, not as "absent".
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("dial tcp: no route to host") })
	if _, err := ghcli.OwnerKind("acme"); err == nil {
		t.Error("a network failure was reported as a definitive classification")
	}
}

// gitScaffold builds a real one-commit repo on `branch` — ensureScaffoldBranch
// only ever talks to git, so the honest test drives git rather than a stub.
func gitScaffold(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	for _, argv := range [][]string{
		{"init", "-q", "-b", branch},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"commit", "-q", "--allow-empty", "-m", "Initial instance scaffold (llz new)"},
	} {
		out, err := exec.Command("git", append([]string{"-C", dir}, argv...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", argv, err, out)
		}
	}
	return dir
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// A scaffold that `git init` put on `master` must be renamed before the push:
// the platform-bootstrap Application tracks apps_repo_revision (default "main"),
// so pushing `master` bootstraps a cluster whose Apps resolve to nothing.
func TestEnsureScaffoldBranchRenamesMaster(t *testing.T) {
	dir := gitScaffold(t, "master")
	out := captureStderr(t, func() {
		if err := ensureScaffoldBranch(globalOpts{}, dir); err != nil {
			t.Fatal(err)
		}
	})
	if got := currentBranch(t, dir); got != "main" {
		t.Errorf("branch = %q, want main", got)
	}
	if !strings.Contains(out, "apps_repo_revision") {
		t.Errorf("rename did not say why:\n%s", out)
	}
}

// --dry-run must not report a rename it did not perform: run() is a no-op there
// and, unlike runGated, does not mark its echo as one.
func TestEnsureScaffoldBranchDryRunSaysWould(t *testing.T) {
	dir := gitScaffold(t, "master")
	out := captureStderr(t, func() {
		if err := ensureScaffoldBranch(globalOpts{dryRun: true}, dir); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "would rename") {
		t.Errorf("dry-run announced a rename in the present tense:\n%s", out)
	}
	if got := currentBranch(t, dir); got != "master" {
		t.Errorf("dry-run mutated the branch: %q", got)
	}
}

// Already on main: nothing to do, and nothing printed.
func TestEnsureScaffoldBranchLeavesMainAlone(t *testing.T) {
	dir := gitScaffold(t, "main")
	out := captureStderr(t, func() {
		if err := ensureScaffoldBranch(globalOpts{}, dir); err != nil {
			t.Fatal(err)
		}
	})
	if out != "" {
		t.Errorf("touched a scaffold already on main:\n%s", out)
	}
}

// A branch that has already been pushed is the operator's, not ours — renaming
// it would orphan the remote branch every consumer is tracking.
func TestEnsureScaffoldBranchLeavesPushedBranchAlone(t *testing.T) {
	dir := gitScaffold(t, "release")
	remote := t.TempDir()
	for _, argv := range [][]string{
		{"-C", remote, "init", "-q", "--bare"},
		{"-C", dir, "remote", "add", "origin", remote},
		{"-C", dir, "push", "-q", "-u", "origin", "release"},
	} {
		if out, err := exec.Command("git", argv...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", argv, err, out)
		}
	}
	if err := ensureScaffoldBranch(globalOpts{}, dir); err != nil {
		t.Fatal(err)
	}
	if got := currentBranch(t, dir); got != "release" {
		t.Errorf("branch = %q, want release (already pushed — must be left alone)", got)
	}
}

// `git branch -M` is --move --force: renaming master onto an EXISTING main
// deletes that branch and anything only it pointed at. Never do that silently.
func TestEnsureScaffoldBranchRefusesToClobberMain(t *testing.T) {
	dir := gitScaffold(t, "master")
	if out, err := exec.Command("git", "-C", dir, "branch", "main").CombinedOutput(); err != nil {
		t.Fatalf("create main: %v\n%s", err, out)
	}
	mainBefore, err := exec.Command("git", "-C", dir, "rev-parse", "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	out := captureStderr(t, func() {
		if err := ensureScaffoldBranch(globalOpts{}, dir); err != nil {
			t.Fatal(err)
		}
	})
	if got := currentBranch(t, dir); got != "master" {
		t.Errorf("branch = %q, want master (the rename must be refused)", got)
	}
	after, err := exec.Command("git", "-C", dir, "rev-parse", "main").Output()
	if err != nil || string(after) != string(mainBefore) {
		t.Errorf("pre-existing main was clobbered: %s -> %s (%v)", mainBefore, after, err)
	}
	if !strings.Contains(out, "already exists — leaving both alone") {
		t.Errorf("refusal was not explained:\n%s", out)
	}
}

// A detached HEAD (symbolic-ref fails) must be a no-op, not a rename.
func TestEnsureScaffoldBranchDetachedHead(t *testing.T) {
	dir := gitScaffold(t, "master")
	if out, err := exec.Command("git", "-C", dir, "checkout", "-q", "--detach").CombinedOutput(); err != nil {
		t.Fatalf("detach: %v\n%s", err, out)
	}
	out := captureStderr(t, func() {
		if err := ensureScaffoldBranch(globalOpts{}, dir); err != nil {
			t.Fatal(err)
		}
	})
	if out != "" {
		t.Errorf("detached HEAD was touched:\n%s", out)
	}
}

// The placeholder instance_repo has to name the way out, not just "skipping".
func TestPushInstanceRepoPlaceholderRepo(t *testing.T) {
	dir := scaffoldDir(t, "your-org/your-instance-repo")
	out := captureStderr(t, func() {
		if pushed, err := pushInstanceRepo(globalOpts{yes: true}, dir); err != nil || pushed {
			t.Fatalf("placeholder = (%v, %v), want (false, nil)", pushed, err)
		}
	})
	for _, want := range []string{
		"still the placeholder", "gh repo create <owner>/<name>", "must already exist",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("placeholder skip missing %q:\n%s", want, out)
		}
	}
	// The old message also had to say `git branch -M main`, because this early
	// return jumped over the only ensureScaffoldBranch call. runNew now normalises
	// the branch for every scaffold before pushInstanceRepo is reached, so telling
	// the operator to do it by hand would be stale advice about a done job.
	if strings.Contains(out, "branch -M main") {
		t.Errorf("still telling the operator to rename a branch runNew already renamed:\n%s", out)
	}
}

func TestRepoStatus(t *testing.T) {
	withLookPath(t, func(f string) (string, error) { return "/usr/bin/" + f, nil })

	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, nil })
	if found, err := repoStatus("o/r"); !found || err != nil {
		t.Errorf("reachable repo = (%v, %v), want (true, nil)", found, err)
	}
	// A 404 is a definitive "not there" — no error.
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		_, err := exec.Command("sh", "-c", "echo 'gh: Not Found (HTTP 404)' >&2; exit 1").Output()
		return nil, err
	})
	if found, err := repoStatus("o/r"); found || err != nil {
		t.Errorf("absent repo = (%v, %v), want (false, nil)", found, err)
	}
	if repoExists("o/r") {
		t.Error("repoExists = true for a 404")
	}
	// Anything else is indeterminate and must surface as an error.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("gh auth login") })
	if found, err := repoStatus("o/r"); found || err == nil {
		t.Errorf("unauthenticated gh = (%v, %v), want (false, non-nil)", found, err)
	}
	// gh missing entirely is reported as such, without shelling out.
	withLookPath(t, func(string) (string, error) { return "", errors.New("not found in $PATH") })
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		t.Error("shelled out to a gh that is not installed")
		return nil, nil
	})
	if _, err := repoStatus("o/r"); err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Errorf("missing gh = %v, want a not-on-PATH error", err)
	}
}

func TestCreateRepoErrGuidance(t *testing.T) {
	err := createRepoErr("acme/inst", "my-instance", "Organization", errors.New("exit status 1"))
	for _, want := range []string{
		"gh api user/memberships/orgs/acme",
		"gh auth status --hostname github.com",
		"gh auth refresh",
		"gh repo create acme/inst --private --source my-instance --remote origin --push",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("createRepoErr missing %q:\n%s", want, err)
		}
	}
}
