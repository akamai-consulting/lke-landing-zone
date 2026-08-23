package secretscope

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// parse turns fixture text into the corpus Scan takes, so the tests exercise the
// real YAML shapes rather than a hand-built struct — the `on:` key alone has three
// legal spellings and a scalar `environment:` and a mapping one both occur here.
func parse(t *testing.T, files map[string]string) map[string]workflow {
	t.Helper()
	out := map[string]workflow{}
	for p, body := range files {
		var w workflow
		if err := yaml.Unmarshal([]byte(body), &w); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out[p] = w
	}
	return out
}

var scoped = map[string]bool{"TF_STATE_ACCESS_KEY": true, "LINODE_API_TOKEN": true}

// ── The expected set ─────────────────────────────────────────────────────────

// TestEnvScopedSecretsComesFromTheRequirementTable. The whole guard rests on this
// set not being derived from the workflows it checks: if it were, a workflow that
// stopped naming a secret would shrink the set it is checked against and the guard
// would go green on the change that broke it.
func TestEnvScopedSecretsComesFromTheRequirementTable(t *testing.T) {
	got := EnvScopedSecrets()
	if len(got) == 0 {
		t.Fatal("no environment-scoped secrets — Run refuses to proceed, and so should this")
	}
	for _, want := range []string{"TF_STATE_ACCESS_KEY", "TF_STATE_SECRET_KEY", "LINODE_API_TOKEN"} {
		if !got[want] {
			t.Errorf("%s is EnvScope in envreq's table but missing from the set", want)
		}
	}
	// The repo-level ones must NOT be here: flagging a job for reading the state
	// passphrase without an environment would fail every correctly configured
	// instance, and a gate that cries wolf gets deleted.
	if got["TF_STATE_ENCRYPTION_PASSPHRASE"] {
		t.Error("TF_STATE_ENCRYPTION_PASSPHRASE is repo-level (one passphrase per instance) and must not be treated as env-scoped")
	}
}

// ── Arm 1: no environment ────────────────────────────────────────────────────

func TestFlagsAJobWithNoEnvironment(t *testing.T) {
	files := parse(t, map[string]string{
		"a/.github/workflows/w.yml": `
on: {workflow_dispatch: {}}
jobs:
  apply:
    steps:
      - env:
          AWS_ACCESS_KEY_ID: ${{ secrets.TF_STATE_ACCESS_KEY }}
        run: tofu init
`})
	f := Scan(files, scoped)
	if len(f) != 1 || f[0].Kind != NoEnvironment || f[0].Job != "apply" {
		t.Fatalf("expected one NoEnvironment finding for apply, got %+v", f)
	}
	if len(f[0].Secrets) != 1 || f[0].Secrets[0] != "TF_STATE_ACCESS_KEY" {
		t.Errorf("finding should name the secret, got %v", f[0].Secrets)
	}
}

func TestAcceptsADispatchJobThatDeclaresItsEnvironment(t *testing.T) {
	for name, env := range map[string]string{
		"scalar":  "    environment: infra-${{ inputs.region }}\n",
		"mapping": "    environment:\n      name: infra-primary\n      url: https://example.invalid\n",
	} {
		t.Run(name, func(t *testing.T) {
			files := parse(t, map[string]string{
				"a/.github/workflows/w.yml": "on: {workflow_dispatch: {}}\njobs:\n  apply:\n" + env +
					"    steps:\n      - env:\n          K: ${{ secrets.TF_STATE_ACCESS_KEY }}\n",
			})
			if f := Scan(files, scoped); len(f) != 0 {
				t.Errorf("a dispatch job inside its environment is correct, got %+v", f)
			}
		})
	}
}

// TestIgnoresSecretsThatAreNotEnvironmentScoped pins the exclusion. An arm that
// only proves it fires is one edit from flagging every job in the tree.
func TestIgnoresSecretsThatAreNotEnvironmentScoped(t *testing.T) {
	files := parse(t, map[string]string{
		"a/.github/workflows/w.yml": `
on: {pull_request: {}}
jobs:
  lint:
    steps:
      - env:
          T: ${{ secrets.GITHUB_TOKEN }}
          P: ${{ secrets.TF_STATE_ENCRYPTION_PASSPHRASE }}
        run: llz ci lint
`})
	if f := Scan(files, scoped); len(f) != 0 {
		t.Errorf("repo-level and built-in secrets are resolvable anywhere, got %+v", f)
	}
}

