package assertplatform

// argocomparisons.go implements `llz ci assert-argo-comparisons` — the read-only
// sweep for Applications whose COMPARISON failed, whatever their sync status says.
//
// WHY A SWEEP WHEN converge ALREADY CLASSIFIES THIS. It does: classifyArgoApp
// grades a non-empty SpecErr as CatFail BEFORE it looks at Synced/Healthy, so a
// ComparisonError has never been able to pass the convergence gate. What was
// missing is a way to ASK. `llz ci converge` is a polling loop that self-heals
// with writes to the cluster — it strips oversized CRD annotations, restarts
// argocd-redis, kicks the Harbor provisioner — so "run converge to see whether
// anything failed to compare" is not a question an operator can put to a
// production cluster. This is that question, as one read, in one call.
//
// WHAT IT IS FOR. An Argo CD Application whose comparison ERRORED keeps its
// previous sync status. `sync.status: Synced` on such an app is not a statement
// that the cluster matches the desired state — it is the last verdict Argo was
// able to reach, restated. Every naive check reads it as current, which is the
// silent-green shape this sweep exists to name: the report calls out that
// combination explicitly rather than printing another condition list an operator
// has to interpret.
//
// FAIL-CLOSED ON VACUITY. Zero Applications is a failure, not a pass. A cluster
// with no Applications at all is either not bootstrapped or not the cluster the
// operator thinks they are pointed at, and a gate that examined nothing and
// reported success is indistinguishable from the outage it exists to catch.
//
// THE BOUNDARY IS THE ONE converge USES. An instance-owned Application (the
// instance-custom AppProject, or the escape-hatch naming convention) is REPORTED
// and does not gate, via health.IsInstanceOwnedApp — the same predicate the
// ownership index reads, called rather than restated, so the two cannot drift
// into disagreeing about whose failure this is.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// readApplications is the transport seam: every Application in the namespace, or
// answered=false when the apiserver did not answer at all. Split from the
// judgement so the evaluator below is testable without a cluster.
var readApplications = func(namespace string) (items []json.RawMessage, answered bool) {
	// No `-o json`: ItemsOK appends it, and passing it twice is the sort of detail
	// that reads as a second opinion about the output format.
	return kubectlprobe.ItemsOK("-n", namespace, "get", "applications.argoproj.io")
}

// comparisonFinding is one Application whose comparison failed.
type comparisonFinding struct {
	App    string
	Sync   string
	Health string
	Err    string
	// Gating is false for an instance-owned Application: its failure is real and
	// is printed, but it is not the platform's to fail on.
	Gating bool
	// Silent marks the shape this sweep exists for — a comparison that errored
	// while sync.status still reads Synced, so every status field a human or a
	// gate reads is the PREVIOUS verdict.
	Silent bool
	// Tolerated names why converge does not fail this one, for the findings that
	// are reported and do not gate. Empty for a finding that gates.
	Tolerated string
}

// comparisonFindings is the pure evaluator: which Applications carry a
// comparison/spec error, and which of those gate.
//
// It reads ArgoApp.SpecErr — the field health.ParseArgoApp joins from the
// ComparisonError and InvalidSpecError conditions, and the same field converge
// grades on. One reading of "Argo could not compare this app", two consumers.
//
// AND IT APPLIES THE SAME DEMOTIONS, because "the boundary is the one converge
// uses" has to mean all of it. classifyArgoApp does not fail every SpecErr: an
// operator-deferred app (health.ExternalDepApps — external-dns sits in a
// permanent ComparisonError on any instance without a DNS token) is deferred, and
// a Redis cache auth split is transient across every app at once and clears on a
// restart converge performs itself. Gating on those would make this lane
// permanently red on healthy instances, which is how a gate gets switched off —
// the argument the harbor overlay exemption makes one package over.
//
// A GIT-AUTH REFUSAL IS NOT DEMOTED, matching classifyArgoApp exactly: the remote
// answered, the answer was no, and polling will not change it.
func comparisonFindings(apps []health.ArgoApp) []comparisonFinding {
	var out []comparisonFinding
	for _, a := range apps {
		if a.SpecErr == "" {
			continue
		}
		f := comparisonFinding{
			App:    a.Name,
			Sync:   a.Sync,
			Health: a.Health,
			Err:    a.SpecErr,
			Gating: !health.IsInstanceOwnedApp(a),
			Silent: a.Sync == "Synced",
		}
		// IN classifyArgoApp'S ORDER, because the order is part of the rule: the
		// annotation-limit wedge is checked before SpecErr is even read, and the Redis
		// case before the git-auth case because Redis's NOAUTH text contains
		// "authentication required".
		reason, deferred := health.MatchExternalDep(a.Name, health.ExternalDepApps())
		switch {
		case deferred:
			f.Gating, f.Tolerated = false, "operator-deferred: "+reason
		case health.IsAnnotationLimitError(a.OpErr):
			// An infra wedge converge self-heals by stripping the oversized CRD
			// annotation, not an app-config fault. An app carrying both that and a
			// ComparisonError would otherwise be pending to converge and red here.
			f.Gating, f.Tolerated = false, "sync wedged on the 256KB annotation limit — converge strips "+
				"the oversized CRD annotation and re-polls"
		case health.IsRepoServerCacheAuthError(a.SpecErr):
			f.Gating, f.Tolerated = false, "argocd-redis cache auth split — transient, and converge "+
				"restarts redis to clear it"
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App < out[j].App })
	return out
}

