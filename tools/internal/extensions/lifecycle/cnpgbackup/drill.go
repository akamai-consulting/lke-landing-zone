package cnpgbackup

// drill.go — `assert-cnpg-backup --drill`: prove the backup the check just took
// RESTORES, on the e2e lane's deployments only.
//
// THE ROUND TRIP, for each database:
//
//  1. BEFORE the backup, a canary database is created in the LIVE cluster — a CNPG
//     Database CR named llz_canary_<nonce>, so no exec into apl-core's pods and no
//     client in apl-core's namespaces is needed (check.go does this, ahead of the
//     backup it takes);
//  2. a scratch namespace gets its own ObjectStore pointed at the SAME destination
//     the live one writes, under the temporary READ-ONLY key the check minted — so
//     the drill can read the archive and cannot change it;
//  3. a one-instance scratch Cluster bootstraps by barman recovery from exactly that
//     backup (recoveryTarget.backupID, targetImmediate), through the same plugin
//     and serverName apl-core's cluster archives under;
//  4. once it is healthy, a Job in the scratch namespace runs psql against it and
//     must find the canary database. A recovery that silently came up from some
//     other backup, or as an empty initdb, does not have it.
//
// WHAT IT NEVER DOES: write to the live archive (read-only key, no WAL archiver on
// the scratch cluster), or restore over a live database. The canary database is
// dropped afterwards (databaseReclaimPolicy: delete), and the scratch namespace —
// cluster, PVC, Job, key Secret — is deleted with it. The scratch PVC's Linode
// Volume is retained by the cluster-default block-storage-retain class and tagged
// lke<id>, so the e2e lane's own teardown sweep reaps it with the cluster.
//
// Armed by its own workflow input (restore_drill, release-e2e only) and refused in
// check.go on any deployment that is not an e2e lane's — two keys, held by
// different files, as for the OpenBao and Velero drills.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

const (
	// canaryCR is the Database CR the drill creates in the live cluster's
	// namespace. Its spec.name is per run.
	canaryCR = "llz-cnpg-canary"
	// drillName names the scratch ObjectStore, Cluster and (with -verify) Job.
	drillName = "llz-cnpg-drill"
	// drillSecret holds the temporary read-only key.
	drillSecret = "llz-cnpg-drill-s3"
	// drillVolume is the scratch cluster's volume size: Linode Block Storage's
	// minimum, which is ample for a canary restore of these databases.
	drillVolume = "10Gi"
)

// DrillNamespace is the scratch namespace for one database. Not a name a real
// workload would have.
func DrillNamespace(db clusterspec.CnpgDatabase) string { return "llz-cnpg-drill-" + db.Key }

// CanaryJSON is the canary: a CNPG Database CR that makes the live cluster
// create (and, on deletion, drop) one database. Pure.
func CanaryJSON(db clusterspec.CnpgDatabase, canary string) []byte {
	b, _ := json.Marshal(map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Database",
		"metadata": map[string]any{"name": canaryCR, "namespace": db.Namespace,
			"labels": map[string]any{"app.kubernetes.io/created-by": createdBy}},
		"spec": map[string]any{
			"name":                  canary,
			"owner":                 db.Owner,
			"cluster":               map[string]any{"name": db.Cluster},
			"ensure":                "present",
			"databaseReclaimPolicy": "delete",
		},
	})
	return b
}

