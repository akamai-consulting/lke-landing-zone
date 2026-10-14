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
//   - profile ↔ rotation router: the dry-run property is decided by
//     tokeninv.routeRotation, not by the profile that names the inputs.
//   - profile ↔ lane: a profile the lane never invokes drives nothing, and the
//     delivered-entry-point ledger (internal/cli) counts it as driving.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/tokeninv"
)

const (
	deliveredWorkflowsDir = "../../../../../instance-template/.github/workflows"
	releaseLane           = "../../../../../.github/workflows/release-e2e-lane.yml"
	// What a {deployment} job-name template reads as in the delivered YAML.
	matrixRegionExpr = "${{ matrix.region }}"
)

type wfInput struct {
	Type     string   `yaml:"type"`
	Required bool     `yaml:"required"`
	Options  []string `yaml:"options"`
}

type wfStep struct {
	Name string `yaml:"name"`
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
		forwarded := strings.Join(mapValues(call.With), " ")
		for _, in := range p.Inputs {
			if !strings.Contains(forwarded, "inputs."+in.Key) {
				t.Errorf("%s: %s does not forward input %q to %s (with: %v)", p.Name, p.Workflow, in.Key, p.Body, call.With)
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

// rotationEnvKey maps a secret-rotation.yml dispatch input to the setup step's
// env var that `llz ci rotation-plan` reads. Both sides of this mapping are in
// llz-secret-rotation.yml's route step; the test below checks every key it uses
// is one the profile actually passes.
var rotationEnvKey = map[string]func(*tokeninv.RotationInputs, string){
	"scope":                 func(in *tokeninv.RotationInputs, v string) { in.Scope = v },
	"confirm":               func(in *tokeninv.RotationInputs, v string) { in.Confirm = v },
	"reason":                func(in *tokeninv.RotationInputs, v string) { in.Reason = v },
	"pat-apply":             func(in *tokeninv.RotationInputs, v string) { in.PATApply = v },
	"revoke-apply":          func(in *tokeninv.RotationInputs, v string) { in.RevokeApply = v },
	"tf-state-apply":        func(in *tokeninv.RotationInputs, v string) { in.TFStateApply = v },
	"tf-state-revoke-apply": func(in *tokeninv.RotationInputs, v string) { in.TFStateRevokeApply = v },
	"db-admin-apply":        func(in *tokeninv.RotationInputs, v string) { in.DBAdminApply = v },
}

// TestRotationProfilesRouteToADryRun feeds each rotation profile's REAL inputs
// through the REAL router and requires: routing accepted, nothing armed, no
// lke-admin / propagate / db-admin / state-passphrase, and exactly the one run-*
// output the profile's working job is gated on.
func TestRotationProfilesRouteToADryRun(t *testing.T) {
	body := readDelivered(t, "llz-secret-rotation.yml")
	seen := 0
	for _, p := range instanceWorkflowProfiles {
		if p.Workflow != "secret-rotation.yml" {
			continue
		}
		seen++
		in := tokeninv.RotationInputs{Event: "workflow_dispatch", Region: "primary", Deployments: `["e2e"]`}
		for _, i := range p.Inputs {
			set, ok := rotationEnvKey[i.Key]
			if !ok {
				t.Fatalf("%s: input %q has no router mapping in this test — add it, or the dry-run proof skips it", p.Name, i.Key)
			}
			set(&in, i.Value)
		}
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

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
