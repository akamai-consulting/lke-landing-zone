package main

import (
	"reflect"
	"testing"
)

func TestClassifyArgoApps(t *testing.T) {
	required := []string{"platform-openbao", "platform-harbor"}
	apps := []argoApp{
		{"platform-openbao", "Synced", "Healthy"},   // required, ok
		{"platform-harbor", "OutOfSync", "Healthy"}, // required, unhealthy
		{"some-app", "Synced", "Degraded"},          // other, unhealthy
		{"another", "Synced", "Healthy"},            // other, ok (ignored)
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
	apps := []argoApp{{"platform-openbao", "Synced", "Healthy"}} // loki absent
	reqUnhealthy, missing, _ := classifyArgoApps(apps, required)
	if len(reqUnhealthy) != 0 {
		t.Errorf("reqUnhealthy = %v, want none", reqUnhealthy)
	}
	if want := []string{"platform-loki"}; !reflect.DeepEqual(missing, want) {
		t.Errorf("missing = %v, want %v", missing, want)
	}
}

func TestArgoAppHealthy(t *testing.T) {
	if !(argoApp{"x", "Synced", "Healthy"}).healthy() {
		t.Error("Synced+Healthy should be healthy")
	}
	if (argoApp{"x", "Synced", "Progressing"}).healthy() {
		t.Error("Progressing is not healthy")
	}
}