// renderComparisonFinding is the operator-facing line for one finding.
//
// THE SILENT CASE GETS ITS OWN SENTENCE. "monitoring-loki (Synced/Healthy) —
// ComparisonError: …" reads like two facts in tension and leaves the reader to
// work out which one to believe. Saying which one is stale is the whole value of
// the line.
func renderComparisonFinding(f comparisonFinding) string {
	scope := "PLATFORM"
	switch {
	case f.Tolerated != "":
		scope = "tolerated (" + f.Tolerated + ")"
	case !f.Gating:
		scope = "instance-owned (reported, not gated)"
	}
	line := fmt.Sprintf("%s [%s] (%s/%s) — %s", f.App, scope, f.Sync, f.Health, health.FirstLine(f.Err))
	if f.Silent {
		line += "\n    ⇒ sync.status still reads Synced. Argo CD keeps the LAST verdict it could reach when a " +
			"comparison fails, so that Synced describes an earlier desired state, not this one — and selfHeal " +
			"never fires because there is no diff to heal."
	}
	return line
}

// assertArgoComparisons is the lane. Read-only: one list, no writes, no polling.
func assertArgoComparisons(namespace string) error {
	items, answered := readApplications(namespace)
	if !answered {
		fmt.Fprintf(os.Stderr, "::error::could not read Applications in %s — this is 'could not tell', "+
			"not 'nothing wrong', and the lane is not vouching for it\n", namespace)
		return fmt.Errorf("reading Applications in %s", namespace)
	}
	if len(items) == 0 {
		fmt.Fprintf(os.Stderr, "::error::no Argo CD Applications found in %s — nothing was examined, so this "+
			"check is not evidence that every app compared cleanly. Either the platform is not bootstrapped or "+
			"this kubeconfig points somewhere unexpected\n", namespace)
		return fmt.Errorf("no Applications in %s — examined nothing", namespace)
	}

	apps := make([]health.ArgoApp, 0, len(items))
	for _, raw := range items {
		a, err := health.ParseArgoApp(raw)
		if err != nil {
			// A malformed Application is itself a finding: it means this sweep cannot
			// speak for that app, and skipping it silently is the vacuous pass again.
			fmt.Fprintf(os.Stderr, "::error::an Application in %s could not be parsed (%v) — the sweep cannot "+
				"vouch for it\n", namespace, err)
			return fmt.Errorf("parsing an Application in %s: %w", namespace, err)
		}
		apps = append(apps, a)
	}

	findings := comparisonFindings(apps)
	if len(findings) == 0 {
		fmt.Printf("All %d Argo CD Applications in %s compared cleanly (no ComparisonError/InvalidSpecError condition).\n",
			len(apps), namespace)
		fmt.Println("NOTE: this says every app could COMPUTE a diff — not that the diff was applied. " +
			"`llz ci assert-overlay-applied` is the check that reads what the cluster actually got.")
		return nil
	}

	gating := 0
	fmt.Printf("%d of %d Applications in %s carry a comparison/spec error:\n", len(findings), len(apps), namespace)
	for _, f := range findings {
		if f.Gating {
			gating++
		}
		fmt.Println("  • " + strings.ReplaceAll(renderComparisonFinding(f), "\n", "\n  "))
	}
	if gating == 0 {
		// WHY IT DOES NOT GATE IS THE WHOLE MESSAGE. "None of them is platform-owned"
		// was printed for every non-gating reason, so a redis auth split — the entire
		// platform estate failing to compare at once, which converge repairs — read as
		// somebody else's content. Name the reason that actually applies.
		tolerated := 0
		for _, f := range findings {
			if f.Tolerated != "" {
				tolerated++
			}
		}
		switch {
		case tolerated == len(findings):
			fmt.Printf("All %d are states converge tolerates and repairs itself — reported, not gated.\n",
				tolerated)
		case tolerated > 0:
			fmt.Printf("%d are states converge tolerates and repairs itself; the remaining %d are "+
				"instance-owned — reported, not gated.\n", tolerated, len(findings)-tolerated)
		default:
			fmt.Println("None of them is platform-owned — reported, not gated.")
		}
		return nil
	}
	fmt.Fprintf(os.Stderr, "::error::%d platform Application(s) could not be compared by Argo CD. Until the "+
		"comparison succeeds their sync status describes an earlier desired state and selfHeal cannot run\n", gating)
	return fmt.Errorf("%d platform Application(s) with a failed comparison", gating)
}
