package main

// import_workloads.go adds the migration-planning parsers for `llz import scan`:
// the per-team secret/credential checklist, the container-image inventory (the
// re-push list for the new Harbor), persistent-volume + database detail (the
// data-migration plan), the security posture (NetworkPolicies/RBAC/Istio mTLS),
// and the authoritative installed-chart inventory decoded from Helm release
// secrets. All pure (raw kubectl JSON in, structs out) and unit-tested; kubectl
// calls live in runImportScan.

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"sort"
	"strings"
)

// ── secrets checklist (pure) ─────────────────────────────────────────────────

type secretRef struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// parseSecretInventory maps namespace → user secrets (name + type), skipping the
// service-account tokens and Helm release bookkeeping. Values are never read —
// this is the checklist of credentials to re-seed in OpenBao/ESO on the new
// cluster.
func parseSecretInventory(js string) map[string][]secretRef {
	var d struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Type string `json:"type"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	out := map[string][]secretRef{}
	for _, it := range d.Items {
		switch it.Type {
		case "kubernetes.io/service-account-token", "helm.sh/release.v1":
			continue
		}
		ns := it.Metadata.Namespace
		out[ns] = append(out[ns], secretRef{Name: it.Metadata.Name, Type: it.Type})
	}
	for ns := range out {
		sort.Slice(out[ns], func(i, j int) bool { return out[ns][i].Name < out[ns][j].Name })
	}
	return out
}

// ── image inventory (pure) ───────────────────────────────────────────────────

// imagesByNamespace returns the deduped, sorted container images per namespace —
// the re-push list for the destination registry.
func imagesByNamespace(workloads []workload) map[string][]string {
	byNS := map[string][]string{}
	for _, w := range workloads {
		byNS[w.Namespace] = append(byNS[w.Namespace], w.Images...)
	}
	for ns := range byNS {
		byNS[ns] = dedupeSorted(byNS[ns])
	}
	return byNS
}

// ── generic per-namespace counters (pure) ────────────────────────────────────

// countByNamespace counts list items per namespace (ConfigMaps, ServiceAccounts,
// NetworkPolicies, Roles, RoleBindings). skip is consulted on each item's name to
// drop noise (e.g. the kube-root-ca.crt ConfigMap); pass nil to count all.
func countByNamespace(js string, skip func(name string) bool) map[string]int {
	var d struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	out := map[string]int{}
	for _, it := range d.Items {
		if skip != nil && skip(it.Metadata.Name) {
			continue
		}
		out[it.Metadata.Namespace]++
	}
	return out
}

// skipNoiseConfigMap drops the auto-injected CA bundle that every namespace carries.
func skipNoiseConfigMap(name string) bool { return name == "kube-root-ca.crt" }

// ── persistent volumes + databases (pure) ────────────────────────────────────

type importStorage struct {
	Volumes         []pvInfo `json:"volumes,omitempty"`
	SnapshotClasses []string `json:"snapshotClasses,omitempty"`
	Databases       []dbInfo `json:"databases,omitempty"`
}

type pvInfo struct {
	Name          string   `json:"name"`
	Claim         string   `json:"claim,omitempty"` // bound PVC as namespace/name
	Capacity      string   `json:"capacity,omitempty"`
	StorageClass  string   `json:"storageClass,omitempty"`
	AccessModes   []string `json:"accessModes,omitempty"`
	ReclaimPolicy string   `json:"reclaimPolicy,omitempty"`
	VolumeHandle  string   `json:"volumeHandle,omitempty"` // CSI handle = the Linode Volume id
}

type dbInfo struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`             // CNPG | workload
	Engine    string `json:"engine,omitempty"` // postgres | mysql | …
	Instances int    `json:"instances,omitempty"`
}

func parsePVs(js string) []pvInfo {
	var d struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Capacity                      map[string]string `json:"capacity"`
				AccessModes                   []string          `json:"accessModes"`
				PersistentVolumeReclaimPolicy string            `json:"persistentVolumeReclaimPolicy"`
				StorageClassName              string            `json:"storageClassName"`
				ClaimRef                      struct {
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
				} `json:"claimRef"`
				CSI struct {
					VolumeHandle string `json:"volumeHandle"`
				} `json:"csi"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	var out []pvInfo
	for _, it := range d.Items {
		claim := ""
		if it.Spec.ClaimRef.Name != "" {
			claim = it.Spec.ClaimRef.Namespace + "/" + it.Spec.ClaimRef.Name
		}
		out = append(out, pvInfo{
			Name:          it.Metadata.Name,
			Claim:         claim,
			Capacity:      it.Spec.Capacity["storage"],
			StorageClass:  it.Spec.StorageClassName,
			AccessModes:   it.Spec.AccessModes,
			ReclaimPolicy: it.Spec.PersistentVolumeReclaimPolicy,
			VolumeHandle:  it.Spec.CSI.VolumeHandle,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// parseCNPGClusters reads CloudNativePG Clusters (clusters.postgresql.cnpg.io).
func parseCNPGClusters(js string) []dbInfo {
	var d struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				Instances int `json:"instances"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	var out []dbInfo
	for _, it := range d.Items {
		out = append(out, dbInfo{
			Namespace: it.Metadata.Namespace,
			Name:      it.Metadata.Name,
			Kind:      "CNPG",
			Engine:    "postgres",
			Instances: it.Spec.Instances,
		})
	}
	return out
}

