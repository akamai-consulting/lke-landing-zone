package openbao

// ci_openbao_restore.go — `llz ci openbao-restore`, the verb behind the
// restore-openbao workflow (instance-template/.github/workflows/
// llz-restore-openbao.yml; maintainer notes in docs/workflows/llz-restore-openbao.md).
//
// WHY A WORKFLOW AT ALL. `llz openbao restore` needs a root token on the target,
// and a workstation gets one only from recovery shares the operator holds. A
// deployment bootstrapped without an escrow key has its shares ONLY as
// infra-<region> environment secrets, which nothing outside a workflow job can
// read — so its operator could not restore without first parking a root token in
// GitHub through break-glass and recovering it by other means. This runs the same
// restore inside a job in that environment, mints root there from the quorum,
// and kills it before the job ends.
//
// IT IS `llz openbao restore`, NOT A COPY. The modes are `llz openbao backups`
// (list) and `llz openbao restore` as a dry run (plan), called in process; the
// restore mode runs the same two halves (prepareRestore / apply), so the
// kube-context check, the archive verification, the HA-pair and --force guards
// and the settle wait are the operator path's own. What this file adds is only
// what a workstation does by hand around it:
//
//	check the request    → mode, snapshot XOR latest, the typed confirmation,
//	                       and that the spec has backups on (refusals, all of
//	                       them before the workflow opens cluster access)
//	prepare              → fetch + verify + kube-context + inspect (no change)
//	refuse a foreign     → BEFORE a quorum is spent on a token for it
//	  snapshot w/o force
//	mint root #1         → recovery quorum; recorded for the always() revoke
//	record credentials   → every tracked credential path, as it is now
//	apply                → re-check the kube-context, restore, wait unsealed
//	mint root #2         → on the RESTORED store, whose token table predates
//	                       root #1 (so #1 is already gone; revokeRootToken
//	                       proves it rather than assuming it)
//	re-apply credentials → baosnap.ReapplyCreds, the drill's own rule
//	check the stored     → is infra-<region>::OPENBAO_ROOT_TOKEN live again?
//	  root token
//	summary              → what ran, and the checklist of what did not
//
// The workflow's next step re-runs `llz ci bao-configure` under root #2, and its
// always() step runs `llz ci bao-revoke-root`. The configure step is not called
// from here because it belongs to another extension (identity-plane); an import
// edge between the two would be the coupling the extension split exists to stop.
//
// WHAT IS DELIBERATELY NOT AUTOMATED, and why (the summary lists each):
//
//   - re-applying operator writes made after the snapshot: only the operator
//     knows which were intended;
//   - rotating the database admin credential: `secret/infra/db-admin/*` is not a
//     tracked path, and rotating it resets the password in place with no overlap,
//     breaking every consumer until ESO re-syncs — an outage nobody asked for;
//   - re-minting secret/linode/api-token or the broad PAT from scratch: the
//     re-apply puts back the value that was live a minute ago, which is current;
//     minting new account-scoped PATs is a different, louder operation;
//   - taking a backup of the restored store: it would count against the newest
//     backupKeep snapshots of that cluster id — after a rebuild, the LOST
//     cluster's history — and push the oldest restore point out. The next
//     scheduled tick does the same thing on its own clock, and the backup alerts
//     cover it;
//   - revoking infra-<region>::OPENBAO_ROOT_TOKEN when the restore brought it
//     back to life: someone may be holding it mid-incident. It is reported, and
//     break-glass's action=revoke is one dispatch;
//   - reconciling an HA peer: the drift check reads both halves and decides
//     which value is newer, which is a judgement.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

// Restore workflow modes.
const (
	RestoreModeList    = "list"
	RestoreModePlan    = "plan"
	RestoreModeRestore = "restore"
)

// RestoreRequest is one dispatch of the restore workflow.
type RestoreRequest struct {
	Region      string
	Mode        string
	Snapshot    string
	Latest      bool
	FromCluster string
	Force       bool
	Confirm     string
}

