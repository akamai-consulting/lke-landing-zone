package mutate

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Opts is what `llz ci mutate` was given. Exported because the RunE closure that
// used to hold this logic was ninety-seven lines — the bulk of the command — and
// leaving it in a cobra callback would have kept the extension's actual work in
// package main behind a flag set.
type Opts struct {
	Pkg          string
	BaselinePath string
	OutPath      string
	Coefficient  int
	Integration  bool
	Out          io.Writer
}

// Run performs the validated mutation run: control, gremlins, harness validation,
// then a survivor diff against the baseline.
func Run(o Opts) error {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Pkg == "" {
		return fmt.Errorf("--package is required (e.g. ./internal/linode)")
	}
	out := o.Out

	// Warn BEFORE the run, not after: a package whose tests reach outside
	// the module dies in gremlins' isolated copy, -failfast aborts the
	// binary, and every mutant comes back "killed" — the second of the
	// three flattering-100% failures this command exists to catch. The
	// canary catches it after the fact; this says so up front, while the
	// operator can still stage the repo root and avoid burning the run.
	if outside, err := testsReachOutsideModule(strings.TrimPrefix(o.Pkg, "./")); err == nil && outside {
		fmt.Fprintf(out, "::warning::%s has tests referencing paths above the module root. "+
			"gremlins runs each mutant in a COPY of the module, where those paths do not exist, "+
			"so the first such test dies and -failfast marks every mutant killed. "+
			"Stage the repo root around gremlins' workdir, or expect the canary to fail.\n", o.Pkg)
	}

	// CONTROL first: a color.Red suite makes every mutant trivially killed.
	_, ctlErr := execOutput("go", "test", o.Pkg, "-count=1")
	controlGreen := ctlErr == nil

	args := []string{"unleash", "--timeout-o.Coefficient", strconv.Itoa(o.Coefficient)}
	if o.Integration {
		args = append(args, "--o.Integration")
	}
	args = append(args, o.Pkg)
	raw, runErr := execOutput("gremlins", args...)
	// gremlins exits non-zero when thresholds are unmet, which is not a
	// harness fault; the output is still parseable. A truly failed launch
	// produces no mutant lines, which validateRun() catches.
	run := parseGremlins(string(raw))
	_ = runErr

	var can *canary
	if v, ok := canaries[strings.TrimPrefix(strings.TrimPrefix(o.Pkg, "./"), "")]; ok {
		can = &v
	}
	if err := validateRun(run, controlGreen, can); err != nil {
		fmt.Fprintf(out, "::error::mutation harness is not trustworthy — no score reported\n")
		for _, r := range err.(*validationError).reasons {
			fmt.Fprintf(out, "  - %s\n", r)
		}
		return fmt.Errorf("harness validation failed")
	}

	accepted, err := loadBaseline(o.BaselinePath)
	if err != nil {
		return err
	}
	fresh := newSurvivors(run, accepted)

	fmt.Fprintf(out, "%s: killed=%d lived=%d not-covered=%d efficacy=%.2f%%\n",
		o.Pkg, run.Killed, run.Lived, run.NotCovered, run.Efficacy())
	fmt.Fprintf(out, "baseline: %d accepted, %d new survivor(s)\n", len(accepted), len(fresh))
	for _, m := range fresh {
		fmt.Fprintf(out, "  NEW  %s %s:%d:%d\n", m.Mutator, m.File, m.Line, m.Col)
	}

	if o.OutPath != "" {
		b, err := json.MarshalIndent(run, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(o.OutPath, b, 0o644); err != nil {
			return err
		}
	}
	if len(fresh) > 0 {
		return fmt.Errorf("%d new survivor(s) not in the baseline", len(fresh))
	}
	return nil
}
