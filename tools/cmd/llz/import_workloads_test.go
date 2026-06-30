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
