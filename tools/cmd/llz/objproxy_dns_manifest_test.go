package main

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const objProxyComponentDir = "../../../platform-apl/components/objProxy/obj-proxy/"

// The CoreDNS rewrite is the only file in the component that changes where Loki's
// and Harbor's bytes go. It spent its first revisions deliberately OUT of the
// kustomization, and a component whose on switch is not wired is a component that
// reports Healthy while every object still lands on Linode in the clear — the
// failure that looks most like success. If someone removes it again, that has to
// be a decision, not a diff nobody noticed.
func TestObjProxyCoreDNSRewriteIsWiredIn(t *testing.T) {
	raw, err := os.ReadFile(objProxyComponentDir + "kustomization.yaml")
	if err != nil {
		t.Fatalf("could not read the kustomization (%v) — a skip here would reproduce the gap it closes", err)
	}
	var k struct {
		Resources []string `yaml:"resources"`
	}
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatal(err)
	}
	for _, r := range k.Resources {
		if r == "coredns-rewrite.yaml" {
			return
		}
	}
	t.Errorf("coredns-rewrite.yaml is not in the objProxy kustomization resources (%v) — without it the "+
		"proxy runs, reports Ready, and takes no traffic: nothing routes the object-storage endpoint to it, "+
		"so Loki and Harbor keep writing plaintext straight to Linode", k.Resources)
}

// syncWave reads the Argo sync wave off a manifest in the component directory.
func syncWave(t *testing.T, file string) int {
	t.Helper()
	raw, err := os.ReadFile(objProxyComponentDir + file)
	if err != nil {
		t.Fatalf("could not read %s: %v", file, err)
	}
	var m struct {
		Metadata struct {
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	got, ok := m.Metadata.Annotations["argocd.argoproj.io/sync-wave"]
	if !ok {
		t.Fatalf("%s carries no argocd.argoproj.io/sync-wave — ordering here is the whole safety story", file)
	}
	w, err := strconv.Atoi(got)
	if err != nil {
		t.Fatalf("%s has a non-numeric sync-wave %q", file, got)
	}
	return w
}

// Applying the rewrite before Harbor can trust the proxy's certificate is a
// cluster-wide object-storage outage, not a degradation: registry pods dial the
// proxy, fail to verify a cert from a CA they do not have, and every push and pull
// fails. The CA bundle and the Kyverno policy that mounts it must therefore both be
// strictly earlier than the switch that sends traffic through it.
func TestObjProxyRewriteSyncsAfterTheCATrustChain(t *testing.T) {
	rewrite := syncWave(t, "coredns-rewrite.yaml")
	for _, f := range []string{"ca-trust.yaml", "kyverno-harbor-ca.yaml"} {
		if w := syncWave(t, f); w >= rewrite {
			t.Errorf("%s is at wave %d and the CoreDNS rewrite at wave %d: the rewrite must be STRICTLY later, "+
				"or traffic is routed through a proxy whose certificate Harbor cannot yet verify", f, w, rewrite)
		}
	}
}

// A later wave orders the OBJECTS; it cannot reach a pod that is already running.
// The Kyverno policy mutates on ADMISSION, so registry pods apl-core started before
// the policy existed carry no CA no matter what wave anything sits at. That gap is
// closed in code (retrofitHarborObjProxyCA), and the file that looks safe because of
// its wave has to say so — otherwise the next reader concludes ordering is handled
// and drops the retrofit.
func TestObjProxyRewriteDocumentsTheAdmissionRace(t *testing.T) {
	raw, err := os.ReadFile(objProxyComponentDir + "coredns-rewrite.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "retrofitHarborObjProxyCA") {
		t.Error("coredns-rewrite.yaml does not name retrofitHarborObjProxyCA — the sync wave orders the " +
			"objects but cannot re-admit a running pod, so the wave alone does not make this safe and the " +
			"file must not read as though it does")
	}
}
