package assertplatform

// overlayfixtures.go builds the PRE-OVERLAY objects the appliability lane probes.
//
// WHY GENERATED AND NOT CHECKED IN. A checked-in fixture is a hand-maintained
// transcription of what the field map points at, and the two drift the moment
// someone adds a row — at which point the lane reports a pass over a row whose
// object was never created. Generating them from clusterspec.OverlayFields()
// means a new row brings its own fixture, and a row pointing at an object nobody
// taught this file to build is a hard failure rather than a silent gap.
//
// MINIMAL ON PURPOSE. Every field the object carries is one that could
// accidentally satisfy a declared value and turn the probe into a no-op patch
// (see the ALREADY-carries arm in overlayappliability.go). So these carry the
// required fields for the Kind and the container names the rows select on, and
// nothing else — no resources, no volumeClaimTemplates, no defaults that any row
// asserts. The lane re-checks that property against the live object anyway rather
// than trusting this file, because "the generator would not have done that" is
// not a thing a running gate can verify.
//
// IDENTITY, NOT ANSWER. What is derived here is which object a row points at.
// Whether that object's fields are create-only comes from the apiserver. Deriving
// the first from the code under test is fine; deriving the second would be the
// mistake the fail-closed doctrine warns about.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// overlayFieldsFor is the seam the emitter reads the map through. A var, so the
// empty-map refusal below is reachable from a test: without it that arm was
// unexecutable and the test named after it asserted something else entirely.
var overlayFieldsFor = clusterspec.OverlayFields

// fixtureTarget is one object the lane needs to exist, and the containers its
// rows select on.
type fixtureTarget struct {
	Kind, Namespace, Name string
	Containers            []string
	// Unsupported is every selector segment in a row's Live path that this builder
	// cannot honour, so the emitter can refuse rather than build a fixture the row
	// does not resolve on.
	Unsupported []string
	// Priors is what the pre-overlay object must CARRY, one entry per MatchScalar
	// row: the Live path, and the chart default the row declares as Prior. Seeding
	// them is what makes the probe test default→declared, the transition a
	// brownfield cluster actually performs, rather than absent→set.
	Priors []fixturePrior
}

// fixturePrior is one pre-overlay value to seed into the fixture.
type fixturePrior struct {
	Live  []string
	Value any
}

// fixtureTargets groups the field map by object. Sorted so the emitted stream is
// byte-stable — a fixture set that reorders between runs makes a diff of it
// useless for review.
func fixtureTargets(fields []clusterspec.OverlayField) []fixtureTarget {
	byKey := map[string]*fixtureTarget{}
	seenContainer := map[string]bool{}
	var order []string
	for _, f := range fields {
		key := f.Kind + "/" + f.Namespace + "/" + appliabilityFixtureName(f)
		t, ok := byKey[key]
		if !ok {
			t = &fixtureTarget{Kind: f.Kind, Namespace: f.Namespace, Name: appliabilityFixtureName(f)}
			byKey[key] = t
			order = append(order, key)
		}
		names, unsupported := containerSelectors(f.Live)
		t.Unsupported = append(t.Unsupported, unsupported...)
		for _, c := range names {
			if seenContainer[key+"/"+c] {
				continue
			}
			seenContainer[key+"/"+c] = true
			t.Containers = append(t.Containers, c)
		}
		if f.Prior != nil {
			t.Priors = append(t.Priors, fixturePrior{Live: f.Live, Value: f.Prior})
		}
	}
	sort.Strings(order)
	out := make([]fixtureTarget, 0, len(order))
	for _, k := range order {
		t := byKey[k]
		sort.Strings(t.Containers)
		out = append(out, *t)
	}
	return out
}

// containerSelectors pulls every `containers[name=x]` target out of a Live path.
//
// IT READS THE ROW'S OWN SELECTOR rather than taking a container name from
// anywhere else, because clusterspec.LiveValue deliberately has no
// only-one-element fallback: a row selecting a container the fixture does not
// carry resolves to nothing, which the lane reports as a row covering nothing.
// Building the fixture from the same string is what keeps the two in step, so a
// chart rename that updates the selector updates the fixture with it.
func containerSelectors(live []string) (names []string, unsupported []string) {
	for _, seg := range live {
		key, sel, hasSel := strings.Cut(seg, "[")
		if !hasSel {
			continue
		}
		if key != "containers" {
			// A SELECTOR ON A LIST THIS BUILDER DOES NOT BUILD — initContainers,
			// ephemeralContainers, volumes. Skipping it silently left the fixture without
			// that list at all, so the row resolved to "(absent)", went on to probe, and
			// graded APPLIABLE. Absent-because-nobody-built-it must not read the same as
			// absent-because-the-object-lacks-it, so it is returned and refused.
			unsupported = append(unsupported, seg)
			continue
		}
		want, ok := strings.CutSuffix(sel, "]")
		if !ok {
			continue
		}
		field, name, ok := strings.Cut(want, "=")
		if !ok || field != "name" || name == "" {
			unsupported = append(unsupported, seg)
			continue
		}
		names = append(names, name)
	}
	return names, unsupported
}

