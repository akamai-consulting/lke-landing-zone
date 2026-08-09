package assertobs

import (
	"testing"
)

func TestParsePromSpec(t *testing.T) {
	ns, svc, port, err := parsePromSpec("monitoring/po-prometheus:9090")
	if err != nil || ns != "monitoring" || svc != "po-prometheus" || port != "9090" {
		t.Fatalf("got ns=%q svc=%q port=%q err=%v", ns, svc, port, err)
	}
	for _, bad := range []string{"noslash:9090", "monitoring/noport", "monitoring/:9090", "monitoring/svc:"} {
		if _, _, _, err := parsePromSpec(bad); err == nil {
			t.Errorf("parsePromSpec(%q) should error", bad)
		}
	}
}
