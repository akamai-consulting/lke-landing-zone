package assertsuite

// ci_assert_suite.go implements `llz ci assert-suite` — the e2e assert battery
// itself, moved out of YAML.
//
// WHAT IT REPLACES. The battery used to be ~40 lines of inline bash in
// llz-bootstrap-openbao.yml implementing a parallel job runner: a `lane()`
// function backgrounding each check into a temp dir, a `wait`, then a `for` loop
// re-reading each lane's captured rc/log to print ::group:: blocks and decide the
// step's exit status. That is a job runner written in YAML, and it carried the
// three problems this file exists to end:
//
//   1. IT COULD NOT BE TESTED. Every other check in this battery is unit-tested
//      Go; the thing that decides whether the battery FAILS was untested bash.
//   2. THE LANE LIST WAS WRITTEN TWICE — once as `lane <name> …`, once in the
//      `for n in …` collection loop — and a lane present in the first but absent
//      from the second RUNS AND CAN NEVER FAIL THE STEP. That is the same vacuous
//      pass the individual gates refuse, one level up, and nothing enforced it.
//      Here the list exists once: a lane cannot be run without being collected.
//   3. IT SHIPPED PER-INSTANCE. The lane list lived in a vendored workflow, so a
//      new gate reached an instance only when someone edited that instance's
//      YAML. Now it travels with the binary: `llz upgrade` carries new gates.
//
// CONCURRENCY MODEL. Lanes run in parallel because they are independent
// read-mostly checks; steps WITHIN a lane run in order and stop at the first
// failure, which is how ordered dependencies are expressed (assert-reconciler
// reads gauges assert-scrape-targets just proved fresh). Wall clock is the
// slowest lane rather than the sum.
//
// The lanes are collision-free only because the two MUTATING ones
// (health-workflow, broad-pat) touch disjoint namespaces, and because every lane
// that port-forwards binds local port :0 so kubectl assigns a free port per lane.
// Preserve both properties when adding a lane.
//
// Each lane is re-exec'd as a subprocess of this same binary rather than called
// in-process. That keeps a lane's output cleanly capturable (no interleaving of
// concurrent writers on one stdout), stops one gate's os.Exit or panic from
// taking the battery with it, and keeps each gate's package-level seams
// independent.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Lane is one parallel lane of the battery.
type Lane struct {
	Name string
	// Steps run IN ORDER inside the lane, stopping at the first failure.
	Steps []Step
	// Gating lanes fail the battery. Report-only lanes are diagnostics whose
	// output is the deliverable — they are still run and still printed, but their
	// exit status is discarded (the old `|| true`).
	Gating bool
	// SummaryTitle, when set, appends the lane's captured output to
	// $GITHUB_STEP_SUMMARY under this heading.
	SummaryTitle string
	// Why documents what the lane proves and what stays color.Green without it. Printed
	// with the lane's group so a failure carries its own rationale.
	Why string
}

// Step is one `llz ci …` invocation inside a lane, and the extension that owns
// the verb it runs.
//
// ────────────────────────────────────────────────────────────────────────────
// THE EXTENSION SITS ON THE STEP, NOT THE LANE, AND THAT WAS MEASURED.
//
// It used to be `Lane.Extension`, documented as the thing that stops a disabled
// lane looking like a passing one — and it was NEVER SET. Not on one lane. The
// resolver was installed, runLane guarded on `l.Extension != ""`, and the
// condition was false every time, so the whole enablement path for this battery
// was unreachable code behind a guard that could not fire. That is the
// vacuous-green shape this file's own header was written to kill, one level up
// again.
//
// Populating the old field would not have fixed it. FIVE OF FIFTEEN LANES SPAN
// TWO EXTENSIONS — `delivery` runs assert-observability's log-ingestion and
// assert-secrets' ESO round-trip; `surfaces` runs two observability checks and
// one network one; `credentials` runs assert-secrets and assert-registry;
// `scrape-reconciler` runs assert-observability then assert-reconciler. A
// singular field cannot name them, so a third of the battery would have kept the
// empty default — and "empty means always run" is exactly the silent default that
// made this dead in the first place.
//
// The step is the unit the REGISTRY names: one step is one `llz ci` verb, and a
// verb belongs to exactly one extension (registry.Commands() says so, and
// TestEveryBatteryStepNamesARegisteredExtension holds it). So enablement resolves per
// step, and a lane counts as skipped only when EVERY step in it was — which keeps
// the other extension's coverage rather than discarding it with its neighbour.
// ────────────────────────────────────────────────────────────────────────────
type Step struct {
	// Extension is the registry name of the extension owning this verb.
	Extension string
	// Argv is the invocation after "ci".
	Argv []string
}

