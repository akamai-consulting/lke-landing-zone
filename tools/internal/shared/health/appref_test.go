package health

// TestArgoAppHealthy followed the method it tests. The other two tests in
// converge's statushealth_test.go drive that package's own classifier, which
// consumes an AppRef without owning it, and correctly stayed behind.

import (
	"testing"
)

func TestArgoAppHealthy(t *testing.T) {
	if !(AppRef{"x", "Synced", "Healthy"}).Healthy() {
		t.Error("Synced+Healthy should be healthy")
	}
	if (AppRef{"x", "Synced", "Progressing"}).Healthy() {
		t.Error("Progressing is not healthy")
	}
}

// ParseAppRefList arrived here at 0%. It is worth covering for the reason its own
// comment gives: "the same anonymous struct and the same append loop were written
// out twice, here and in selectPlatformApps (verify.go) — identical down to the
// field tags." A decoder duplicated by hand is one whose field tags can drift from
// the API shape silently, and the failure looks like an empty list rather than an
// error.
func TestParseAppRefList(t *testing.T) {
	const raw = `{"items":[
	  {"metadata":{"name":"platform-openbao"},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}},
	  {"metadata":{"name":"platform-harbor"},"status":{"sync":{"status":"OutOfSync"},"health":{"status":"Degraded"}}}
	]}`
	apps, err := ParseAppRefList([]byte(raw))
	if err != nil {
		t.Fatalf("ParseAppRefList: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("got %d apps, want 2: %+v", len(apps), apps)
	}
	if apps[0] != (AppRef{Name: "platform-openbao", Sync: "Synced", Health: "Healthy"}) {
		t.Errorf("first app = %+v", apps[0])
	}
	if apps[1].Healthy() {
		t.Error("OutOfSync/Degraded must not read as healthy")
	}

	// An empty list is a legitimate answer (no Applications yet), and must not be
	// confused with a parse failure — convergence polls this before Argo has
	// created anything.
	empty, err := ParseAppRefList([]byte(`{"items":[]}`))
	if err != nil || len(empty) != 0 {
		t.Errorf("empty items = %+v, %v; want an empty list and no error", empty, err)
	}

	if _, err := ParseAppRefList([]byte("not json")); err == nil {
		t.Error("malformed JSON must error rather than yield an empty list — a silent " +
			"empty list reads as 'nothing is deployed yet' and the poll waits forever")
	}
}
