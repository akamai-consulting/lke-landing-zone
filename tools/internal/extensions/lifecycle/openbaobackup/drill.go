package openbaobackup

// drill.go — `llz ci assert-openbao-restore`, the gate that fails when backup or
// restore stops working.
//
// WHAT IT PROVES, end to end, on the e2e cluster:
//
//  1. write a canary (nonce A) into OpenBao;
//  2. run the REAL producer — a Job built from the openbaoBackup CronJob, so its
//     ServiceAccount, k8s-auth role, policy, mTLS identity, NetworkPolicy and
//     bucket key are all the ones production uses;
//  3. read back, from the bucket, the exact object that run says it uploaded, and
//     verify it (consumer side — the producer's audit line is a claim);
//  4. overwrite the canary (nonce B), so a restore that does nothing is visible;
//  5. restore that snapshot through baosnap.Fetch/Inspect/Apply — the functions
//     `llz openbao restore` calls, not a copy of them;
//  6. read the canary back and require A.
//
// WHAT STAYS GREEN WITHOUT IT: every other check. The CronJob's carved App goes
// Healthy the moment the CronJob object exists; converge sees no failed Job until
// the first six-hourly tick; and a bucket full of objects says nothing about
// whether OpenBao will take one back. Backups are the textbook silent failure —
// nobody looks until the day they are needed.
//
// WHERE IT RUNS, AND WHY THERE. Restore needs a token with `update` on the
// snapshot endpoint, which no in-cluster role holds by design. The bootstrap job
// revokes its root token before the assert suite runs, so this runs as its own
// step just before that revoke — the same placement, for the same reason, as
// assert-database. It self-skips when spec.components.openbaoBackup is disabled.
//
// IT RESTORES THE LIVE e2e STORE IN PLACE. The snapshot it restores was taken
// seconds earlier, so the only state rolled back is the canary and anything
// written in that window; that is acceptable on a throwaway e2e cluster and is
// exactly why this is not part of any instance's scheduled checks.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

const (
	// DrillJob is the exercise Job's name. NOT an ephemeral-e2e name on purpose:
	// it carries the CronJob as its owner, so a successful drill supersedes a
	// scheduled tick that failed earlier in bootstrap (before the role existed),
	// exactly as a later successful tick would — see health.SupersededFailedJobs.
	DrillJob = "openbao-backup-drill"
	// CanaryPath is under `infra`, a namespace no spec.teams entry may claim.
	CanaryPath = "secret/infra/openbao-restore-drill"
)

// Budgets. Package vars so tests can shrink them.
var (
	cronJobWait    = 10 * time.Minute
	jobWait        = 8 * time.Minute
	pollInterval   = 10 * time.Second
	restoreSettle  = 3 * time.Minute
	canaryReadWait = 3 * time.Minute
)

// tempStoreFn is baosnap.TempStore, seamed so a test can point the drill's
// consumer-side read at a fake bucket.
var tempStoreFn = baosnap.TempStore

