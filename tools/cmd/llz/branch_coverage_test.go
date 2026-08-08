package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/environments"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/onboard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envdef"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghapi"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghcli"
)

// ── shared helpers ───────────────────────────────────────────────────────────

// captureStderr mirrors captureStdout for the os.Stderr path (the remediation /
// warning printers write there).
// withGhOwnerKind stubs the instance_repo owner classifier. A local copy: the
// other one went to internal/newinstance with the repo-creation tests, and what
// is left here tests ghapi.RemediateMissingRepo, which is neither package's.
func withGhOwnerKind(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	orig := ghcli.OwnerKindFn
	t.Cleanup(func() { ghcli.OwnerKindFn = orig })
	ghcli.OwnerKindFn = fn
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = orig
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// writeFileMkdir writes content at path, creating parent dirs (mustWrite does not).
func writeFileMkdir(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, content)
}

// ── item C: no-remote-repo detection (tokens.go) ─────────────────────────────

func TestRepoExists(t *testing.T) {
	// ghapi.RepoStatus resolves `gh` on PATH before shelling out; stub it so the test
	// asserts llz's logic rather than whether this machine has gh installed.
	withLookPath(t, func(f string) (string, error) { return "/usr/bin/" + f, nil })
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "gh" || len(args) < 2 || args[0] != "api" || args[1] != "repos/o/r" {
			t.Errorf("ghapi.RepoExists shelled out to %q %v, want `gh api repos/o/r ...`", name, args)
		}
		return nil, nil
	})
	if !ghapi.RepoExists("o/r") {
		t.Error("ghapi.RepoExists = false when gh succeeds, want true")
	}
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("HTTP 404") })
	if ghapi.RepoExists("o/r") {
		t.Error("ghapi.RepoExists = true when gh errors (repo absent), want false")
	}
}

func TestRemediateMissingRepo(t *testing.T) {
	withGhOwnerKind(t, func(string) (string, error) { return "Organization", nil })
	out := captureStderr(t, func() { ghapi.RemediateMissingRepo("acme/inst") })
	for _, want := range []string{
		`instance repo "acme/inst" is not reachable`,
		"gh repo create acme/inst",
		"llz new",
		// `--source . --push` pushes the CHECKED-OUT branch, and this remediation
		// is typed by hand where ensureScaffoldBranch never ran — so it has to name
		// the branch the bootstrap Application actually tracks.
		"git branch -M main",
		"apps_repo_revision",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ghapi.RemediateMissingRepo output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "OWNER") {
		t.Errorf("named the owner as missing although it exists:\n%s", out)
	}

	// An absent owner needs a different first step — creating the repo under an
	// org that does not exist fails with a bare CreateRepository error.
	withGhOwnerKind(t, func(string) (string, error) { return "", nil })
	out = captureStderr(t, func() { ghapi.RemediateMissingRepo("acme/inst") })
	for _, want := range []string{
		`OWNER "acme" does not exist`,
		"check how it is spelled", // a typo is likelier than an uncreated org
		"https://github.com/organizations/new",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("absent-owner remediation missing %q:\n%s", want, out)
		}
	}
}

// ── item D: standing OPENBAO_ROOT_TOKEN check (commands.go, `llz status`) ─────

func TestWarnIfRootTokenPresent(t *testing.T) {
	withLookPath(t, func(f string) (string, error) { return "/usr/bin/" + f, nil })
	dir := chdirTempDir(t)
	mustWrite(t, filepath.Join(dir, ".copier-answers.yml"), "instance_repo: acme/inst\n")

	// Present in infra-<env> → the nag + the exact delete command.
	withExecOutput(t, func(_ string, _ ...string) ([]byte, error) {
		return []byte(`{"secrets":[{"name":"OPENBAO_ROOT_TOKEN"},{"name":"LINODE_API_TOKEN"}]}`), nil
	})
	out := captureStdout(t, func() { warnIfRootTokenPresent("lab") })
	for _, want := range []string{
		"OPENBAO_ROOT_TOKEN is still set in infra-lab",
		"gh secret delete OPENBAO_ROOT_TOKEN --env infra-lab --repo acme/inst",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("warnIfRootTokenPresent missing %q:\n%s", want, out)
		}
	}

	// Absent → silent.
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return []byte(`{"secrets":[{"name":"LINODE_API_TOKEN"}]}`), nil
	})
	if out := captureStdout(t, func() { warnIfRootTokenPresent("lab") }); strings.Contains(out, "OPENBAO_ROOT_TOKEN") {
		t.Errorf("absent root token must print nothing, got:\n%s", out)
	}

	// No gh on PATH → no-op (must not shell out or panic).
	withLookPath(t, func(string) (string, error) { return "", errors.New("not found") })
	if out := captureStdout(t, func() { warnIfRootTokenPresent("lab") }); out != "" {
		t.Errorf("no gh must print nothing, got:\n%s", out)
	}
}

