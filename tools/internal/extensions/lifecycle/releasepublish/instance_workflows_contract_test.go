package releasepublish

// instance_workflows_contract_test.go — the static half of
// `llz ci assert-instance-workflow`: every profile is checked against the
// DELIVERED workflows and the REAL rotation router, so a rename, a dropped input
// or a routing change fails at PR time instead of fifteen minutes into a release
// run (or, for the rotation fence, instead of minting a credential on a shared
// account).
//
// Three contracts, each split across files that change for different reasons:
//
//   - profile ↔ delivered YAML: the job and step names the verb waits for are
//     restated in the profile; the delivered workflow is where they live.
//   - profile ↔ rotation router: the dry-run property is decided by the
//     delivered Setup step's env, read by tokeninv.InputsFromLookup and routed by
//     tokeninv.routeRotation — not by the profile that names the inputs.
//   - profile ↔ credential verbs: the record a profile's job must have logged is
//     produced by the real `llz credentials` verbs and judged by the real check.
//   - profile ↔ lane: a profile the lane never invokes drives nothing, and the
//     delivered-entry-point ledger (internal/cli) counts it as driving.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertsuite"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/tokeninv"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
)

const (
	deliveredWorkflowsDir = "../../../../../instance-template/.github/workflows"
	releaseLane           = "../../../../../.github/workflows/release-e2e-lane.yml"
	e2eInstantiate        = "../../../../../.github/workflows/e2e-instantiate.yml"
	// What a {deployment} job-name template reads as in the delivered YAML.
	matrixRegionExpr = "${{ matrix.region }}"
)

type wfInput struct {
	Type     string   `yaml:"type"`
	Required bool     `yaml:"required"`
	Options  []string `yaml:"options"`
	Default  any      `yaml:"default"`
}

