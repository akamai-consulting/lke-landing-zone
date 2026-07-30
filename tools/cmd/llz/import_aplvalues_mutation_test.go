package main

import (
	"reflect"
	"testing"
)

// TestParseAplValuesDerivesDomains pins the Domains list. Downstream, buildReport
// folds these APL-authoritative facts into the cluster-derived report, and
// Domains is what an operator compares against the DNS/ingress they already run.
// A cluster with a domainSuffix and an EMPTY Domains list reads as "APL manages
// no domain here", which is the opposite of the truth; a list holding a single
// empty string is worse still — it looks populated.
func TestParseAplValuesDerivesDomains(t *testing.T) {
	sig, err := parseAplValues(aplValuesFixture)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"lke579582.akamai-apl.net"}
	if !reflect.DeepEqual(sig.Domains, want) {
		t.Errorf("Domains = %#v, want %#v (derived from cluster.domainSuffix)", sig.Domains, want)
	}

	// No domainSuffix → no domains at all, not a list holding "".
	noSuffix := "cluster:\n  name: aplinstall1\notomi:\n  version: v4.14.1\n"
	sig, err = parseAplValues(noSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig.Domains) != 0 {
		t.Errorf("Domains = %#v, want empty when cluster.domainSuffix is absent", sig.Domains)
	}
}
