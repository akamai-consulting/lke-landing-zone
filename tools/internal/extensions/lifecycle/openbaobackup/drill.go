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
// IT RESTORES THE LIVE STORE IN PLACE, SO IT REFUSES ANY DEPLOYMENT BUT THE e2e
// LANE'S. The workflow runs it only on the dedicated `restore_drill` input, which
// only release-e2e sets; and this verb independently refuses unless --region is
// one of the e2e lane's deployment names (DrillAllowed). It used to ride on
// `assert_loki`, an input whose description promised a read-only Loki check — so
// anyone running that check against a real deployment would have rolled its
// OpenBao back as a side effect. Two keys, held by different files, so neither a
// workflow edit nor a mistyped dispatch alone can point it at production.
//
// WHAT THE ROLLBACK COSTS, AND WHAT THE DRILL PUTS BACK. The snapshot is seconds
// old, so what rolls back is the canary and anything written in that window. The
// one class of write that matters there is a ROTATED CREDENTIAL: if a rotator
// re-minted secret/obj/* or secret/linode/* between the snapshot and the restore,
// the restore resurrects the key it has since revoked, and every later assert
// that reads through it flakes. So the drill records every tracked credential
// path (credpaths.CredPaths) just before the restore and re-writes any the
// restore reverted. That is the drill cleaning up after itself — it is not what
// `llz openbao restore` does, whose snapshot is hours old and whose operator
// follows the runbook's "After a restore" instead.
//
// --backup-only IS THE SAFE HALF, FOR EVERY DEPLOYMENT. Steps 2 and 3 alone — run
// the real producer, then fetch and verify the exact object it uploaded through
// the restore path's temporary read-only key. No canary, no root, no restore.
// bootstrap-openbao runs it on every deployment, so a producer that cannot work
// (an OpenBao version whose snapshot endpoint wants a different capability, an
// archive layout Verify does not recognise, a bucket that was never applied)
// fails the adopter's own bootstrap — not six hours later on the first tick, and
// not only in release e2e.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/credpaths"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/forge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

// E2ELaneBase is the deployment name the release-e2e lanes instantiate, before
// forge.LaneSuffix makes it forge-unique (e2e, e2e-ghec, e2e-ghes, e2e-gitlab).
const E2ELaneBase = "e2e"

// DrillAllowed reports whether region is one of the e2e lane's throwaway
// deployments — the only place an in-place restore is acceptable. Derived from
// forge.LaneName, the function that names the lanes, rather than a restated list.
func DrillAllowed(region string) bool {
	for _, f := range []forge.Flavor{forge.GitHub, forge.GHEC, forge.GHES, forge.GitLab} {
		if region == forge.LaneName(f, E2ELaneBase) {
			return true
		}
	}
	return false
}

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

// drillTarget is what both modes resolve from the spec before touching anything.
type drillTarget struct {
	objCluster, bucket, endpoint string
}

// resolveDrill loads the spec for region. skip=true means the component is off
// and there is nothing to exercise.
func resolveDrill(region, what string) (t drillTarget, skip bool, err error) {
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return t, false, fmt.Errorf("assert-openbao-restore: load spec: %w", err)
	}
	e, ok := lz.Env(region)
	if !ok {
		return t, false, fmt.Errorf("no deployment %q in the spec", region)
	}
	if !clusterspec.ComponentEnabled(e.Components, "openbaoBackup") {
		fmt.Printf("openbaoBackup is disabled for %s — no producer to exercise; skipping the %s.\n", region, what)
		return t, true, nil
	}
	oc := e.Cluster.ObjectStorage.Cluster
	return drillTarget{objCluster: oc, bucket: clusterspec.ObjOpenBaoBackupBucket(lz.ObjLabelPrefix(), region),
		endpoint: clusterspec.ObjEndpointHost(oc)}, false, nil
}

