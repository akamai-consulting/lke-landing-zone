package argodiag

import (
	"testing"
)

func TestNeedsFullAppStatusDump(t *testing.T) {
	for _, tc := range []struct {
		name, appName, sync, health string
		want                        bool
	}{
		{"the run-6 shape: apply rejected, so OutOfSync but nothing unhealthy",
			"llz-openbao", "OutOfSync", "Healthy", true},
		{"degraded workload is equally worth the dump",
			"llz-reconciler", "Synced", "Degraded", true},
		{"status not written yet — a stalled child looks exactly like this",
			"llz-harbor", "", "", true},
		{"fully converged apps are skipped, or the dump buries the failures",
			"keycloak-keycloak", "Synced", "Healthy", false},
		{"platform-bootstrap is skipped — it already gets its own full dump",
			"platform-bootstrap", "OutOfSync", "Healthy", false},
		{"an unparseable/nameless item cannot be fetched",
			"", "OutOfSync", "Degraded", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsFullAppStatusDump(tc.appName, tc.sync, tc.health); got != tc.want {
				t.Errorf("needsFullAppStatusDump(%q, %q, %q) = %v, want %v",
					tc.appName, tc.sync, tc.health, got, tc.want)
			}
		})
	}
}