// Lanes is the battery. ONE list — a lane here is both run and
// collected, so the "declared but never checked" hazard is structurally gone.
//
// region is threaded in rather than read from the environment at each call site
// so the table stays a pure value the tests can build.
func Lanes(region string) []Lane {
	// step and regionStep both carry the owning extension, so the table cannot
	// grow a verb whose extension nobody wrote down.
	step := func(ext, verb string, args ...string) Step {
		return Step{Extension: ext, Argv: append([]string{verb}, args...)}
	}
	regionStep := func(ext, verb string) Step {
		if region == "" {
			return Step{Extension: ext, Argv: []string{verb}}
		}
		return Step{Extension: ext, Argv: []string{verb, "--region", region}}
	}
	return []Lane{
		{
			Name: "loki", Gating: true,
			// --region because the write PROOF has to resolve this deployment's chunks
			// bucket from the spec. Without it the proof degrades to a skip, which is
			// the quiet failure this lane exists to stop having.
			Steps: []Step{regionStep("assert-observability", "assert-loki")},
			Why:   "Loki is bootstrapped and S3-backed. Says nothing about anything REACHING it — that is openbao-audit and delivery.",
		},
		{
			Name: "scrape-reconciler", Gating: true,
			Steps: []Step{
				step("assert-observability", "assert-scrape-targets"),
				step("assert-reconciler", "assert-reconciler"),
				step("assert-reconciler", "assert-reconciler-effects"),
			},
			Why: "ORDERED on purpose. assert-scrape-targets proves every landing-zone ServiceMonitor has a live `up` target and every PrometheusRule group loaded; " +
				"assert-reconciler then reads gauges that assert just proved fresh (and gates per-lane freshness, which llz_reconcile_up's max() across lanes cannot see); " +
				"assert-reconciler-effects finally checks the cluster invariants those lanes maintain. Splitting them would race the gauge's first scrape.",
		},
		{
			Name: "openbao-audit", Gating: true,
			Steps: []Step{step("assert-secrets", "assert-openbao-audit")},
			Why: "OpenBao's audit records are ARRIVING in Loki. Separate from `loki` on purpose: \"Loki is bootstrapped\" and \"records reach it\" are different failures, " +
				"and this pipeline shipped to a Service that never existed for its whole life with a NetworkPolicy allow pointed at the same empty namespace.",
		},
		{
			Name: "delivery", Gating: true,
			Steps: []Step{
				step("assert-observability", "assert-log-ingestion"),
				step("assert-secrets", "assert-eso-roundtrip"),
			},
			Why: "Did the data actually arrive? assert-log-ingestion covers apl-core's cluster-wide collector over pod stdout — the path the OpenBao sidecar lane does not touch. " +
				"assert-eso-roundtrip proves ESO still RE-READS OpenBao: a Secret it can no longer refresh keeps serving its frozen value and every consumer keeps working.",
		},
		{
			Name: "surfaces", Gating: true,
			Steps: []Step{
				step("assert-observability", "assert-alert-delivery"),
				step("assert-observability", "assert-grafana-dashboards"),
				step("assert-network", "assert-admission-enforcement"),
			},
			Why: "Is the thing that is supposed to ACT still live? A firing alert with no Alertmanager goes nowhere and Prometheus does not consider that an error; " +
				"a dashboard whose sidecar label was dropped is Synced, valid and invisible; a Kyverno policy that stopped enforcing looks identical to one that works, " +
				"and both ship failurePolicy: Ignore so a downed Kyverno ADMITS everything silently.",
		},
		{
			Name: "net-enforcement", Gating: true,
			Steps: []Step{step("assert-network", "assert-network-enforcement")},
			Why: "MUTATING (its own scratch namespace, deleted on every path). The two enforcement properties that cannot be dry-run: a dropped packet is only knowable by sending one. " +
				"Proves the CNI enforces NetworkPolicy at all — without which every default-deny in this repo is decorative — and that Istio refuses plaintext on a STRICT-mesh port. " +
				"Each negative is paired with a POSITIVE CONTROL from the same pod, so a probe that simply could not reach anything reports INCONCLUSIVE instead of passing as enforcement.",
		},
		{
			Name: "obj-encryption", Gating: true,
			Steps: []Step{regionStep("obj-encryption", "assert-obj-encryption")},
			Why: "Objects actually land ENCRYPTED. Separate from obj-storage on purpose: assert-obj-roundtrip proves a consumer can WRITE, " +
				"and a plaintext write passes it perfectly. This proves the SSE-C gateway is in the path — the registry pods carry the CA, " +
				"the DNS rewrite is in force, sampled objects answer 400 to a keyless HEAD, and a blob pushed BY HARBOR lands encrypted. " +
				"That last one is the only check that proves the CA chain: Loki reaches the proxy with insecure_skip_verify, so every other " +
				"check here is color.Green with Harbor's trust completely broken. MUTATING (leaves one untagged probe blob, which Harbor GC reclaims). " +
				"Endpoint and bucket names are derived from the spec, not passed in — a gate configured from env vars nothing exports fails on a " +
				"missing flag rather than on encryption. SELF-SKIPS (loudly) with no --region or when spec.components.objProxy is off.",
		},
		{
			Name: "obj-storage", Gating: true,
			Steps: []Step{step("assert-objstore", "assert-obj-roundtrip")},
			Why: "Loki and Harbor can WRITE to their object storage — a PUT and a read-back at each consumer's OWN endpoint with its OWN credential. " +
				"verify-object-storage asks the Linode API whether the buckets exist by label, and every one of those checks passed while both consumers returned NoSuchBucket: " +
				"the object-storage generations are disjoint namespaces, so an obj-cluster id stripped to its region puts the bucket on one while the consumers address the other, " +
				"and both answers are truthful about different places. Reads back because a PUT can succeed against the wrong endpoint.",
		},
		{
			Name: "certificates", Gating: true,
			Steps: []Step{step("assert-identity", "assert-certificates")},
			Why: "Every cert-manager Certificate is Ready and not expiring inside the renewal window. The signal existed and nothing consumed it: llz_certificates_not_ready is " +
				"published and LLZCertificatesNotReady alerts on it, but alert-eval is report-only and --strict ignores FIRING, so a stuck Certificate reds nothing. " +
				"Reports broken ISSUANCE and broken RENEWAL separately — the remedies share nothing.",
		},
		{
			Name: "credentials", Gating: true,
			Steps: []Step{
				step("assert-secrets", "assert-rotation-health"),
				step("assert-registry", "assert-harbor-roundtrip"),
			},
			Why: "The credential lifecycle. assert-rotation-health gates every credential credpaths.CredPaths declares: a declared credential publishing NO age series is invisible on the " +
				"single pane AND unalertable, because a rule over an absent series never evaluates. assert-harbor-roundtrip then USES a minted robot — the auth handshake for pull AND " +
				"push — rather than trusting it was created; the host-truncation regression left every credential valid and every push and pull 401ing. Neither forces a rotation: " +
				"broad-pat already exercises one full cycle safely, and forcing lke-admin/obj-key/db-admin/state-passphrase mid-run would break the cluster being measured. " +
				"assert-database covers Managed Postgres and is deliberately NOT in this battery: the db-admin credential lives only in OpenBao (no ExternalSecret materializes it) " +
				"and the bootstrap job REVOKES the root token before this suite runs, so it would fail here for want of a token rather than for anything about the database. " +
				"It runs as its own step earlier in the job, while the token still exists.",
		},
		{
			Name: "health-workflow", Gating: true,
			Steps: []Step{regionStep("assert-platform", "assert-health-workflow")},
			Why:   "MUTATING (own namespace). The day-2 RUN path: submits a one-shot Workflow from the llz-cluster-health WorkflowTemplate and requires it to Succeed. Skips clean when the component is disabled.",
		},
		{
			Name: "broad-pat", Gating: true,
			Steps: []Step{regionStep("assert-secrets", "assert-broad-pat-rotation")},
			Why: "MUTATING (own namespace). Forces one rotation Job and requires action=rotated — exercising mint → OpenBao → GitHub publish → revoke against real backends. " +
				"Safe by construction: an e2e-unique label and BROAD_PAT_DEPLOYMENTS=e2e scope mint/revoke to the e2e PAT family only.",
		},
		{
			Name: "wave-vap", Gating: true,
			Steps: []Step{step("assert-network", "assert-wave-health-vap")},
			Why:   "The llz-wave-health-guard VAP is BOUND and ENFORCING: server-dry-runs a Deployment at sync-wave -5 and requires the guard's own denial.",
		},
		{
			Name: "instance-custom", Gating: true,
			Steps: []Step{step("assert-platform", "assert-instance-custom")},
			Why:   "The operator escape hatch generated something. converge and assert-loki gate the PLATFORM apps and stay color.Green when the hatch generated NOTHING — only an assertion that names it catches that.",
		},
		{
			Name: "team-write", Gating: true,
			Steps: []Step{regionStep("assert-identity", "team-login-smoke")},
			Why: "The team-scoped OpenBao credential path, BOTH halves — spec.teams → Keycloak group/role → OpenBao role → <name>-writer, and the ESO SA → k8s-auth → <name>-reader. " +
				"A color.Red lane is either a Keycloak fault or a bao-configure policy fault; the log names which assertion tripped. No-op when no teams are declared.",
		},
		{
			Name: "overlay", Gating: true,
			Steps: []Step{step("assert-platform", "assert-argo-comparisons")},
			Why: "Argo CD could COMPARE every platform Application. A comparison that errors leaves the previous sync status standing, so `Synced` on such an app describes an " +
				"earlier desired state and selfHeal never fires — every naive status read sees a healthy app. Says nothing about whether the compared diff was APPLIED; that is a " +
				"different failure and a different check.",
		},
		{
			Name: "metric-surface", Gating: false,
			Steps: []Step{step("assert-observability", "prom-metrics", "--match", "^(loki_|cortex_|otelcol_|harbor_)")},
			Why:   "REPORT-ONLY. Dumps the exporter metric NAMES so error-rate/saturation alerts get written against series that actually exist (promtool checks syntax, not existence).",
		},
		{
			Name: "alert-eval", Gating: false,
			Steps:        []Step{step("assert-observability", "alert-eval")},
			SummaryTitle: "alert-eval — live rule evaluation",
			Why: "REPORT-ONLY, and its FIRING/ARMED/DEAD?/BROKEN report is the deliverable. promtool cannot tell a rule referencing a non-existent metric (silent never-fire) " +
				"from one simply not tripping; this can.",
		},
	}
}

