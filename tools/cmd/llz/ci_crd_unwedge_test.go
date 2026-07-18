package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// crdListJSON builds a `kubectl get crd -o json` body from name->last-applied-
// configuration-size pairs (0 = the annotation is absent).
func crdListJSON(t *testing.T, sizes map[string]int) string {
	t.Helper()
	type meta struct {
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}
	type item struct {
		Metadata meta `json:"metadata"`
	}
	var items []item
	for name, sz := range sizes {
		m := meta{Name: name}
		if sz > 0 {
			m.Annotations = map[string]string{staleApplyAnnotation: strings.Repeat("x", sz)}
		}
		items = append(items, item{Metadata: m})
	}
	b, err := json.Marshal(struct {
		Items []item `json:"items"`
	}{items})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStripOversizedCRDLastApplied(t *testing.T) {
	over := crdUnwedgeThreshold + 1
	under := crdUnwedgeThreshold - 1

	t.Run("strips only over-threshold CRDs", func(t *testing.T) {
		body := crdListJSON(t, map[string]int{
			"httproutes.gateway.networking.k8s.io": over,  // wedged/near-limit
			"clusterpolicies.kyverno.io":           0,     // SSA-clean (no annotation)
			"certificates.cert-manager.io":         under, // present but small
		})
		var stripped []string
		got := stripOversizedCRDLastApplied(func(args ...string) (string, bool) {
			if len(args) >= 2 && args[0] == "get" && args[1] == "crd" {
				return body, true
			}
			if len(args) >= 3 && args[0] == "annotate" && args[1] == "crd" {
				if args[3] != staleApplyAnnotation+"-" {
					t.Errorf("annotate removed %q, want %q", args[3], staleApplyAnnotation+"-")
				}
				stripped = append(stripped, args[2])
				return "", true
			}
			t.Errorf("unexpected kubectl call: %v", args)
			return "", false
		})
		if strings.Join(got, ",") != strings.Join(stripped, ",") {
			t.Errorf("returned %v but recorded %v strips", got, stripped)
		}
		if len(got) != 1 || got[0] != "httproutes.gateway.networking.k8s.io" {
			t.Errorf("stripped %v, want only the over-threshold httproutes CRD", got)
		}
	})

	t.Run("no CRDs / read failure is a no-op", func(t *testing.T) {
		got := stripOversizedCRDLastApplied(func(args ...string) (string, bool) {
			return "", false // e.g. fresh cluster, CRD API not up yet
		})
		if got != nil {
			t.Errorf("expected no strips on read failure, got %v", got)
		}
	})

	t.Run("annotate failure is non-fatal and not counted", func(t *testing.T) {
		body := crdListJSON(t, map[string]int{"httproutes.gateway.networking.k8s.io": over})
		got := stripOversizedCRDLastApplied(func(args ...string) (string, bool) {
			if args[0] == "get" {
				return body, true
			}
			return "forbidden", false // annotate denied
		})
		if got != nil {
			t.Errorf("annotate failure must not be counted as stripped, got %v", got)
		}
	})
}
