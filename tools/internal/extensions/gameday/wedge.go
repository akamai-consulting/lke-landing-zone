package gameday

// ci_wedge_gameday.go implements `llz ci wedge-gameday` — the fault-injection
// proof that the blast-radius decomposition (docs/designs/blast-radius-decomposition.md)
// actually contains the wedge it was built to contain.
//
// SCENARIO externalsecret-notready (the PR-C negative test): break ONE platform
// ExternalSecret (repoint its secretStoreRef at a store that does not exist, forcing
// it not-Ready), then assert the wedge is CONTAINED:
//   - the carved Application that OWNS that ExternalSecret goes non-Healthy
//     (Progressing/Degraded) — the fault surfaces in its own App, as designed; AND
//   - platform-bootstrap and every SIBLING carved App stay Healthy throughout — the
//     fault does NOT cascade.
// Before the decomposition the same broken ExternalSecret stalled the single
// platform-bootstrap sync at its wave and starved every later-wave resource across
// all bundles (the #163 class). This command is the concrete before/after proof.
//
// It ALWAYS restores the ExternalSecret (deferred), and it refuses to run unless the
// cluster is Healthy to begin with (you cannot prove containment on an already-sick
// cluster). Exit contract mirrors the other live-cluster CI commands: 0 contained,
// 1 not contained / cluster not initially healthy, 3 cluster unreachable.
//
// The verdict logic (evalWedge) is pure and unit-tested; the kubectl I/O is a thin,
// best-effort shell around it (same aplGateDeps injection as assert-argo-app).

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// appHealth is an Argo Application's (sync, health) status pair.
type appHealth struct{ sync, health string }

// gamedaySnapshot is one observation of every watched Application's health.
type gamedaySnapshot map[string]appHealth

// wedgeVerdict is the pure outcome of a fault window.
type wedgeVerdict struct {
	faultPropagated bool     // the target App went non-Healthy at some point
	containmentHeld bool     // parent + siblings stayed Healthy at EVERY observation
	breaches        []string // parent/sibling Apps that went non-Healthy (containment failures)
	targetStatus    string   // the observed non-Healthy target status (for the report)
}

func (v wedgeVerdict) contained() bool { return v.faultPropagated && v.containmentHeld }

// argoHealthy reports whether an Application health status counts as Healthy. An
// empty string (App not yet observed / unreadable) is NOT Healthy.
func argoHealthy(h string) bool { return h == "Healthy" }

// evalWedge reduces a sequence of snapshots taken during the fault window into a
// verdict: the fault must PROPAGATE to the target App (it goes non-Healthy) while
// CONTAINMENT holds (parent + every sibling stay Healthy in every snapshot). A
// sibling/parent that is ever non-Healthy — including missing from a snapshot — is a
// containment breach.
func evalWedge(target string, guarded []string, snaps []gamedaySnapshot) wedgeVerdict {
	v := wedgeVerdict{containmentHeld: true}
	breached := map[string]bool{}
	for _, snap := range snaps {
		if t, ok := snap[target]; ok && !argoHealthy(t.health) {
			v.faultPropagated = true
			v.targetStatus = t.health
		}
		for _, g := range guarded {
			gh := snap[g] // zero value (empty health) if absent → treated as a breach
			if !argoHealthy(gh.health) {
				v.containmentHeld = false
				if !breached[g] {
					breached[g] = true
					v.breaches = append(v.breaches, g)
				}
			}
		}
	}
	return v
}

// carvedAppNames returns every carved Application name in registry order (the set of
// blast-radius-isolated Apps this game-day reasons about).
func carvedAppNames() []string {
	var names []string
	for _, c := range clusterspec.Components {
		if c.CarvedApp != nil {
			names = append(names, c.CarvedApp.AppName)
		}
	}
	return names
}

type Opts struct {
	ESRef, TargetApp, Namespace string
	Timeout, Interval           time.Duration
}