// ExpectedRestoreConfirm is the typed confirmation a restore of region needs —
// the same token `llz openbao restore --confirm` takes.
func ExpectedRestoreConfirm(region string) string { return "restore:" + region }

// Validate checks the request's shape. Pure; it is the workflow's first step, so
// a mistyped dispatch fails before anything — cluster access included — is
// opened.
func (r RestoreRequest) Validate() error {
	if strings.TrimSpace(r.Region) == "" {
		return fmt.Errorf("region is required")
	}
	switch r.Mode {
	case RestoreModeList, RestoreModePlan, RestoreModeRestore:
	default:
		return fmt.Errorf("unknown mode %q (expected list | plan | restore)", r.Mode)
	}
	if r.Snapshot != "" && r.Latest {
		return fmt.Errorf("give snapshot OR latest, not both")
	}
	if r.FromCluster != "" && !r.Latest {
		return fmt.Errorf("from_cluster only narrows latest=true; with an explicit snapshot it would be ignored, so it is refused")
	}
	if r.Mode == RestoreModeList {
		return nil
	}
	if r.Snapshot == "" && !r.Latest {
		return fmt.Errorf("mode=%s needs the snapshot chosen explicitly: snapshot=<key> (run mode=list to see them) or latest=true. "+
			"There is no default: after an incident the newest snapshot may already hold the damage", r.Mode)
	}
	if r.Mode == RestoreModeRestore && r.Confirm != ExpectedRestoreConfirm(r.Region) {
		return fmt.Errorf("mode=restore REPLACES %s's whole OpenBao store: set confirm to exactly %q to proceed (got %q)",
			r.Region, ExpectedRestoreConfirm(r.Region), r.Confirm)
	}
	return nil
}

// requireBackupEnabled refuses plan/restore on a deployment whose spec turns the
// openbaoBackup component off.
//
// WHY REFUSE rather than try: with the component off there is no producer, and
// a bucket it may once have filled is stale by an unknown margin — the restore
// would roll the store back to whenever backups stopped, and nothing on the
// dispatch form says how long ago that was. The one legitimate case — a rebuild,
// where the runbook has the component off so the empty rebuilt store does not
// back itself up — is handled by turning it back on before the restore (the
// runbook's workflow path does), or by the workstation path, which does not
// refuse. `list` stays allowed: reading what is in the bucket is how an operator
// finds out.
func requireBackupEnabled(region string) error {
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return fmt.Errorf("load the instance spec: %w", err)
	}
	e, ok := lz.Env(region)
	if !ok {
		return fmt.Errorf("no deployment %q in the spec", region)
	}
	if !clusterspec.ComponentEnabled(e.Components, "openbaoBackup") {
		return fmt.Errorf("refusing: components.openbaoBackup is disabled for %s in environments/%s.yaml, so nothing is producing "+
			"the snapshots this would restore from. If you turned it off for a rebuild (docs/runbooks/openbao-backup-restore.md), "+
			"turn it back on, run `llz render %s`, commit, and dispatch again — or restore from a workstation with `llz openbao restore`",
			region, region, region)
	}
	return nil
}

// RunRestoreCI is `llz ci openbao-restore`.
func RunRestoreCI(req RestoreRequest, preflightOnly bool, tokenFile string) error {
	if err := req.Validate(); err != nil {
		return err
	}
	if req.Mode != RestoreModeList {
		if err := requireBackupEnabled(req.Region); err != nil {
			return err
		}
	}
	if preflightOnly {
		fmt.Printf("✓ request checked: mode=%s on %s%s\n", req.Mode, req.Region, describeChoice(req))
		return nil
	}
	switch req.Mode {
	case RestoreModeList:
		return RunBackups(req.Region)
	case RestoreModePlan:
		if err := RunRestore(req.opts(false)); err != nil {
			return err
		}
		fmt.Printf("(From the workflow: dispatch restore-openbao.yml again with mode=restore and confirm=%s.)\n", ExpectedRestoreConfirm(req.Region))
		return nil
	}
	if tokenFile == "" {
		tokenFile = RestoreRootTokenFile()
	}
	res := runRestoreInCI(context.Background(), req, tokenFile, os.Getenv("OPENBAO_STORED_ROOT_TOKEN"))
	if err := ghaout.Append("GITHUB_STEP_SUMMARY", restoreSummary(req, res, restoreNow())...); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::could not write the job summary: %v\n", err)
	}
	return res.err
}