// dbEngineByImage maps an image-name substring to its database engine.
var dbEngineByImage = []struct{ sub, engine string }{
	{"postgres", "postgres"},
	{"mariadb", "mysql"},
	{"mysql", "mysql"},
	{"mongo", "mongodb"},
	{"redis", "redis"},
}

// detectDBWorkloads flags running workloads whose image looks like a database —
// the stateful stores a CNPG scan would miss (self-managed DBs).
func detectDBWorkloads(workloads []workload) []dbInfo {
	var out []dbInfo
	for _, w := range workloads {
		for _, img := range w.Images {
			name := strings.ToLower(imageName(img))
			for _, m := range dbEngineByImage {
				if strings.Contains(name, m.sub) {
					out = append(out, dbInfo{Namespace: w.Namespace, Name: w.Name, Kind: "workload", Engine: m.engine})
					goto next
				}
			}
		}
	next:
	}
	return out
}

func parseSnapshotClasses(js string) []string {
	var d struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	var out []string
	for _, it := range d.Items {
		if it.Metadata.Name != "" {
			out = append(out, it.Metadata.Name)
		}
	}
	sort.Strings(out)
	return out
}

// ── security posture (pure) ──────────────────────────────────────────────────

type importSecurity struct {
	NetworkPolicies       int      `json:"networkPolicies,omitempty"`
	AuthorizationPolicies int      `json:"authorizationPolicies,omitempty"`
	MTLSModes             []string `json:"mtlsModes,omitempty"` // Istio PeerAuthentication modes in use
}

// parsePeerAuthModes returns the distinct Istio PeerAuthentication mTLS modes
// (STRICT/PERMISSIVE/…) configured across the cluster.
func parsePeerAuthModes(js string) []string {
	var d struct {
		Items []struct {
			Spec struct {
				Mtls struct {
					Mode string `json:"mode"`
				} `json:"mtls"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	set := map[string]bool{}
	for _, it := range d.Items {
		if m := it.Spec.Mtls.Mode; m != "" {
			set[m] = true
		}
	}
	return sortedSetKeys(set)
}

// totalCount sums a per-namespace count map.
func totalCount(byNS map[string]int) int {
	n := 0
	for _, c := range byNS {
		n += c
	}
	return n
}

// ── Helm release inventory (pure) ────────────────────────────────────────────

type helmRelease struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Chart        string `json:"chart,omitempty"`
	ChartVersion string `json:"chartVersion,omitempty"`
	Status       string `json:"status,omitempty"`
	Revision     int    `json:"revision,omitempty"`
}

// parseHelmReleases decodes the helm.sh/release.v1 secrets into the authoritative
// installed-chart inventory (name, namespace, chart + version, status), keeping
// the highest revision per release. Helm stores each release as
// base64(gzip(json)); the kubectl -o json secret data adds the usual base64
// layer on top, so the payload is double-base64 then (usually) gzip.
func parseHelmReleases(secretJSON string) []helmRelease {
	var d struct {
		Items []struct {
			Type string `json:"type"`
			Data struct {
				Release string `json:"release"`
			} `json:"data"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(secretJSON), &d) != nil {
		return nil
	}
	latest := map[string]helmRelease{} // key ns/name → highest-revision release
	for _, it := range d.Items {
		if it.Type != "helm.sh/release.v1" || it.Data.Release == "" {
			continue
		}
		rel, ok := decodeHelmRelease(it.Data.Release)
		if !ok {
			continue
		}
		key := rel.Namespace + "/" + rel.Name
		if cur, seen := latest[key]; !seen || rel.Revision > cur.Revision {
			latest[key] = rel
		}
	}
	if len(latest) == 0 {
		return nil
	}
	out := make([]helmRelease, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// decodeHelmRelease unwraps one secret's data.release field (already base64-decoded
// once by serialization into the JSON string here — so we decode the remaining
// base64, gunzip if gzipped, and parse the release JSON).
func decodeHelmRelease(data string) (helmRelease, bool) {
	// kubectl -o json gives secret data base64-encoded; undo that first.
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return helmRelease{}, false
	}
	// Helm's stored value is itself base64(gzip(json)).
	inner, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil {
		inner = raw // tolerate single-encoded payloads
	}
	if len(inner) >= 2 && inner[0] == 0x1f && inner[1] == 0x8b { // gzip magic
		gz, err := gzip.NewReader(bytes.NewReader(inner))
		if err != nil {
			return helmRelease{}, false
		}
		defer gz.Close()
		if inner, err = io.ReadAll(gz); err != nil {
			return helmRelease{}, false
		}
	}
	var rel struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Version   int    `json:"version"`
		Info      struct {
			Status string `json:"status"`
		} `json:"info"`
		Chart struct {
			Metadata struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"metadata"`
		} `json:"chart"`
	}
	if json.Unmarshal(inner, &rel) != nil || rel.Name == "" {
		return helmRelease{}, false
	}
	return helmRelease{
		Name:         rel.Name,
		Namespace:    rel.Namespace,
		Chart:        rel.Chart.Metadata.Name,
		ChartVersion: rel.Chart.Metadata.Version,
		Status:       rel.Info.Status,
		Revision:     rel.Version,
	}, true
}