// DrillManifest is the scratch namespace and everything the recovery needs: the
// key Secret, an ObjectStore on the live destination, the proxy's CA where
// objProxy is on, and the recovery Cluster. Pure.
func DrillManifest(db clusterspec.CnpgDatabase, w Wiring, d Destination, b BackupStatus, accessKey, secretKey string, objProxy bool) string {
	ns := DrillNamespace(db)
	labels := map[string]any{"app.kubernetes.io/created-by": createdBy}
	cfg := map[string]any{
		"destinationPath": d.Path,
		"endpointURL":     d.EndpointURL,
		"s3Credentials": map[string]any{
			"accessKeyId":     map[string]any{"name": drillSecret, "key": "ACCESS_KEY_ID"},
			"secretAccessKey": map[string]any{"name": drillSecret, "key": "ACCESS_SECRET_KEY"},
		},
		"wal":  map[string]any{"compression": "gzip"},
		"data": map[string]any{"compression": "gzip"},
	}
	objs := []map[string]any{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": ns, "labels": map[string]any{
			"app.kubernetes.io/created-by":       createdBy,
			"pod-security.kubernetes.io/enforce": "restricted",
		}}},
		{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": drillSecret, "namespace": ns, "labels": labels},
			"type": "Opaque", "stringData": map[string]any{"ACCESS_KEY_ID": accessKey, "ACCESS_SECRET_KEY": secretKey}},
	}
	if objProxy {
		// Through the proxy, as apl-core's own store goes: the CoreDNS rewrite is
		// cluster-wide, so the drill's barman reaches the proxy too and must trust
		// it — the same bundle objProxy issues into harbor and keycloak.
		objs = append(objs, map[string]any{
			"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
			"metadata": map[string]any{"name": "obj-proxy-ca", "namespace": ns, "labels": labels},
			"spec": map[string]any{
				"secretName": "obj-proxy-ca", "commonName": "obj-proxy-ca-bundle", "duration": "2160h",
				"privateKey": map[string]any{"algorithm": "ECDSA", "size": 256},
				"issuerRef":  map[string]any{"name": "llz-serving-ca", "kind": "ClusterIssuer"},
			},
		})
		cfg["endpointCA"] = map[string]any{"name": "obj-proxy-ca", "key": "ca.crt"}
	}
	store := map[string]any{"configuration": cfg}
	if len(d.SidecarEnv) > 0 {
		store["instanceSidecarConfiguration"] = map[string]any{"env": d.SidecarEnv}
	}
	objs = append(objs,
		map[string]any{"apiVersion": "barmancloud.cnpg.io/v1", "kind": "ObjectStore",
			"metadata": map[string]any{"name": drillName, "namespace": ns, "labels": labels}, "spec": store},
		map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
			"metadata": map[string]any{"name": drillName, "namespace": ns, "labels": labels},
			"spec": map[string]any{
				"instances":             1,
				"imageName":             w.ImageName,
				"enableSuperuserAccess": true,
				"storage":               map[string]any{"size": drillVolume},
				"bootstrap": map[string]any{"recovery": map[string]any{
					"source": "origin",
					// EXACTLY the backup the check took, and no further: a recovery
					// that wandered to a later point would still hold the canary, but
					// one that picked an EARLIER backup would not — which is the
					// difference this drill exists to see.
					"recoveryTarget": map[string]any{"backupID": b.BackupID, "targetImmediate": true},
				}},
				// No `plugins:` stanza, deliberately: without a WAL archiver the scratch
				// cluster cannot write a byte to the archive it restored from.
				"externalClusters": []any{map[string]any{
					"name": "origin",
					"plugin": map[string]any{
						"name":       clusterspec.CnpgBarmanPlugin,
						"parameters": map[string]any{"barmanObjectName": drillName, "serverName": w.ServerName},
					},
				}},
			}},
	)
	out, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": objs})
	return string(out)
}

// VerifyJobJSON runs psql against the restored cluster and prints the canary's
// row from pg_database if it exists. It reads the superuser Secret CNPG creates
// for the scratch cluster; restricted-PSS throughout. Pure.
func VerifyJobJSON(db clusterspec.CnpgDatabase, image, canary string) []byte {
	ns := DrillNamespace(db)
	secret := func(key string) map[string]any {
		return map[string]any{"secretKeyRef": map[string]any{"name": drillName + "-superuser", "key": key}}
	}
	b, _ := json.Marshal(map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": drillName + "-verify", "namespace": ns,
			"labels": map[string]any{"app.kubernetes.io/created-by": createdBy}},
		"spec": map[string]any{
			"backoffLimit": 3,
			"template": map[string]any{"spec": map[string]any{
				"restartPolicy": "Never",
				"securityContext": map[string]any{"runAsNonRoot": true,
					"seccompProfile": map[string]any{"type": "RuntimeDefault"}},
				"containers": []any{map[string]any{
					"name": "psql", "image": image,
					"command": []any{"psql", "-tA", "-c",
						"SELECT datname FROM pg_database WHERE datname = '" + canary + "'"},
					"env": []any{
						map[string]any{"name": "PGHOST", "valueFrom": secret("host")},
						map[string]any{"name": "PGUSER", "valueFrom": secret("username")},
						map[string]any{"name": "PGPASSWORD", "valueFrom": secret("password")},
						map[string]any{"name": "PGDATABASE", "value": "postgres"},
						map[string]any{"name": "PGSSLMODE", "value": "require"},
					},
					"resources": map[string]any{
						"requests": map[string]any{"cpu": "50m", "memory": "64Mi"},
						"limits":   map[string]any{"memory": "128Mi"},
					},
					"securityContext": map[string]any{"allowPrivilegeEscalation": false,
						"capabilities": map[string]any{"drop": []any{"ALL"}}},
				}},
			}},
		},
	})
	return b
}