func (r RestoreRequest) opts(yes bool) RestoreOpts {
	o := RestoreOpts{Region: r.Region, Snapshot: r.Snapshot, Latest: r.Latest, FromCluster: r.FromCluster, Force: r.Force}
	if yes {
		o.Yes, o.Confirm = true, r.Confirm
	}
	return o
}

func describeChoice(r RestoreRequest) string {
	switch {
	case r.Mode == RestoreModeList:
		return ""
	case r.Snapshot != "":
		return ", snapshot " + r.Snapshot
	case r.FromCluster != "":
		return ", the newest snapshot of OpenBao cluster " + r.FromCluster
	}
	return ", the newest snapshot"
}

// restoreResult is what the restore mode did, for the summary.
type restoreResult struct {
	snap     baosnap.Snapshot
	sameID   bool
	restored bool
	// postRoot: a root token on the restored store was minted, so the
	// credential re-apply ran and the workflow's configure step can.
	postRoot   bool
	reapplied  []string
	tracked    []string
	storedLive bool
	storedOK   bool
	peer       string
	err        error
}

func runRestoreInCI(ctx context.Context, req RestoreRequest, tokenFile, storedRoot string) (res restoreResult) {
	res.peer = haPeer(req.Region)
	s, err := prepareRestore(ctx, req.opts(true))
	if err != nil {
		res.err = err
		return res
	}
	defer s.Close()
	res.snap, res.sameID = s.snap, s.plan.SameCluster

	// Every guard Apply holds, asked BEFORE the quorum is spent: a foreign snapshot
	// without force, a sealed or standby target.
	if err := baosnap.CheckApplicable(s.plan, req.Force); err != nil {
		res.err = err
		return res
	}

	root1, err := mintEphemeralRoot(req.Region, tokenFile)
	if err != nil {
		res.err = fmt.Errorf("mint a root token from the infra-%s recovery quorum: %w", req.Region, err)
		return res
	}
	before, err := baosnap.CaptureCreds(ctx, openbao.NewWithClient(s.t.Addr, root1, "", s.t.HTTP), baosnap.TrackedCredPaths())
	if err != nil {
		res.err = fmt.Errorf("record the tracked credentials before the restore (nothing has been restored): %w", err)
		return res
	}
	for p, v := range before {
		if v != nil {
			res.tracked = append(res.tracked, p)
		}
	}
	sort.Strings(res.tracked)
	if err := s.apply(ctx, root1); err != nil {
		res.err = err
		return res
	}
	res.restored = true

	root2, err := mintEphemeralRoot(req.Region, tokenFile)
	if err != nil {
		res.err = fmt.Errorf("the restore SUCCEEDED, but no root token could be minted on the restored store, so the "+
			"post-restore steps did not run: %w", err)
		return res
	}
	res.postRoot = true

	t, err := baosnap.OpenActive(s.cl.Run, restorePF)
	if err != nil {
		res.err = fmt.Errorf("the restore SUCCEEDED, but re-opening the active pod to re-apply credentials failed: %w", err)
		return res
	}
	defer t.Close()
	res.reapplied, err = baosnap.ReapplyCreds(ctx, openbao.NewWithClient(t.Addr, root2, "", t.HTTP), before)
	if err != nil {
		res.err = fmt.Errorf("the restore SUCCEEDED, but the credentials it reverted could not all be re-applied: %w", err)
		return res
	}
	if len(res.reapplied) > 0 {
		fmt.Printf("✓ re-applied %d tracked credential(s) the restore had reverted: %s\n", len(res.reapplied), strings.Join(res.reapplied, ", "))
	} else {
		fmt.Println("✓ no tracked credential changed across the restore")
	}

	if storedRoot != "" {
		res.storedLive, res.storedOK = storedRootLive(storedRoot)
		switch {
		case !res.storedOK:
			fmt.Printf("::warning::could not tell whether infra-%s::OPENBAO_ROOT_TOKEN is live in the restored store\n", req.Region)
		case res.storedLive:
			fmt.Printf("::warning::infra-%s::OPENBAO_ROOT_TOKEN is LIVE in the restored store — the snapshot was taken while it was valid. "+
				"Dispatch breakglass-openbao.yml with action=revoke once nobody is using it.\n", req.Region)
		}
	}
	return res
}