// RunDrill is the gate.
func RunDrill(ctx context.Context, region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return fmt.Errorf("assert-openbao-restore: load spec: %w", err)
	}
	e, ok := lz.Env(region)
	if !ok {
		return fmt.Errorf("no deployment %q in the spec", region)
	}
	if !clusterspec.ComponentEnabled(e.Components, "openbaoBackup") {
		fmt.Printf("openbaoBackup is disabled for %s — no producer to exercise; skipping the restore drill.\n", region)
		return nil
	}
	root := os.Getenv("OPENBAO_ROOT_TOKEN")
	if root == "" {
		return fmt.Errorf("OPENBAO_ROOT_TOKEN is not set — a restore needs `update` on sys/storage/raft/snapshot, which only root holds; " +
			"this step must run BEFORE the bootstrap job revokes its root token")
	}
	linodeTok := firstNonEmpty(os.Getenv("LINODE_API_TOKEN"), os.Getenv("LINODE_TOKEN"))
	if linodeTok == "" {
		return fmt.Errorf("LINODE_API_TOKEN is not set — the drill reads the bucket through a temporary key it mints, exactly as `llz openbao restore` does")
	}
	objCluster := e.Cluster.ObjectStorage.Cluster
	bucket := clusterspec.ObjOpenBaoBackupBucket(lz.ObjLabelPrefix(), region)
	endpoint := clusterspec.ObjEndpointHost(objCluster)

	// 1. Canary A.
	nonceA, nonceB := nonce(), nonce()
	if err := withActive(ctx, func(t baosnap.Target) error {
		return openbao.NewWithClient(t.Addr, root, "", t.HTTP).Write(ctx, CanaryPath, map[string]string{"nonce": nonceA})
	}); err != nil {
		return fmt.Errorf("write the canary: %w", err)
	}
	defer cleanupCanary(ctx, root)
	fmt.Printf("✓ canary written to %s\n", CanaryPath)

	// 2. The real producer.
	rec, err := runProducerJob(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("✓ backup Job uploaded %s/%s (%d bytes, raft index %d)\n", rec.Bucket, rec.Key, rec.Bytes, rec.RaftIndex)
	if rec.Bucket != bucket {
		// The producer reads the bucket from OpenBao; the restore derives it from
		// the spec. Disagreement means a restore would look in the wrong place.
		return fmt.Errorf("the backup Job wrote to bucket %q but the spec derives %q for %s — `llz openbao restore` would not find this deployment's snapshots",
			rec.Bucket, bucket, region)
	}

	// 3. Consumer-side fetch of exactly that object, through the restore path.
	st, revoke, err := tempStoreFn(ctx, caps.Keys(linodeTok), "llz-restore-drill-"+region, objCluster, bucket, endpoint, "read_only")
	if err != nil {
		return err
	}
	defer revoke()
	snap, data, m, err := baosnap.Fetch(ctx, st, rec.Key)
	if err != nil {
		return err
	}
	if sum := baosnap.SHA256(data); sum != rec.SHA256 {
		return fmt.Errorf("the object read back (%s) is not the one the Job reports uploading (%s)", sum, rec.SHA256)
	}
	fmt.Printf("✓ fetched and verified %s (raft index %d, sealed hashes %v)\n", snap.Key, m.Index, m.Sealed)

	// 4. Canary B — so a restore that changes nothing reads as a failure.
	if err := withActive(ctx, func(t baosnap.Target) error {
		return openbao.NewWithClient(t.Addr, root, "", t.HTTP).Write(ctx, CanaryPath, map[string]string{"nonce": nonceB})
	}); err != nil {
		return fmt.Errorf("overwrite the canary: %w", err)
	}

	// 5. Restore, through the operator path's own functions.
	if err := withActive(ctx, func(t baosnap.Target) error {
		plan, err := baosnap.Inspect(ctx, t, snap, m)
		if err != nil {
			return err
		}
		if !plan.SameCluster {
			return fmt.Errorf("the snapshot names cluster %s but the live cluster is %s — the producer recorded the wrong identity, "+
				"so the HA-pair guard in `llz openbao restore` would refuse every snapshot of this cluster", snap.ClusterID, plan.Live.ClusterID)
		}
		return baosnap.Apply(ctx, t, root, plan, data, false, restoreSettle)
	}); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	fmt.Println("✓ restore accepted and OpenBao is serving again")

	// 6. The canary must be A again.
	got, err := readCanary(ctx, root)
	if err != nil {
		return err
	}
	switch got {
	case nonceA:
		fmt.Println("✓ canary reads back the value the snapshot captured — backup and restore round-trip")
		return nil
	case nonceB:
		return fmt.Errorf("the canary still reads the post-snapshot value: OpenBao accepted the restore but did not roll its state back")
	default:
		return fmt.Errorf("the canary reads %q, neither the snapshotted nor the overwritten value", got)
	}
}