// fixturePlaceholderImage is what every generated container runs.
//
// A REAL, TINY, PINNED IMAGE rather than a made-up name: the objects are applied
// for real (not dry-run) so the apiserver will hold them, and while nothing here
// waits for a pod, an unpullable image would leave the kind lane's events full of
// ImagePullBackOff noise that reads like a broken gate to whoever opens it next.
const fixturePlaceholderImage = "registry.k8s.io/pause:3.10"

// EmitFixtures renders the pre-overlay objects as a JSON v1 List, ready for
// `kubectl apply -f -`.
//
// A LIST RATHER THAN A YAML STREAM because kubectl takes it as one document and
// the emitter needs no separator logic; and JSON rather than YAML because the
// only consumer is kubectl, which accepts both, and JSON has no indentation
// mistakes to make.
func EmitFixtures() (string, error) {
	fields := overlayFieldsFor()
	// FAIL BEFORE BUILDING ANYTHING if a scalar row does not say what a pre-overlay
	// object carries. Falling back to omission would silently return the emitter to
	// probing absent→set — the transition a brownfield cluster never performs — and
	// the row would be graded on a question nobody asked.
	for _, f := range fields {
		if f.Match == clusterspec.MatchScalar && f.Prior == nil {
			return "", fmt.Errorf("%s is a scalar row with no Prior: the fixture would OMIT the field, so "+
				"the probe would test absent→set while a brownfield cluster performs default→declared. "+
				"Set Prior to what a pre-overlay %s %s/%s carries at %s",
				clusterspec.OverlayFieldPath(f), f.Kind, f.Namespace, f.Name, strings.Join(f.Live, "."))
		}
	}
	targets := fixtureTargets(fields)
	for _, t := range targets {
		if len(t.Unsupported) > 0 {
			return "", fmt.Errorf("%s %s/%s: this builder cannot honour the selector(s) %s in a row's Live "+
				"path, so the fixture would not carry what the row reads — the row would resolve to "+
				"\"(absent)\", probe anyway, and grade APPLIABLE. Teach statefulSetFixture to build that "+
				"list", t.Kind, t.Namespace, t.Name, strings.Join(t.Unsupported, ", "))
		}
	}
	if len(targets) == 0 {
		// The field map is empty, so the lane that consumes this would examine
		// nothing. Fail here rather than emitting an empty list and letting the
		// vacuity check downstream be the only thing standing between an empty map
		// and a green lane.
		return "", fmt.Errorf("clusterspec.OverlayFields() names no objects — there is nothing to build a " +
			"pre-overlay fixture from, and a lane with no fixtures examines nothing")
	}

	// NO NAMESPACE OBJECTS, AND THE REASON IS MEASURED. This used to emit one per
	// target namespace carrying only metadata.name. Applying that over a namespace
	// the kind lane's earlier "Create namespaces" step had already applied
	// three-way-merged every other field AWAY — verified live: `monitoring` lost
	// `monitoring: enabled` and its -20 sync-wave, before the VAP and Kyverno
	// admission gates run in that same job. `--server-side` does not fix it either:
	// kubectl migrates the client-side last-applied annotation on takeover and the
	// unlisted fields still go.
	//
	// (An earlier version of this note also claimed a lost
	// `pod-security.kubernetes.io/enforce: restricted` label. It could not have been:
	// llz-cluster-foundation declares `monitoring` with `restricted: false`, and
	// templates/namespaces.yaml emits the PSA labels only under `{{- if .restricted
	// }}`, so the rendered namespace carries none. The two real losses are enough;
	// an overstated one invites the next reader to disbelieve the rest.)
	//
	// So namespaces are the CALLER'S, and a missing one fails loudly rather than
	// silently: `kubectl apply` of the workload below reports "namespaces not found"
	// and the lane never runs. That is the correct failure — a namespace this gate
	// had to invent is one the lane it runs in does not really create.
	items := []any{}
	for _, t := range targets {
		obj, err := fixtureObject(t)
		if err != nil {
			return "", err
		}
		for _, pr := range t.Priors {
			if err := seedPrior(obj, pr); err != nil {
				return "", fmt.Errorf("%s %s/%s: %w", t.Kind, t.Namespace, t.Name, err)
			}
		}
		items = append(items, obj)
	}

	b, err := json.MarshalIndent(map[string]any{
		"apiVersion": "v1",
		"kind":       "List",
		"items":      items,
	}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// appliabilityFixtureName is what the fixture for a row is CALLED, and it is
// deliberately not the row's own object name.
//
// THE FIXTURE MUST NOT BE ABLE TO IMPERSONATE THE REAL OBJECT. The emitted
// StatefulSet used to carry the production identity `monitoring/loki-ingester`
// exactly — 0 replicas, a pause container, and a selector nothing else matches.
// On the throwaway kind cluster that is harmless; anywhere else it is not, and
// this verb ships in every adopter's `llz ci --help` beside the one the Loki
// runbook tells an operator to run WHEN LOKI IS BROKEN — i.e. precisely when
// loki-ingester may be missing and an apply would CREATE it. A StatefulSet's
// selector is immutable, so apl-core could never reconcile it: monitoring-loki
// SyncFailed until somebody deletes it by hand.
//
// IT COSTS THE PROBE NOTHING. Create-time immutability is a property of the KIND's
// validation, not of the object's name, so the apiserver answers identically for
// a differently-named object of the same Kind. The lane and the emitter both go
// through this function, so the two cannot come to disagree about what to probe.
func appliabilityFixtureName(f clusterspec.OverlayField) string {
	return f.Name + "-llz-appliability-fixture"
}

// FixtureNamespaces is every namespace the emitted objects land in. Exported
// because the emitter no longer creates them, so a caller — or a reader of the
// workflow — needs to know which ones must already exist, and
// TestEveryFixtureNamespaceIsOneTheLaneActuallyCreates holds the workflow to it.
func FixtureNamespaces() []string { return fixtureNamespaces(fixtureTargets(overlayFieldsFor())) }

func fixtureNamespaces(targets []fixtureTarget) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range targets {
		if t.Namespace == "" || seen[t.Namespace] {
			continue
		}
		seen[t.Namespace] = true
		out = append(out, t.Namespace)
	}
	sort.Strings(out)
	return out
}

// fixtureObject builds the minimal object for one target.
//
// FAIL-CLOSED ON AN UNKNOWN KIND, and that is the whole reason this is a switch
// and not a generic builder. The day someone maps a Deployment or a DaemonSet
// row, this refuses rather than emitting nothing for it — because emitting
// nothing would leave the lane probing an absent object, which is a state the
// lane treats as fatal but which would otherwise arrive as a mysterious red on a
// PR that touched something else.
func fixtureObject(t fixtureTarget) (map[string]any, error) {
	switch strings.ToLower(t.Kind) {
	case "statefulset", "statefulsets", "sts":
		return statefulSetFixture(t), nil
	default:
		return nil, fmt.Errorf("no pre-overlay fixture is defined for kind %q (%s/%s) — teach "+
			"assertplatform.fixtureObject how to build a minimal one, or the appliability lane will probe "+
			"an object that does not exist", t.Kind, t.Namespace, t.Name)
	}
}

func statefulSetFixture(t fixtureTarget) map[string]any {
	// THE OBJECT NAME IS NOT ALWAYS A VALID LABEL VALUE. Kubernetes caps a label
	// value at 63 characters and an object name at 253, so a long-named target would
	// have made the whole fixture rejected at create — which this lane reports as an
	// absent object, three steps from the cause. Truncated for the label; the object
	// still carries its real name, which is what the probe addresses.
	labels := map[string]any{"llz.appliability-fixture": truncateLabelValue(t.Name)}

	containers := make([]any, 0, len(t.Containers))
	for _, name := range t.Containers {
		containers = append(containers, map[string]any{
			"name":            name,
			"image":           fixturePlaceholderImage,
			"securityContext": restrictedSecurityContext(),
		})
	}
	if len(containers) == 0 {
		// A row that selects no container still needs a valid pod template. The
		// container is named after the object rather than left out, because a
		// StatefulSet with an empty containers list is rejected by validation and the
		// lane would report that as an unclassified refusal on the first row.
		containers = append(containers, map[string]any{
			"name":            t.Name,
			"image":           fixturePlaceholderImage,
			"securityContext": restrictedSecurityContext(),
		})
	}

	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "StatefulSet",
		"metadata": map[string]any{
			"name":      t.Name,
			"namespace": t.Namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			// ZERO REPLICAS, AND IT IS NOT A MICRO-OPTIMISATION. This gate only ever
			// talks to the apiserver: it GETs the object and sends
			// `patch --dry-run=server`. Neither needs a pod, and spec-level validation —
			// which is the entire question being asked — does not depend on one.
			//
			// A replica is not free where this runs. The kind lane is a 2-CPU GitHub
			// runner, and the very next steps install Kyverno and wait for its admission
			// webhook to have Ready endpoints, with WEBHOOK_RACE_FATAL=true. A fixture
			// pod scheduling and pulling an image in that window is contention this gate
			// has no reason to add, and losing that race fails the job somewhere else
			// entirely — the worst kind of coupling, because the red names Kyverno.
			//
			// It also removes the PodSecurity question from the fixture on any namespace
			// enforcing `restricted`: no pod is created, so none can be rejected. The
			// container's securityContext stays anyway — the object should be admissible
			// on its own terms, not because nothing ever evaluates it.
			"replicas":    0,
			"serviceName": t.Name + "-headless",
			"selector":    map[string]any{"matchLabels": labels},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     map[string]any{"containers": containers},
			},
		},
	}
}