type wfStep struct {
	ID   string            `yaml:"id"`
	Name string            `yaml:"name"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

type wfJob struct {
	Name  string            `yaml:"name"`
	If    string            `yaml:"if"`
	Uses  string            `yaml:"uses"`
	With  map[string]string `yaml:"with"`
	Steps []wfStep          `yaml:"steps"`
}

type wfFile struct {
	On struct {
		WorkflowDispatch *struct {
			Inputs map[string]wfInput `yaml:"inputs"`
		} `yaml:"workflow_dispatch"`
		WorkflowCall *struct {
			Inputs map[string]wfInput `yaml:"inputs"`
		} `yaml:"workflow_call"`
	} `yaml:"on"`
	Jobs map[string]wfJob `yaml:"jobs"`
}

func readDelivered(t *testing.T, name string) wfFile {
	t.Helper()
	path := filepath.Join(deliveredWorkflowsDir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var f wfFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(f.Jobs) == 0 {
		t.Fatalf("%s parsed to no jobs — the reader is broken, and every check below would pass vacuously", path)
	}
	return f
}

// jobNamed finds the delivered job whose `name:` is the profile's template, with
// {deployment} standing for the matrix expression the YAML spells it as.
func jobNamed(f wfFile, tmpl string) (wfJob, bool) {
	want := strings.ReplaceAll(tmpl, deploymentToken, matrixRegionExpr)
	for _, j := range f.Jobs {
		if j.Name == want {
			return j, true
		}
	}
	return wfJob{}, false
}

func deliveredJobNames(f wfFile) string {
	var names []string
	for id, j := range f.Jobs {
		names = append(names, id+"="+j.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// TestInstanceWorkflowProfilesMatchTheDeliveredWorkflows — every input, job and
// step a profile names exists where the delivered tree declares it.
func TestInstanceWorkflowProfilesMatchTheDeliveredWorkflows(t *testing.T) {
	if len(instanceWorkflowProfiles) < 2 {
		t.Fatalf("only %d profile(s) — the table was emptied and this test would pass vacuously", len(instanceWorkflowProfiles))
	}
	for _, p := range instanceWorkflowProfiles {
		stub := readDelivered(t, p.Workflow)
		if stub.On.WorkflowDispatch == nil {
			t.Errorf("%s: %s has no workflow_dispatch trigger — the verb cannot run it", p.Name, p.Workflow)
			continue
		}
		declared := stub.On.WorkflowDispatch.Inputs
		given := map[string]string{}
		for _, in := range p.Inputs {
			given[in.Key] = in.Value
			d, ok := declared[in.Key]
			if !ok {
				t.Errorf("%s: input %q is not declared by %s's workflow_dispatch — GitHub rejects the "+
					"dispatch with HTTP 422 'Unexpected inputs'", p.Name, in.Key, p.Workflow)
				continue
			}
			switch d.Type {
			case "choice":
				if !contains(d.Options, in.Value) {
					t.Errorf("%s: %s=%q is not one of %s's options %v", p.Name, in.Key, in.Value, p.Workflow, d.Options)
				}
			case "boolean":
				if in.Value != "true" && in.Value != "false" {
					t.Errorf("%s: boolean input %s=%q", p.Name, in.Key, in.Value)
				}
			}
		}
		for key, d := range declared {
			if _, ok := given[key]; d.Required && !ok {
				t.Errorf("%s: %s requires input %q and the profile does not pass it", p.Name, p.Workflow, key)
			}
		}

		// The stub calls the body the profile names, and forwards every input the
		// profile sets — an input dispatched but not forwarded is an input the body
		// defaults, which is how a "dry run" stops being one without anything here
		// noticing.
		var call *wfJob
		for _, j := range stub.Jobs {
			if strings.HasSuffix(j.Uses, "/"+p.Body) {
				call = &j
				break
			}
		}
		if call == nil {
			t.Errorf("%s: %s no longer calls %s — the profile's expected jobs are read from the wrong body", p.Name, p.Workflow, p.Body)
			continue
		}
		// PER KEY: with[k] must read inputs.k. A search over the joined values
		// would pass `pat-apply: ${{ inputs.revoke-apply }}` (the key is
		// mentioned somewhere) and `tf-state-apply` against `inputs.tf-state-apply-x`.
		for _, in := range p.Inputs {
			if !forwardsInput(call.With[in.Key], in.Key) {
				t.Errorf("%s: %s does not forward input %q as %s's %q (with[%q] = %q)", p.Name, p.Workflow,
					in.Key, p.Body, in.Key, in.Key, call.With[in.Key])
			}
		}

		body := readDelivered(t, p.Body)
		for _, ej := range p.Jobs {
			src := body
			srcName := p.Body
			if ej.Source != "" {
				src, srcName = readDelivered(t, ej.Source), ej.Source
			}
			j, ok := jobNamed(src, ej.Name)
			if !ok {
				t.Errorf("%s: no job named %q in %s — the verb would report it as never appeared. Jobs there: %s",
					p.Name, strings.ReplaceAll(ej.Name, deploymentToken, matrixRegionExpr), srcName, deliveredJobNames(src))
				continue
			}
			if len(ej.Steps) == 0 {
				t.Errorf("%s: job %q asserts no step — a job that succeeded having run no check is the vacuous green "+
					"this verb exists to refuse", p.Name, ej.Name)
			}
			for _, s := range ej.Steps {
				found := false
				for _, st := range j.Steps {
					found = found || st.Name == s
				}
				if !found {
					var have []string
					for _, st := range j.Steps {
						have = append(have, st.Name)
					}
					t.Errorf("%s: job %q in %s has no step %q. Steps there: %q", p.Name, ej.Name, srcName, s, have)
				}
			}
		}
		// A fence over a job name that no longer exists guards nothing.
		for _, m := range p.MustNotRun {
			if _, ok := jobNamed(body, m); !ok {
				t.Errorf("%s: mustNotRun job %q is not in %s — renamed? The dry-run fence would silently stop "+
					"covering it. Jobs there: %s", p.Name, m, p.Body, deliveredJobNames(body))
			}
		}
	}
}

// forwardsInput reports whether a `with:` value reads inputs.<key> — that key,
// not a longer name it prefixes.
func forwardsInput(value, key string) bool {
	return regexp.MustCompile(`\binputs\.` + regexp.QuoteMeta(key) + `(?:[^A-Za-z0-9_-]|$)`).MatchString(value)
}

// ── A model of the GitHub expressions these two files use ────────────────────
//
// Just enough to evaluate the stub's `with:` and the Setup step's `env:` for one
// dispatch: `${{ a || b || 'lit' }}` over context paths, quoted strings and
// booleans, with GitHub's truthiness. Anything else is an ERROR, not a guess: a
// new expression in either file has to be modelled here before the dry-run proof
// can claim to cover it.

var wholeExprRe = regexp.MustCompile(`^\$\{\{\s*(.*?)\s*\}\}$`)

func ghaTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	default:
		return true
	}
}

func ghaString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case bool:
		if x {
			return "true"
		}
		return "false"
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

func evalGHA(raw string, ctx map[string]any) (any, error) {
	raw = strings.TrimSpace(raw)
	m := wholeExprRe.FindStringSubmatch(raw)
	if m == nil {
		if strings.Contains(raw, "${{") {
			return nil, fmt.Errorf("expression embedded in a string is not modelled: %q", raw)
		}
		return raw, nil // a literal (the stub's `<@ instance_repo @>` included)
	}
	var last any
	for _, term := range strings.Split(m[1], "||") {
		term = strings.TrimSpace(term)
		var v any
		switch {
		case term == "true":
			v = true
		case term == "false":
			v = false
		case len(term) >= 2 && strings.HasPrefix(term, "'") && strings.HasSuffix(term, "'"):
			v = strings.ReplaceAll(term[1:len(term)-1], "''", "'")
		default:
			got, ok := ctx[term]
			if !ok {
				return nil, fmt.Errorf("expression term %q in %q is not modelled (or names an undeclared input)", term, raw)
			}
			v = got
		}
		last = v
		if ghaTruthy(v) {
			return v, nil
		}
	}
	return last, nil
}

// typed converts a value to the type GitHub gives an input of declared type t.
func typed(t string, v any) any {
	if s, ok := v.(string); ok && t == "boolean" {
		return s == "true"
	}
	return v
}

// setupEnvFor evaluates the delivered Setup route step's env for one dispatch of
// p, through the stub's `with:` into the body's inputs — every hop the dispatch
// takes in production.
func setupEnvFor(t *testing.T, p workflowProfile, stub, body wfFile) (map[string]string, wfStep) {
	t.Helper()
	// 1. The dispatch: the profile's inputs, typed as the stub declares them,
	//    and the stub's defaults for the rest.
	declared := stub.On.WorkflowDispatch.Inputs
	dispatch := map[string]any{}
	for name, d := range declared {
		dispatch["inputs."+name] = typed(d.Type, d.Default)
	}
	for _, in := range p.Inputs {
		dispatch["inputs."+in.Key] = typed(declared[in.Key].Type, in.Value)
	}
	// 2. The stub's call: with: evaluated against the dispatch, typed as the body
	//    declares them, body defaults for anything not passed.
	var call wfJob
	for _, j := range stub.Jobs {
		if strings.HasSuffix(j.Uses, "/"+p.Body) {
			call = j
		}
	}
	if body.On.WorkflowCall == nil {
		t.Fatalf("%s declares no workflow_call inputs", p.Body)
	}
	bodyIn := map[string]any{}
	for name, d := range body.On.WorkflowCall.Inputs {
		bodyIn["inputs."+name] = typed(d.Type, d.Default)
	}
	for k, raw := range call.With {
		d, ok := body.On.WorkflowCall.Inputs[k]
		if !ok {
			t.Fatalf("%s passes %q, which %s does not declare", p.Workflow, k, p.Body)
		}
		v, err := evalGHA(raw, dispatch)
		if err != nil {
			t.Fatalf("%s with.%s: %v", p.Workflow, k, err)
		}
		bodyIn["inputs."+k] = typed(d.Type, v)
	}
	// 3. The route step's env, against the body's inputs and the run context.
	bodyIn["github.event_name"] = "workflow_dispatch"
	bodyIn["github.event.schedule"] = nil
	bodyIn["github.actor"] = "release-e2e"
	bodyIn["needs.discover.outputs.deployments"] = `["e2e"]`
	var route wfStep
	found := false
	for _, j := range body.Jobs {
		if j.Name != "Setup" {
			continue
		}
		for _, st := range j.Steps {
			if st.ID == "route" {
				route, found = st, true
			}
		}
	}
	if !found || len(route.Env) == 0 {
		t.Fatalf("%s: no Setup step with id `route` and an env: — the dry-run proof has nothing to evaluate", p.Body)
	}
	env := map[string]string{}
	for k, raw := range route.Env {
		v, err := evalGHA(raw, bodyIn)
		if err != nil {
			t.Fatalf("%s Setup env %s: %v", p.Body, k, err)
		}
		env[k] = ghaString(v)
	}
	return env, route
}

// TestRotationProfilesRouteToADryRun evaluates the DELIVERED Setup step's env for
// each rotation profile's dispatch, feeds it through the REAL env reader and the
// REAL router, and requires: routing accepted, nothing armed, no lke-admin /
// propagate / db-admin / state-passphrase, and exactly the one run-* output the
// profile's working job is gated on.
//
// Nothing in this test maps an input to a router field by hand. It used to — a
// table restating which env var each input became — and that table was the one
// part of the fence nothing checked: an input wired to the wrong variable in the
// delivered YAML would have been routed correctly HERE.
func TestRotationProfilesRouteToADryRun(t *testing.T) {
	body := readDelivered(t, "llz-secret-rotation.yml")
	stub := readDelivered(t, "secret-rotation.yml")
	seen := 0
	for _, p := range instanceWorkflowProfiles {
		if p.Workflow != "secret-rotation.yml" {
			continue
		}
		seen++
		env, route := setupEnvFor(t, p, stub, body)
		// The reader below is the one the step runs — check that it is.
		if strings.TrimSpace(route.Run) != "llz ci rotation-plan" {
			t.Fatalf("the Setup route step runs %q, not `llz ci rotation-plan` — tokeninv.InputsFromEnv is not what reads this env", route.Run)
		}
		in := tokeninv.InputsFromLookup(func(k string) string { return env[k] })
		lines, err := tokeninv.RouteOutputs(in)
		if err != nil {
			t.Errorf("%s: the router REFUSES these inputs (%v) — the dispatch would fail at Setup", p.Name, err)
			continue
		}
		var on []string
		for _, l := range lines {
			k, v, _ := strings.Cut(l, "=")
			if strings.HasSuffix(k, "-apply") && v != "false" {
				t.Errorf("%s: the router ARMS %s for these inputs — the release gate would mutate real credentials on a shared account", p.Name, k)
			}
			if strings.HasPrefix(k, "run-") && v == "true" {
				on = append(on, k)
			}
		}
		if len(on) != 1 || on[0] != p.RouteOutput {
			t.Errorf("%s: the router turns on %v; want exactly [%s]", p.Name, on, p.RouteOutput)
		}
		// And the job the profile asserts really is the one that output gates.
		work := p.Jobs[len(p.Jobs)-1]
		j, ok := jobNamed(body, work.Name)
		if !ok {
			continue // reported by the delivered-workflows test
		}
		if !strings.Contains(j.If, "needs.setup.outputs."+p.RouteOutput) {
			t.Errorf("%s: job %q is not gated on needs.setup.outputs.%s (if: %q) — the profile asserts a job "+
				"the routed output does not select", p.Name, work.Name, p.RouteOutput, j.If)
		}
	}
	if seen < 4 {
		t.Fatalf("only %d rotation profile(s) — the dry-run proof covered less than it did", seen)
	}
}

// The evaluator above is load-bearing, so its own refusals are pinned: an
// unmodelled term or an embedded expression must stop the proof, not pass it.
func TestExpressionModelRefusesWhatItDoesNotModel(t *testing.T) {
	ctx := map[string]any{"inputs.a": false, "inputs.b": "x"}
	for raw, want := range map[string]string{
		"${{ inputs.a || 'd' }}":   "d",
		"${{ inputs.b || 'd' }}":   "x",
		"${{ inputs.a || false }}": "false",
		"literal":                  "literal",
	} {
		v, err := evalGHA(raw, ctx)
		if err != nil || ghaString(v) != want {
			t.Errorf("evalGHA(%q) = %v, %v; want %q", raw, v, err, want)
		}
	}
	for _, raw := range []string{"${{ inputs.nope }}", "${{ secrets.X }}", "pre-${{ inputs.b }}"} {
		if _, err := evalGHA(raw, ctx); err == nil {
			t.Errorf("evalGHA(%q) guessed instead of refusing", raw)
		}
	}
	if !forwardsInput("${{ inputs.tf-state-apply || false }}", "tf-state-apply") ||
		forwardsInput("${{ inputs.tf-state-apply-x }}", "tf-state-apply") ||
		forwardsInput("${{ inputs.revoke-apply }}", "pat-apply") {
		t.Error("forwardsInput must match the key exactly")
	}
}

var laneProfileRe = regexp.MustCompile(`assert-instance-workflow\b[^\n]*--profile\s+"?([a-z0-9-]+)`)

// TestEveryProfileIsInvokedByTheLane — a profile nothing runs drives nothing, and
// the internal/cli coverage ledger counts a profile's workflow as DRIVEN. Both
// directions: an invocation of a profile that does not exist fails at runtime,
// fifteen minutes into a release run, so it fails here first.
func TestEveryProfileIsInvokedByTheLane(t *testing.T) {
	raw, err := os.ReadFile(releaseLane)
	if err != nil {
		t.Fatalf("read %s: %v", releaseLane, err)
	}
	invoked := map[string]bool{}
	for _, m := range laneProfileRe.FindAllStringSubmatch(string(raw), -1) {
		invoked[m[1]] = true
	}
	if len(invoked) == 0 {
		t.Fatalf("%s invokes `llz ci assert-instance-workflow` with no --profile at all — the day-2 jobs are gone "+
			"and the ledger in internal/cli would still call their workflows DRIVEN", releaseLane)
	}
	for _, p := range instanceWorkflowProfiles {
		if !invoked[p.Name] {
			t.Errorf("profile %q is never invoked by %s — nothing dispatches %s", p.Name, releaseLane, p.Workflow)
		}
		delete(invoked, p.Name)
	}
	for name := range invoked {
		t.Errorf("%s invokes --profile %q, which does not exist (known: %v)", releaseLane, name, profileNames())
	}
}

// ── profile ↔ credential verbs: the record is the REAL verbs' ───────────────

// fakeCredLinode answers the credential verbs' Linode calls without a network.
type fakeCredLinode struct{ buckets []map[string]any }

func (f *fakeCredLinode) CreateProfileToken(context.Context, string, string, string) (map[string]any, error) {
	return nil, fmt.Errorf("a dry run must not mint")
}
func (f *fakeCredLinode) ListProfileTokens(context.Context) ([]map[string]any, error) {
	return nil, nil
}
func (f *fakeCredLinode) DeleteProfileToken(context.Context, uint64) error {
	return fmt.Errorf("a dry run must not revoke")
}
func (f *fakeCredLinode) CreateObjectStorageKey(context.Context, string, string, string, string) (map[string]any, error) {
	return nil, fmt.Errorf("a dry run must not mint")
}
func (f *fakeCredLinode) ListObjectStorageKeys(context.Context) ([]map[string]any, error) {
	return []map[string]any{{"id": json.Number("7"), "label": "llz-llz-e2e-tf-state-key"}}, nil
}
func (f *fakeCredLinode) DeleteObjectStorageKey(context.Context, uint64) error {
	return fmt.Errorf("a dry run must not revoke")
}
func (f *fakeCredLinode) ListObjectStorageBuckets(context.Context) ([]map[string]any, error) {
	return f.buckets, nil
}

// fixtureSpec is the e2e instance's spec in the shape that matters here: the
// broad-PAT rotator enabled with an e2e-unique label, as e2e-instantiate.yml sets.
const (
	fixtureLandingZone = "apiVersion: llz.akamai.com/v1alpha1\nkind: LandingZone\nmetadata:\n  name: e2e\nspec:\n  instance:\n    objLabelPrefix: llz-e2e\n"
	fixtureEnv         = "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: ClusterDefinition\nmetadata:\n  name: e2e\nspec:\n  components:\n    broadPatRotator:\n      enabled: true\n      broadPATLabel: llz-e2e-broad-pat\n"
)

// TestCredentialRecordsTheVerbsEmitPassTheirProfile — archetype ②. Each rotation
// profile's job is judged on the record its verb logged. This runs the REAL
// `llz credentials` verbs, dry, in an instance shaped like the e2e fixture, and
// hands what they wrote to the job log to the REAL profile check. Neither side's
// rule is restated: a verb that stops logging, renames its event, drops its
// probe or stops standing down fails here, at PR time.
func TestCredentialRecordsTheVerbsEmitPassTheirProfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "environments"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"landingzone.yaml": fixtureLandingZone, "environments/e2e.yaml": fixtureEnv} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	t.Setenv("LINODE_TOKEN", "fake")
	t.Setenv("ROTATION_APPLY", "false")
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(dir, "summary.md"))
	t.Setenv("TF_STATE_ENDPOINT", "https://us-ord-1.linodeobjects.com")

	fake := &fakeCredLinode{buckets: []map[string]any{{"label": "llz-e2e-tfstate", "region": "us-ord", "cluster": "us-ord-1"}}}
	origPAT, origObj := credrotate.NewPATClient, credrotate.NewObjKeyClient
	credrotate.NewPATClient = func(string) credrotate.PATAPI { return fake }
	credrotate.NewObjKeyClient = func(string) credrotate.ObjKeyAPI { return fake }
	t.Cleanup(func() { credrotate.NewPATClient, credrotate.NewObjKeyClient = origPAT, origObj })

	// The same flags the delivered linode-credentials action passes (label empty:
	// derived; bucket from vars.TF_STATE_BUCKET).
	verbs := map[string]func() (interface{ Execute() error }, []string){
		"rotation-linode-pat": func() (interface{ Execute() error }, []string) {
			return credrotate.CredentialsPATCreateCmd(&credrotate.Opts{}), []string{"--label", "", "--scopes", "s", "--validity-days", "90"}
		},
		"rotation-linode-pat-revoke": func() (interface{ Execute() error }, []string) {
			return credrotate.CredentialsPATRevokeOldCmd(&credrotate.Opts{}), []string{"--label", "", "--grace-days", "7"}
		},
		"rotation-tf-state-key": func() (interface{ Execute() error }, []string) {
			return credrotate.CredentialsObjKeyCreateCmd(&credrotate.Opts{}), []string{"--label", "", "--bucket-cluster", "", "--bucket-name", "llz-e2e-tfstate", "--bucket-permissions", "read_write"}
		},
		"rotation-tf-state-key-revoke": func() (interface{ Execute() error }, []string) {
			return credrotate.CredentialsObjKeyRevokeOldCmd(&credrotate.Opts{}), []string{"--label", "", "--keep-newest", "2"}
		},
	}
	checked := 0
	for _, p := range instanceWorkflowProfiles {
		if p.Workflow != "secret-rotation.yml" {
			continue
		}
		mk, ok := verbs[p.Name]
		if !ok {
			t.Errorf("%s: no verb wired in this test — its record check is unproven", p.Name)
			continue
		}
		var buf bytes.Buffer
		origLog := cli.RecordLog
		cli.RecordLog = &buf
		c, args := mk()
		cmd := c.(interface {
			Execute() error
			SetArgs([]string)
		})
		cmd.SetArgs(args)
		var execErr error
		captureStdout(t, func() { execErr = cmd.Execute() })
		cli.RecordLog = origLog
		if execErr != nil {
			t.Errorf("%s: the dry-run verb failed: %v", p.Name, execErr)
			continue
		}
		// What GitHub serves back: timestamped lines.
		var log strings.Builder
		for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			log.WriteString("2026-10-15T01:02:03.0000000Z " + l + "\n")
		}
		work := p.Jobs[len(p.Jobs)-1]
		if work.Log == nil {
			t.Errorf("%s: its working job judges no log", p.Name)
			continue
		}
		if probs := work.Log(log.String(), testInstance); len(probs) != 0 {
			t.Errorf("%s: the REAL verb's record fails the profile's check: %v\nlog:\n%s", p.Name, probs, log.String())
		}
		checked++
	}
	if checked < 4 {
		t.Fatalf("only %d rotation profile(s) checked against a real verb", checked)
	}
}

