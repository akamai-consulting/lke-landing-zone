package main

import (
	"strings"
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

func TestReadForwardPort(t *testing.T) {
	out := "Forwarding from 127.0.0.1:54321 -> 9090\nForwarding from [::1]:54321 -> 9090\n"
	got, err := readForwardPort(strings.NewReader(out))
	if err != nil || got != "54321" {
		t.Fatalf("readForwardPort = %q, %v; want 54321", got, err)
	}
	if _, err := readForwardPort(strings.NewReader("no port line here\n")); err == nil {
		t.Error("readForwardPort should error when no Forwarding line is present")
	}
}
