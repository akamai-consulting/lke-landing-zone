package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"reflect"
	"testing"
)

func TestParseSecretInventory(t *testing.T) {
	js := `{"items":[
		{"metadata":{"name":"app-creds","namespace":"team-gsap"},"type":"Opaque"},
		{"metadata":{"name":"tls","namespace":"team-gsap"},"type":"kubernetes.io/tls"},
		{"metadata":{"name":"sa-token","namespace":"team-gsap"},"type":"kubernetes.io/service-account-token"},
		{"metadata":{"name":"sh.helm.release.v1.x","namespace":"team-gsap"},"type":"helm.sh/release.v1"}
	]}`
	got := parseSecretInventory(js)
	want := []secretRef{{Name: "app-creds", Type: "Opaque"}, {Name: "tls", Type: "kubernetes.io/tls"}}
	if !reflect.DeepEqual(got["team-gsap"], want) {
		t.Errorf("secretRefs=%+v, want %+v", got["team-gsap"], want)
	}
}

func TestImagesByNamespace(t *testing.T) {
	wls := []workload{
		{Namespace: "team-gsap", Images: []string{"nginx:1.25", "nginx:1.25"}},
		{Namespace: "team-gsap", Images: []string{"app:v1"}},
	}
	got := imagesByNamespace(wls)
	if !reflect.DeepEqual(got["team-gsap"], []string{"app:v1", "nginx:1.25"}) { // deduped + sorted
		t.Errorf("images=%v", got["team-gsap"])
	}
}

func TestCountByNamespace(t *testing.T) {
	js := `{"items":[
		{"metadata":{"name":"cfg","namespace":"team-gsap"}},
		{"metadata":{"name":"kube-root-ca.crt","namespace":"team-gsap"}},
		{"metadata":{"name":"cfg2","namespace":"team-x"}}
	]}`
	got := countByNamespace(js, skipNoiseConfigMap)
	if got["team-gsap"] != 1 || got["team-x"] != 1 { // kube-root-ca.crt excluded
		t.Errorf("counts=%v", got)
	}
	all := countByNamespace(js, nil)
	if all["team-gsap"] != 2 {
		t.Errorf("unfiltered team-gsap=%d, want 2", all["team-gsap"])
	}
}

func TestParsePVs(t *testing.T) {
	js := `{"items":[{
		"metadata":{"name":"pvc-abc"},
		"spec":{
			"capacity":{"storage":"8Gi"},
			"accessModes":["ReadWriteOnce"],
			"persistentVolumeReclaimPolicy":"Delete",
			"storageClassName":"linode-block-storage",
			"claimRef":{"namespace":"team-gsap","name":"data-web-0"},
			"csi":{"volumeHandle":"123456-pvcabc"}
		}}]}`
	got := parsePVs(js)
	if len(got) != 1 {
		t.Fatalf("got %d PVs, want 1", len(got))
	}
	want := pvInfo{
		Name: "pvc-abc", Claim: "team-gsap/data-web-0", Capacity: "8Gi",
		StorageClass: "linode-block-storage", AccessModes: []string{"ReadWriteOnce"},
		ReclaimPolicy: "Delete", VolumeHandle: "123456-pvcabc",
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("pv=%+v, want %+v", got[0], want)
	}
}

func TestParsePVCConsumersAndWorkloadFromOwner(t *testing.T) {
	js := `{"items":[
		{"metadata":{"name":"harbor-otomi-db-1","namespace":"harbor","labels":{"cnpg.io/cluster":"harbor-otomi-db"},
		 "ownerReferences":[{"kind":"StatefulSet","name":"harbor-otomi-db"}]},
		 "spec":{"containers":[{"image":"ghcr.io/cloudnative-pg/postgresql:16"}],
		         "volumes":[{"persistentVolumeClaim":{"claimName":"harbor-otomi-db-1"}}]}},
		{"metadata":{"name":"grafana-7d9f8c-abc","namespace":"monitoring","labels":{"app.kubernetes.io/name":"grafana"},
		 "ownerReferences":[{"kind":"ReplicaSet","name":"grafana-7d9f8c"}]},
		 "spec":{"containers":[{"image":"grafana/grafana:10"}],
		         "volumes":[{"persistentVolumeClaim":{"claimName":"grafana-pvc"}}]}},
		{"metadata":{"name":"loner","namespace":"x"},
		 "spec":{"containers":[{"image":"app:1"}],"volumes":[{"persistentVolumeClaim":{"claimName":"loner-data"}}]}}
	]}`
	got := parsePVCConsumers(js)

	if c := got["harbor/harbor-otomi-db-1"]; c.Workload != "StatefulSet/harbor-otomi-db" || c.Image != "ghcr.io/cloudnative-pg/postgresql:16" {
		t.Errorf("cnpg consumer=%+v", c)
	}
	if c := got["monitoring/grafana-pvc"]; c.Workload != "Deployment/grafana" || c.App != "grafana" {
		t.Errorf("ReplicaSet should fold to Deployment/grafana, got %+v", c)
	}
	if c := got["x/loner-data"]; c.Workload != "Pod/loner" { // no controller
		t.Errorf("ownerless pod=%+v", c)
	}
}

