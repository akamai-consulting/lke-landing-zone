package openbao

// ci_openbao_restore_workflow_test.go — the shape of the restore-openbao workflow
// pair, held to the properties the verb cannot enforce from inside one step.
//
// The verb proves what a single step does. What it cannot see is the ORDER the
// steps run in, which one runs `if: always()`, what each step is handed, and what
// group serializes the job — and every one of those is a way to ship a workflow
// whose Go is correct and whose behaviour is not: a revoke that is skipped on the
// failure it exists for, a confirmation checked after the ACL is already open, a
// restore racing a bootstrap's quorum. So the YAML is parsed, not grepped, and
// each property is asserted on the step that carries it.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type wfStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
}

type wfJob struct {
	Environment string            `yaml:"environment"`
	Env         map[string]string `yaml:"env"`
	Uses        string            `yaml:"uses"`
	With        map[string]string `yaml:"with"`
	Steps       []wfStep          `yaml:"steps"`
	Container   struct {
		Image string `yaml:"image"`
	} `yaml:"container"`
}

type wfInput struct {
	Type    string `yaml:"type"`
	Default any    `yaml:"default"`
}

type workflow struct {
	On struct {
		Dispatch *struct {
			Inputs map[string]wfInput `yaml:"inputs"`
		} `yaml:"workflow_dispatch"`
		Call *struct {
			Inputs  map[string]wfInput `yaml:"inputs"`
			Secrets map[string]struct {
				Required bool `yaml:"required"`
			} `yaml:"secrets"`
		} `yaml:"workflow_call"`
	} `yaml:"on"`
	Concurrency *struct {
		Group string `yaml:"group"`
	} `yaml:"concurrency"`
	Defaults struct {
		Run struct {
			Shell string `yaml:"shell"`
		} `yaml:"run"`
	} `yaml:"defaults"`
	Jobs map[string]wfJob `yaml:"jobs"`
	raw  string
}

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "instance-template")); err == nil {
			b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("repo root (instance-template/) not found — this test would otherwise pass having read nothing")
	return ""
}

func loadWorkflow(t *testing.T, rel string) workflow {
	t.Helper()
	raw := repoFile(t, rel)
	var w workflow
	if err := yaml.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	w.raw = raw
	return w
}

const (
	restoreBody = "instance-template/.github/workflows/llz-restore-openbao.yml"
	restoreStub = "instance-template/.github/workflows/restore-openbao.yml"
)

func restoreJob(t *testing.T) wfJob {
	t.Helper()
	w := loadWorkflow(t, restoreBody)
	j, ok := w.Jobs["restore"]
	if !ok || len(j.Steps) < 5 {
		t.Fatalf("%s has no `restore` job with steps (jobs: %v)", restoreBody, w.Jobs)
	}
	return j
}

// stepIndex is the position of the step whose name contains name; -1 if none.
func stepIndex(steps []wfStep, pred func(wfStep) bool) int {
	for i, s := range steps {
		if pred(s) {
			return i
		}
	}
	return -1
}

func runs(verb string) func(wfStep) bool {
	return func(s wfStep) bool { return strings.Contains(s.Run, verb) }
}

// THE ROOT TOKEN IS ALWAYS REVOKED, after everything that uses it, and before the
// control-plane ACL it needs is closed.
func TestRestoreWorkflowAlwaysRevokesRootAfterTheRestore(t *testing.T) {
	steps := restoreJob(t).Steps
	restore := stepIndex(steps, runs("llz ci openbao-restore --region \"$REGION\" --mode restore"))
	configure := stepIndex(steps, runs("llz ci bao-configure"))
	revoke := stepIndex(steps, runs("llz ci bao-revoke-root"))
	acl := stepIndex(steps, func(s wfStep) bool { return strings.HasSuffix(s.Uses, "/lke-runner-acl") })
	if restore < 0 || configure < 0 || revoke < 0 || acl < 0 {
		t.Fatalf("missing a step: restore=%d configure=%d revoke=%d acl-revoke=%d", restore, configure, revoke, acl)
	}
	if !(restore < configure && configure < revoke && revoke < acl) {
		t.Errorf("order is restore=%d configure=%d revoke-root=%d acl=%d; want restore < configure < revoke-root < acl "+
			"(revoke after every root user, and while the cluster is still reachable)", restore, configure, revoke, acl)
	}
	if !strings.HasPrefix(strings.TrimSpace(steps[revoke].If), "always()") {
		t.Errorf("the root revoke runs `if: %s` — it must be always(), or a failed or cancelled restore leaves a live root token",
			steps[revoke].If)
	}
	if !strings.HasPrefix(strings.TrimSpace(steps[acl].If), "always()") {
		t.Errorf("the ACL revoke runs `if: %s`, want always()", steps[acl].If)
	}
	// Every always() step after the restore must not depend on the restore's
	// outcome — a `success()` there would skip it on the failure it exists for.
	if strings.Contains(steps[revoke].If, "success()") || strings.Contains(steps[revoke].If, "steps.") {
		t.Errorf("the root revoke is conditioned on an outcome (%q)", steps[revoke].If)
	}
}