// Run orchestrates the live game-day: verify healthy start, inject the
// fault, watch, restore, and evaluate containment.
func Run(d cigate.Deps, o Opts) error {
	esNS, esName, ok := splitNSName(o.ESRef)
	if !ok {
		return fmt.Errorf("--externalsecret must be <namespace>/<name>, got %q", o.ESRef)
	}
	guarded := append([]string{"platform-bootstrap"}, siblingsOf(o.TargetApp)...)

	// 1. Refuse to run on an already-sick cluster: the target + every guarded App
	//    must be Healthy first, or a "breach" we observe isn't ours.
	start := snapshotApps(d, o.Namespace, append([]string{o.TargetApp}, guarded...))
	for app, h := range start {
		if !argoHealthy(h.health) {
			fmt.Fprintf(os.Stderr, "::error::wedge-gameday: %s is %s/%s before fault injection — refusing to run on an unhealthy cluster.\n", app, h.sync, h.health)
			return fmt.Errorf("cluster not Healthy at start (%s = %s)", app, h.health)
		}
	}
	if len(start) == 0 {
		fmt.Fprintln(os.Stderr, "::error::wedge-gameday: no Applications readable — cluster unreachable?")
		return fmt.Errorf("no Argo Applications readable (cluster unreachable?)")
	}

	// 2a. Suspend the target App's self-heal for the window. The carved Apps run
	//     selfHeal: true, which would revert our ExternalSecret patch (drift) within
	//     seconds — often before the fault surfaces in App health, making the game-day
	//     flaky/inconclusive. Turning selfHeal off lets the injected fault persist;
	//     auto-sync of NEW git commits is unaffected. Restored in the defer (LIFO:
	//     registered first so it runs LAST — after the ExternalSecret is put back, so
	//     re-enabled self-heal sees an already-correct spec).
	selfHealWasOn := setSelfHeal(d, o.Namespace, o.TargetApp, false)
	if selfHealWasOn {
		defer setSelfHeal(d, o.Namespace, o.TargetApp, true)
	}

	// 2b. Capture the current secretStoreRef.name, then break it. Restore on EVERY
	//     exit path (defer) — a game-day must never leave the cluster wedged.
	origStore, ok := esStoreRef(d, esNS, esName)
	if !ok {
		return fmt.Errorf("read ExternalSecret %s: not found (adjust --externalsecret)", o.ESRef)
	}
	defer restoreES(d, esNS, esName, origStore)
	const bogus = "llz-gameday-nonexistent-store"
	if !patchESStore(d, esNS, esName, bogus) {
		return fmt.Errorf("could not patch ExternalSecret %s to inject the fault", o.ESRef)
	}
	fmt.Printf("wedge-gameday: broke %s (secretStoreRef %s → %s); watching %ds for containment…\n", o.ESRef, origStore, bogus, int(o.Timeout.Seconds()))

	// 3. Watch: snapshot the target + guarded Apps until the fault surfaces in the
	//    target (contained success) or the deadline passes.
	var snaps []gamedaySnapshot
	deadline := d.Now().Add(o.Timeout)
	watch := append([]string{o.TargetApp}, guarded...)
	for {
		snap := snapshotApps(d, o.Namespace, watch)
		snaps = append(snaps, snap)
		v := evalWedge(o.TargetApp, guarded, snaps)
		if !v.containmentHeld {
			// A guarded App broke — stop early, this is a containment FAILURE.
			break
		}
		if v.faultPropagated {
			break // fault reached the target and nothing else broke — contained.
		}
		if !d.Now().Before(deadline) {
			break
		}
		d.Sleep(o.Interval)
	}

	// 4. Evaluate + report.
	v := evalWedge(o.TargetApp, guarded, snaps)
	switch {
	case v.contained():
		fmt.Printf("✓ wedge-gameday CONTAINED: %s went %s while platform-bootstrap + siblings (%s) stayed Healthy.\n",
			o.TargetApp, v.targetStatus, strings.Join(siblingsOf(o.TargetApp), ", "))
		return nil
	case !v.containmentHeld:
		fmt.Fprintf(os.Stderr, "::error::wedge-gameday NOT CONTAINED: breaking %s took down %s — the fault cascaded past %s's own App.\n",
			o.ESRef, strings.Join(v.breaches, ", "), o.TargetApp)
		return fmt.Errorf("containment breached: %s", strings.Join(v.breaches, ", "))
	default:
		fmt.Fprintf(os.Stderr, "::error::wedge-gameday INCONCLUSIVE: %s never went non-Healthy within %s — did the fault take? (check %s status)\n",
			o.TargetApp, o.Timeout, o.ESRef)
		return fmt.Errorf("fault never surfaced in %s", o.TargetApp)
	}
}