func TestClassifyVolumes(t *testing.T) {
	pvs := []pvInfo{
		{Name: "pv-db", Claim: "harbor/harbor-otomi-db-1"},
		{Name: "pv-cache", Claim: "argocd/redis-data"},
		{Name: "pv-loki", Claim: "monitoring/loki-0"},
		{Name: "pv-metrics", Claim: "monitoring/prometheus-0"},
		{Name: "pv-app", Claim: "team-gsap/yakpurger-data"},
		{Name: "pv-tekton", Claim: "team-gsap/pvc-build123"},
		{Name: "pv-orphan", Claim: "team-gsap/leftover"},
	}
	consumers := map[string]pvcConsumer{
		"harbor/harbor-otomi-db-1": {Workload: "StatefulSet/harbor-otomi-db", Image: "cloudnative-pg/postgresql:16"},
		"argocd/redis-data":        {Workload: "StatefulSet/argocd-redis", Image: "redis:7"},
		"monitoring/loki-0":        {Workload: "StatefulSet/loki", App: "loki", Image: "grafana/loki:2.9"},
		"monitoring/prometheus-0":  {Workload: "StatefulSet/prometheus", Image: "prom/prometheus:v2"},
		"team-gsap/yakpurger-data": {Workload: "Deployment/yakpurger", Image: "ghcr.io/gsap/yakpurger:1"},
		"team-gsap/pvc-build123":   {Workload: "TaskRun/docker-trigger-build-fetch-source", Image: "gcr.io/tekton/entrypoint"},
		// leftover has no consumer → unused
	}
	out, byClass := classifyVolumes(pvs, consumers)

	want := map[string]string{
		"pv-db": "database", "pv-cache": "cache", "pv-loki": "object-store-cache",
		"pv-metrics": "metrics", "pv-app": "standalone", "pv-tekton": "ephemeral", "pv-orphan": "unused",
	}
	for _, pv := range out {
		if pv.Classification != want[pv.Name] {
			t.Errorf("%s classified %q, want %q", pv.Name, pv.Classification, want[pv.Name])
		}
	}
	if out[0].UsedBy != "StatefulSet/harbor-otomi-db" || !out[0].InUse {
		t.Errorf("pv-db usage not set: %+v", out[0])
	}
	if pv := out[6]; pv.Name != "pv-orphan" || pv.InUse || pv.UsedBy != "" {
		t.Errorf("orphan should be not-in-use: %+v", pv)
	}
	if byClass["database"] != 1 || byClass["unused"] != 1 || byClass["standalone"] != 1 {
		t.Errorf("byClass=%v", byClass)
	}
}

func TestParsePodSecretRefsAndAttachDBClients(t *testing.T) {
	// harbor-core references the CNPG -app secret via env; an unrelated pod doesn't.
	pods := `{"items":[
		{"metadata":{"name":"harbor-core-5dbcf89759-x","namespace":"harbor","ownerReferences":[{"kind":"ReplicaSet","name":"harbor-core-5dbcf89759"}]},
		 "spec":{"containers":[{"env":[{"valueFrom":{"secretKeyRef":{"name":"harbor-otomi-db-app"}}}]}]}},
		{"metadata":{"name":"keycloak-keycloakx-0","namespace":"keycloak","ownerReferences":[{"kind":"StatefulSet","name":"keycloak-keycloakx"}]},
		 "spec":{"containers":[{"envFrom":[{"secretRef":{"name":"keycloak-db-app"}}]}]}},
		{"metadata":{"name":"random","namespace":"harbor"},
		 "spec":{"containers":[{"env":[{"valueFrom":{"secretKeyRef":{"name":"some-other-secret"}}}]}]}}
	]}`
	uses := parsePodSecretRefs(pods)

	dbs := []dbInfo{
		{Namespace: "harbor", Name: "harbor-otomi-db", Kind: "CNPG", Engine: "postgres"},
		{Namespace: "keycloak", Name: "keycloak-db", Kind: "CNPG", Engine: "postgres"},
		{Namespace: "team-gsap", Name: "redis-cache", Kind: "workload", Engine: "redis"}, // not CNPG → no clients
	}
	got := attachDBClients(dbs, uses)

	if !reflect.DeepEqual(got[0].Clients, []string{"Deployment/harbor-core"}) { // ReplicaSet folded
		t.Errorf("harbor clients=%v", got[0].Clients)
	}
	if !reflect.DeepEqual(got[1].Clients, []string{"StatefulSet/keycloak-keycloakx"}) {
		t.Errorf("keycloak clients=%v", got[1].Clients)
	}
	if got[2].Clients != nil { // workload-kind DBs are left alone
		t.Errorf("workload DB should have no clients, got %v", got[2].Clients)
	}
}