// ── the readiness scan's new checks (readiness.go) ───────────────────────────

// writeGoodReadiness lays down a minimal, fully-consistent scaffold for env so a
// single mutation per test isolates the branch under test.
func writeGoodReadiness(t *testing.T, dir, env string) {
	t.Helper()
	writeTFVars(t, dir, "cluster", env, "region = \"us-ord\"\n")
	writeTFVars(t, dir, "cluster-bootstrap", env, "deployment = \""+env+"\"\napl_values_env = \""+env+"\"\n")
	writeTFVars(t, dir, "object-storage", env, "region_suffix = \""+env+"\"\nobj_cluster = \"us-ord-10\"\n")
	// databases is opt-in, but render always writes at least the region_suffix stub,
	// so a consistent scaffold carries the databases tfvars too.
	writeTFVars(t, dir, "databases", env, "region_suffix = \""+env+"\"\n")
	writeFileMkdir(t, filepath.Join(dir, "apl-values", env, "manifest", "kustomization.yaml"),
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n")
}

func TestRunEnvReadinessHappyPath(t *testing.T) {
	dir := chdirTempDir(t)
	writeGoodReadiness(t, dir, "e2e") // obj_cluster us-ord-10 must be accepted (regression guard)
	var err error
	out := captureStdout(t, func() { err = configreadiness.RunEnvReadiness("e2e") })
	if err != nil {
		t.Fatalf("consistent scaffold should pass, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "ready to build") {
		t.Errorf("missing ready-to-build message:\n%s", out)
	}
}

func TestRunEnvReadinessDiscriminatorMismatch(t *testing.T) {
	dir := chdirTempDir(t)
	writeGoodReadiness(t, dir, "e2e")
	// deployment disagrees with the env name → silent state-key desync.
	writeTFVars(t, dir, "cluster-bootstrap", "e2e", "deployment = \"wrong\"\napl_values_env = \"e2e\"\n")
	var err error
	out := captureStdout(t, func() { err = configreadiness.RunEnvReadiness("e2e") })
	if err == nil {
		t.Fatalf("discriminator mismatch must fail:\n%s", out)
	}
	if !strings.Contains(out, "must equal the deployment name") {
		t.Errorf("missing discriminator finding:\n%s", out)
	}
}

func TestRunEnvReadinessBadObjCluster(t *testing.T) {
	dir := chdirTempDir(t)
	writeGoodReadiness(t, dir, "e2e")
	writeTFVars(t, dir, "object-storage", "e2e", "region_suffix = \"e2e\"\nobj_cluster = \"0.0.0.0/0\"\n")
	var err error
	out := captureStdout(t, func() { err = configreadiness.RunEnvReadiness("e2e") })
	if err == nil {
		t.Fatalf("malformed obj_cluster must fail:\n%s", out)
	}
	if !strings.Contains(out, "not a Linode OBJ cluster id") {
		t.Errorf("missing obj_cluster finding:\n%s", out)
	}
}

func TestRunEnvReadinessChartPlaceholder(t *testing.T) {
	dir := chdirTempDir(t)
	writeGoodReadiness(t, dir, "e2e")
	// A REPLACE_ME left in an out-of-scaffold chart values file must be caught.
	writeFileMkdir(t, filepath.Join(dir, "kubernetes-charts", "llz-argo-bootstrap-apps", "values.yaml"),
		"global:\n  gitRepoURL: \"REPLACE_ME-git-repo-url\"\n")
	var err error
	out := captureStdout(t, func() { err = configreadiness.RunEnvReadiness("e2e") })
	if err == nil {
		t.Fatalf("REPLACE_ME in chart values must fail:\n%s", out)
	}
	if !strings.Contains(out, "REPLACE_ME") {
		t.Errorf("missing chart-values placeholder finding:\n%s", out)
	}
}

// ── onboard.RunDoctor: the envExplicit gating (wizard.go) ────────────────────────────

func TestRunDoctorEnvGating(t *testing.T) {
	withLookPath(t, func(f string) (string, error) { return "/usr/bin/" + f, nil })
	withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte("ok"), nil })
	chdirTempDir(t) // no .copier-answers.yml, no apl-values scaffold

	// A bare `llz doctor` (default env, NOT explicit) must not error just because
	// no deployment has been scaffolded — readiness is skipped.
	var errBare error
	captureStdout(t, func() { errBare = onboard.RunDoctor("", "e2e", false, false, "", "") })
	if errBare != nil {
		t.Errorf("bare doctor errored on a missing scaffold: %v", errBare)
	}

	// `llz doctor --env lab` (explicit) must surface the missing scaffold.
	var errExplicit error
	out := captureStdout(t, func() { errExplicit = onboard.RunDoctor("", "lab", false, true, "", "") })
	if errExplicit == nil {
		t.Fatalf("explicit --env with no scaffold should error:\n%s", out)
	}
	if !strings.Contains(errExplicit.Error(), "no scaffold") {
		t.Errorf("want a 'no scaffold' error, got: %v", errExplicit)
	}
}

