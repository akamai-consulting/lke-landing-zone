package velero

// drill.go — `llz ci assert-velero-restore`, the gate that fails when Velero's
// backups stop working.
//
// WHAT THE CHECK PROVES, on every deployment that enables velero (the default
// mode, run by bootstrap-openbao after converge):
//
//  1. the LIVE Schedule still excludes llz-openbao and llz-velero — read off the
//     cluster, not the manifest, so a hand edit or an overlay that slipped past
//     the render is caught where it runs;
//  2. the BackupStorageLocation is Available — Velero can list the bucket with
//     the key it was given;
//  3. a Backup built from that Schedule's own template (so its scope, TTL and
//     storage location are the ones production uses) reaches Completed with
//     items in it and no errors;
//  4. CONSUMER SIDE: the backup's tarball is IN the velero bucket, read through a
//     temporary read-only key minted for the purpose, and a GET without the SSE-C
//     key is refused — the object is ciphertext, so a bucket key alone reads
//     nothing;
//  5. Prometheus scrapes Velero and has loaded its alert rules, so the freshness
//     alerts can fire at all.
//
// --drill ADDS THE ROUND TRIP, on the e2e lane's deployments only:
//
//  6. write a canary namespace (a ConfigMap and a Secret holding a nonce);
//  7. back it up through the same Schedule template, narrowed to that namespace;
//  8. require the tarball in the bucket and encrypted, as in 4;
//  9. DELETE the namespace and wait until it is gone;
//  10. restore it, and require the ConfigMap and the Secret back with the nonce
//     and the restore's own label on them — so a restore that did nothing, or a
//     namespace that was never really deleted, reads as a failure.
//
// WHAT STAYS GREEN WITHOUT IT: everything else. The llz-velero App goes Healthy
// the moment the Deployment does; the BSL reads Available against an empty
// bucket; the Schedule exists whether or not a backup it takes can be restored.
// Backups are the textbook silent failure.
//
// THE DRILL NEVER RESTORES OVER LIVE DATA. Its backup holds only the canary
// namespace and its Restore is scoped to that namespace, which nothing else
// uses. It is still armed by its own workflow input (restore_drill, release-e2e
// only) and refused here on any deployment that is not an e2e lane's — two keys,
// held by different files, as for the OpenBao drill.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/forge"
)

const (
	// E2ELaneBase is the deployment name the release-e2e lanes instantiate before
	// forge.LaneName makes it forge-unique.
	E2ELaneBase = "e2e"
	// DrillNamespace is the canary namespace. Not in any default exclude list, and
	// not a name a real workload would have.
	DrillNamespace = "llz-velero-drill"
	// canaryName names the drill's ConfigMap and Secret.
	canaryName = "llz-velero-canary"
	// createdBy marks every object this verb creates.
	createdBy = "llz-velero-check"
	// bsl is the BackupStorageLocation the component configures.
	bsl = "default"
)

// Budgets. Package vars so tests can shrink them.
var (
	scheduleWait = 10 * time.Minute
	bslWait      = 5 * time.Minute
	backupWait   = 10 * time.Minute
	nsGoneWait   = 3 * time.Minute
	restoreWait  = 10 * time.Minute
	pollInterval = 10 * time.Second
	now          = time.Now
)

// target is what every mode resolves from the spec before touching anything.
type target struct {
	region, objCluster, bucket, endpoint string
}

func resolve(region, what string) (target, bool, error) {
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return target{}, false, fmt.Errorf("assert-velero-restore: load spec: %w", err)
	}
	e, ok := lz.Env(region)
	if !ok {
		return target{}, false, fmt.Errorf("no deployment %q in the spec", region)
	}
	if !clusterspec.ComponentEnabled(e.Components, "velero") {
		fmt.Printf("velero is disabled for %s — nothing to back up; skipping the %s.\n", region, what)
		return target{}, true, nil
	}
	oc := e.Cluster.ObjectStorage.Cluster
	if oc == "" {
		return target{}, false, fmt.Errorf("deployment %q declares no cluster.objectStorage.cluster", region)
	}
	return target{region: region, objCluster: oc, bucket: clusterspec.ObjVeleroBucket(lz.ObjLabelPrefix(), region),
		endpoint: clusterspec.ObjEndpointHost(oc)}, false, nil
}

