package main

import (
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// aplValuesFixture mirrors the shape of a real APL "DOWNLOAD PLATFORM VALUES"
// file, including the secret-bearing sections (kms/obj keys/adminPassword) that
// the parser MUST ignore.
const aplValuesFixture = `
cluster:
  domainSuffix: lke579582.akamai-apl.net
  name: aplinstall579582
apps:
  harbor:
    enabled: true
  gitea:
    enabled: false
  loki:
    enabled: true
teamConfig:
  gsap: {}
  payments: {}
  admin: {}
otomi:
  version: v4.14.1
  hasExternalDNS: false
  hasExternalIDP: false
  isMultitenant: true
  adminPassword: SUPER-SECRET-PASSWORD
kms:
  sops:
    age:
      privateKey: AGE-SECRET-KEY-LEAKME
obj:
  provider:
    linode:
      accessKeyId: LEAKEDACCESSKEY
      secretAccessKey: LEAKEDSECRETKEY
      region: us-ord-1
      buckets:
        harbor: lke579582-harbor
        loki: lke579582-loki
`

func TestParseAplValues(t *testing.T) {
	sig, err := parseAplValues(aplValuesFixture)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sig.Teams, []string{"gsap", "payments"}) { // admin excluded
		t.Errorf("teams=%v", sig.Teams)
	}
	if !reflect.DeepEqual(sig.EnabledApps, []string{"harbor", "loki"}) {
		t.Errorf("enabledApps=%v", sig.EnabledApps)
	}
	if !reflect.DeepEqual(sig.DisabledApps, []string{"gitea"}) {
		t.Errorf("disabledApps=%v", sig.DisabledApps)
	}
	if sig.DomainSuffix != "lke579582.akamai-apl.net" || sig.AplVersion != "v4.14.1" {
		t.Errorf("suffix=%q version=%q", sig.DomainSuffix, sig.AplVersion)
	}
	if sig.ExternalDNS == nil || *sig.ExternalDNS || sig.Multitenant == nil || !*sig.Multitenant {
		t.Errorf("flags: externalDNS=%v multitenant=%v", sig.ExternalDNS, sig.Multitenant)
	}
	if sig.ObjectRegion != "us-ord-1" || sig.ObjectBuckets["harbor"] != "lke579582-harbor" {
		t.Errorf("object storage=%+v region=%q", sig.ObjectBuckets, sig.ObjectRegion)
	}
}

// TestParseAplValuesNeverLeaksSecrets is the security guard: no secret value from
// the fixture may appear anywhere in the marshaled aplSignals.
func TestParseAplValuesNeverLeaksSecrets(t *testing.T) {
	sig, err := parseAplValues(aplValuesFixture)
	if err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(sig)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"SUPER-SECRET-PASSWORD", "AGE-SECRET-KEY-LEAKME", "LEAKEDACCESSKEY", "LEAKEDSECRETKEY",
	} {
		if strings.Contains(string(out), secret) {
			t.Errorf("SECRET LEAK: %q present in parsed output:\n%s", secret, out)
		}
	}
}

func TestFirstAplSignals(t *testing.T) {
	repos := []repoInventory{
		{Role: "git", Path: "/x"},
		{Role: "apl", Path: "/vals", APL: &aplSignals{AplVersion: "v4.14.1"}},
	}
	if got := firstAplSignals(repos); got == nil || got.AplVersion != "v4.14.1" {
		t.Errorf("firstAplSignals=%+v", got)
	}
	if got := firstAplSignals([]repoInventory{{Role: "git"}}); got != nil {
		t.Errorf("expected nil when no apl inventory, got %+v", got)
	}
}

func TestAplComponentsFromApps(t *testing.T) {
	got := aplComponentsFromApps([]string{"harbor", "loki", "grafana", "kyverno", "unknown-app"})
	want := map[string]bool{"harbor": true, "observability": true, "policyEngine": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("components=%v, want %v", got, want)
	}
}