// TestReadsTheBracketSpelling — `secrets['X']` is the syntax nobody greps for, and
// a guard that only understands the dotted form is one rename from blind.
func TestReadsTheBracketSpelling(t *testing.T) {
	for _, spelling := range []string{`secrets['TF_STATE_ACCESS_KEY']`, `secrets["TF_STATE_ACCESS_KEY"]`} {
		files := parse(t, map[string]string{
			"a/.github/workflows/w.yml": "on: {workflow_dispatch: {}}\njobs:\n  apply:\n    steps:\n      - env:\n          K: ${{ " + spelling + " }}\n",
		})
		if f := Scan(files, scoped); len(f) != 1 {
			t.Errorf("%s should be read as a secret reference, got %+v", spelling, f)
		}
	}
}

// TestScansTheWholeJob, not an enumerated list of keys. A secret can be written
// into `env:`, `with:`, a `run:` line or a reusable call's `secrets:` mapping, and
// naming those keys is how the next place one can appear gets missed.
func TestScansTheWholeJob(t *testing.T) {
	for name, body := range map[string]string{
		"with":         "    with:\n      key: ${{ secrets.LINODE_API_TOKEN }}\n",
		"run":          "    steps:\n      - run: echo ${{ secrets.LINODE_API_TOKEN }}\n",
		"call secrets": "    uses: ./.github/workflows/x.yml\n    secrets:\n      TOKEN: ${{ secrets.LINODE_API_TOKEN }}\n",
	} {
		t.Run(name, func(t *testing.T) {
			files := parse(t, map[string]string{
				"a/.github/workflows/w.yml": "on: {workflow_dispatch: {}}\njobs:\n  j:\n" + body,
			})
			if f := Scan(files, scoped); len(f) != 1 {
				t.Errorf("a secret in %s must be seen, got %+v", name, f)
			}
		})
	}
}

// ── Arm 2: reachable from a pull request ─────────────────────────────────────

// TestFlagsAPRJobEvenWhenItDeclaresTheEnvironment is the arm that stops the wrong
// fix. infra-<env> is locked to ref=main, so a pull_request run cannot enter it —
// adding `environment:` moves the failure from `tofu init` to job start.
func TestFlagsAPRJobEvenWhenItDeclaresTheEnvironment(t *testing.T) {
	files := parse(t, map[string]string{
		"a/.github/workflows/w.yml": `
on: {pull_request: {}}
jobs:
  plan:
    environment: infra-primary
    steps:
      - env:
          K: ${{ secrets.TF_STATE_ACCESS_KEY }}
`})
	f := Scan(files, scoped)
	if len(f) != 1 || f[0].Kind != PRReachable {
		t.Fatalf("expected a PRReachable finding, got %+v", f)
	}
	if !strings.Contains(f[0].detail(), "infra-primary") {
		t.Errorf("the detail must name the environment the job DOES declare — the job looks correct otherwise: %q", f[0].detail())
	}
}

// TestReachabilityCrossesALocalReusableCall. The bug this guard was written for
// lived in a reusable body whose own `on:` is `workflow_call` only; nothing in
// that file says a pull request can start it.
func TestReachabilityCrossesALocalReusableCall(t *testing.T) {
	files := parse(t, map[string]string{
		"a/.github/workflows/caller.yml": `
on: {pull_request: {}}
jobs:
  call:
    uses: ./.github/workflows/body.yml
    secrets: inherit
`,
		"a/.github/workflows/body.yml": `
on: {workflow_call: {}}
jobs:
  plan:
    environment: infra-primary
    steps:
      - env:
          K: ${{ secrets.TF_STATE_ACCESS_KEY }}
`})
	f := Scan(files, scoped)
	if len(f) != 1 || f[0].File != "a/.github/workflows/body.yml" {
		t.Fatalf("expected the finding in the reusable body, got %+v", f)
	}
	if !strings.Contains(f[0].Via, "caller.yml") {
		t.Errorf("the finding must name the pull_request entry point — nothing in body.yml does: Via=%q", f[0].Via)
	}
}

