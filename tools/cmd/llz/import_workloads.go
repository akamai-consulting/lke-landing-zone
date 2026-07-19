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
// secretList is a `kubectl get secrets -o json` list read for metadata + type
// only — never data. Shared by parseSecretCounts and parseSecretInventory.
type secretList struct {
	Items []struct {
		Metadata k8sObjectMeta `json:"metadata"`
		Type     string        `json:"type"`
	} `json:"items"`
}

func parseSecretInventory(js string) map[string][]secretRef {
	var d secretList
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
	var d k8sObjectList
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
	Volumes         []pvInfo       `json:"volumes,omitempty"`
	VolumesByClass  map[string]int `json:"volumesByClass,omitempty"` // classification → count, for the migrate-vs-rebuild plan
	SnapshotClasses []string       `json:"snapshotClasses,omitempty"`
	Databases       []dbInfo       `json:"databases,omitempty"`
}

type pvInfo struct {
	Name          string   `json:"name"`
	Claim         string   `json:"claim,omitempty"` // bound PVC as namespace/name
	Capacity      string   `json:"capacity,omitempty"`
	StorageClass  string   `json:"storageClass,omitempty"`
	AccessModes   []string `json:"accessModes,omitempty"`
	ReclaimPolicy string   `json:"reclaimPolicy,omitempty"`
	VolumeHandle  string   `json:"volumeHandle,omitempty"` // CSI handle = the Linode Volume id
	// Usage + migration classification (filled by classifyVolumes from the pods).
	UsedBy         string `json:"usedBy,omitempty"` // Kind/name of the workload that mounts the claim
	App            string `json:"app,omitempty"`    // app label of the mounting pod
	InUse          bool   `json:"inUse,omitempty"`  // a running pod mounts the claim
	Classification string `json:"classification,omitempty"`
}