// CanaryVerdict requires the canary database's name, alone on a line, in the
// verify Job's output. Pure.
func CanaryVerdict(logs, canary string) error {
	for _, l := range strings.Split(logs, "\n") {
		if strings.TrimSpace(l) == canary {
			return nil
		}
	}
	return fmt.Errorf("the restored cluster has no database %s — it came up, but not from the backup that captured the canary "+
		"(an earlier backup, or an empty initdb). psql printed: %q", canary, strings.TrimSpace(logs))
}

// ── transport ────────────────────────────────────────────────────────────────

func createCanary(ctx context.Context, db clusterspec.CnpgDatabase, canary string) error {
	if out, err := caps.Writer.ApplyStdin(string(CanaryJSON(db, canary)), createdBy); err != nil {
		return fmt.Errorf("create the canary Database: %w\n%s", err, out)
	}
	var msg string
	err := poll(ctx, canaryWait, func() (bool, error) {
		out, err := getJSON(db.Namespace, databaseKind, canaryCR)
		if err != nil {
			return false, nil
		}
		var d struct {
			Status struct {
				Applied *bool  `json:"applied"`
				Message string `json:"message"`
			} `json:"status"`
		}
		if err := json.Unmarshal(out, &d); err != nil {
			return false, nil
		}
		msg = d.Status.Message
		return d.Status.Applied != nil && *d.Status.Applied, nil
	})
	if err != nil {
		return fmt.Errorf("the canary Database %s/%s was not applied within %s (%s)", db.Namespace, canaryCR, canaryWait, orUnknown(msg))
	}
	fmt.Printf("✓ canary database %s created in %s/%s before the backup\n", canary, db.Namespace, db.Cluster)
	return nil
}

func dropCanary(db clusterspec.CnpgDatabase) {
	// databaseReclaimPolicy: delete — removing the CR drops the database.
	if _, err := caps.Writer.Delete(db.Namespace, databaseKind, canaryCR); err != nil {
		fmt.Printf("::warning::could not delete the canary Database %s/%s (its database stays in the live cluster): %v\n", db.Namespace, canaryCR, err)
	}
}