// TestReachabilityStopsAtADispatchOnlyCallSite is the exclusion this arm lives or
// dies by. llz-terraform.yml calls llz-bootstrap-openbao.yml from a job gated on
// `github.event_name == 'workflow_dispatch'`. Propagating through the FILE instead
// of through the calling job's gate flags every credential-holding job in the
// bootstrap body — a page of false findings, and a guard that gets turned off.
func TestReachabilityStopsAtADispatchOnlyCallSite(t *testing.T) {
	files := parse(t, map[string]string{
		"a/.github/workflows/caller.yml": `
on:
  pull_request: {}
  workflow_dispatch: {}
jobs:
  bootstrap:
    if: github.event_name == 'workflow_dispatch'
    uses: ./.github/workflows/body.yml
    secrets: inherit
`,
		"a/.github/workflows/body.yml": `
on: {workflow_call: {}}
jobs:
  seed:
    environment: infra-primary
    steps:
      - env:
          K: ${{ secrets.LINODE_API_TOKEN }}
`})
	if f := Scan(files, scoped); len(f) != 0 {
		t.Errorf("a dispatch-only call site does not put its callee on the pull-request path, got %+v", f)
	}
}

// TestADispatchGatedJobInAPRWorkflowIsNotFlagged — the same exclusion one level
// down: apply-cluster and its siblings live in a pull-request-reachable file and
// hold exactly these credentials on purpose.
func TestADispatchGatedJobInAPRWorkflowIsNotFlagged(t *testing.T) {
	files := parse(t, map[string]string{
		"a/.github/workflows/w.yml": `
on:
  pull_request: {}
  workflow_dispatch: {}
jobs:
  apply:
    if: >
      github.event_name == 'workflow_dispatch' &&
      inputs.action == 'apply'
    environment: infra-primary
    steps:
      - env:
          K: ${{ secrets.TF_STATE_ACCESS_KEY }}
`})
	if f := Scan(files, scoped); len(f) != 0 {
		t.Errorf("a dispatch-gated job is not on the pull-request path, got %+v", f)
	}
}

// TestAnUngatedJobInAPRWorkflowIsFlagged. "I could not tell" and "it does not run
// there" are different answers, and only one of them is safe to assume about a job
// holding production credentials — so a job with no event gate counts as reachable.
func TestAnUngatedJobInAPRWorkflowIsFlagged(t *testing.T) {
	files := parse(t, map[string]string{
		"a/.github/workflows/w.yml": `
on:
  pull_request: {}
  workflow_dispatch: {}
jobs:
  apply:
    if: inputs.action == 'apply'
    environment: infra-primary
    steps:
      - env:
          K: ${{ secrets.TF_STATE_ACCESS_KEY }}
`})
	if f := Scan(files, scoped); len(f) != 1 || f[0].Kind != PRReachable {
		t.Errorf("an `if:` that names no event does not exclude the pull-request path, got %+v", f)
	}
}

// TestTriggersOnPRReadsEveryShapeOfTheOnKey. A workflow written `on: pull_request`
// or `on: [push, pull_request]` would otherwise read as having no triggers at all,
// and every job in it would leave scope without a word.
func TestTriggersOnPRReadsEveryShapeOfTheOnKey(t *testing.T) {
	for name, tc := range map[string]struct {
		on   string
		want bool
	}{
		"scalar":              {"on: pull_request\n", true},
		"scalar non-PR":       {"on: push\n", false},
		"sequence":            {"on: [push, pull_request]\n", true},
		"sequence non-PR":     {"on: [push, workflow_dispatch]\n", false},
		"mapping":             {"on:\n  pull_request:\n    branches: [main]\n", true},
		"mapping non-PR":      {"on:\n  workflow_dispatch: {}\n", false},
		"pull_request_target": {"on:\n  pull_request_target: {}\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			var w workflow
			if err := yaml.Unmarshal([]byte(tc.on+"jobs: {}\n"), &w); err != nil {
				t.Fatal(err)
			}
			if got := triggersOnPR(w.On); got != tc.want {
				t.Errorf("triggersOnPR(%q) = %v, want %v", tc.on, got, tc.want)
			}
		})
	}
}

