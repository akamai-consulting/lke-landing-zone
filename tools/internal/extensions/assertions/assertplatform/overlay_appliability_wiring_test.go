package assertplatform

// overlay_appliability_wiring_test.go holds the lane to actually RUNNING.
//
// A GATE NOBODY GATES CAN BE DELETED IN SILENCE. Before this test, removing the
// three lint.yml steps that drive `llz ci assert-overlay-appliability` left
// `go test ./...`, `make lint`, `llz ci gates` and the core-surface budget all
// green — the verb would still exist, still be unit-tested, still be registered,
// and simply never be pointed at an apiserver again. Every unit test here passes
// with the lane switched off, because none of them needs a cluster; that is
// exactly why they cannot be what keeps it wired.
//
// THE QUIET WAYS OFF, all of which this refuses, follow
// runinjection/guard_test.go's precedent: the step disappears, the step becomes
// conditional or continue-on-error, the JOB does, the step's `run:` ends in
// `|| true`, the verb is renamed out from under the YAML, or the steps stay but
// the fixtures that feed them stop being applied. The last one matters most: the
// lane treats an absent object as FATAL, so a missing apply step is at least loud
// — but a missing EMIT step with the apply still present would fail on a stale
// file, which reads as an infrastructure problem rather than a gate someone
// unwired.
//
// THE JOB-LEVEL ONES ARE NOT HYPOTHETICAL. lint.yml's `kubernetes` and `terraform`
// jobs both already carry a fork guard —
// `if: github.event_name != 'pull_request' || …head.repo.full_name == …repository`
// — that `dry-run` does not. Adding it there, a plausible hardening that matches
// two siblings in the same file, would switch this lane off for every fork PR
// while a test that only reads steps stayed green.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type wiringWorkflow struct {
	Jobs map[string]wiringJob `json:"jobs"`
}

type wiringJob struct {
	// A CONDITION OR A CONTINUE-ON-ERROR ON THE JOB switches every step inside it
	// off at once, and reading only the steps could not see it.
	If              string `json:"if"`
	ContinueOnError any    `json:"continue-on-error"`
	Steps           []struct {
		Name            string `json:"name"`
		Run             string `json:"run"`
		If              string `json:"if"`
		ContinueOnError any    `json:"continue-on-error"`
	} `json:"steps"`
}

// appliabilityVerb is the verb as COBRA REGISTERS IT, not as the workflow spells
// it. Reading it off the command coupled the two: renaming the cobra `Use` while
// lint.yml kept the old string used to leave every test green and the workflow
// invoking a verb that no longer exists — a `llz ci` unknown-subcommand failure
// six minutes into CI, or worse, an exit 0 if the group ever stops being strict.
var appliabilityVerb = "ci " + strings.Fields(OverlayAppliabilityCmd().Use)[0]

// readWiringWorkflow loads lint.yml once for both tests.
func readWiringWorkflow(t *testing.T) wiringWorkflow {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "..", ".github", "workflows", "lint.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf wiringWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf
}

// swallowsFailure reports whether a `run:` ends by discarding its own exit status.
// `|| true` is the quietest way of all: the step is present, unconditional, and
// its failure means nothing.
func swallowsFailure(run string) bool {
	for _, line := range strings.Split(run, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasSuffix(l, "|| true") || strings.HasSuffix(l, "|| :") ||
			strings.HasSuffix(l, "|| exit 0") {
			return true
		}
	}
	return false
}

func TestTheAppliabilityLaneIsActuallyRunSomewhere(t *testing.T) {
	wf := readWiringWorkflow(t)

	// The verb, and the emit that feeds it. Both, because either alone is a lane
	// that cannot do its job.
	verb := appliabilityVerb
	var gateSteps, emitSteps int
	for job, j := range wf.Jobs {
		// THE JOB FIRST. Everything below is about a step, and a step's guarantees are
		// worth nothing if the job holding it never runs.
		hostsLane := false
		for _, s := range j.Steps {
			if strings.Contains(s.Run, verb) {
				hostsLane = true
			}
		}
		if hostsLane {
			if j.If != "" {
				t.Errorf("job %s hosts the appliability lane and runs under a condition (if: %q). "+
					"Two sibling jobs in this same file carry a fork guard shaped exactly like that, "+
					"so this is the likeliest way the lane goes quiet — and a step-level check "+
					"cannot see it", job, j.If)
			}
			if j.ContinueOnError != nil && j.ContinueOnError != false {
				t.Errorf("job %s hosts the appliability lane with continue-on-error at the JOB level, "+
					"which makes every gate in it advisory at once", job)
			}
		}
		for _, s := range j.Steps {
			if !strings.Contains(s.Run, verb) {
				continue
			}
			if swallowsFailure(s.Run) {
				t.Errorf("job %s runs %q and discards its exit status (`|| true` or equivalent) — the "+
					"step is present, unconditional, and its failure means nothing", job, verb)
			}
			if s.If != "" {
				t.Errorf("job %s runs %q under a condition (if: %q) — a gate that can be skipped is a "+
					"gate that will be", job, verb, s.If)
			}
			if s.ContinueOnError != nil && s.ContinueOnError != false {
				t.Errorf("job %s runs %q with continue-on-error — its failure would not fail the job, "+
					"which is the quietest way there is to switch a gate off", job, verb)
			}
			if strings.Contains(s.Run, "--emit-fixtures") {
				emitSteps++
				continue
			}
			gateSteps++
		}
	}
	if emitSteps == 0 {
		t.Errorf("no lint.yml step runs `llz %s --emit-fixtures` — without the fixtures the lane has "+
			"no pre-overlay object to probe and every row reports an absent one", verb)
	}
	if gateSteps == 0 {
		t.Fatalf("no lint.yml step runs `llz %s` — the verb exists, is registered and is unit-tested, "+
			"and is pointed at no apiserver. Every other test in this package passes with the lane "+
			"switched off, so this is the only thing keeping it wired", verb)
	}
}

func TestTheFixturesAreAppliedBetweenTheEmitAndTheGate(t *testing.T) {
	// ORDER IS PART OF THE WIRING. Emit → apply → gate. Any other order leaves the
	// gate probing an object that does not exist yet, or a stale one from a previous
	// step, and the lane's absent-object arm would report the fixture step as
	// missing when it is merely late.
	wf := readWiringWorkflow(t)
	found := false
	for job, j := range wf.Jobs {
		emit, apply, gate := -1, -1, -1
		for i, s := range j.Steps {
			switch {
			case strings.Contains(s.Run, "assert-overlay-appliability") && strings.Contains(s.Run, "--emit-fixtures"):
				emit = i
			case strings.Contains(s.Run, "assert-overlay-appliability"):
				gate = i
			case strings.Contains(s.Run, "kubectl apply") && strings.Contains(s.Run, "overlay-fixtures"):
				apply = i
			}
		}
		if emit < 0 && gate < 0 {
			continue
		}
		found = true
		if apply < 0 {
			t.Errorf("job %s emits the fixtures but never applies them — the gate would probe an object "+
				"nothing created", job)
			continue
		}
		if !(emit < apply && apply < gate) {
			t.Errorf("job %s runs the appliability steps in the order emit=%d apply=%d gate=%d; it must "+
				"be emit → apply → gate", job, emit, apply, gate)
		}
	}
	if !found {
		t.Fatal("no job runs the appliability lane at all — see TestTheAppliabilityLaneIsActuallyRunSomewhere")
	}
}