// Run is the verb: the backup check, plus the round trip when drill is set.
func Run(ctx context.Context, region string, drill bool) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	if drill && !forge.IsLaneDeployment(E2ELaneBase, region) {
		return fmt.Errorf("refusing --drill: %q is not an e2e lane deployment (%s or a forge-suffixed %s-*). The drill creates, "+
			"deletes and restores its own canary namespace only, but it is a release-e2e gate and runs nowhere else. Without --drill "+
			"this verb proves backups work on any deployment", region, E2ELaneBase, E2ELaneBase)
	}
	what := "backup check"
	if drill {
		what = "backup check and restore drill"
	}
	tg, skip, err := resolve(region, what)
	if err != nil || skip {
		return err
	}
	tok := firstNonEmpty(os.Getenv("LINODE_API_TOKEN"), os.Getenv("LINODE_TOKEN"))
	if tok == "" {
		return fmt.Errorf("LINODE_API_TOKEN is not set — the check reads the velero bucket through a temporary key it mints from it")
	}

	sched, err := waitSchedule(ctx)
	if err != nil {
		return err
	}
	scope, err := ScheduleScope(sched)
	if err != nil {
		return err
	}
	if err := clusterspec.VeleroScopeProtectsOpenBao(scope); err != nil {
		return fmt.Errorf("the LIVE Schedule %s/%s is unsafe: %w. The rendered patch always excludes it, so something "+
			"changed the Schedule after render — fix that before taking another backup, and treat existing backups as "+
			"holding the seal key", clusterspec.VeleroNamespace, clusterspec.VeleroScheduleName, err)
	}
	fmt.Printf("✓ the live Schedule excludes %s (excludedNamespaces %v)\n", strings.Join(clusterspec.VeleroMandatoryExcludes, " and "), scope.Exclude)
	if err := waitBSLAvailable(ctx); err != nil {
		return err
	}

	// The check backup: the Schedule's own template, labelled as the Schedule's,
	// so it counts toward the schedule's freshness metrics — the first-backup
	// check is also the schedule's first success, at install rather than a day on.
	name := fmt.Sprintf("%s-check-%d", clusterspec.VeleroScheduleName, now().Unix())
	body, err := BackupFromSchedule(sched, name, nil, "", true)
	if err != nil {
		return err
	}
	st, err := createAndWaitBackup(ctx, name, body)
	if err != nil {
		return err
	}
	if err := BackupVerdict(name, st, 1); err != nil {
		return err
	}
	fmt.Printf("✓ backup %s Completed: %d item(s), %d warning(s)\n", name, st.Progress.ItemsBackedUp, st.Warnings)
	if err := verifyInBucket(ctx, tok, tg, name); err != nil {
		return err
	}
	if err := caps.Scrape("--monitors", clusterspec.VeleroNamespace+"/velero", "--rule-groups", "llz-velero"); err != nil {
		return fmt.Errorf("velero backs up, but Prometheus is not scraping it or has not loaded its rules — every Velero alert "+
			"would evaluate over nothing (check the ServiceMonitor's prometheus=system label and the velero-server "+
			"NetworkPolicy's :8085 ingress from monitoring): %w", err)
	}
	if !drill {
		fmt.Printf("✓ %s's Velero works end to end: the live Schedule's backup completed and sits encrypted in %s\n", region, tg.bucket)
		return nil
	}
	return runDrill(ctx, tok, tg, sched)
}