// laneResult is one lane's captured outcome.
type laneResult struct {
	Lane     Lane
	ExitCode int
	Output   string
	Duration time.Duration
	// Failed is the GATING verdict: report-only lanes never fail the battery even
	// when their exit code is non-zero.
	Failed bool
	// Skipped means the lane did not run because the instance does not have its
	// extension. A skipped lane is NOT a passing lane, and nothing may count it as
	// evidence that `verified` holds.
	Skipped bool
	// SkipReason names what turned it off, so an operator asking why a lane is
	// missing does not have to guess which toggle did it.
	SkipReason string
}

// Disabled reports whether an extension is off for this instance, and why. Nil
// means "run everything", which is the template repo and any caller that has no
// spec to consult.
//
// INJECTED RATHER THAN RESOLVED HERE, because internal/shared/extension/registry
// imports this package to declare it — asking the registry directly would be a
// cycle. The caller that has both is the one that supplies it.
type Disabled func(extension string) (reason string, off bool)

// runLaneFn executes one `llz ci …` step and returns its combined output and
// exit code. Seamed so the orchestration is testable without spawning processes.
var runLaneFn = func(args []string) (string, int) {
	self, err := os.Executable()
	if err != nil {
		return fmt.Sprintf("cannot locate the llz binary to re-exec: %v\n", err), 1
	}
	cmd := exec.Command(self, append([]string{"ci"}, args...)...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		code := 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return buf.String(), code
	}
	return buf.String(), 0
}