// seedPrior writes one pre-overlay value into the built object at the row's Live
// path, creating the intermediate maps it needs.
//
// IT WALKS THE ROW'S OWN Live PATH, not a transcription of it, so a fixture can
// never be seeded somewhere the gate does not look — the failure that would make
// the seed invisible and quietly restore absent→set. A `containers[name=x]`
// segment resolves against the containers the target already built; a selector
// that finds nothing is an error rather than a silent skip, for the same reason
// clusterspec.LiveValue reports a missed selector rather than returning false.
func seedPrior(obj map[string]any, pr fixturePrior) error {
	if len(pr.Live) == 0 {
		return fmt.Errorf("a prior with no Live path cannot be seeded")
	}
	cur := any(obj)
	for i, seg := range pr.Live {
		last := i == len(pr.Live)-1
		key, sel, hasSel := strings.Cut(seg, "[")
		m, ok := cur.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: %s is not a mapping", strings.Join(pr.Live, "."), key)
		}
		if last && !hasSel {
			m[key] = pr.Value
			return nil
		}
		next, exists := m[key]
		if !exists {
			if hasSel {
				// A selector segment addresses a list the target builder owns
				// (containers). Inventing one here would seed a container the pod
				// template does not run, so this is a miswired row, not a gap to fill.
				return fmt.Errorf("%s: no %s list to select in", strings.Join(pr.Live, "."), key)
			}
			created := map[string]any{}
			m[key] = created
			next = created
		}
		if !hasSel {
			cur = next
			continue
		}
		want, ok := strings.CutSuffix(sel, "]")
		if !ok {
			return fmt.Errorf("%s: malformed selector %q", strings.Join(pr.Live, "."), seg)
		}
		field, wantVal, ok := strings.Cut(want, "=")
		if !ok {
			return fmt.Errorf("%s: malformed selector %q", strings.Join(pr.Live, "."), seg)
		}
		list, ok := next.([]any)
		if !ok {
			return fmt.Errorf("%s: %s is not a list", strings.Join(pr.Live, "."), key)
		}
		var found map[string]any
		for _, e := range list {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if s, _ := em[field].(string); s == wantVal {
				found = em
				break
			}
		}
		if found == nil {
			return fmt.Errorf("%s: no element with %s=%s to seed into", strings.Join(pr.Live, "."), field, wantVal)
		}
		if last {
			return fmt.Errorf("%s: a selector cannot be the last segment of a value path", strings.Join(pr.Live, "."))
		}
		cur = found
	}
	return fmt.Errorf("%s: path ended without writing a value", strings.Join(pr.Live, "."))
}

// restrictedSecurityContext is the minimum that satisfies the `restricted` Pod
// Security Standard.
//
// FOR THE NAMESPACES A FUTURE ROW COULD TARGET, not for the one it targets today.
// llz-openbao and llz-observability ship
// `pod-security.kubernetes.io/enforce: restricted`, and under enforce the
// apiserver would reject these pods outright. `monitoring` — the only namespace
// any current row lands in — is declared `restricted: false` and carries no PSA
// label at all, so nothing here is warning today; an earlier version of this note
// said otherwise. The lane never waits for a pod, so a rejection would not even
// fail the gate directly: it would surface as an absent object three steps from
// its cause, which is the failure this is cheap insurance against.
func restrictedSecurityContext() map[string]any {
	return map[string]any{
		"allowPrivilegeEscalation": false,
		"runAsNonRoot":             true,
		"capabilities":             map[string]any{"drop": []any{"ALL"}},
		"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
	}
}

// maxLabelValue is Kubernetes' limit on a label value.
const maxLabelValue = 63

func truncateLabelValue(v string) string {
	if len(v) <= maxLabelValue {
		return v
	}
	// Trailing '-' or '.' is also invalid in a label value, so trim back past any.
	return strings.TrimRight(v[:maxLabelValue], "-._")
}