// runDrill is steps 6–10.
func runDrill(ctx context.Context, tok string, tg target, sched []byte) error {
	stamp := now().Unix()
	backup := fmt.Sprintf("llz-velero-drill-%d", stamp)
	restore := backup
	defer cleanupDrill(backup, restore)

	if err := deleteNamespaceAndWait(ctx); err != nil {
		return fmt.Errorf("clear a leftover %s before the drill: %w", DrillNamespace, err)
	}
	nonce := newNonce()
	if out, err := caps.Writer.ApplyStdin(CanaryManifest(nonce), createdBy); err != nil {
		return fmt.Errorf("write the canary: %w\n%s", err, out)
	}
	fmt.Printf("✓ canary %s/%s written (ConfigMap + Secret)\n", DrillNamespace, canaryName)

	body, err := BackupFromSchedule(sched, backup, []string{DrillNamespace}, "1h", false)
	if err != nil {
		return err
	}
	st, err := createAndWaitBackup(ctx, backup, body)
	if err != nil {
		return err
	}
	// Namespace, ConfigMap and Secret at the least: fewer means the backup did
	// not see the canary, and a restore of it would prove nothing.
	if err := BackupVerdict(backup, st, 3); err != nil {
		return err
	}
	fmt.Printf("✓ drill backup %s Completed: %d item(s)\n", backup, st.Progress.ItemsBackedUp)
	if err := verifyInBucket(ctx, tok, tg, backup); err != nil {
		return err
	}

	if err := deleteNamespaceAndWait(ctx); err != nil {
		return fmt.Errorf("delete the canary namespace: %w", err)
	}
	fmt.Printf("✓ %s deleted\n", DrillNamespace)

	if out, err := caps.Writer.ApplyStdin(string(RestoreJSON(restore, backup)), createdBy); err != nil {
		return fmt.Errorf("create the Restore: %w\n%s", err, out)
	}
	rst, err := waitRestore(ctx, restore)
	if err != nil {
		return err
	}
	if err := RestoreVerdict(restore, rst); err != nil {
		return err
	}
	fmt.Printf("✓ restore %s Completed: %d item(s)\n", restore, rst.Progress.ItemsRestored)

	cm, err := caps.Cluster.Run("-n", DrillNamespace, "get", "configmap", canaryName, "-o", "json")
	if err != nil {
		return fmt.Errorf("the restore Completed but the canary ConfigMap is not back: %w", err)
	}
	sec, err := caps.Cluster.Run("-n", DrillNamespace, "get", "secret", canaryName, "-o", "json")
	if err != nil {
		return fmt.Errorf("the restore Completed but the canary Secret is not back: %w", err)
	}
	if err := CanaryVerdict(cm, sec, nonce, restore); err != nil {
		return err
	}
	fmt.Println("✓ the canary ConfigMap and Secret are back with the nonce the backup captured — backup and restore round-trip")
	return nil
}

// ── the pure half ────────────────────────────────────────────────────────────

