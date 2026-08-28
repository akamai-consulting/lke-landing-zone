package clusterspec

// inertfields.go names the spec fields this repo ACCEPTS and RENDERS NOWHERE.
//
// WHY A LIST OF THINGS THAT DO NOT WORK IS BETTER THAN NO LIST. Every field here
// parses, merges, validates, and shows up in `llz apl app list` — and then reaches
// no cluster. Their only render target was the per-env
// `apl-values/<env>/values.yaml` that `llz render` stopped emitting at the managed
// App Platform pivot (ADR 0005). Nothing failed when that target disappeared,
// because a field with no renderer produces no error; it produces nothing at all.
//
// So an operator sets `spec.alerting.receivers: [slack]`, seeds the webhook, reads
// the docs describing the flow, and is paged by nothing. The gap is not that the
// capability is missing — it is that the spec keeps claiming to have it. This list
// is what turns that silence into a `llz doctor` finding, and it is deliberately
// small and hand-maintained: every row is a promise the spec is currently breaking.
//
// THE RATCHET. TestInertSpecFieldsAreStillInert renders a spec that sets each
// field and asserts its value does NOT appear in the render output. That test
// FAILING IS THE GOOD OUTCOME — it means someone wired the field, and the row must
// be deleted in the same commit. A row left behind after wiring would tell
// operators a working feature is broken, which is the same class of lie in the
// other direction.
//
// NOT A DUMPING GROUND. A field belongs here only while there is a real obstacle
// to wiring it, and the obstacle belongs in docs/upstream-asks.md with an exit
// condition. "Nobody got round to it" is a TODO, not an entry.

import "fmt"

// InertSpecField is one spec field that is accepted and never rendered.
type InertSpecField struct {
	// Path is the dotted spec path as an operator writes it.
	Path string
	// Why says what the field WOULD have done and what stands in the way, in one
	// sentence an operator reading `llz doctor` output can act on.
	Why string
	// Probe reports whether this instance actually sets the field. Doctor stays
	// quiet otherwise: warning every instance about a field it never touched is
	// how a real finding gets tuned out.
	Probe func(lz *LandingZone) bool
	// Marker is a distinctive value the ratchet test writes into the field, then
	// looks for in the render output. It must be a string that could not occur by
	// chance in a rendered tree.
	Marker func(lz *LandingZone)
}

// InertSpecFields is the whole list. See docs/upstream-asks.md §3 and §4 for what
// would let each row be deleted.
func InertSpecFields() []InertSpecField {
	return []InertSpecField{
		{
			Path: "alerting.receivers / alerting.slack",
			Why: "apl-core renders Alertmanager's route+receiver config from a TOP-LEVEL `alerts:` " +
				"values block. The apl-overlay's only channel into apl-core is per-app " +
				"(apps.<name>._rawValues on its AplApp CR), which cannot reach a top-level key, and " +
				"the per-env values file that used to carry it is gone. So this setting reaches no " +
				"cluster: alerts aggregate in Alertmanager and notify nobody. The webhook half DOES " +
				"work (secret/alerts/webhooks is seeded and rotated) — it is the routing that is " +
				"unwired. See docs/upstream-asks.md §4",
			Probe: func(lz *LandingZone) bool {
				for _, r := range lz.Spec.Alerting.Receivers {
					if r != "" && r != "none" {
						return true
					}
				}
				return lz.Spec.Alerting.Slack.Channel != "" || lz.Spec.Alerting.Slack.ChannelCrit != ""
			},
			Marker: func(lz *LandingZone) {
				lz.Spec.Alerting.Receivers = []string{"slack"}
				lz.Spec.Alerting.Slack.Channel = inertMarker + "-channel"
				lz.Spec.Alerting.Slack.ChannelCrit = inertMarker + "-channel-crit"
			},
		},
		{
			Path: "components.observability.retention / .storage / .replicas, components.harbor.registryStorage",
			Why: "these are FIRST-CLASS keys on apl-core's AplApp CR (retention, storageSize, " +
				"replicas), not _rawValues. The apl-overlay reconciler is lab-confirmed to merge " +
				"_rawValues onto that CR and have apl-operator keep it; whether the same holds for " +
				"the first-class keys is unconfirmed, and guessing is how the last unapplied " +
				"override got written. The cluster runs apl-core's defaults. " +
				"See docs/upstream-asks.md §3",
			Probe: func(lz *LandingZone) bool {
				for _, e := range lz.Spec.Environments {
					for _, name := range []string{"observability", "harbor"} {
						t, ok := e.Components[name]
						if !ok {
							continue
						}
						if t.Retention != "" || t.Storage != "" || t.RegistryStorage != "" || t.Replicas != nil {
							return true
						}
					}
				}
				return false
			},
			Marker: func(lz *LandingZone) {
				// EVERY field the Probe reads, including replicas. A Marker that
				// sets three of four leaves the fourth untestable: the render-side
				// ratchet hunts for a marker string, and an *int cannot carry one —
				// so replicas gets a distinctive VALUE instead, and the ratchet's
				// positive control (does the marker survive into the spec?) is what
				// keeps the other three honest. If replicas is ever wired, the
				// render output gains a `replicas: 9137` nobody would write.
				inertReplicas := inertMarkerInt
				for env, e := range lz.Spec.Environments {
					if e.Components == nil {
						e.Components = map[string]ComponentToggle{}
					}
					obs := e.Components["observability"]
					obs.Retention = inertMarker + "h"
					obs.Storage = inertMarker + "Gi"
					obs.Replicas = &inertReplicas
					e.Components["observability"] = obs
					h := e.Components["harbor"]
					h.RegistryStorage = inertMarker + "Gi"
					e.Components["harbor"] = h
					lz.Spec.Environments[env] = e
				}
			},
		},
	}
}

// inertMarker is the needle the ratchet writes and then hunts for. Not a plausible
// value: if a renderer ever emits it, that is the field being wired and not a
// coincidence.
const inertMarker = "LLZINERTPROBE9137"

// inertMarkerInt is the marker for integer-typed fields, which cannot carry the
// string one. Not a plausible replica count, and it shares the string marker's
// digits so a reader who greps either finds both.
const inertMarkerInt = 9137

// InertFindings returns one operator-facing line per inert field this instance
// actually sets. Empty when the instance sets none, which is the common case.
func InertFindings(lz *LandingZone) []string {
	var out []string
	for _, f := range InertSpecFields() {
		if f.Probe != nil && f.Probe(lz) {
			out = append(out, fmt.Sprintf("%s is set in the spec but reaches NO cluster — %s", f.Path, f.Why))
		}
	}
	return out
}
