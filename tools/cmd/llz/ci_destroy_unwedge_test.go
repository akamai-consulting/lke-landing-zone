package main

import (
	"reflect"
	"strings"
	"testing"
)

// apiservicesJSON models `kubectl get apiservices -o json` with a mix of:
// a stale aggregated service (deleted), a healthy aggregated service (kept), a
// built-in/local group with no .spec.service (kept), and an aggregated service
// with no Available condition at all (kept — only Available=False is stale).
const apiservicesJSON = `{
  "items": [
    {"metadata":{"name":"v1."},"spec":{},"status":{"conditions":[{"type":"Available","status":"True"}]}},
    {"metadata":{"name":"v1beta1.metrics.k8s.io"},"spec":{"service":{"name":"metrics-server","namespace":"kube-system"}},"status":{"conditions":[{"type":"Available","status":"False"}]}},
    {"metadata":{"name":"v1.acme.cert-manager.io"},"spec":{"service":{"name":"cm-webhook","namespace":"cert-manager"}},"status":{"conditions":[{"type":"Available","status":"True"}]}},
    {"metadata":{"name":"v1.pending.example.io"},"spec":{"service":{"name":"pending","namespace":"x"}},"status":{"conditions":[]}}
  ]
}`

func TestStaleAggregatedAPIServices(t *testing.T) {
	got := staleAggregatedAPIServices(apiservicesJSON)
	if want := []string{"v1beta1.metrics.k8s.io"}; !reflect.DeepEqual(got, want) {
		t.Errorf("staleAggregatedAPIServices = %v, want %v", got, want)
	}
	// Unparseable payload → nil (nothing deleted), never a panic.
	if got := staleAggregatedAPIServices("not json"); got != nil {
		t.Errorf("invalid JSON → %v, want nil", got)
	}
}

func TestParseNsNamePairs(t *testing.T) {
	out := "argocd app1\nargocd app2\nplatform proj\n\nlonely\n"
	got := parseNsNamePairs(out)
	want := []nsName{{"argocd", "app1"}, {"argocd", "app2"}, {"platform", "proj"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseNsNamePairs = %v, want %v (blank + name-less lines skipped)", got, want)
	}
}

func unwedgeFake() *fakeKubectl {
	return &fakeKubectl{responses: []kubectlRule{
		{match: "get applications.argoproj.io -A", out: "argocd app1\n", ok: true},
		{match: "get appprojects.argoproj.io -A", out: "argocd proj1\n", ok: true},
		{match: "get apiservices -o json", out: apiservicesJSON, ok: true},
		{match: "get crd clusters.postgresql.cnpg.io", out: "", ok: true},
		{match: "get crd poolers.postgresql.cnpg.io", out: "", ok: true},
		{match: "get clusters.postgresql.cnpg.io -A", out: "cnpg-system db1\n", ok: true},
		{match: "get poolers.postgresql.cnpg.io -A", out: "", ok: true},
		// everything else (healthz, scale, patch, delete) → default success
	}}
}

func TestDestroyUnwedgeHappyPath(t *testing.T) {
	f := unwedgeFake()
	if err := destroyUnwedge(f.run); err != nil {
		t.Fatalf("destroyUnwedge: %v", err)
	}
	for _, want := range []string{
		"scale statefulset/argocd-application-controller --replicas=0",
		"scale deployment/argocd-applicationset-controller --replicas=0",
		"patch applications.argoproj.io app1 -n argocd --type=merge",
		"delete applications.argoproj.io -A --all --wait=false",
		"patch appprojects.argoproj.io proj1 -n argocd --type=merge",
		// only the stale aggregated APIService is deleted
		"delete apiservice v1beta1.metrics.k8s.io --wait=false",
		"patch clusters.postgresql.cnpg.io db1 -n cnpg-system --type=merge",
	} {
		if !f.called(want) {
			t.Errorf("expected kubectl call containing %q\ncalls: %v", want, f.calls)
		}
	}
	// Exactly one APIService is deleted — the stale aggregated one; the healthy
	// aggregated, the no-Available-condition, and the core groups are all kept.
	deletes := 0
	for _, c := range f.calls {
		if strings.Contains(c, "delete apiservice") {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("want exactly 1 APIService delete (the stale aggregated one), got %d:\n%v", deletes, f.calls)
	}
}

func TestDestroyUnwedgeClusterUnreachable(t *testing.T) {
	f := &fakeKubectl{responses: []kubectlRule{
		{match: "--raw=/healthz", out: "", ok: false}, // cluster gone
	}}
	if err := destroyUnwedge(f.run); err != nil {
		t.Fatalf("unreachable cluster must be a clean no-op, got %v", err)
	}
	for _, forbidden := range []string{"scale", "patch", "delete", "get apiservices"} {
		if f.called(forbidden) {
			t.Errorf("must issue no mutations when the cluster is unreachable; saw %q", forbidden)
		}
	}
}

func TestDestroyUnwedgeCNPGCRDAbsent(t *testing.T) {
	f := unwedgeFake()
	// Override: the CNPG CRDs aren't installed on this cluster.
	f.responses = append([]kubectlRule{
		{match: "get crd clusters.postgresql.cnpg.io", out: "", ok: false},
		{match: "get crd poolers.postgresql.cnpg.io", out: "", ok: false},
	}, f.responses...)
	if err := destroyUnwedge(f.run); err != nil {
		t.Fatal(err)
	}
	if f.called("get clusters.postgresql.cnpg.io -A") || f.called("patch clusters.postgresql.cnpg.io") {
		t.Error("CNPG CRD absent → phase 4 must skip the list/patch for that kind")
	}
	// Argo phases still ran.
	if !f.called("patch applications.argoproj.io app1") {
		t.Error("Argo finalizer strip should still run when CNPG is absent")
	}
}

func TestRunCIDestroyUnwedgeRequiresKubeconfigAndWiring(t *testing.T) {
	// No KUBECONFIG_B64 / KUBECONFIG / --region → can't locate a kubeconfig.
	t.Setenv("KUBECONFIG_B64", "")
	t.Setenv("KUBECONFIG", "")
	if err := runCIDestroyUnwedge(""); err == nil || !strings.Contains(err.Error(), "KUBECONFIG_B64") {
		t.Errorf("err = %v, want a KUBECONFIG_B64/KUBECONFIG/--region requirement", err)
	}
	t.Setenv("KUBECONFIG_B64", "!!not base64!!")
	if err := runCIDestroyUnwedge(""); err == nil || !strings.Contains(err.Error(), "base64") {
		t.Errorf("err = %v, want invalid-base64 error", err)
	}
	if c := ciDestroyUnwedgeCmd(); c.Use != "destroy-unwedge" {
		t.Errorf("Use = %q, want destroy-unwedge", c.Use)
	}
}

// --region resolves the kubeconfig by label; an already-reaped cluster (found=false)
// is a clean no-op (exit 0, no kubectl).
func TestRunCIDestroyUnwedgeRegionSkipsWhenClusterGone(t *testing.T) {
	t.Setenv("KUBECONFIG_B64", "")
	t.Setenv("KUBECONFIG", "")
	prev := unwedgeResolveKubeconfigFn
	unwedgeResolveKubeconfigFn = func(string) (string, bool, error) { return "", false, nil }
	t.Cleanup(func() { unwedgeResolveKubeconfigFn = prev })
	if err := runCIDestroyUnwedge("primary"); err != nil {
		t.Errorf("a gone cluster should be a clean no-op, got %v", err)
	}
}