// TestBroadPATProfilesMatchTheFixture holds three facts together: the e2e
// fixture enables the in-cluster broadPatRotator, so the two broad-PAT profiles
// assert a STAND-DOWN (not a dry run) — and the PAT rotation that does happen is
// gated by the converge battery's broad-pat lane. Drop the fixture's enable and
// the profiles would demand a stand-down that cannot happen; drop the lane and
// nothing in e2e rotates the broad PAT at all while these profiles stay green.
func TestBroadPATProfilesMatchTheFixture(t *testing.T) {
	raw, err := os.ReadFile(e2eInstantiate)
	if err != nil {
		t.Fatalf("read %s: %v", e2eInstantiate, err)
	}
	enabled := strings.Contains(string(raw), "components.broadPatRotator.enabled=true")
	for _, name := range []string{"rotation-linode-pat", "rotation-linode-pat-revoke"} {
		p := mustProfile(t, name)
		log := recordLog(map[string]any{"event": expectationFor[name].Event, "skipped": cli.BroadPATStandDown})
		standsDown := len(p.Jobs[len(p.Jobs)-1].Log(log, testInstance)) == 0
		if enabled != standsDown {
			t.Errorf("%s: fixture enables broadPatRotator=%v but the profile accepts a stand-down=%v — they must agree",
				name, enabled, standsDown)
		}
	}
	if !enabled {
		return
	}
	for _, l := range assertsuite.Lanes("e2e") {
		for _, st := range l.Steps {
			if len(st.Argv) > 0 && st.Argv[0] == "assert-broad-pat-rotation" && l.Gating {
				return
			}
		}
	}
	t.Error("no GATING assert-suite lane runs assert-broad-pat-rotation — with the CI PAT verbs standing down, " +
		"nothing in e2e would exercise a broad-PAT rotation at all")
}

// The fixture's .copier-answers.yml must carry provenance, or template-drift
// compares the instance with itself — which the scheduled-checks profile now
// fails on at runtime. This holds the producer half at PR time.
func TestE2EFixtureAnswersCarryTemplateProvenance(t *testing.T) {
	raw, err := os.ReadFile(e2eInstantiate)
	if err != nil {
		t.Fatalf("read %s: %v", e2eInstantiate, err)
	}
	for _, key := range []string{"_src_path: ", "_commit: "} {
		if !strings.Contains(string(raw), "'"+key) && !strings.Contains(string(raw), `"`+key) {
			t.Errorf("%s writes the e2e instance's .copier-answers.yml without %q — `llz drift` then falls back to "+
				"the instance's own remote and template-drift compares it with itself", e2eInstantiate, strings.TrimSpace(key))
		}
	}
}
