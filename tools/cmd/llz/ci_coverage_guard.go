package main

// ci_coverage_guard.go implements `llz ci check-coverage` — the native port of
// template-scripts/ci/check-go-coverage.sh. It enforces PER-PACKAGE minimum
// statement coverage from a Go coverprofile: each `<pkg-suffix>=<min>` argument
// names a package by the END of its import path (so `cmd/llz` matches
// .../tools/cmd/llz) and a minimum percentage, and the command fails if any
// listed package is below its floor or produced no coverage data at all (a
// renamed/removed package must not pass silently). Packages without a threshold
// are not gated.
//
// The parse-and-compare core is pure (evaluateCoverage) and unit-tested; RunE
// only reads the profile file and prints.

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// covThreshold is one `<pkg-suffix>=<min>` gate. MinStr preserves the operator's
// original spelling for the report line; Min is its parsed value.
type covThreshold struct {
	Suffix string
	MinStr string
	Min    float64
}

// covResult is the evaluation of one threshold against the profile.
type covResult struct {
	Threshold covThreshold
	Pct       float64
	HasData   bool
	OK        bool
}

func ciCheckCoverageCmd() *cobra.Command {
	var profile string
	c := &cobra.Command{
		Use:   "check-coverage <pkg-suffix=min>...",
		Short: "enforce per-package minimum statement coverage from a Go coverprofile",
		Long: "Native port of template-scripts/ci/check-go-coverage.sh (the per-package\n" +
			"floor enforced by `make coverage`). Each <pkg-suffix>=<min> argument matches\n" +
			"the END of a package import path (cmd/llz -> .../tools/cmd/llz) and a minimum\n" +
			"statement-coverage percentage. Fails if any gated package is below its floor\n" +
			"or produced no coverage data. Packages without a threshold are not gated.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runCheckCoverage(profile, args, os.Stdout)
		},
	}
	c.Flags().StringVar(&profile, "profile", "", "path to the Go coverprofile (required)")
	return c
}

func runCheckCoverage(profile string, args []string, out io.Writer) error {
	if profile == "" {
		return fmt.Errorf("--profile (path to the Go coverprofile) is required")
	}
	raw, err := os.ReadFile(profile)
	if err != nil {
		return fmt.Errorf("coverage: profile not found: %s", profile)
	}

	thresholds, err := parseCovThresholds(args)
	if err != nil {
		return err
	}

	results := evaluateCoverage(string(raw), thresholds)
	failed := 0
	for _, r := range results {
		switch {
		case !r.HasData:
			fmt.Fprintf(out, "  ??   %-26s no coverage data (min %s%%)\n", r.Threshold.Suffix, r.Threshold.MinStr)
			failed++
		case !r.OK:
			fmt.Fprintf(out, "  FAIL %-26s %5.1f%% (min %s%%)\n", r.Threshold.Suffix, r.Pct, r.Threshold.MinStr)
			failed++
		default:
			fmt.Fprintf(out, "  ok   %-26s %5.1f%% (min %s%%)\n", r.Threshold.Suffix, r.Pct, r.Threshold.MinStr)
		}
	}

	if failed > 0 {
		return fmt.Errorf("coverage: %d package(s) below their per-package threshold "+
			"(add tests to raise them, or adjust COVERAGE_MINS in the Makefile)", failed)
	}
	fmt.Fprintln(out, "coverage: all gated packages meet their per-package thresholds.")
	return nil
}

// parseCovThresholds parses `<pkg-suffix>=<min>` arguments.
func parseCovThresholds(args []string) ([]covThreshold, error) {
	out := make([]covThreshold, 0, len(args))
	for _, a := range args {
		eq := strings.LastIndex(a, "=")
		if eq < 0 {
			return nil, fmt.Errorf("coverage: bad threshold %q (want <pkg-suffix>=<min>)", a)
		}
		suffix, minStr := a[:eq], a[eq+1:]
		min, err := strconv.ParseFloat(minStr, 64)
		if err != nil {
			return nil, fmt.Errorf("coverage: bad threshold %q: %v", a, err)
		}
		out = append(out, covThreshold{Suffix: suffix, MinStr: minStr, Min: min})
	}
	return out, nil
}

// evaluateCoverage computes per-package statement coverage from a coverprofile
// and grades each threshold against it. A package with no profile data yields
// HasData=false (a hard failure, like the shell version).
func evaluateCoverage(profile string, thresholds []covThreshold) []covResult {
	byPkg := coverageByPackage(profile)
	results := make([]covResult, 0, len(thresholds))
	for _, t := range thresholds {
		pct, ok := coverageForSuffix(byPkg, t.Suffix)
		results = append(results, covResult{
			Threshold: t,
			Pct:       pct,
			HasData:   ok,
			OK:        ok && pct+1e-9 >= t.Min,
		})
	}
	return results
}

// coverageByPackage returns covered/total*100 per package import path. Profile
// data lines look like `<import-path>/<file>.go:<a>.<b>,<c>.<d> <numStmt>
// <hitCount>`; the package is the path with the trailing /<file>.go stripped.
func coverageByPackage(profile string) map[string]float64 {
	type acc struct{ total, covered int }
	pkgs := map[string]*acc{}
	for _, line := range strings.Split(profile, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		numStmt, err1 := strconv.Atoi(fields[1])
		hits, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil {
			continue
		}
		colon := strings.Index(fields[0], ":")
		if colon < 0 {
			continue
		}
		file := fields[0][:colon]
		slash := strings.LastIndex(file, "/")
		if slash < 0 {
			continue
		}
		pkg := file[:slash]
		a := pkgs[pkg]
		if a == nil {
			a = &acc{}
			pkgs[pkg] = a
		}
		a.total += numStmt
		if hits > 0 {
			a.covered += numStmt
		}
	}
	out := make(map[string]float64, len(pkgs))
	for pkg, a := range pkgs {
		if a.total == 0 {
			out[pkg] = 0
			continue
		}
		out[pkg] = float64(a.covered) / float64(a.total) * 100
	}
	return out
}

// coverageForSuffix returns the coverage of the package whose import path ends
// in "/"+suffix. Keys are sorted so a match is deterministic.
func coverageForSuffix(byPkg map[string]float64, suffix string) (float64, bool) {
	keys := make([]string, 0, len(byPkg))
	for k := range byPkg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasSuffix(k, "/"+suffix) {
			return byPkg[k], true
		}
	}
	return 0, false
}