// runLane executes a lane's steps in order, stopping at the first failure, and
// skipping any step whose extension this instance no longer has.
//
// A LANE IS "SKIPPED" ONLY WHEN EVERY STEP WAS, which is the whole reason the
// extension moved onto the step. `delivery` runs assert-observability's
// log-ingestion and assert-secrets' ESO round-trip; an instance that dropped one
// must still be measured by the other, and skipping the lane wholesale would
// discard a passing check because of its neighbour.
//
// THE SKIPPED STEPS ARE NAMED IN THE OUTPUT, always. A lane reporting green over
// two steps when it ran one is the same vacuous-green this battery exists to
// refuse, arriving one level down.
func runLane(l Lane, now func() time.Time, disabled Disabled) laneResult {
	start := now()
	res := laneResult{Lane: l}
	var out strings.Builder
	var ran, skipped int
	var reasons []string
	for _, step := range l.Steps {
		if disabled != nil && step.Extension != "" {
			if why, off := disabled(step.Extension); off {
				skipped++
				reasons = append(reasons, fmt.Sprintf("%s (%s)", step.Extension, why))
				fmt.Fprintf(&out, "skipped %s: %s\n", strings.Join(step.Argv, " "), why)
				continue
			}
		}
		ran++
		stepOut, code := runLaneFn(step.Argv)
		out.WriteString(stepOut)
		if code != 0 {
			res.ExitCode = code
			break
		}
	}
	if ran == 0 && skipped > 0 {
		res.Skipped, res.SkipReason = true, strings.Join(reasons, ", ")
		res.Output = out.String()
		res.Duration = now().Sub(start)
		return res
	}
	res.Output = out.String()
	res.Duration = now().Sub(start)
	// A report-only lane is run and printed but never gates — the old `|| true`,
	// expressed as data rather than as shell.
	res.Failed = l.Gating && res.ExitCode != 0
	return res
}