// ScheduleScope reads the namespace scope off a live Schedule's JSON. A Schedule
// with no template is "could not tell", never "safe".
func ScheduleScope(scheduleJSON []byte) (clusterspec.VeleroScope, error) {
	var s struct {
		Spec struct {
			Template *struct {
				IncludedNamespaces []string `json:"includedNamespaces"`
				ExcludedNamespaces []string `json:"excludedNamespaces"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(scheduleJSON, &s); err != nil {
		return clusterspec.VeleroScope{}, fmt.Errorf("parse the Schedule: %w", err)
	}
	if s.Spec.Template == nil {
		return clusterspec.VeleroScope{}, fmt.Errorf("the Schedule has no spec.template — cannot tell what it backs up")
	}
	return clusterspec.VeleroScope{Include: s.Spec.Template.IncludedNamespaces, Exclude: s.Spec.Template.ExcludedNamespaces}, nil
}

// BackupFromSchedule builds a Backup from a live Schedule's template — the
// Schedule's own scope, TTL and storage location — exactly as `velero backup
// create --from-schedule` does. include, when set, NARROWS the namespaces (the
// drill's canary); the Schedule's excludes stay. fromSchedule labels it as the
// Schedule's, so it counts toward that schedule's metrics. Pure.
func BackupFromSchedule(scheduleJSON []byte, name string, include []string, ttl string, fromSchedule bool) ([]byte, error) {
	var s struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Template map[string]any `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(scheduleJSON, &s); err != nil {
		return nil, fmt.Errorf("parse the Schedule: %w", err)
	}
	if s.Spec.Template == nil || s.Metadata.Name == "" {
		return nil, fmt.Errorf("the Schedule JSON carries no name or spec.template — it was not read from the cluster")
	}
	spec := map[string]any{}
	for k, v := range s.Spec.Template {
		spec[k] = v
	}
	if include != nil {
		spec["includedNamespaces"] = include
	}
	if ttl != "" {
		spec["ttl"] = ttl
	}
	labels := map[string]any{"app.kubernetes.io/created-by": createdBy}
	if fromSchedule {
		labels["velero.io/schedule-name"] = s.Metadata.Name
	}
	return json.Marshal(map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": name, "namespace": clusterspec.VeleroNamespace, "labels": labels},
		"spec":     spec,
	})
}

// RestoreJSON restores backup into the canary namespace ONLY, never replacing an
// object that exists (existingResourcePolicy none). Pure.
func RestoreJSON(name, backup string) []byte {
	b, _ := json.Marshal(map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Restore",
		"metadata": map[string]any{"name": name, "namespace": clusterspec.VeleroNamespace,
			"labels": map[string]any{"app.kubernetes.io/created-by": createdBy}},
		"spec": map[string]any{
			"backupName":             backup,
			"includedNamespaces":     []string{DrillNamespace},
			"existingResourcePolicy": "none",
		},
	})
	return b
}

// CanaryManifest is the canary namespace, a ConfigMap and a Secret, each holding
// nonce. The Secret matters: Secrets are what make these backups sensitive, and
// a backup path that loses them (or a restore that cannot decrypt them) must fail.
func CanaryManifest(nonce string) string {
	objs := []map[string]any{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": DrillNamespace, "labels": map[string]any{
			"app.kubernetes.io/created-by":       createdBy,
			"pod-security.kubernetes.io/enforce": "restricted",
		}}},
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": canaryName, "namespace": DrillNamespace},
			"data": map[string]any{"nonce": nonce}},
		{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": canaryName, "namespace": DrillNamespace},
			"type": "Opaque", "stringData": map[string]any{"nonce": nonce}},
	}
	b, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": objs})
	return string(b)
}

// BackupStatus is the slice of a Backup's status the verdict reads.
type BackupStatus struct {
	Phase            string   `json:"phase"`
	Errors           int      `json:"errors"`
	Warnings         int      `json:"warnings"`
	FailureReason    string   `json:"failureReason"`
	ValidationErrors []string `json:"validationErrors"`
	Progress         struct {
		TotalItems    int `json:"totalItems"`
		ItemsBackedUp int `json:"itemsBackedUp"`
	} `json:"progress"`
}

// BackupVerdict fails anything but a Completed backup with at least minItems in
// it and no errors. Zero items is a FAILURE: a backup that saved nothing looks
// exactly like one whose scope was emptied out. Pure.
func BackupVerdict(name string, st BackupStatus, minItems int) error {
	switch {
	case st.Phase != "Completed":
		why := st.FailureReason
		if len(st.ValidationErrors) > 0 {
			why = strings.Join(st.ValidationErrors, "; ")
		}
		return fmt.Errorf("backup %s ended %s (%d error(s)): %s — `kubectl -n %s get backup %s -o yaml`; logs are SSE-C in the bucket, "+
			"so read the server's: `kubectl -n %s logs deploy/velero | grep %s`", name, orUnknown(st.Phase), st.Errors, orUnknown(why),
			clusterspec.VeleroNamespace, name, clusterspec.VeleroNamespace, name)
	case st.Errors > 0:
		return fmt.Errorf("backup %s Completed with %d error(s) — some items are not in it", name, st.Errors)
	case st.Progress.ItemsBackedUp < minItems:
		return fmt.Errorf("backup %s Completed with %d item(s) (of %d), want at least %d — a backup that saved nothing passes every "+
			"phase check, which is why this one refuses it", name, st.Progress.ItemsBackedUp, st.Progress.TotalItems, minItems)
	}
	return nil
}

// RestoreStatus is the slice of a Restore's status the verdict reads.
type RestoreStatus struct {
	Phase         string `json:"phase"`
	Errors        int    `json:"errors"`
	Warnings      int    `json:"warnings"`
	FailureReason string `json:"failureReason"`
	Progress      struct {
		TotalItems    int `json:"totalItems"`
		ItemsRestored int `json:"itemsRestored"`
	} `json:"progress"`
}

// RestoreVerdict fails anything but a Completed restore that restored something
// with no errors. Pure.
func RestoreVerdict(name string, st RestoreStatus) error {
	switch {
	case st.Phase != "Completed":
		return fmt.Errorf("restore %s ended %s (%d error(s)): %s — `kubectl -n %s get restore %s -o yaml`", name, orUnknown(st.Phase),
			st.Errors, orUnknown(st.FailureReason), clusterspec.VeleroNamespace, name)
	case st.Errors > 0:
		return fmt.Errorf("restore %s Completed with %d error(s)", name, st.Errors)
	case st.Progress.ItemsRestored == 0:
		return fmt.Errorf("restore %s Completed having restored nothing", name)
	}
	return nil
}

// CanaryVerdict requires both canary objects back with the nonce, each carrying
// the restore's own label — proof they came from THIS restore, not from a
// namespace that was never really deleted. Pure.
func CanaryVerdict(cmJSON, secretJSON []byte, nonce, restore string) error {
	type obj struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	var cm, sec obj
	if err := json.Unmarshal(cmJSON, &cm); err != nil {
		return fmt.Errorf("parse the restored ConfigMap: %w", err)
	}
	if err := json.Unmarshal(secretJSON, &sec); err != nil {
		return fmt.Errorf("parse the restored Secret: %w", err)
	}
	if got := cm.Data["nonce"]; got != nonce {
		return fmt.Errorf("the restored ConfigMap holds nonce %q, want %q", got, nonce)
	}
	raw, err := base64.StdEncoding.DecodeString(sec.Data["nonce"])
	if err != nil || string(raw) != nonce {
		return fmt.Errorf("the restored Secret does not hold the nonce the backup captured (%q)", string(raw))
	}
	for what, o := range map[string]obj{"ConfigMap": cm, "Secret": sec} {
		if o.Metadata.Labels["velero.io/restore-name"] != restore {
			return fmt.Errorf("the canary %s is present but not labelled velero.io/restore-name=%s — it was not recreated by the "+
				"restore, so the drill proved nothing about restoring", what, restore)
		}
	}
	return nil
}

// BucketVerdict judges the consumer-side read of one backup's objects: the
// tarball must be listed under backups/<name>/, and a GET without the SSE-C key
// must be refused with 400 — a 200 means the backup (every included Secret) sits
// in the bucket in plaintext. Pure.
func BucketVerdict(bucket, name string, keys []string, getStatus int, getCode string) error {
	tar := "backups/" + name + "/" + name + ".tar.gz"
	found := false
	for _, k := range keys {
		if k == tar {
			found = true
		}
	}
	if !found {
		sort.Strings(keys)
		return fmt.Errorf("%s holds no %s — Velero reported the backup Completed but its tarball is not in the bucket the "+
			"spec derives for this deployment (found under backups/%s/: %v)", bucket, tar, name, keys)
	}
	switch {
	case getStatus == 400:
		return nil
	case getStatus/100 == 2:
		return fmt.Errorf("%s/%s READ BACK WITHOUT ITS SSE-C KEY (HTTP %d) — the backup is plaintext in the bucket: every included "+
			"Secret is readable by anyone holding a key on it. Check the BackupStorageLocation's customerKeyEncryptionSecret", bucket, tar, getStatus)
	default:
		return fmt.Errorf("could not tell whether %s/%s is encrypted: a keyless GET answered HTTP %d (%s), not the 400 an SSE-C "+
			"object gives", bucket, tar, getStatus, orUnknown(getCode))
	}
}

// ── the transport half ───────────────────────────────────────────────────────

func verifyInBucket(ctx context.Context, tok string, tg target, name string) error {
	st, revoke, err := caps.TempStore(ctx, caps.Keys(tok), "llz-velero-check-"+tg.region, tg.objCluster, tg.bucket, tg.endpoint, "read_only")
	if err != nil {
		return err
	}
	defer revoke()
	objs, err := st.List(ctx, "backups/"+name+"/")
	if err != nil {
		return fmt.Errorf("list %s (could not tell whether the backup arrived): %w", tg.bucket, err)
	}
	keys := make([]string, 0, len(objs))
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	tar := "backups/" + name + "/" + name + ".tar.gz"
	code, s3code, err := 0, "", error(nil)
	for _, k := range keys {
		if k == tar {
			code, s3code, err = st.GetStatus(ctx, tar)
		}
	}
	if err != nil {
		return fmt.Errorf("probe %s/%s: %w", tg.bucket, tar, err)
	}
	if err := BucketVerdict(tg.bucket, name, keys, code, s3code); err != nil {
		return err
	}
	fmt.Printf("✓ %s/%s is in the bucket and refuses a keyless read (HTTP 400): SSE-C encrypted\n", tg.bucket, tar)
	return nil
}

func waitSchedule(ctx context.Context) ([]byte, error) {
	var found []byte
	var last error
	err := poll(ctx, scheduleWait, func() (bool, error) {
		out, err := caps.Cluster.Run("-n", clusterspec.VeleroNamespace, "get", "schedules.velero.io", clusterspec.VeleroScheduleName, "-o", "json")
		if err == nil && len(out) > 0 {
			found = out
			return true, nil
		}
		last = err
		return false, nil
	})
	if err != nil {
		return nil, fmt.Errorf("schedule %s/%s never appeared within %s (%v) — is the llz-velero Application synced? "+
			"`kubectl -n argocd get application llz-velero llz-velero-server`", clusterspec.VeleroNamespace, clusterspec.VeleroScheduleName,
			scheduleWait, last)
	}
	return found, nil
}

func waitBSLAvailable(ctx context.Context) error {
	var phase, msg string
	err := poll(ctx, bslWait, func() (bool, error) {
		out, err := caps.Cluster.Run("-n", clusterspec.VeleroNamespace, "get", "backupstoragelocations.velero.io", bsl, "-o", "json")
		if err != nil {
			return false, nil
		}
		var b struct {
			Status struct {
				Phase   string `json:"phase"`
				Message string `json:"message"`
			} `json:"status"`
		}
		if err := json.Unmarshal(out, &b); err != nil {
			return false, nil
		}
		phase, msg = b.Status.Phase, b.Status.Message
		return phase == "Available", nil
	})
	if err != nil {
		return fmt.Errorf("BackupStorageLocation %s/%s is %s, not Available, after %s (%s) — Velero cannot list its bucket: check the "+
			"velero-cloud-credentials and velero-ssec-key Secrets (both from OpenBao via ESO) and that the object-storage root was applied",
			clusterspec.VeleroNamespace, bsl, orUnknown(phase), bslWait, orUnknown(msg))
	}
	fmt.Printf("✓ BackupStorageLocation %s is Available\n", bsl)
	return nil
}

func createAndWaitBackup(ctx context.Context, name string, body []byte) (BackupStatus, error) {
	if out, err := caps.Writer.ApplyStdin(string(body), createdBy); err != nil {
		return BackupStatus{}, fmt.Errorf("create Backup %s: %w\n%s", name, err, out)
	}
	fmt.Printf("created Backup %s/%s — waiting up to %s…\n", clusterspec.VeleroNamespace, name, backupWait)
	var st BackupStatus
	err := poll(ctx, backupWait, func() (bool, error) {
		out, err := caps.Cluster.Run("-n", clusterspec.VeleroNamespace, "get", "backups.velero.io", name, "-o", "json")
		if err != nil {
			return false, nil
		}
		var b struct {
			Status BackupStatus `json:"status"`
		}
		if err := json.Unmarshal(out, &b); err != nil {
			return false, nil
		}
		st = b.Status
		switch st.Phase {
		case "Completed", "PartiallyFailed", "Failed", "FailedValidation":
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return st, fmt.Errorf("backup %s did not finish within %s (last phase %s)", name, backupWait, orUnknown(st.Phase))
	}
	return st, nil
}

func waitRestore(ctx context.Context, name string) (RestoreStatus, error) {
	var st RestoreStatus
	err := poll(ctx, restoreWait, func() (bool, error) {
		out, err := caps.Cluster.Run("-n", clusterspec.VeleroNamespace, "get", "restores.velero.io", name, "-o", "json")
		if err != nil {
			return false, nil
		}
		var r struct {
			Status RestoreStatus `json:"status"`
		}
		if err := json.Unmarshal(out, &r); err != nil {
			return false, nil
		}
		st = r.Status
		switch st.Phase {
		case "Completed", "PartiallyFailed", "Failed", "FailedValidation":
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return st, fmt.Errorf("restore %s did not finish within %s (last phase %s)", name, restoreWait, orUnknown(st.Phase))
	}
	return st, nil
}

func deleteNamespaceAndWait(ctx context.Context) error {
	if out, err := caps.Writer.Delete("", "namespace", DrillNamespace); err != nil {
		return fmt.Errorf("delete namespace %s: %w\n%s", DrillNamespace, err, out)
	}
	err := poll(ctx, nsGoneWait, func() (bool, error) {
		out, err := caps.Cluster.Run("get", "namespace", DrillNamespace, "--ignore-not-found", "-o", "name")
		if err != nil {
			return false, nil
		}
		return strings.TrimSpace(string(out)) == "", nil
	})
	if err != nil {
		return fmt.Errorf("namespace %s still exists %s after deleting it", DrillNamespace, nsGoneWait)
	}
	return nil
}

// cleanupDrill removes what the drill created: the canary namespace, the drill
// backup (through a DeleteBackupRequest, which also deletes its objects from the
// bucket) and the Restore. Best-effort — a failed cleanup warns, and the 1h TTL
// on the drill backup collects it regardless.
func cleanupDrill(backup, restore string) {
	if _, err := caps.Writer.Delete("", "namespace", DrillNamespace); err != nil {
		fmt.Printf("::warning::could not delete the drill namespace %s: %v\n", DrillNamespace, err)
	}
	dbr, _ := json.Marshal(map[string]any{
		"apiVersion": "velero.io/v1", "kind": "DeleteBackupRequest",
		"metadata": map[string]any{"name": backup + "-delete", "namespace": clusterspec.VeleroNamespace,
			"labels": map[string]any{"app.kubernetes.io/created-by": createdBy}},
		"spec": map[string]any{"backupName": backup},
	})
	if _, err := caps.Writer.ApplyStdin(string(dbr), createdBy); err != nil {
		fmt.Printf("::warning::could not request deletion of the drill backup %s (its 1h TTL will collect it): %v\n", backup, err)
	}
	if _, err := caps.Writer.Delete(clusterspec.VeleroNamespace, "restores.velero.io", restore); err != nil {
		fmt.Printf("::warning::could not delete the drill Restore %s: %v\n", restore, err)
	}
}

// poll calls fn every pollInterval until it reports done, fn errors, the
// budget runs out (an error) or ctx ends.
func poll(ctx context.Context, budget time.Duration, fn func() (bool, error)) error {
	deadline := time.Now().Add(budget)
	for {
		done, err := fn()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", budget)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none reported)"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
