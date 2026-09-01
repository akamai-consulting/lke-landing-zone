package reconcilelanes

// overlay_delivery.go — the overlay-delivery gauges: is what the apl-overlay
// declares actually on the objects, on THIS cluster, right now?
//
// WHY A CONTINUOUS LANE AND NOT ONLY A CI GATE. `llz ci assert-overlay-applied`
// runs in the e2e battery, and the e2e battery runs against a cluster the code
// under test just created — where every object was born in its final shape and
// this class of failure cannot occur. docs/e2e-gates.md names that blind spot
// ("The configuration no lane runs: an upgrade over existing state") and argues,
// for the Terraform half, that the cheaper variant worth having first is a
// REPORTING one on real upgrades rather than a gate on a synthetic cluster. The
// Kubernetes half is luckier: the older state we cannot manufacture in CI is
// already running in every adopter's cluster, and the reconciler is already
// standing in it.
//
// So each site samples itself. A field the overlay declares and the object does
// not carry becomes a series — which makes it alertable, and makes "how many of
// our clusters are carrying an undelivered change?" a question with an answer.
//
// READ-ONLY BY CONSTRUCTION, and that is why the lane can be ungated and run on
// every replica: it GETs the objects the field map names and publishes two
// gauges. It does NOT dry-run the change to learn whether it is appliable — that
// costs a write-shaped request (`?dryRun=All`) which this lane has no grant for
// and should not have. The distinction between "undelivered" and "unappliable"
// stays with the CI verb, which holds the handle for it; here, undelivered is the
// signal and the verb is where an operator goes for the reason.
//
// A FIELD THE CLUSTER CANNOT ANSWER FOR PUBLISHES NOTHING. Not a 0, not a 1: an
// unreachable apiserver must not read as "the value is missing" (a false alarm
// that would page someone about a healthy cluster) nor as "the value is there"
// (the silent green this whole family of checks exists to end). No series is the
// honest third answer, and llz_reconcile_up already says the pass failed.
//
// WHICH MEANS SetGaugeFamily, NOT SetGauge, and the difference is the whole
// invariant. SetGauge upserts and has no delete, so in a long-lived reconciler a
// series that stops being published keeps serving its LAST value forever — an
// object that goes away, or a row that stops resolving, would go on reporting
// "delivered" indefinitely. SetGaugeFamily replaces the family each pass, so "not
// published this pass" actually means absent. metrics.go's own header describes
// this exact failure; the credential lane paid for it first.
//
// SAFE HERE because this lane is the only writer of both families — the condition
// SetGaugeFamily documents.

import (
	"context"
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
)

// SampleOverlayDelivery publishes one gauge per mapped overlay field, plus one
// per brownfield migration, and returns an error if any object could not be read.
//
// An error here does not stop the other fields being published: a Loki that is
// unreadable says nothing about Harbor, and dropping every series because one
// object was unreachable would turn a partial outage into a blind spot.
func SampleOverlayDelivery(ctx context.Context, client capability.KubeAPI, reg *metrics.Registry) error {
	raw := clusterspec.AplAppRawValues()
	var firstErr error
	// Collected and published as whole families at the end, so a series omitted
	// this pass disappears rather than freezing at its last value.
	delivered := []metrics.GaugeSample{}
	migrations := []metrics.GaugeSample{}
	// Migrations are per-field: a CreateOnly field that is not delivered is a
	// migration pending. Collected as we go so the two gauges cannot disagree.
	for _, f := range clusterspec.OverlayFields() {
		declared, ok := clusterspec.RawValue(raw[f.App], f.Value...)
		if !ok {
			// The field map names a path the overlay does not declare. That is a repo
			// fault the PR-time guard catches; here it means this row measures nothing,
			// and publishing a 0 would report a delivery failure that is not one.
			continue
		}
		landed, readable, err := overlayFieldDelivered(ctx, client, f, declared)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !readable {
			// The object exists and the row does not resolve on it — the chart moved
			// what this row points at. Publishing "undelivered" would blame the cluster
			// for a stale table, so the series is withheld and the CI verb, which can
			// say WHY in words, is where that surfaces.
			continue
		}
		labels := map[string]string{"app": f.App, "path": clusterspec.OverlayFieldPath(f), "object": f.Kind + "/" + f.Name}
		delivered = append(delivered, metrics.GaugeSample{Labels: labels, Value: BoolGauge(landed)})
		if f.CreateOnly && f.Migration != "" {
			migrations = append(migrations, metrics.GaugeSample{
				Labels: map[string]string{"id": f.Migration, "path": clusterspec.OverlayFieldPath(f)},
				Value:  BoolGauge(!landed)})
		}
	}
	reg.SetGaugeFamily("llz_overlay_field_delivered",
		"1 if the live object carries the value the apl-overlay declares for this path", delivered)
	reg.SetGaugeFamily("llz_brownfield_migration_pending",
		"1 if an overlay field the API server fixes at create time is still undelivered on an object that predates it",
		migrations)
	return firstErr
}

// overlayFieldDelivered reads one object and compares. readable=false is "the row
// does not resolve on this object"; an error is "the cluster did not answer".
//
// A 404 IS NEITHER. An object that does not exist means the app is not deployed
// here, and a cluster that does not run Loki is not a cluster with an undelivered
// Loki value — it has no series at all, which is the difference between "we do
// not do that here" and "we do it and it is broken".
func overlayFieldDelivered(ctx context.Context, client capability.KubeAPI, f clusterspec.OverlayField, declared any) (delivered, readable bool, err error) {
	obj, status, err := client.GetJSON(ctx, f.APIPath())
	if err != nil {
		return false, false, fmt.Errorf("GET %s: %w", f.APIPath(), err)
	}
	if status == 404 || obj == nil {
		return false, false, nil
	}
	if status < 200 || status >= 300 {
		return false, false, fmt.Errorf("GET %s: status %d", f.APIPath(), status)
	}
	// The gate's own comparison, not a copy of it — the reconciler saying a value
	// landed while assert-overlay-applied says it did not is the disagreement a
	// second walk would eventually produce.
	match, _, ok := clusterspec.OverlayFieldDelivered(f, declared, obj)
	return match, ok, nil
}