// runAssertSuiteLanes runs every lane concurrently and returns results in the
// lane table's order (deterministic output regardless of completion order).
func runAssertSuiteLanes(lanes []Lane, now func() time.Time, disabled Disabled) []laneResult {
	results := make([]laneResult, len(lanes))
	var wg sync.WaitGroup
	for i, l := range lanes {
		wg.Add(1)
		go func(i int, l Lane) {
			defer wg.Done()
			results[i] = runLane(l, now, disabled)
		}(i, l)
	}
	wg.Wait()
	return results
}

// laneDisabled is the battery's enablement source. It is a package var rather
// than a parameter because RunAssertSuite is a cobra RunE with a fixed signature,
// and package main installs the real resolver at init — the same seam pattern
// every other capability in this tree uses.
//
// NIL BY DEFAULT MEANS RUN EVERYTHING, deliberately. An uninstalled resolver must
// not silence the battery: a default that skipped lanes would turn assertions off
// wherever the wiring was forgotten, which is the dangerous direction.
var laneDisabled Disabled

// InstallDisabled wires the enablement resolver. Call once, before RunAssertSuite.
func InstallDisabled(d Disabled) { laneDisabled = d }

// skippedLaneNames returns the lanes that did not run, sorted. Reported
// separately from failures because they are a different thing and a reader who
// conflates them will believe the battery proved more than it did.
func skippedLaneNames(rs []laneResult) []string {
	var out []string
	for _, r := range rs {
		if r.Skipped {
			out = append(out, r.Lane.Name)
		}
	}
	sort.Strings(out)
	return out
}

// failedLaneNames returns the gating lanes that failed, sorted.
func failedLaneNames(rs []laneResult) []string {
	var out []string
	for _, r := range rs {
		if r.Failed {
			out = append(out, r.Lane.Name)
		}
	}
	sort.Strings(out)
	return out
}

// selectLanes filters the table by name, returning an error naming any unknown
// selection.
//
// An unknown name is an ERROR, not a silent skip: a typo would otherwise quietly
// shrink the battery, which is precisely the failure this file removed from the
// YAML.
func selectLanes(all []Lane, only []string) ([]Lane, error) {
	if len(only) == 0 {
		return all, nil
	}
	byName := map[string]Lane{}
	for _, l := range all {
		byName[l.Name] = l
	}
	var out []Lane
	var unknown []string
	for _, n := range only {
		l, ok := byName[n]
		if !ok {
			unknown = append(unknown, n)
			continue
		}
		out = append(out, l)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown lane(s): %s (known: %s)",
			strings.Join(unknown, ", "), strings.Join(laneNames(all), ", "))
	}
	return out, nil
}

func laneNames(ls []Lane) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