type dbInfo struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`             // CNPG | workload
	Engine    string   `json:"engine,omitempty"` // postgres | mysql | …
	Instances int      `json:"instances,omitempty"`
	Clients   []string `json:"clients,omitempty"` // workloads that reference the DB's connection secret (the actual writers)
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

// ── PV usage + classification (pure) ─────────────────────────────────────────

type ownerRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// pvcConsumer is the workload that mounts a PVC — resolved from the pods that
// reference it, so we know whether a PV is used and by what.
type pvcConsumer struct {
	Workload string // Kind/name (ReplicaSet folded to its Deployment)
	App      string // app label
	Image    string // first container image (drives the classification)
}

// parsePVCConsumers maps "namespace/pvcName" → the workload that mounts it, read
// from `kubectl get pods`. A PVC with no entry is mounted by no running pod
// (orphaned / scaled to zero). First pod wins for an RWX claim.
func parsePVCConsumers(podsJSON string) map[string]pvcConsumer {
	var d struct {
		Items []struct {
			Metadata struct {
				Name            string            `json:"name"`
				Namespace       string            `json:"namespace"`
				Labels          map[string]string `json:"labels"`
				OwnerReferences []ownerRef        `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				Containers []struct {
					Image string `json:"image"`
				} `json:"containers"`
				Volumes []struct {
					PersistentVolumeClaim struct {
						ClaimName string `json:"claimName"`
					} `json:"persistentVolumeClaim"`
				} `json:"volumes"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(podsJSON), &d) != nil {
		return nil
	}
	out := map[string]pvcConsumer{}
	for _, p := range d.Items {
		c := pvcConsumer{
			Workload: workloadFromOwner(p.Metadata.OwnerReferences, p.Metadata.Name),
			App:      firstLabel(p.Metadata.Labels, "app.kubernetes.io/name", "app.kubernetes.io/instance", "app"),
		}
		if len(p.Spec.Containers) > 0 {
			c.Image = p.Spec.Containers[0].Image
		}
		for _, v := range p.Spec.Volumes {
			if cn := v.PersistentVolumeClaim.ClaimName; cn != "" {
				key := p.Metadata.Namespace + "/" + cn
				if _, seen := out[key]; !seen {
					out[key] = c
				}
			}
		}
	}
	return out
}

// workloadFromOwner reduces a pod's ownerReferences to a "Kind/name" — folding a
// ReplicaSet to its owning Deployment (trim the pod-template-hash suffix). A pod
// with no controller reports as "Pod/<name>".
func workloadFromOwner(refs []ownerRef, podName string) string {
	if len(refs) == 0 {
		return "Pod/" + podName
	}
	kind, name := refs[0].Kind, refs[0].Name
	if kind == "ReplicaSet" {
		kind = "Deployment"
		if i := strings.LastIndex(name, "-"); i > 0 {
			name = name[:i]
		}
	}
	return kind + "/" + name
}

// pvClassByHint maps a substring (of the mounting pod's image / app / workload /
// claim) to a migration classification. First match wins; default is "standalone".
// "unused" is assigned separately when no pod mounts the claim.
var pvClassByHint = []struct{ sub, class string }{
	{"postgres", "database"}, {"cnpg", "database"}, {"mariadb", "database"}, {"mysql", "database"}, {"mongo", "database"},
	{"redis", "cache"},
	{"prometheus", "metrics"},
	{"loki", "object-store-cache"}, {"thanos", "object-store-cache"}, {"harbor", "object-store-cache"},
}

// classifyVolumes annotates each PV with its mounting workload + a migration
// classification, and returns the per-class counts. Classifications:
//   - database          → replicate (postgres logical/physical, etc.); don't copy the datadir
//   - cache             → rebuild (or app-native replication)
//   - metrics           → rebuild (long-term history is in object storage)
//   - object-store-cache→ skip; the real data lives in the app's bucket
//   - ephemeral         → Tekton TaskRun/PipelineRun build workspace; rebuild, never migrate
//   - standalone        → back up + restore (Velero/restic) — the genuine PV migration set
//   - unused            → no pod mounts it; verify before migrating
func classifyVolumes(pvs []pvInfo, consumers map[string]pvcConsumer) ([]pvInfo, map[string]int) {
	byClass := map[string]int{}
	out := make([]pvInfo, 0, len(pvs))
	for _, pv := range pvs {
		c, inUse := consumers[pv.Claim]
		if inUse {
			pv.UsedBy = c.Workload
			pv.App = c.App
			pv.InUse = true
		}
		pv.Classification = classifyPV(pv, c, inUse)
		byClass[pv.Classification]++
		out = append(out, pv)
	}
	if len(byClass) == 0 {
		byClass = nil
	}
	return out, byClass
}

func classifyPV(pv pvInfo, c pvcConsumer, inUse bool) string {
	if !inUse {
		return "unused"
	}
	// Tekton build workspaces are transient CI scratch — never migrate.
	if strings.HasPrefix(c.Workload, "TaskRun/") || strings.HasPrefix(c.Workload, "PipelineRun/") {
		return "ephemeral"
	}
	hay := strings.ToLower(imageName(c.Image) + " " + c.App + " " + c.Workload + " " + pv.Claim)
	for _, m := range pvClassByHint {
		if strings.Contains(hay, m.sub) {
			return m.class
		}
	}
	return "standalone"
}

// ── DB clients (pure) ────────────────────────────────────────────────────────

// podSecretUse is one pod's workload + the secret names it references — used to
// find which workload actually connects to a database (CNPG publishes a
// "<cluster>-app" secret; the client mounts it).
type podSecretUse struct {
	Namespace string
	Workload  string
	Secrets   []string
}

// podContainer captures just the secret-bearing fields of a container spec.
type podContainer struct {
	Env []struct {
		ValueFrom struct {
			SecretKeyRef struct {
				Name string `json:"name"`
			} `json:"secretKeyRef"`
		} `json:"valueFrom"`
	} `json:"env"`
	EnvFrom []struct {
		SecretRef struct {
			Name string `json:"name"`
		} `json:"secretRef"`
	} `json:"envFrom"`
}

// parsePodSecretRefs lists, per pod, the secrets it references (via env
// secretKeyRef, envFrom secretRef, or a secret volume) plus its workload.
func parsePodSecretRefs(podsJSON string) []podSecretUse {
	var d struct {
		Items []struct {
			Metadata struct {
				Name            string     `json:"name"`
				Namespace       string     `json:"namespace"`
				OwnerReferences []ownerRef `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				Containers     []podContainer `json:"containers"`
				InitContainers []podContainer `json:"initContainers"`
				Volumes        []struct {
					Secret struct {
						SecretName string `json:"secretName"`
					} `json:"secret"`
				} `json:"volumes"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(podsJSON), &d) != nil {
		return nil
	}
	var out []podSecretUse
	for _, p := range d.Items {
		set := map[string]bool{}
		for _, c := range append(append([]podContainer{}, p.Spec.Containers...), p.Spec.InitContainers...) {
			for _, e := range c.Env {
				if n := e.ValueFrom.SecretKeyRef.Name; n != "" {
					set[n] = true
				}
			}
			for _, ef := range c.EnvFrom {
				if n := ef.SecretRef.Name; n != "" {
					set[n] = true
				}
			}
		}
		for _, v := range p.Spec.Volumes {
			if n := v.Secret.SecretName; n != "" {
				set[n] = true
			}
		}
		if len(set) == 0 {
			continue
		}
		out = append(out, podSecretUse{
			Namespace: p.Metadata.Namespace,
			Workload:  workloadFromOwner(p.Metadata.OwnerReferences, p.Metadata.Name),
			Secrets:   sortedSetKeys(set),
		})
	}
	return out
}

// attachDBClients fills dbInfo.Clients for each CNPG cluster with the workloads
// that reference its connection secret (a same-namespace secret whose name starts
// with the cluster name, e.g. "<cluster>-app"). Workload-kind DBs are left alone
// (no published connection secret to key on).
func attachDBClients(dbs []dbInfo, uses []podSecretUse) []dbInfo {
	for i := range dbs {
		if dbs[i].Kind != "CNPG" {
			continue
		}
		set := map[string]bool{}
		for _, u := range uses {
			if u.Namespace != dbs[i].Namespace {
				continue
			}
			for _, s := range u.Secrets {
				if strings.HasPrefix(s, dbs[i].Name) {
					set[u.Workload] = true
					break
				}
			}
		}
		dbs[i].Clients = sortedSetKeys(set)
	}
	return dbs
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

// dbEngineForImages returns the engine of the FIRST image that looks like a
// database, or "" when none do — one engine per workload, first match wins.
func dbEngineForImages(images []string) string {
	for _, img := range images {
		name := strings.ToLower(imageName(img))
		for _, m := range dbEngineByImage {
			if strings.Contains(name, m.sub) {
				return m.engine
			}
		}
	}
	return ""
}

// detectDBWorkloads flags running workloads whose image looks like a database —
// the stateful stores a CNPG scan would miss (self-managed DBs).
func detectDBWorkloads(workloads []workload) []dbInfo {
	var out []dbInfo
	for _, w := range workloads {
		if engine := dbEngineForImages(w.Images); engine != "" {
			out = append(out, dbInfo{Namespace: w.Namespace, Name: w.Name, Kind: "workload", Engine: engine})
		}
	}
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