// produceAndFetch is steps 2 and 3: run the real producer, then fetch and verify
// the exact object it reports uploading, through the restore path's own
// temporary read-only key and baosnap.Fetch. Shared by both modes.
func produceAndFetch(ctx context.Context, region, linodeTok string, tg drillTarget) (Record, baosnap.Snapshot, []byte, baosnap.Manifest, error) {
	rec, err := runProducerJob(ctx)
	if err != nil {
		return rec, baosnap.Snapshot{}, nil, baosnap.Manifest{}, err
	}
	fmt.Printf("✓ backup Job uploaded %s/%s (%d bytes, raft index %d)\n", rec.Bucket, rec.Key, rec.Bytes, rec.RaftIndex)
	if rec.Bucket != tg.bucket {
		// The producer reads the bucket from OpenBao; the restore derives it from
		// the spec. Disagreement means a restore would look in the wrong place.
		return rec, baosnap.Snapshot{}, nil, baosnap.Manifest{}, fmt.Errorf(
			"the backup Job wrote to bucket %q but the spec derives %q for %s — `llz openbao restore` would not find this deployment's snapshots",
			rec.Bucket, tg.bucket, region)
	}
	st, revoke, err := tempStoreFn(ctx, caps.Keys(linodeTok), "llz-restore-drill-"+region, tg.objCluster, tg.bucket, tg.endpoint, "read_only")
	if err != nil {
		return rec, baosnap.Snapshot{}, nil, baosnap.Manifest{}, err
	}
	defer revoke()
	snap, data, m, err := baosnap.Fetch(ctx, st, rec.Key, "")
	if err != nil {
		return rec, snap, nil, m, err
	}
	if sum := baosnap.SHA256(data); sum != rec.SHA256 {
		return rec, snap, nil, m, fmt.Errorf("the object read back (%s) is not the one the Job reports uploading (%s)", sum, rec.SHA256)
	}
	fmt.Printf("✓ fetched and verified %s (raft index %d, sealed hashes %v)\n", snap.Key, m.Index, m.Sealed)
	return rec, snap, data, m, nil
}

func linodeToken() (string, error) {
	tok := firstNonEmpty(os.Getenv("LINODE_API_TOKEN"), os.Getenv("LINODE_TOKEN"))
	if tok == "" {
		return "", fmt.Errorf("LINODE_API_TOKEN is not set — the drill reads the bucket through a temporary key it mints, exactly as `llz openbao restore` does")
	}
	return tok, nil
}

// RunBackupCheck is `assert-openbao-restore --backup-only`: prove this
// deployment's producer works, now, without restoring anything. Safe anywhere.
func RunBackupCheck(ctx context.Context, region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	tg, skip, err := resolveDrill(region, "backup check")
	if err != nil || skip {
		return err
	}
	linodeTok, err := linodeToken()
	if err != nil {
		return err
	}
	if _, _, _, _, err := produceAndFetch(ctx, region, linodeTok, tg); err != nil {
		return err
	}
	fmt.Printf("✓ %s's OpenBao backup works end to end: the real CronJob's Job took, verified and uploaded a snapshot that reads back intact through the restore path\n", region)
	return nil
}

