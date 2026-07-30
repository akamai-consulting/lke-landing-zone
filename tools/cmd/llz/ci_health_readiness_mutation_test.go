package main

// Gap-closing tests for ci_health_readiness.go surfaced by mutation testing.
// These checks are warning-only, so their ONLY product is the report — which
// makes every derived cell and every count load-bearing: a wrong HA/Leader
// column, an "unreadable" banner on a healthy cluster, a ClusterSecretStore
// state relabelled NotFound, or a not-Ready certificate counted downwards all
// send the operator somewhere the problem is not (or nowhere at all).

import (
	"os"
	"strings"
	"testing"
)

// summaryBody runs fn against a fresh step-summary file and returns what was
// written to it, plus what fn printed to stdout.
func summaryBody(t *testing.T, fn func()) (summary, stdout string) {
	t.Helper()
	setSummary(t)
	out := captureStdout(t, fn)
	return readSummaryFile(t), out
}

func readSummaryFile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(os.Getenv("GITHUB_STEP_SUMMARY"))
	if err != nil {
		t.Fatalf("read step summary: %v", err)
	}
	return string(b)
}

// The HA Enabled / Leader columns are DERIVED from ha_mode, and the derivation is
// the whole content of those cells: HA Enabled means "not standalone", Leader
// means "active". Swapping either turns the Raft topology report into fiction —
// e.g. a healthy 3-pod Raft cluster reading as three standalone non-leaders.
func TestHealthOpenbaoRendersHAModeColumns(t *testing.T) {
	byPod := map[string]string{
		// is_self:true  → active     → HA enabled, leader
		"platform-openbao-0": `{"initialized":true,"sealed":false,"is_self":true,"ha_enabled":true}`,
		// ha_enabled only → standby  → HA enabled, not leader
		"platform-openbao-1": `{"initialized":true,"sealed":false,"is_self":false,"ha_enabled":true}`,
		// neither         → standalone → not HA enabled, not leader
		"platform-openbao-2": `{"initialized":true,"sealed":false}`,
	}
	stubBaoExec(t, func(pod string, _ []string) (string, error) { return byPod[pod], nil })
	stubKubectl(t, func(args []string) ([]byte, error) {
		if argsContain(args, "clustersecretstores") {
			return []byte("True"), nil
		}
		return itemsJSON(), nil
	})

	body, _ := summaryBody(t, func() {
		if err := runHealthOpenbao(); err != nil {
			t.Errorf("err = %v, want nil (warn-only)", err)
		}
	})
	// | Pod | Initialized | Sealed | HA Enabled | Leader |
	for _, want := range []string{
		"| platform-openbao-0 | true | false | true | true |",
		"| platform-openbao-1 | true | false | true | false |",
		"| platform-openbao-2 | true | false | false | false |",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("summary missing row %q — HA Enabled (ha_mode != standalone) / Leader (ha_mode == active) are mis-derived:\n%s", want, body)
		}
	}
}

// A cluster whose pods ALL answered has nothing unknown about it. Announcing
// "0/3 pod(s) unreadable" on a healthy cluster is the false alarm this warning
// was split out to avoid — it points at konnectivity when nothing is wrong.
func TestHealthOpenbaoAllReadableRaisesNoUnknownBanner(t *testing.T) {
	stubBaoExec(t, func(string, []string) (string, error) {
		return `{"initialized":true,"sealed":false,"is_self":true,"ha_enabled":true}`, nil
	})
	stubKubectl(t, func(args []string) ([]byte, error) {
		if argsContain(args, "clustersecretstores") {
			return []byte("True"), nil
		}
		return itemsJSON(), nil
	})
	body, out := summaryBody(t, func() {
		if err := runHealthOpenbao(); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
	if strings.Contains(body, "unreadable") || strings.Contains(body, "unknown") {
		t.Errorf("every pod answered, so nothing is unknown — summary must not raise the unreadable banner:\n%s", body)
	}
	if !strings.Contains(out, "All OpenBao pods unsealed") {
		t.Errorf("expected the all-unsealed verdict, got:\n%s", out)
	}
}

// The ClusterSecretStore's reported state must be the state that was READ. Only
// an EMPTY read means "NotFound"; a store that answered Ready=False must be
// reported as False, because the two send the operator to different places
// (never-created vs. created-and-failing).
func TestHealthOpenbaoClusterSecretStoreStateIsReportedVerbatim(t *testing.T) {
	run := func(t *testing.T, css string) string {
		t.Helper()
		stubBaoExec(t, func(string, []string) (string, error) {
			return `{"initialized":true,"sealed":false,"is_self":true,"ha_enabled":true}`, nil
		})
		stubKubectl(t, func(args []string) ([]byte, error) {
			if argsContain(args, "clustersecretstores") {
				return []byte(css), nil
			}
			return itemsJSON(), nil
		})
		body, _ := summaryBody(t, func() {
			if err := runHealthOpenbao(); err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
		return body
	}

	t.Run("empty read is NotFound", func(t *testing.T) {
		body := run(t, "")
		if !strings.Contains(body, "ClusterSecretStore `openbao`: NotFound") {
			t.Errorf("an empty Ready status means the store is absent:\n%s", body)
		}
	})

	t.Run("a False status stays False", func(t *testing.T) {
		body := run(t, "False")
		if !strings.Contains(body, "ClusterSecretStore `openbao`: False") {
			t.Errorf("a store that answered Ready=False must be reported as False:\n%s", body)
		}
		if strings.Contains(body, "NotFound") {
			t.Errorf("a store that ANSWERED must never be relabelled NotFound — that sends the operator to look for a missing CR:\n%s", body)
		}
	})
}

// The not-Ready tally drives the whole verdict: it decides between "Action
// required: N Certificate(s) not Ready" and "All Certificates Ready". Counting
// the wrong way lets a stuck ACME renewal report as a clean bill of health.
func TestHealthCertManagerCountsNotReadyCertificates(t *testing.T) {
	certs := []string{
		`{"metadata":{"namespace":"llz-observability","name":"otel"},"status":{"conditions":[{"type":"Ready","status":"False","message":"DNS-01 challenge failed"}]}}`,
		`{"metadata":{"namespace":"llz-openbao","name":"openbao-tls"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`,
	}
	stubKubectl(t, func([]string) ([]byte, error) { return itemsJSON(certs...), nil })
	body, out := summaryBody(t, func() {
		if err := runHealthCertManager(); err != nil {
			t.Errorf("err = %v, want nil (warn-only)", err)
		}
	})
	if !strings.Contains(body, "**Action required:** 1 Certificate(s) not Ready") {
		t.Errorf("one not-Ready Certificate must produce the action-required verdict with the count:\n%s", body)
	}
	if strings.Contains(body, "All Certificates Ready") || strings.Contains(out, "All cert-manager Certificates Ready") {
		t.Errorf("a not-Ready Certificate must never report as all-Ready:\n%s\n%s", body, out)
	}
}
