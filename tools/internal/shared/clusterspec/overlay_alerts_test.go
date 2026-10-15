package clusterspec

import (
	"slices"
	"strings"
	"testing"
)

// apl-core's own fixture (tests/fixtures/env/settings/alerts.yaml) plus the
// Console-owned keys LLZ must never clobber.
const consoleAlertSet = `kind: AplAlertSet
metadata:
    name: alerts
spec:
    groupInterval: 10m
    msteams: {}
    receivers:
        - msteams
    repeatInterval: 6h
    slack:
        channel: old
`

// THE RENDERED CR IS apl-core's SHAPE AND CARRIES NO SECRET. kind/name match
// apl-core's file map; slack.url — the x-secret apl-core would seal into git —
// is absent, because LLZ delivers it from OpenBao.
func TestAlertSetOverlayIsAplCoresShapeWithoutTheSecret(t *testing.T) {
	got := RenderAlertSetOverlay(Alerting{Receivers: []string{"pagerduty", "slack"}, Slack: AlertingSlack{Channel: "#ops"}})
	m, err := unmarshalMap([]byte(got))
	if err != nil {
		t.Fatalf("not YAML: %v\n%s", err, got)
	}
	if m["kind"] != alertSetKind || dig(t, m, "metadata", "name") != alertSetName {
		t.Errorf("kind/name = %v/%v, want %s/%s", m["kind"], dig(t, m, "metadata", "name"), alertSetKind, alertSetName)
	}
	recv := asList(t, dig(t, m, "spec", "receivers"))
	if len(recv) != 1 || recv[0] != "slack" {
		t.Errorf("apl-core receivers = %v, want [slack] — pagerduty is LLZ's CR, not apl-core's", recv)
	}
	slack := dig(t, m, "spec", "slack").(map[string]any)
	if slack["channel"] != "ops" || slack["channelCrit"] != "ops" {
		t.Errorf("slack = %v, want both channels = ops (one webhook, one channel)", slack)
	}
	if _, ok := slack["url"]; ok || strings.Contains(got, "url") {
		t.Errorf("the overlay carries a Slack url — it would put the webhook in git:\n%s", got)
	}
	// pagerduty-only pins apl-core's receivers to [none].
	if got := RenderAlertSetOverlay(Alerting{Receivers: []string{"pagerduty"}}); !strings.Contains(got, "- none") || strings.Contains(got, "slack:") {
		t.Errorf("pagerduty-only must render receivers [none] and no slack block:\n%s", got)
	}
}

// THE MERGE IS KEY-LEVEL: the Console's groupInterval, repeatInterval and msteams
// block survive; receivers and the channel are LLZ's.
func TestSetAlertSetSpecPreservesTheConsolesKeys(t *testing.T) {
	src := RenderAlertSetOverlay(Alerting{Receivers: []string{"slack"}, Slack: AlertingSlack{Channel: "ops"}})
	want, err := ParseAlertSetOverlay([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	updated, changed, err := SetAlertSetSpec([]byte(consoleAlertSet), want)
	if err != nil || !changed {
		t.Fatalf("SetAlertSetSpec: changed=%v err=%v", changed, err)
	}
	m, _ := unmarshalMap(updated)
	if r := asList(t, dig(t, m, "spec", "receivers")); len(r) != 1 || r[0] != "slack" {
		t.Errorf("receivers = %v, want [slack]", r)
	}
	for _, k := range []string{"groupInterval", "repeatInterval", "msteams"} {
		if _, ok := dig(t, m, "spec").(map[string]any)[k]; !ok {
			t.Errorf("the merge dropped the Console's spec.%s:\n%s", k, updated)
		}
	}
	if dig(t, m, "spec", "slack", "channel") != "ops" || dig(t, m, "spec", "slack", "channelCrit") != "ops" {
		t.Errorf("slack channels not asserted:\n%s", updated)
	}
	// A second pass over its own output is a no-op — no churn against apl-core.
	if _, changed, _ := SetAlertSetSpec(updated, want); changed {
		t.Error("an already-correct AplAlertSet reported changed — the reconciler would push every pass")
	}
}

func TestAlertSetMergeabilityAndSourceParsing(t *testing.T) {
	for _, tc := range []struct {
		target string
		ok     bool
	}{
		{consoleAlertSet, true},
		{"", false},
		{"{}", false},
		{"kind: AplCapabilitySet\nmetadata: {name: otomi}\n", false},
	} {
		if got := AlertSetCRIsMergeable([]byte(tc.target)); got != tc.ok {
			t.Errorf("AlertSetCRIsMergeable(%q) = %v, want %v", tc.target, got, tc.ok)
		}
	}
	if _, err := ParseAlertSetOverlay([]byte("kind: AplAlertSet\nspec: {}\n")); err == nil {
		t.Error("a source asserting no receivers parsed — the reconciler would write receivers: [] over apl-core's")
	}
	d, err := ParseAlertSetOverlay([]byte(RenderAlertSetOverlay(Alerting{Receivers: []string{"none"}})))
	if err != nil || !slices.Equal(d.Receivers, []string{"none"}) || d.Slack != nil {
		t.Errorf("[none] source parsed as %+v, %v", d, err)
	}
}