// ── the human-facing printers (commands.go / scaffold.go) ────────────────────

func TestPrintManualActions(t *testing.T) {
	out := captureStdout(t, func() { printManualActions("lab") })
	for _, want := range []string{
		"llz status lab",
		"unseal keys 4 & 5",
		"OPENBAO_ROOT_TOKEN from infra-lab",
		"TF_VAR_linode_dns_token",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("printManualActions missing %q:\n%s", want, out)
		}
	}
}

// ── cmdUp orchestration (commands.go) ────────────────────────────────────────

// stubUpSteps swaps the cmdUp step seams for recorders, optionally failing at one
// named step ("tokens"/"doctor"/"build", or "" for all-pass). Returns a pointer to
// the ordered call log.
func stubUpSteps(t *testing.T, failAt string) *[]string {
	t.Helper()
	var calls []string
	mk := func(name string) error {
		calls = append(calls, name)
		if name == failAt {
			return errors.New(name + " boom")
		}
		return nil
	}
	origT, origD, origB, origP := upTokens, upDoctor, upBuild, upPreflight
	upTokens = func(globalOpts, bool, string) error { return mk("tokens") }
	upDoctor = func(globalOpts, bool, string) error { return mk("doctor") }
	upBuild = func(globalOpts, string) error { return mk("build") }
	upPreflight = func(string) error { return mk("preflight") }
	t.Cleanup(func() { upTokens, upDoctor, upBuild, upPreflight = origT, origD, origB, origP })
	return &calls
}

func TestCmdUp(t *testing.T) {
	g := globalOpts{}

	// Invalid env name → no steps run.
	calls := stubUpSteps(t, "")
	if err := cmdUp("BAD ENV", g, false, false); err == nil {
		t.Error("invalid env name should error")
	} else if len(*calls) != 0 {
		t.Errorf("invalid env must run no steps, got %v", *calls)
	}

	// Happy path → tokens → doctor → build, then the manual-actions summary.
	calls = stubUpSteps(t, "")
	var err error
	out := captureStdout(t, func() { err = cmdUp("lab", g, false, false) })
	if err != nil {
		t.Fatalf("happy path errored: %v", err)
	}
	// The dispatch check leads: it is read-only, and stage 1 is an interactive
	// wizard that mints credentials — no point running it for a build that cannot
	// dispatch.
	if got := strings.Join(*calls, ","); got != "preflight,tokens,doctor,build" {
		t.Errorf("step order = %q, want preflight,tokens,doctor,build", got)
	}
	if !strings.Contains(out, "remaining manual actions") {
		t.Errorf("manual-actions summary not printed:\n%s", out)
	}

	// --skip-tokens → doctor → build only.
	calls = stubUpSteps(t, "")
	captureStdout(t, func() { _ = cmdUp("lab", g, false, true) })
	if got := strings.Join(*calls, ","); got != "preflight,doctor,build" {
		t.Errorf("skip-tokens order = %q, want preflight,doctor,build", got)
	}

	// tokens fails → short-circuit; doctor/build never run; error wraps `tokens:`.
	calls = stubUpSteps(t, "tokens")
	captureStdout(t, func() { err = cmdUp("lab", g, false, false) })
	if err == nil || !strings.HasPrefix(err.Error(), "tokens:") {
		t.Errorf("tokens failure: err = %v, want a tokens: wrap", err)
	}
	if got := strings.Join(*calls, ","); got != "preflight,tokens" {
		t.Errorf("tokens failure must stop after tokens, got %q", got)
	}

	// doctor fails → build not reached; error wraps `doctor:`.
	calls = stubUpSteps(t, "doctor")
	captureStdout(t, func() { err = cmdUp("lab", g, false, false) })
	if err == nil || !strings.HasPrefix(err.Error(), "doctor:") {
		t.Errorf("doctor failure: err = %v, want a doctor: wrap", err)
	}
	if got := strings.Join(*calls, ","); got != "preflight,tokens,doctor" {
		t.Errorf("doctor failure order = %q, want preflight,tokens,doctor", got)
	}

	// build fails → error wraps `build:`; manual actions must NOT print.
	calls = stubUpSteps(t, "build")
	out = captureStdout(t, func() { err = cmdUp("lab", g, false, false) })
	if err == nil || !strings.HasPrefix(err.Error(), "build:") {
		t.Errorf("build failure: err = %v, want a build: wrap", err)
	}
	// The sibling cases above assert the call order; this one re-stubbed `calls`
	// and then never read it, so the build case verified the error wrap but not
	// that the earlier steps actually ran first. staticcheck's SA4006 ("this value
	// is never used") is what surfaced the omission.
	if got := strings.Join(*calls, ","); got != "preflight,tokens,doctor,build" {
		t.Errorf("build failure order = %q, want preflight,tokens,doctor,build", got)
	}
	if strings.Contains(out, "remaining manual actions") {
		t.Errorf("manual actions must not print on build failure:\n%s", out)
	}
}