func Run(region string, only []string, list bool) error {
	lanes, err := selectLanes(Lanes(region), only)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::%v\n", err)
		return err
	}

	if list {
		for _, l := range lanes {
			kind := "gating"
			if !l.Gating {
				kind = "report-only"
			}
			steps := make([]string, 0, len(l.Steps))
			for _, s := range l.Steps {
				steps = append(steps, "llz ci "+strings.Join(s.Argv, " "))
			}
			fmt.Printf("%-18s %-12s %s\n", l.Name, kind, strings.Join(steps, " && "))
		}
		return nil
	}

	fmt.Printf("## E2E assert suite — %d lanes (%d gating)\n", len(lanes), countGating(lanes))
	results := runAssertSuiteLanes(lanes, time.Now, laneDisabled)

	for _, r := range results {
		// A SKIPPED LANE IS ANNOUNCED, NOT OMITTED. Printing nothing would leave the
		// battery reporting fewer lanes than its own header counted, which reads as
		// a passing run over a reduced set — the failure this state exists to end.
		if r.Skipped {
			fmt.Printf("::group::assert %s — SKIPPED (%s)\n", r.Lane.Name, r.SkipReason)
			fmt.Printf("# %s\n", r.Lane.Why)
			fmt.Printf("NOT RUN, and NOT PASSED: this instance does not have %s. Nothing here "+
				"is evidence about the platform either way.\n", r.SkipReason)
			fmt.Println("::endgroup::")
			continue
		}
		fmt.Printf("::group::assert %s (rc=%d, %s)\n", r.Lane.Name, r.ExitCode, r.Duration.Round(time.Second))
		if r.Lane.Why != "" {
			fmt.Printf("# %s\n", r.Lane.Why)
		}
		fmt.Print(r.Output)
		if !strings.HasSuffix(r.Output, "\n") && r.Output != "" {
			fmt.Println()
		}
		fmt.Println("::endgroup::")
		if r.Failed {
			fmt.Fprintf(os.Stderr, "::error::e2e assert '%s' failed (rc=%d) — see its group above\n", r.Lane.Name, r.ExitCode)
		} else if !r.Lane.Gating && r.ExitCode != 0 {
			// Worth saying out loud: a diagnostic that errored produced no report,
			// which is different from a diagnostic that ran and found nothing.
			fmt.Printf("note: report-only lane %s exited %d — its output above is incomplete\n", r.Lane.Name, r.ExitCode)
		}
	}

	appendLaneSummaries(results)

	if bad := failedLaneNames(results); len(bad) > 0 {
		return fmt.Errorf("e2e assert lane(s) failed: %s", strings.Join(bad, ", "))
	}

	// THE COUNT IS OF LANES THAT RAN, NOT LANES THAT EXIST, and that distinction is
	// the whole point of the skip state. This printed countGating(lanes) — the
	// DECLARED gating lanes — so a battery that skipped three of them still
	// announced that all of them passed. A reader takes that line as the verdict.
	skipped := skippedLaneNames(results)
	ran := countGating(lanes) - countGatingSkipped(results)
	if len(skipped) > 0 {
		fmt.Printf("All %d gating lane(s) that RAN passed. %d skipped: %s\n",
			ran, len(skipped), strings.Join(skipped, ", "))
		fmt.Println("A skipped lane proves nothing. `verified` is a claim about the lanes " +
			"that ran, and these did not.")
		return nil
	}
	fmt.Printf("All %d gating lane(s) passed.\n", ran)
	return nil
}

// countGatingSkipped counts the GATING lanes that did not run. Report-only lanes
// are excluded for the same reason they never fail the battery: they are not part
// of the verdict either way.
func countGatingSkipped(rs []laneResult) int {
	n := 0
	for _, r := range rs {
		if r.Skipped && r.Lane.Gating {
			n++
		}
	}
	return n
}

func countGating(ls []Lane) int {
	n := 0
	for _, l := range ls {
		if l.Gating {
			n++
		}
	}
	return n
}

// appendLaneSummaries writes the lanes whose OUTPUT is the deliverable to
// $GITHUB_STEP_SUMMARY. Best-effort: a summary that cannot be written must never
// change the battery's verdict.
func appendLaneSummaries(rs []laneResult) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	var b strings.Builder
	for _, r := range rs {
		if r.Lane.SummaryTitle == "" {
			continue
		}
		b.WriteString("### " + r.Lane.SummaryTitle + "\n```\n" + r.Output + "\n```\n")
	}
	if b.Len() == 0 {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(b.String())
}
