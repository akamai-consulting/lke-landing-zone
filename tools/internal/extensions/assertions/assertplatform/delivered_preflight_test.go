package assertplatform

// delivered_preflight_test.go — A PREFLIGHT NOTHING RUNS IS NOT A PREFLIGHT.
//
// Both lanes in this file are correct, unit-tested, and worth nothing unless the
// delivered pipeline invokes them BEFORE the ~15-minute cluster apply. That is not
// a property of this package: it lives in a YAML file in another tree, edited by
// different hands, and it is exactly the claim that has failed here before —
// `doctor` already reported the k8sVersion question advisorily, and the reason a
// bad pin still cost a release-e2e round on 2026-08-11 is that nothing ran doctor
// before an apply.
//
// So this couples the two: the verbs exist here, the job that must run them is
// pinned there. Moving a step out of apply-vpc — into the cluster job, or into a
// job that only runs on `apply` — fails this test rather than quietly restoring
// the ~15-minute feedback loop.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const deliveredPipeline = "../../../../../instance-template/.github/workflows/llz-terraform.yml"

// applyVPCJob returns the body of the apply-vpc job — THE job apply-cluster
// depends on, and therefore the only place a check is guaranteed to land before
// the cluster is created.
func applyVPCJob(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(deliveredPipeline)
	if err != nil {
		t.Fatalf("read %s: %v", deliveredPipeline, err)
	}
	body := string(raw)
	loc := regexp.MustCompile(`(?m)^  apply-vpc:\s*$`).FindStringIndex(body)
	if loc == nil {
		t.Fatalf("no `apply-vpc:` job in %s — the front-loaded preflight battery has moved, "+
			"and this test would pass having read nothing", deliveredPipeline)
	}
	rest := body[loc[1]:]
	if end := regexp.MustCompile(`(?m)^  [a-zA-Z_-]+:`).FindStringIndex(rest); end != nil {
		rest = rest[:end[0]]
	}
	return rest
}

// runsVerb finds the `run:` line invoking verb, and returns its offset in job.
//
// IT MATCHES THE run: LINE, NOT THE VERB ANYWHERE. Both tests here used to search
// the whole job body, comments included — so a single comment naming
// `llz ci assert-k8s-version` (and this job's comments are long enough to want
// one) would satisfy the presence check with the STEP DELETED, and would send the
// `if:` check off to scan whatever step the comment happened to sit above. Both
// arms this file was verified failing on would have gone vacuous, silently, from
// an edit that looks like documentation. This repo has met the comments-read-as-
// commands trap before, from the other side: TestDeliveredWorkflowCommands
// resolves prose inside `run:` blocks as real invocations.
func runsVerb(job, verb string) int {
	return strings.Index(job, "run: "+verb)
}

func TestSpecPreflightsRunBeforeTheClusterApply(t *testing.T) {
	job := applyVPCJob(t)
	for verb, why := range map[string]string{
		"llz ci assert-apl-version": "an unsupported apl-core chart surfaces ~2h in as CreateContainerConfigError pods",
		"llz ci assert-k8s-version": "a k8sVersion this account cannot build fails ~15 min into the cluster apply, " +
			"and apply-vpc is the job apply-cluster needs — moving it elsewhere restores that wait",
	} {
		if runsVerb(job, verb) < 0 {
			t.Errorf("the apply-vpc job no longer RUNS `%s` — %s. "+
				"Both are answerable in seconds from the spec plus one API call; that is the "+
				"entire reason this job front-loads them.", verb, why)
		}
	}
}

// The battery is NOT gated on `action`. A `plan` that would apply against a pin
// the account cannot build should say so now — the check costs one HTTP request,
// and an operator who plans today applies tomorrow.
func TestTheVersionPreflightIsNotGatedOnApply(t *testing.T) {
	job := applyVPCJob(t)
	i := runsVerb(job, "llz ci assert-k8s-version")
	if i < 0 {
		t.Fatal("the version preflight is gone; TestSpecPreflightsRunBeforeTheClusterApply says why that matters")
	}
	// THE WHOLE STEP, not name→run. The first draft scanned only as far as the
	// `run:` line, on the assumption that `if:` precedes it — but a step is a YAML
	// MAPPING and its keys are unordered, so `if:` written after `run:` is equally
	// valid and would have slipped past a test whose failure message promises it
	// cannot. Only one of the two orderings was ever exercised.
	start := strings.LastIndex(job[:i], "      - name:")
	if start < 0 {
		t.Fatal("could not find the step the preflight belongs to")
	}
	step := job[start:]
	if end := regexp.MustCompile(`(?m)^      - name:`).FindStringIndex(step[1:]); end != nil {
		step = step[:end[0]+1] // stop at the next step
	}
	if regexp.MustCompile(`(?m)^        if:`).MatchString(step) {
		t.Error("the k8sVersion preflight has grown an `if:` — conditioning it on ACTION=apply " +
			"means a `plan` reports success against a version the account cannot build")
	}
}
