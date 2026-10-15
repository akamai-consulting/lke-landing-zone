package overlay

// alertset_test.go — spec.alerting's Slack half must actually REACH apl-core's
// AplAlertSet on the machine branch. The render half alone is the otomi.yaml
// lesson: a file `llz render` writes and no target consumes looks wired and does
// nothing. These assert on what lands on the TARGET branch.

import (
	"context"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
)

// The Console's alert settings, as apl-core keeps them (its fixture's shape),
// with keys LLZ does not own.
const consoleAlertsTarget = `kind: AplAlertSet
metadata:
    name: alerts
spec:
    groupInterval: 10m
    msteams: {}
    receivers:
        - none
    repeatInterval: 6h
`

func alertsRepo(a clusterspec.Alerting, target string, targetExists bool) *branchRepo {
	src := map[string]string{}
	if body := clusterspec.RenderAlertSetOverlay(a); body != "" {
		src[envOverlayPath("primary", clusterspec.OverlayAlertsFile)] = body
	}
	tgt := map[string]string{}
	if targetExists {
		tgt[aplAlertsTarget] = target
	}
	return &branchRepo{src: src, tgt: tgt}
}

func slackSpec() clusterspec.Alerting {
	return clusterspec.Alerting{Receivers: []string{"slack"}, Slack: clusterspec.AlertingSlack{Channel: "ops"}}
}

// THE GATE. A declared Slack receiver lands on apl-core's own CR, and the
// Console's other settings survive the merge.
func TestSlackReachesAplCoresAlertSet(t *testing.T) {
	repo := alertsRepo(slackSpec(), consoleAlertsTarget, true)
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got, ok := repo.gotFiles[aplAlertsTarget]
	if !ok {
		t.Fatalf("%s was never written — the rendered overlay reached nothing: %v", aplAlertsTarget, keysOf(repo.gotFiles))
	}
	for _, want := range []string{"- slack", "channel: ops", "channelCrit: ops", "groupInterval: 10m", "repeatInterval: 6h", "msteams: {}"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s missing %q after the merge:\n%s", aplAlertsTarget, want, got)
		}
	}
}

// NO SOURCE MEANS NO OPINION: an instance without spec.alerting leaves apl-core's
// alert settings exactly as the Console set them.
func TestUndeclaredAlertingWritesNothing(t *testing.T) {
	repo := alertsRepo(clusterspec.Alerting{}, consoleAlertsTarget, true)
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := repo.gotFiles[aplAlertsTarget]; ok {
		t.Errorf("no spec.alerting must mean no opinion, but %s was written", aplAlertsTarget)
	}
}

// AN ABSENT TARGET IS CREATED — unlike otomi.yaml, an AplAlertSet has no
// required key, so the source is a complete CR and refusing would leave Slack
// asserted by nothing.
func TestAnAbsentAlertSetIsCreated(t *testing.T) {
	repo := alertsRepo(slackSpec(), "", false)
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := repo.gotFiles[aplAlertsTarget]; !strings.Contains(got, "kind: AplAlertSet") || !strings.Contains(got, "- slack") {
		t.Errorf("an absent target must be created from the source, got:\n%s", got)
	}
}

// ALREADY CORRECT → NO PUSH, or the reconciler fights the Console forever.
func TestAnAlreadyCorrectAlertSetDoesNotPush(t *testing.T) {
	src := []byte(clusterspec.RenderAlertSetOverlay(slackSpec()))
	want, _ := clusterspec.ParseAlertSetOverlay(src)
	current, _, _ := clusterspec.SetAlertSetSpec([]byte(consoleAlertsTarget), want)
	repo := alertsRepo(slackSpec(), string(current), true)
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := repo.gotFiles[aplAlertsTarget]; ok {
		t.Error("the AplAlertSet already matches — pushing it again churns")
	}
}

// A TARGET THAT IS NOT AN AplAlertSet IS NEVER MERGED INTO, and says so as a
// gauge (the pod's stdout is read by nothing).
func TestAForeignAlertsTargetIsRefusedVisibly(t *testing.T) {
	for _, target := range []string{"", "{}", "kind: Something\n", "kind: AplAlertSet\n---\nkind: AplAlertSet\n"} {
		repo := alertsRepo(slackSpec(), target, true)
		reg := metrics.NewRegistry()
		if err := Reconcile(context.Background(), testCfg(), repo, credsOK, reg); err != nil {
			t.Fatalf("Reconcile(%q): %v", target, err)
		}
		if _, ok := repo.gotFiles[aplAlertsTarget]; ok {
			t.Errorf("target %q: merged into a file that is not one AplAlertSet", target)
		}
		if !strings.Contains(renderedMetrics(t, reg), `llz_apl_overlay_alerts_target_unusable{branch="apl-primary"} 1`) {
			t.Errorf("target %q: refused silently — no unusable gauge:\n%s", target, renderedMetrics(t, reg))
		}
	}
}