// restoreSummary is the job summary: what this run did, and the checklist of
// what it deliberately did not. It carries names, keys and counts — never a
// token or a credential value.
func restoreSummary(req RestoreRequest, res restoreResult, now time.Time) []string {
	region := req.Region
	lines := []string{fmt.Sprintf("## OpenBao restore — %s", region), "", BreakglassActorLine(), ""}
	if !res.restored {
		lines = append(lines, "**Nothing was restored.** "+firstLine(res.err), "",
			"The store is unchanged. If a root token was minted before the failure, the job's final step revokes it.")
		return lines
	}
	match := "taken by this OpenBao cluster"
	if !res.sameID {
		match = fmt.Sprintf("taken by OpenBao cluster `%s` (restored with force)", res.snap.ClusterID)
	}
	lines = append(lines,
		fmt.Sprintf("Restored `%s` — %s, %s old. The store is now as it was then.", res.snap.Key, match, now.Sub(res.snap.Taken).Round(time.Minute)),
		"",
		"### Done by this run",
		"",
		"- A root token was minted from the recovery quorum, used for the restore, and is revoked by this job's final step. It was never written to GitHub.",
	)
	if !res.postRoot {
		lines = append(lines,
			"",
			"### ⚠️ The post-restore steps did NOT run",
			"",
			"No root token could be minted on the restored store: "+firstLine(res.err),
			"",
			"After a restore the store answers only to the recovery shares it had when the snapshot was taken. If this deployment was "+
				"**rebuilt**, the rebuild's init replaced the shares in `infra-"+region+"`: put the snapshot-era shares back as "+
				"`OPENBAO_RECOVERY_KEY_1`..`3`, then re-run `bootstrap-openbao.yml` for "+region+" (it re-applies the configuration).",
			"",
			"These tracked credentials were NOT re-applied, so the restored store may name keys that have since been revoked — "+
				"re-mint them per the runbook's \"After a restore\": "+codeList(res.tracked),
		)
		return append(lines, manualChecklist(region, res)...)
	}
	if len(res.reapplied) > 0 {
		lines = append(lines, "- Re-applied the live value of every tracked credential the restore had reverted: "+codeList(res.reapplied)+
			". If the damage you restored from was to one of these paths, re-mint that credential.")
	} else {
		lines = append(lines, "- No tracked credential changed across the restore.")
	}
	lines = append(lines, "- The next step re-applies the declared OpenBao configuration (`llz ci bao-configure`, as bootstrap does). If it failed, re-run `bootstrap-openbao.yml` for "+region+".")
	return append(lines, manualChecklist(region, res)...)
}

