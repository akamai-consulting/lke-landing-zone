package converge

import (
	"reflect"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
)

func TestClassifyArgoApps(t *testing.T) {
	required := []string{"platform-openbao", "platform-harbor"}
	apps := []health.AppRef{
		{Name: "platform-openbao", Sync: "Synced", Health: "Healthy"},   // required, ok
		{Name: "platform-harbor", Sync: "OutOfSync", Health: "Healthy"}, // required, unhealthy
		{Name: "some-app", Sync: "Synced", Health: "Degraded"},          // other, unhealthy
		{Name: "another", Sync: "Synced", Health: "Healthy"},            // other, ok (ignored)
		// platform-... harbor present but openbao... note: missing "platform-…" handled below
	}
	reqUnhealthy, missing, other := classifyArgoApps(apps, required)

	if want := []string{"platform-harbor sync=OutOfSync health=Healthy"}; !reflect.DeepEqual(reqUnhealthy, want) {
		t.Errorf("reqUnhealthy = %v, want %v", reqUnhealthy, want)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
	if want := []string{"some-app sync=Synced health=Degraded"}; !reflect.DeepEqual(other, want) {
		t.Errorf("otherUnhealthy = %v, want %v", other, want)
	}
}

func TestClassifyArgoAppsMissingRequired(t *testing.T) {
	required := []string{"platform-openbao", "platform-loki"}
	apps := []health.AppRef{{Name: "platform-openbao", Sync: "Synced", Health: "Healthy"}} // loki absent
	reqUnhealthy, missing, _ := classifyArgoApps(apps, required)
	if len(reqUnhealthy) != 0 {
		t.Errorf("reqUnhealthy = %v, want none", reqUnhealthy)
	}
	if want := []string{"platform-loki"}; !reflect.DeepEqual(missing, want) {
		t.Errorf("missing = %v, want %v", missing, want)
	}
}
