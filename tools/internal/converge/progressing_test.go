package converge

// Followed progressingCondition out of package main's coverage_tier1_test.go.

import (
	"errors"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
)

func TestProgressingCondition(t *testing.T) {
	conds := []health.Condition{
		{Type: "Available", Reason: "MinimumReplicasAvailable"},
		{Type: "Progressing", Reason: "NewReplicaSetAvailable", Message: "rollout complete"},
	}
	if r, m := progressingCondition(conds); r != "NewReplicaSetAvailable" || m != "rollout complete" {
		t.Errorf("got (%q,%q), want (NewReplicaSetAvailable, rollout complete)", r, m)
	}
	if r, m := progressingCondition([]health.Condition{{Type: "Available"}}); r != "" || m != "" {
		t.Errorf("no Progressing condition should yield empty, got (%q,%q)", r, m)
	}
	if r, m := progressingCondition(nil); r != "" || m != "" {
		t.Errorf("nil conditions should yield empty, got (%q,%q)", r, m)
	}
}

func TestPrintHealthSummary(t *testing.T) {
	hardFail := captureStdout(t, func() {
		printHealthSummary(&health.Report{Failed: []string{"openbao sealed"}, Drift: []string{"argocd OutOfSync"}})
	})
	if !strings.Contains(hardFail, "FAILED:   openbao sealed") || !strings.Contains(hardFail, "1 check(s) hard-failed.") {
		t.Errorf("hard-fail summary wrong:\n%s", hardFail)
	}
	if !strings.Contains(hardFail, "drift:    argocd OutOfSync") {
		t.Errorf("drift line missing:\n%s", hardFail)
	}

	inProgress := captureStdout(t, func() {
		printHealthSummary(&health.Report{Pending: []string{"cert Issuing"}})
	})
	if !strings.Contains(inProgress, "still converging") {
		t.Errorf("in-progress summary wrong:\n%s", inProgress)
	}

	convergedDeferred := captureStdout(t, func() {
		printHealthSummary(&health.Report{Deferred: []string{"dns token"}})
	})
	if !strings.Contains(convergedDeferred, "1 operator-deferred item(s) remain") {
		t.Errorf("deferred-converged summary wrong:\n%s", convergedDeferred)
	}

	clean := captureStdout(t, func() { printHealthSummary(&health.Report{}) })
	if !strings.Contains(clean, "Cluster converged.") {
		t.Errorf("clean summary wrong:\n%s", clean)
	}
}

func TestListArgoApps(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return []byte(`{"items":[
			{"metadata":{"name":"app1"},"status":{"sync":{"Status":"Synced"},"health":{"Status":"Healthy"}}},
			{"metadata":{"name":"app2"},"status":{"sync":{"Status":"OutOfSync"},"health":{"Status":"Degraded"}}}
		]}`), nil
	})
	apps, err := listArgoApps()
	if err != nil || len(apps) != 2 {
		t.Fatalf("listArgoApps = (%d apps, %v), want 2", len(apps), err)
	}
	if apps[0].Name != "app1" || !apps[0].Healthy() {
		t.Errorf("app1 = %+v, want Synced+Healthy", apps[0])
	}
	if apps[1].Healthy() {
		t.Errorf("app2 = %+v, want not healthy", apps[1])
	}
}

func TestListArgoAppsErrors(t *testing.T) {
	// Transport error propagates.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("no cluster") })
	if _, err := listArgoApps(); err == nil {
		t.Error("listArgoApps(exec error) = nil, want error")
	}
	// Malformed JSON is a parse error.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte("not json"), nil })
	if _, err := listArgoApps(); err == nil {
		t.Error("listArgoApps(bad json) = nil, want error")
	}
}