// runProducerJob creates the exercise Job from the CronJob, waits for it, and
// returns the audit record it printed.
func runProducerJob(ctx context.Context) (Record, error) {
	cronJSON, err := waitCronJob(ctx)
	if err != nil {
		return Record{}, err
	}
	job, err := DrillJobJSON(cronJSON)
	if err != nil {
		return Record{}, err
	}
	_, _ = caps.Writer.Delete(baosnap.Namespace, "job", DrillJob)
	if out, err := caps.Writer.ApplyStdin(string(job), "llz-openbao-backup-drill"); err != nil {
		return Record{}, fmt.Errorf("create the backup Job: %w\n%s", err, out)
	}
	fmt.Printf("created Job %s/%s from cronjob/%s — waiting up to %s…\n", baosnap.Namespace, DrillJob, baosnap.CronJob, jobWait)
	ok, failed := waitJob(ctx)
	logs := jobLogs()
	fmt.Println("── openbao-backup Job logs ──")
	fmt.Println(logs)
	if !ok {
		dumpDiag()
		why := "timed out"
		if failed {
			why = "the Job failed"
		}
		return Record{}, fmt.Errorf("the backup Job did not succeed (%s) — see the logs and diagnostics above", why)
	}
	rec, found := ParseRecord(logs)
	if !found {
		return Record{}, fmt.Errorf("the backup Job succeeded but printed no %q audit record", baosnap.AuditEvent)
	}
	if rec.Action != "uploaded" || rec.Key == "" {
		return Record{}, fmt.Errorf("the backup Job reports action=%q key=%q, want action=uploaded with a key", rec.Action, rec.Key)
	}
	return rec, nil
}

func waitCronJob(ctx context.Context) ([]byte, error) {
	deadline := time.Now().Add(cronJobWait)
	for {
		out, err := caps.Cluster.Run("-n", baosnap.Namespace, "get", "cronjob", baosnap.CronJob, "-o", "json")
		if err == nil && len(out) > 0 {
			return out, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("cronjob %s/%s never appeared within %s (%v) — is the llz-openbao-backup Argo Application synced? "+
				"Check `kubectl -n argocd get application llz-openbao-backup`", baosnap.Namespace, baosnap.CronJob, cronJobWait, err)
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return nil, err
		}
	}
}

func waitJob(ctx context.Context) (succeeded, failed bool) {
	deadline := time.Now().Add(jobWait)
	for {
		out, _ := caps.Cluster.Run("-n", baosnap.Namespace, "get", "job", DrillJob,
			"-o", "jsonpath={.status.succeeded}/{.status.failed}")
		s, f, _ := strings.Cut(strings.TrimSpace(string(out)), "/")
		if positive(s) {
			return true, false
		}
		if positive(f) {
			return false, true
		}
		if time.Now().After(deadline) || sleep(ctx, pollInterval) != nil {
			return false, false
		}
	}
}

func positive(s string) bool { s = strings.TrimSpace(s); return s != "" && s != "0" }