// TestCalleeResolvesInsideTheCallersOwnTree. `./.github/workflows/x.yml` inside
// instance-template/ means the DELIVERED copy. Resolving it against the repo root
// would make the template's own file answer for the delivered one — and those two
// are allowed to differ, which is the whole reason both trees are scanned.
func TestCalleeResolvesInsideTheCallersOwnTree(t *testing.T) {
	got, ok := calleePath("instance-template/.github/workflows/terraform.yml", "./.github/workflows/llz-terraform.yml")
	if !ok || got != "instance-template/.github/workflows/llz-terraform.yml" {
		t.Errorf("callee = %q (ok=%v), want the delivered copy", got, ok)
	}
	if _, ok := calleePath("a/.github/workflows/w.yml", "org/repo/.github/workflows/x.yml@v1"); ok {
		t.Error("a remote call cannot be resolved from this tree and must not be guessed at")
	}
}

// ── Fail-closed arms ─────────────────────────────────────────────────────────

// runIn builds a tree and runs the real Run against it.
func runIn(t *testing.T, files map[string]string) (string, string, error) {
	t.Helper()
	root := t.TempDir()
	for p, body := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	err := Run(root, &out, &errOut)
	return out.String(), errOut.String(), err
}

func TestRunRefusesAnEmptyCorpus(t *testing.T) {
	_, _, err := runIn(t, map[string]string{"README.md": "nothing here\n"})
	if err == nil || !strings.Contains(err.Error(), "refusing to pass vacuously") {
		t.Fatalf("a wrong --root must not report OK, got %v", err)
	}
}

func TestRunRefusesAnEmptyFirstPartyRoot(t *testing.T) {
	_, _, err := runIn(t, map[string]string{
		".github/workflows/.keep":                   "",
		"instance-template/.github/workflows/w.yml": "on: {workflow_dispatch: {}}\njobs: {}\n",
	})
	if err == nil || !strings.Contains(err.Error(), "holds no workflows") {
		t.Fatalf("a present-but-empty root is not a pass, got %v", err)
	}
}

// TestRunRefusesAMovedDeliveredTree — instance-template/ exists but its workflow
// tree does not. That is a rename, and it is the half that reaches adopters.
func TestRunRefusesAMovedDeliveredTree(t *testing.T) {
	_, _, err := runIn(t, map[string]string{
		".github/workflows/w.yml":      "on: {workflow_dispatch: {}}\njobs: {}\n",
		"instance-template/copier.yml": "x: 1\n",
	})
	if err == nil || !strings.Contains(err.Error(), "delivered workflow tree moved") {
		t.Fatalf("a moved delivered tree must not pass, got %v", err)
	}
}

// TestRunPassesInAnInstanceCheckout — no instance-template/ at all is legitimate,
// and must be told apart from the tree having moved.
func TestRunPassesInAnInstanceCheckout(t *testing.T) {
	out, _, err := runIn(t, map[string]string{
		".github/workflows/w.yml": "on: {workflow_dispatch: {}}\njobs:\n  j:\n    steps:\n      - run: true\n",
	})
	if err != nil {
		t.Fatalf("an instance checkout has no delivered tree and that is correct, got %v", err)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("expected an OK line, got %q", out)
	}
}

func TestRunRefusesAnUnparseableWorkflow(t *testing.T) {
	_, _, err := runIn(t, map[string]string{
		".github/workflows/w.yml":                   "on: {workflow_dispatch: {}}\njobs:\n  j: {}\n",
		"instance-template/.github/workflows/b.yml": "jobs:\n  - this is: [not\n",
	})
	if err == nil {
		t.Fatal("a workflow this guard cannot parse is one it cannot vouch for")
	}
	if !strings.Contains(err.Error(), "b.yml") {
		t.Errorf("the error must name the file, got %v", err)
	}
}

// TestRunReportsTheRealFinding names both trees present and one job wrong — the
// end-to-end path, including that the annotation carries the file.
func TestRunReportsTheRealFinding(t *testing.T) {
	_, errOut, err := runIn(t, map[string]string{
		".github/workflows/w.yml": "on: {workflow_dispatch: {}}\njobs:\n  j:\n    steps:\n      - run: true\n",
		"instance-template/.github/workflows/b.yml": `
on: {pull_request: {}}
jobs:
  plan:
    steps:
      - env:
          K: ${{ secrets.TF_STATE_ACCESS_KEY }}
`,
	})
	if err == nil {
		t.Fatal("expected a failure")
	}
	for _, want := range []string{"::error file=instance-template/.github/workflows/b.yml", "plan", "TF_STATE_ACCESS_KEY"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("report should contain %q:\n%s", want, errOut)
		}
	}
}