// THE GATE PRECEDES EVERYTHING THAT CHANGES ANYTHING. The request check — with the
// typed confirmation — is the first step after checkout, runs unconditionally, and
// is handed the confirmation.
func TestRestoreWorkflowChecksTheConfirmationBeforeAnyMutation(t *testing.T) {
	steps := restoreJob(t).Steps
	gate := stepIndex(steps, runs("llz ci openbao-restore --preflight"))
	if gate < 0 {
		t.Fatal("no `llz ci openbao-restore --preflight` step")
	}
	for i := 0; i < gate; i++ {
		if !strings.HasPrefix(steps[i].Uses, "actions/checkout@") {
			t.Errorf("step %d (%q) runs before the request check; only the checkout may", i, steps[i].Name)
		}
	}
	if steps[gate].If != "" {
		t.Errorf("the request check is conditional (`if: %s`) — it must run on every dispatch", steps[gate].If)
	}
	for _, flag := range []string{`--confirm "$CONFIRM"`, `--mode "$MODE"`, `--region "$REGION"`} {
		if !strings.Contains(steps[gate].Run, flag) {
			t.Errorf("the request check is not handed %s:\n%s", flag, steps[gate].Run)
		}
	}
	// And the restore step itself is gated on mode, and carries the confirmation
	// to the verb, which checks it again.
	restore := steps[stepIndex(steps, runs("--mode restore"))]
	if restore.If != "env.MODE == 'restore'" || !strings.Contains(restore.Run, `--confirm "$CONFIRM"`) {
		t.Errorf("restore step: if=%q run=%q", restore.If, restore.Run)
	}
	// The recovery quorum reaches the restore step and no other.
	for i, s := range steps {
		for k, v := range s.Env {
			if strings.Contains(v, "OPENBAO_RECOVERY_KEY") && i != stepIndex(steps, runs("--mode restore")) {
				t.Errorf("step %q is handed %s", s.Name, k)
			}
		}
	}
}

// NOTHING WRITES A TOKEN BACK TO GITHUB: the workflow is not handed the PAT that
// could, and no step writes $GITHUB_ENV or an output.
func TestRestoreWorkflowCannotWriteATokenBack(t *testing.T) {
	w := loadWorkflow(t, restoreBody)
	for _, banned := range []string{"OPENBAO_SECRETS_WRITE_TOKEN", "GH_TOKEN", "GITHUB_ENV", "GITHUB_OUTPUT", "bao-regen-root", "bao-breakglass"} {
		if strings.Contains(withoutComments(w.raw), banned) {
			t.Errorf("%s mentions %s outside a comment — the restore must not be able to persist a root token", restoreBody, banned)
		}
	}
	for name, s := range w.On.Call.Secrets {
		if s.Required {
			t.Errorf("secret %s is required: true — env-scoped secrets cannot cross `secrets: inherit` statically", name)
		}
	}
}