func restoreDrill(ctx context.Context, tg target, db clusterspec.CnpgDatabase, w Wiring, d Destination, b BackupStatus, st baosnap.Store, canary string) error {
	ns := DrillNamespace(db)
	defer cleanupDrill(ctx, ns)
	if err := deleteNamespaceAndWait(ctx, ns); err != nil {
		return fmt.Errorf("clear a leftover %s before the drill: %w", ns, err)
	}
	if w.ImageName == "" {
		return fmt.Errorf("the live Cluster %s/%s names no imageName, so the drill cannot run the same Postgres major", db.Namespace, db.Cluster)
	}
	if out, err := caps.Writer.ApplyStdin(DrillManifest(db, w, d, b, st.AccessKey, st.SecretKey, tg.objProxy), createdBy); err != nil {
		return fmt.Errorf("create the scratch recovery cluster: %w\n%s", err, out)
	}
	fmt.Printf("restoring backup %s into %s/%s — waiting up to %s…\n", b.BackupID, ns, drillName, restoreWait)
	var phase string
	err := poll(ctx, restoreWait, func() (bool, error) {
		out, err := getJSON(ns, clusterKind, drillName)
		if err != nil {
			return false, nil
		}
		var c struct {
			Status struct {
				Phase          string `json:"phase"`
				ReadyInstances int    `json:"readyInstances"`
			} `json:"status"`
		}
		if err := json.Unmarshal(out, &c); err != nil {
			return false, nil
		}
		phase = c.Status.Phase
		return c.Status.ReadyInstances >= 1 && phase == "Cluster in healthy state", nil
	})
	if err != nil {
		return fmt.Errorf("the recovery of backup %s did not produce a healthy cluster within %s (last phase %q) — "+
			"`kubectl -n %s get pods`, the full-recovery Job's log, and the plugin-barman-cloud container's", b.BackupID, restoreWait, phase, ns)
	}
	fmt.Printf("✓ %s/%s recovered from backup %s and is healthy\n", ns, drillName, b.BackupID)

	if out, err := caps.Writer.ApplyStdin(string(VerifyJobJSON(db, w.ImageName, canary)), createdBy); err != nil {
		return fmt.Errorf("create the verify Job: %w\n%s", err, out)
	}
	var failed int
	err = poll(ctx, verifyWait, func() (bool, error) {
		out, err := getJSON(ns, "jobs.batch", drillName+"-verify")
		if err != nil {
			return false, nil
		}
		var j struct {
			Status struct {
				Succeeded int `json:"succeeded"`
				Failed    int `json:"failed"`
			} `json:"status"`
		}
		if err := json.Unmarshal(out, &j); err != nil {
			return false, nil
		}
		failed = j.Status.Failed
		return j.Status.Succeeded >= 1, nil
	})
	logs, lerr := caps.Cluster.Run("-n", ns, "logs", "job/"+drillName+"-verify")
	if err != nil {
		return fmt.Errorf("the verify Job did not succeed within %s (%d failed attempt(s)); its log: %q (%v)", verifyWait, failed, string(logs), lerr)
	}
	if lerr != nil {
		return fmt.Errorf("the verify Job succeeded but its log could not be read — cannot tell whether the canary came back: %w", lerr)
	}
	if err := CanaryVerdict(string(logs), canary); err != nil {
		return err
	}
	fmt.Printf("✓ the restored %s database holds the canary %s written before the backup — backup and restore round-trip\n", db.Key, canary)
	return nil
}

func deleteNamespaceAndWait(ctx context.Context, ns string) error {
	if out, err := caps.Writer.Delete("", "namespace", ns); err != nil {
		return fmt.Errorf("delete namespace %s: %w\n%s", ns, err, out)
	}
	err := poll(ctx, nsGoneWait, func() (bool, error) {
		out, err := caps.Cluster.Run("get", "namespace", ns, "--ignore-not-found", "-o", "name")
		if err != nil {
			return false, nil
		}
		return strings.TrimSpace(string(out)) == "", nil
	})
	if err != nil {
		return fmt.Errorf("namespace %s still exists %s after deleting it", ns, nsGoneWait)
	}
	return nil
}

// cleanupDrill removes the scratch namespace and the PersistentVolumes its claim
// left Released. Best-effort: a failure warns, and the namespace name is the
// drill's own, so the next run clears it first.
func cleanupDrill(ctx context.Context, ns string) {
	if err := deleteNamespaceAndWait(ctx, ns); err != nil {
		fmt.Printf("::warning::could not remove the drill namespace: %v\n", err)
		return
	}
	out, err := caps.Cluster.Run("get", "pv", "-o", "json")
	if err != nil {
		fmt.Printf("::warning::could not list PersistentVolumes to remove the drill's: %v\n", err)
		return
	}
	for _, pv := range ReleasedPVsOf(out, ns) {
		if _, err := caps.Writer.Delete("", "pv", pv); err != nil {
			fmt.Printf("::warning::could not delete the drill's PersistentVolume %s: %v\n", pv, err)
		}
	}
}

// ReleasedPVsOf names the PersistentVolumes whose claim lived in ns. Pure.
func ReleasedPVsOf(pvListJSON []byte, ns string) []string {
	var l struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				ClaimRef *struct {
					Namespace string `json:"namespace"`
				} `json:"claimRef"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(pvListJSON, &l); err != nil {
		return nil
	}
	var out []string
	for _, it := range l.Items {
		if it.Spec.ClaimRef != nil && it.Spec.ClaimRef.Namespace == ns && ns != "" {
			out = append(out, it.Metadata.Name)
		}
	}
	return out
}

func newNonce() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