// siblingsOf returns the carved App names other than target.
func siblingsOf(target string) []string {
	var out []string
	for _, n := range carvedAppNames() {
		if n != target {
			out = append(out, n)
		}
	}
	return out
}

// snapshotApps reads (sync, health) for each named Application, best-effort (an
// unreadable App is simply absent from the snapshot).
func snapshotApps(d cigate.Deps, namespace string, apps []string) gamedaySnapshot {
	snap := gamedaySnapshot{}
	for _, app := range apps {
		out, ok := d.Kubectl("-n", namespace, "get", "application.argoproj.io", app,
			"-o", "jsonpath={.status.sync.status}{\"\\t\"}{.status.health.status}")
		if !ok {
			continue
		}
		parts := strings.SplitN(strings.TrimSpace(out), "\t", 2)
		h := appHealth{sync: strings.TrimSpace(parts[0])}
		if len(parts) == 2 {
			h.health = strings.TrimSpace(parts[1])
		}
		snap[app] = h
	}
	return snap
}

// esStoreRef returns the ExternalSecret's current spec.secretStoreRef.name.
func esStoreRef(d cigate.Deps, ns, name string) (string, bool) {
	out, ok := d.Kubectl("-n", ns, "get", "externalsecret.external-secrets.io", name,
		"-o", "jsonpath={.spec.secretStoreRef.name}")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// patchESStore repoints the ExternalSecret's secretStoreRef.name — the injected
// fault (a store that does not exist forces the ExternalSecret not-Ready).
func patchESStore(d cigate.Deps, ns, name, store string) bool {
	patch := fmt.Sprintf(`{"spec":{"secretStoreRef":{"name":%q}}}`, store)
	_, err := d.W().PatchMerge(ns, "externalsecret.external-secrets.io", name, patch)
	return err == nil
}

// setSelfHeal flips the target Application's syncPolicy.automated.selfHeal and
// reports whether it was previously ON (so the caller only bothers restoring it when
// it actually changed something). Best-effort: an unreadable/unpatchable App returns
// false and the game-day proceeds (it may just be flakier against self-heal).
func setSelfHeal(d cigate.Deps, namespace, app string, on bool) bool {
	was, _ := d.Kubectl("-n", namespace, "get", "application.argoproj.io", app,
		"-o", "jsonpath={.spec.syncPolicy.automated.selfHeal}")
	patch := fmt.Sprintf(`{"spec":{"syncPolicy":{"automated":{"selfHeal":%t}}}}`, on)
	if _, err := d.W().PatchMerge(namespace, "application.argoproj.io", app, patch); err != nil {
		return false
	}
	if on {
		fmt.Printf("wedge-gameday: re-enabled selfHeal on %s.\n", app)
	} else {
		fmt.Printf("wedge-gameday: suspended selfHeal on %s for the fault window.\n", app)
	}
	// "was ON" iff the prior value wasn't explicitly false (Argo defaults selfHeal
	// off only when automated is set without it; the carved Apps set it true).
	return strings.TrimSpace(was) == "true"
}

// restoreES puts the original secretStoreRef.name back. Best-effort but logged: a
// leftover broken store would keep the App unhealthy.
func restoreES(d cigate.Deps, ns, name, store string) {
	if store == "" {
		return
	}
	if !patchESStore(d, ns, name, store) {
		fmt.Fprintf(os.Stderr, "::warning::wedge-gameday: FAILED to restore %s/%s secretStoreRef to %q — restore it manually (Argo self-heal should also revert it).\n", ns, name, store)
		return
	}
	fmt.Printf("wedge-gameday: restored %s/%s secretStoreRef → %s.\n", ns, name, store)
}

// splitNSName parses "namespace/name".
func splitNSName(s string) (ns, name string, ok bool) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