// The configure step reads the token from the file the verb writes — the same
// name, from the same constant.
func TestRestoreWorkflowConfigureReadsTheVerbsTokenFile(t *testing.T) {
	steps := restoreJob(t).Steps
	cfg := steps[stepIndex(steps, runs("llz ci bao-configure"))]
	if !strings.Contains(cfg.Run, `"$RUNNER_TEMP/`+RestoreRootTokenFileName+`"`) {
		t.Errorf("configure step does not read $RUNNER_TEMP/%s:\n%s", RestoreRootTokenFileName, cfg.Run)
	}
	if !strings.Contains(cfg.If, "steps.restore.outcome == 'success'") {
		t.Errorf("configure runs `if: %s` — it must run only after a restore that fully succeeded", cfg.If)
	}
}

// NO DISPATCH INPUT IS INTERPOLATED INTO A SCRIPT, in either file.
func TestRestoreWorkflowRoutesInputsThroughEnv(t *testing.T) {
	expr := regexp.MustCompile(`\$\{\{`)
	for _, f := range []string{restoreBody, restoreStub} {
		w := loadWorkflow(t, f)
		for name, j := range w.Jobs {
			for _, s := range j.Steps {
				if expr.MatchString(s.Run) {
					t.Errorf("%s job %s step %q interpolates an expression into run: — route it through env:", f, name, s.Name)
				}
			}
		}
	}
	if w := loadWorkflow(t, restoreBody); w.Defaults.Run.Shell != "bash" {
		t.Errorf("%s defaults.run.shell = %q, want bash", restoreBody, w.Defaults.Run.Shell)
	}
}

// ONE SERIALIZATION GROUP with bootstrap and break-glass — all three drive the
// generate-root quorum — and none on the stub, which would deadlock.
func TestRestoreWorkflowSharesTheBootstrapGroup(t *testing.T) {
	body := loadWorkflow(t, restoreBody)
	for _, f := range []string{
		"instance-template/.github/workflows/llz-bootstrap-openbao.yml",
		"instance-template/.github/workflows/llz-breakglass-openbao.yml",
	} {
		other := loadWorkflow(t, f)
		if body.Concurrency == nil || other.Concurrency == nil || body.Concurrency.Group != other.Concurrency.Group {
			t.Errorf("concurrency group %v != %s's %v", body.Concurrency, f, other.Concurrency)
		}
	}
	if stub := loadWorkflow(t, restoreStub); stub.Concurrency != nil {
		t.Errorf("%s declares concurrency %q — a caller sharing its reusable's group deadlocks", restoreStub, stub.Concurrency.Group)
	}
	if env := restoreJob(t).Environment; env != "infra-${{ inputs.region }}" {
		t.Errorf("restore job environment = %q, want infra-${{ inputs.region }}", env)
	}
}

// THE SAFE DEFAULT. A dispatch that changes nothing on the form is a dry run.
func TestRestoreWorkflowDefaultsToPlan(t *testing.T) {
	stub := loadWorkflow(t, restoreStub)
	in, ok := stub.On.Dispatch.Inputs["mode"]
	if !ok || in.Type != "choice" || in.Default != "plan" {
		t.Errorf("%s mode input = %+v, want a choice defaulting to plan", restoreStub, in)
	}
	body := loadWorkflow(t, restoreBody)
	if in := body.On.Call.Inputs["mode"]; in.Default != "plan" {
		t.Errorf("%s mode input defaults to %v, want plan", restoreBody, in.Default)
	}
	for _, b := range []string{"latest", "force"} {
		if in := stub.On.Dispatch.Inputs[b]; in.Type != "boolean" || in.Default != false {
			t.Errorf("%s %s input = %+v, want a boolean defaulting to false", restoreStub, b, in)
		}
	}
	// The stub forwards every input it declares to the body, and the body declares
	// every one it is given.
	call := stub.Jobs["call"]
	if call.Uses != "./.github/workflows/llz-restore-openbao.yml" {
		t.Errorf("stub calls %q", call.Uses)
	}
	for name := range stub.On.Dispatch.Inputs {
		if _, ok := call.With[name]; !ok {
			t.Errorf("stub input %s is not forwarded to the body", name)
		}
		if _, ok := body.On.Call.Inputs[name]; !ok {
			t.Errorf("body does not declare input %s", name)
		}
	}
}

func withoutComments(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			lines[i] = ""
		} else if j := strings.Index(l, " #"); j >= 0 {
			lines[i] = l[:j]
		}
	}
	return strings.Join(lines, "\n")
}