func jobLogs() string {
	names := caps.Cluster.Combined("-n", baosnap.Namespace, "get", "pods", "-l", "job-name="+DrillJob,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	var b strings.Builder
	for _, p := range strings.Fields(names) {
		b.WriteString(caps.Cluster.Combined("-n", baosnap.Namespace, "logs", p, "--tail=-1"))
		b.WriteString("\n")
	}
	return b.String()
}

func dumpDiag() {
	fmt.Println("── openbao-backup diagnostics ──")
	fmt.Println(caps.Cluster.Combined("-n", baosnap.Namespace, "describe", "job", DrillJob))
	fmt.Println(caps.Cluster.Combined("-n", baosnap.Namespace, "describe", "pods", "-l", "job-name="+DrillJob))
	fmt.Println(caps.Cluster.Combined("-n", baosnap.Namespace, "get", "events", "--sort-by=.lastTimestamp"))
}

// DrillJobJSON builds the exercise Job from the CronJob's own jobTemplate, owned
// by the CronJob. Pure; unit-tested. Everything that matters — image, args,
// ServiceAccount, mounts, labels the NetworkPolicy selects — is the CronJob's.
func DrillJobJSON(cronJobJSON []byte) ([]byte, error) {
	var cj struct {
		Metadata struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"metadata"`
		Spec struct {
			JobTemplate struct {
				Metadata map[string]any `json:"metadata"`
				Spec     map[string]any `json:"spec"`
			} `json:"jobTemplate"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(cronJobJSON, &cj); err != nil {
		return nil, fmt.Errorf("parse CronJob JSON: %w", err)
	}
	if cj.Spec.JobTemplate.Spec == nil {
		return nil, fmt.Errorf("CronJob has no .spec.jobTemplate.spec")
	}
	if cj.Metadata.UID == "" {
		return nil, fmt.Errorf("CronJob JSON carries no metadata.uid — it was not read from the cluster")
	}
	meta := map[string]any{
		"name":        DrillJob,
		"namespace":   baosnap.Namespace,
		"labels":      map[string]any{"app.kubernetes.io/created-by": "llz-e2e"},
		"annotations": map[string]any{"cronjob.kubernetes.io/instantiate": "manual"},
		"ownerReferences": []any{map[string]any{
			"apiVersion": "batch/v1", "kind": "CronJob", "name": cj.Metadata.Name,
			"uid": cj.Metadata.UID, "controller": true,
		}},
	}
	return json.Marshal(map[string]any{
		"apiVersion": "batch/v1", "kind": "Job", "metadata": meta, "spec": cj.Spec.JobTemplate.Spec,
	})
}

// ParseRecord finds the producer's audit line in a log stream.
func ParseRecord(logs string) (Record, bool) {
	var found Record
	ok := false
	for _, line := range strings.Split(logs, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil || r.Event != baosnap.AuditEvent {
			continue
		}
		found, ok = r, true // the last one wins: one Job, one record
	}
	return found, ok
}

// withActive runs fn against a fresh port-forward to the CURRENT active pod.
// Fresh each time because a restore can move leadership.
func withActive(ctx context.Context, fn func(baosnap.Target) error) error {
	t, err := ActiveTarget()
	if err != nil {
		return err
	}
	defer t.Close()
	return fn(t)
}

// ActiveTarget opens a tunnel to the active pod with the drill's own handles.
func ActiveTarget() (baosnap.Target, error) {
	return baosnap.OpenActive(caps.Cluster.Run, caps.PortForwardPod)
}

func readCanary(ctx context.Context, root string) (string, error) {
	deadline := time.Now().Add(canaryReadWait)
	var last error
	for {
		var got string
		err := withActive(ctx, func(t baosnap.Target) error {
			v, ok, err := openbao.NewWithClient(t.Addr, root, "", t.HTTP).Get(ctx, CanaryPath, "nonce")
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%s has no nonce after the restore", CanaryPath)
			}
			got = v
			return nil
		})
		if err == nil {
			return got, nil
		}
		last = err
		if time.Now().After(deadline) {
			return "", fmt.Errorf("could not read the canary back within %s after the restore: %w", canaryReadWait, last)
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return "", err
		}
	}
}

func cleanupCanary(ctx context.Context, root string) {
	err := withActive(ctx, func(t baosnap.Target) error {
		// Rollback to version 0 is a full metadata delete: the canary and its
		// history go, so no drill leaves a stale path behind on a reused cluster.
		return openbao.NewWithClient(t.Addr, root, "", t.HTTP).Rollback(ctx, CanaryPath, 0)
	})
	if err != nil {
		fmt.Printf("::warning::could not delete the drill canary %s: %v\n", CanaryPath, err)
	}
}

func nonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