func TestPrintPlaceholderChecklist(t *testing.T) {
	// With a residual placeholder → it's listed.
	dir := chdirTempDir(t)
	writeFileMkdir(t, filepath.Join(dir, "apl-values", "lab", "issuer.yaml"), "email: REPLACE_PER_ENV\n")
	out := captureStdout(t, func() { environments.PrintPlaceholderChecklist("apl-values", "lab") })
	if !strings.Contains(out, "Placeholders still to fill") || !strings.Contains(out, "REPLACE_PER_ENV") {
		t.Errorf("checklist did not flag the placeholder:\n%s", out)
	}

	// Clean overlay → the "nothing left" message.
	dir2 := chdirTempDir(t)
	writeFileMkdir(t, filepath.Join(dir2, "apl-values", "lab", "issuer.yaml"), "email: ops@example.com\n")
	out2 := captureStdout(t, func() { environments.PrintPlaceholderChecklist("apl-values", "lab") })
	if !strings.Contains(out2, "no placeholders left") {
		t.Errorf("clean overlay should report none left:\n%s", out2)
	}
}

// `llz env add`'s output used to print an unconditional "Still to fill … the
// REPLACE_PER_ENV / REPLACE_ME placeholders" block and then, two lines later,
// "✓ no placeholders left to fill" — so a clean scaffold sent the operator
// hunting for placeholders that were not there. environments.PrintPlaceholderChecklist is now
// the sole reporter; this pins that.
func TestEnvAddNextSteps_DoesNotClaimPlaceholdersUnconditionally(t *testing.T) {
	out := captureStdout(t, func() {
		environments.PrintNextSteps("lab", "environments/lab.yaml", envdef.Opts{})
	})
	for _, banned := range []string{"Still to fill", "REPLACE_PER_ENV", "REPLACE_ME"} {
		if strings.Contains(out, banned) {
			t.Errorf("environments.PrintNextSteps must leave placeholder reporting to the checklist, but printed %q:\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "scaffolded") {
		t.Errorf("expected the scaffolded confirmation:\n%s", out)
	}
}

// --cluster-domain writes nothing (Linode owns the domain; the validator rejects
// cluster.bootstrap.domainSuffix). The banner used to echo it back as
// `domainSuffix: <value>`, which read exactly like it had been applied.
func TestEnvAdd_ClusterDomainIsNotEchoedAsApplied(t *testing.T) {
	dir := chdirTempDir(t)
	writeFileMkdir(t, filepath.Join(dir, "terraform-iac-bootstrap", "cluster", ".keep"), "")
	out := captureStdout(t, func() {
		_ = environments.Run(true, "lab", envdef.Opts{
			Region: "us-sea", ObjCluster: "us-sea-1", ClusterDomain: "lab.example.com", DryRun: true,
		})
	})
	if strings.Contains(out, "domainSuffix:") {
		t.Errorf("the banner must not echo a domainSuffix it never writes:\n%s", out)
	}
}

// cobra's MarkDeprecated already warns at parse time; a second warning printed
// from environments.Run said the same thing twice AND split the summary banner in half.
func TestEnvAdd_ClusterDomainWarnsExactlyOnce(t *testing.T) {
	dir := chdirTempDir(t)
	writeFileMkdir(t, filepath.Join(dir, "terraform-iac-bootstrap", "cluster", ".keep"), "")
	var out, errOut string
	errOut = captureStderr(t, func() {
		out = captureStdout(t, func() {
			_ = environments.Run(true, "lab", envdef.Opts{
				Region: "us-sea", ObjCluster: "us-sea-1", ClusterDomain: "lab.example.com", DryRun: true,
			})
		})
	})
	if n := strings.Count(errOut+out, "cluster-domain"); n > 0 {
		t.Errorf("environments.Run must not warn about --cluster-domain itself (cobra already does); saw %d mention(s):\n%s%s", n, errOut, out)
	}
}