func TestParseCNPGClusters(t *testing.T) {
	js := `{"items":[{"metadata":{"name":"pg","namespace":"team-gsap"},"spec":{"instances":3}}]}`
	got := parseCNPGClusters(js)
	if len(got) != 1 || got[0].Kind != "CNPG" || got[0].Engine != "postgres" || got[0].Instances != 3 {
		t.Errorf("cnpg=%+v", got)
	}
}

func TestDetectDBWorkloads(t *testing.T) {
	wls := []workload{
		{Namespace: "team-gsap", Name: "db", Images: []string{"bitnami/postgresql:16"}},
		{Namespace: "team-gsap", Name: "cache", Images: []string{"redis:7"}},
		{Namespace: "team-gsap", Name: "web", Images: []string{"nginx:1.25"}},
	}
	got := detectDBWorkloads(wls)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (postgres + redis)", len(got))
	}
	if got[0].Engine != "postgres" || got[1].Engine != "redis" {
		t.Errorf("engines=%+v", got)
	}
}

func TestParsePeerAuthModes(t *testing.T) {
	js := `{"items":[
		{"spec":{"mtls":{"mode":"STRICT"}}},
		{"spec":{"mtls":{"mode":"PERMISSIVE"}}},
		{"spec":{"mtls":{"mode":"STRICT"}}}
	]}`
	got := parsePeerAuthModes(js)
	if !reflect.DeepEqual(got, []string{"PERMISSIVE", "STRICT"}) {
		t.Errorf("modes=%v", got)
	}
}

func TestTotalCount(t *testing.T) {
	if totalCount(map[string]int{"a": 3, "b": 2}) != 5 {
		t.Error("expected 5")
	}
	if totalCount(nil) != 0 {
		t.Error("nil → 0")
	}
}

// helmSecretJSON builds a secrets-list JSON containing one helm.sh/release.v1
// secret whose data.release is the real double-base64(gzip(json)) encoding.
func helmSecretJSON(t *testing.T, releaseJSON string) string {
	t.Helper()
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write([]byte(releaseJSON)); err != nil {
		t.Fatal(err)
	}
	w.Close()
	inner := base64.StdEncoding.EncodeToString(gz.Bytes())    // helm's base64(gzip(json))
	outer := base64.StdEncoding.EncodeToString([]byte(inner)) // kubectl's base64 of the stored bytes
	return `{"items":[{"type":"helm.sh/release.v1","data":{"release":"` + outer + `"}}]}`
}

func TestParseHelmReleases(t *testing.T) {
	rel1 := `{"name":"harbor","namespace":"harbor","version":2,"info":{"status":"deployed"},"chart":{"metadata":{"name":"harbor","version":"1.13.0"}}}`
	js := helmSecretJSON(t, rel1)
	got := parseHelmReleases(js)
	if len(got) != 1 {
		t.Fatalf("got %d releases, want 1", len(got))
	}
	want := helmRelease{Name: "harbor", Namespace: "harbor", Chart: "harbor", ChartVersion: "1.13.0", Status: "deployed", Revision: 2}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("release=%+v, want %+v", got[0], want)
	}
}

func TestParseHelmReleasesIgnoresNonHelmAndGarbage(t *testing.T) {
	js := `{"items":[
		{"type":"Opaque","data":{"release":"not-a-release"}},
		{"type":"helm.sh/release.v1","data":{"release":"%%%notbase64"}}
	]}`
	if got := parseHelmReleases(js); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}