func manualChecklist(region string, res restoreResult) []string {
	l := []string{"", "### Still to do by hand", ""}
	if res.storedLive {
		l = append(l, "- [ ] `infra-"+region+"::OPENBAO_ROOT_TOKEN` is **live** again in the restored store. Dispatch `breakglass-openbao.yml` with action=revoke once nobody is using it.")
	} else if res.postRoot && !res.storedOK {
		l = append(l, "- [ ] Could not tell whether `infra-"+region+"::OPENBAO_ROOT_TOKEN` is live in the restored store; if it is set, dispatch `breakglass-openbao.yml` with action=revoke.")
	}
	l = append(l,
		"- [ ] Re-apply any `llz openbao set` written after the snapshot — they are gone.",
		"- [ ] If `rotate-db-admin` ran after the snapshot, rotate again (`secret-rotation.yml` scope `db-admin`) so OpenBao and the database agree.",
		"- [ ] Tokens issued after the snapshot are invalid. In-cluster workloads re-authenticate on their own; confirm with `cluster-health.yml`.",
		"- [ ] Confirm the next scheduled backup succeeds (`LLZOpenBaoBackupFailing` stays quiet), or run one: `kubectl -n "+baosnap.Namespace+" create job --from=cronjob/"+baosnap.CronJob+" openbao-backup-manual`.",
	)
	if res.peer != "" {
		l = append(l, "- [ ] "+region+" is an HA pair with "+res.peer+": run the drift check in docs/secrets.md against both and re-apply the newer values with `llz openbao set`.")
	}
	if !res.sameID {
		l = append(l, "- [ ] The snapshot came from another OpenBao cluster id (a rebuild): if backups were turned off for the rebuild, make sure `components.openbaoBackup` is back on.")
	}
	return append(l, "", "Runbook: docs/runbooks/openbao-backup-restore.md, \"After a restore\".")
}

func codeList(paths []string) string {
	if len(paths) == 0 {
		return "(none)"
	}
	return "`" + strings.Join(paths, "`, `") + "`"
}

func firstLine(err error) string {
	if err == nil {
		return ""
	}
	s, _, _ := strings.Cut(err.Error(), "\n")
	return s
}

// OpenBaoRestoreCICmd is `llz ci openbao-restore`.
func OpenBaoRestoreCICmd() *cobra.Command {
	var req RestoreRequest
	var preflight bool
	var tokenFile string
	c := &cobra.Command{
		Use:   "openbao-restore --region <deployment> --mode list|plan|restore [--snapshot <key> | --latest [--from-cluster <id>]] [--force] [--confirm restore:<deployment>]",
		Short: "the restore-openbao workflow: list, plan or restore a deployment's OpenBao from a Raft snapshot, root minted in-job",
		Long: "Runs inside llz-restore-openbao.yml, in the infra-<deployment> environment.\n" +
			"\n" +
			"  list     `llz openbao backups`\n" +
			"  plan     `llz openbao restore` as a dry run (fetch, verify, kube-context check, inspect)\n" +
			"  restore  mints a root token from the recovery quorum (RECOVERY_K1..3), records\n" +
			"           every tracked credential, restores through `llz openbao restore`'s own\n" +
			"           path, mints root again on the restored store, re-applies the credentials\n" +
			"           the restore reverted, and writes a checklist to the job summary. The\n" +
			"           token lives in --token-file only (default $RUNNER_TEMP/" + RestoreRootTokenFileName + ");\n" +
			"           `llz ci bao-revoke-root` kills it. Nothing is written to GitHub.\n" +
			"\n" +
			"plan and restore refuse a deployment whose spec disables components.openbaoBackup;\n" +
			"restore needs --confirm restore:<deployment> exactly. --preflight checks the request\n" +
			"and the spec and stops, touching nothing.\n" +
			"See docs/runbooks/openbao-backup-restore.md.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return RunRestoreCI(req, preflight, tokenFile)
		},
	}
	f := c.Flags()
	f.StringVar(&req.Region, "region", "", "deployment (spec env name) (required)")
	f.StringVar(&req.Mode, "mode", RestoreModePlan, "list | plan | restore")
	f.StringVar(&req.Snapshot, "snapshot", "", "snapshot key, as `--mode list` prints it")
	f.BoolVar(&req.Latest, "latest", false, "use the newest snapshot (must be asked for explicitly)")
	f.StringVar(&req.FromCluster, "from-cluster", "", "with --latest: only snapshots taken by this OpenBao cluster id")
	f.BoolVar(&req.Force, "force", false, "restore a snapshot taken by a different OpenBao cluster (a rebuild)")
	f.StringVar(&req.Confirm, "confirm", "", "must be exactly restore:<deployment> for --mode restore")
	f.BoolVar(&preflight, "preflight", false, "check the request and the spec, then stop")
	f.StringVar(&tokenFile, "token-file", "", "where the minted root token is kept (default $RUNNER_TEMP/"+RestoreRootTokenFileName+")")
	return c
}