// RunDrill is the gate.
func RunDrill(ctx context.Context, region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	if !DrillAllowed(region) {
		return fmt.Errorf("refusing: assert-openbao-restore ROLLS BACK the live OpenBao of the deployment it runs on, and %q is not "+
			"an e2e lane deployment (%s or a forge-suffixed %s-*). Use --backup-only to prove backups work on a real deployment; "+
			"restore one deliberately with `llz openbao restore`", region, E2ELaneBase, E2ELaneBase)
	}
	tg, skip, err := resolveDrill(region, "restore drill")
	if err != nil || skip {
		return err
	}
	root := os.Getenv("OPENBAO_ROOT_TOKEN")
	if root == "" {
		return fmt.Errorf("OPENBAO_ROOT_TOKEN is not set — a restore needs `update` on sys/storage/raft/snapshot, which only root holds; " +
			"this step must run BEFORE the bootstrap job revokes its root token")
	}
	linodeTok, err := linodeToken()
	if err != nil {
		return err
	}

	// 1. Canary A.
	nonceA, nonceB := nonce(), nonce()
	if err := withActive(ctx, func(t baosnap.Target) error {
		return openbao.NewWithClient(t.Addr, root, "", t.HTTP).Write(ctx, CanaryPath, map[string]string{"nonce": nonceA})
	}); err != nil {
		return fmt.Errorf("write the canary: %w", err)
	}
	defer cleanupCanary(ctx, root)
	fmt.Printf("✓ canary written to %s\n", CanaryPath)

	// 2 + 3. The real producer, then a consumer-side fetch of exactly that object.
	_, snap, data, m, err := produceAndFetch(ctx, region, linodeTok, tg)
	if err != nil {
		return err
	}

	// 4. Canary B — so a restore that changes nothing reads as a failure.
	if err := withActive(ctx, func(t baosnap.Target) error {
		return openbao.NewWithClient(t.Addr, root, "", t.HTTP).Write(ctx, CanaryPath, map[string]string{"nonce": nonceB})
	}); err != nil {
		return fmt.Errorf("overwrite the canary: %w", err)
	}

	// 5. Restore, through the operator path's own functions — after recording the
	// credentials the rollback could revert (see the file comment).
	var before credState
	if err := withActive(ctx, func(t baosnap.Target) error {
		var err error
		before, err = captureCreds(ctx, openbao.NewWithClient(t.Addr, root, "", t.HTTP), drillCredPaths())
		return err
	}); err != nil {
		return fmt.Errorf("record the credentials before the restore: %w", err)
	}
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
	// Put back any credential the rollback reverted, whatever the canary says: a
	// failed drill must not also leave the cluster holding revoked keys.
	if rerr := reapplyCreds(ctx, root, before); rerr != nil {
		return fmt.Errorf("the restore reverted rotated credentials and they could not be re-applied — later checks will read revoked keys: %w", rerr)
	}
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

// credState is a set of KV paths' data as read at one moment; a nil entry means
// the path did not exist.
type credState map[string]map[string]string

// drillCredPaths are the paths the drill protects from its own rollback: every
// tracked credential. Read from credpaths.CredPaths, the table the rotators and
// the age sampler share, so a new credential is covered the day it is tracked.
func drillCredPaths() []string {
	out := make([]string, 0, len(credpaths.CredPaths))
	for _, p := range credpaths.CredPaths {
		out = append(out, p.Path)
	}
	return out
}

func captureCreds(ctx context.Context, c *openbao.Client, paths []string) (credState, error) {
	st := credState{}
	for _, p := range paths {
		raw, ok, err := c.Read(ctx, openbao.DataPath(p))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		if !ok {
			st[p] = nil
			continue
		}
		st[p] = kvStrings(raw)
	}
	return st, nil
}

// CredsToReapply returns, sorted, the paths whose value after the restore is not
// what it was just before it — the writes the rollback undid. A path that did not
// exist before is never "re-applied" (there is nothing to put back). Pure.
func CredsToReapply(before, after credState) []string {
	var out []string
	for p, b := range before {
		if b == nil {
			continue
		}
		if !sameFields(b, after[p]) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func sameFields(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// reapplyCreds re-reads the tracked paths after the restore and writes back the
// pre-restore value of every one the restore changed.
func reapplyCreds(ctx context.Context, root string, before credState) error {
	if len(before) == 0 {
		return nil
	}
	return withActive(ctx, func(t baosnap.Target) error {
		c := openbao.NewWithClient(t.Addr, root, "", t.HTTP)
		paths := make([]string, 0, len(before))
		for p := range before {
			paths = append(paths, p)
		}
		after, err := captureCreds(ctx, c, paths)
		if err != nil {
			return err
		}
		todo := CredsToReapply(before, after)
		for _, p := range todo {
			if err := c.Write(ctx, p, before[p]); err != nil {
				return fmt.Errorf("re-apply %s: %w", p, err)
			}
		}
		if len(todo) > 0 {
			fmt.Printf("✓ re-applied %d credential(s) the rollback had reverted to a pre-rotation value: %s\n", len(todo), strings.Join(todo, ", "))
		} else {
			fmt.Println("✓ no tracked credential changed across the rollback")
		}
		return nil
	})
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
