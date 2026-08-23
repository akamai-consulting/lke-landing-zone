package teardown

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
	"github.com/spf13/cobra"
)

// cobra_reap.go — the orphan-resource sweeps and their flag sets.
//
// DECLARED, THOUGH NOT BY NAME: this extension's own doc already says "the reaper
// is the transition's job and the gate only counts", so these belong to the
// transition:destroyed binding that was already here.
//
// nbBelongsToCluster did NOT come with them — this package already had a
// byte-identical copy, so the duplicate in ci.go was deleted rather than moved.

// sweepOpts carries the per-resource wording and retry knobs for sweepUntilEmpty.
// The wording is spelled out per resource rather than derived from one noun
// because these lines are the destroy job's log surface — keep them identical to
// what each sweep printed before.
type sweepOpts struct {
	cmd          string // prefixes the terminal error ("reap-volumes")
	banner       string // per-attempt banner; " [attempt n/N] ===" is appended
	singular     string // "Volume" — the retry line
	plural       string // "Volumes" — the verify-error line
	unit         string // "tracked Volume(s)" — the still-present lines
	goneMsg      string // printed once the verified count reaches zero
	attempts     int
	retryDelay   int
	requireEmpty bool
}

// volumeDetachPollInterval is the pause between detach re-checks. The Volumes
// detach asynchronously as the LKE nodes tear down, so a tighter poll catches
// "all detached" sooner (a ListVolumes read is cheap) — 10s trims up to ~20s off
// teardown vs the former 30s without meaningfully more API load. A package var so
// tests can zero it.
var volumeDetachPollInterval = 10 * time.Second

func ReapVolumesCmd() *cobra.Command {
	var region, volumeIDs, tagMustInclude, env string
	var waitDetach, attempts, retryDelay int
	var requireEmpty bool
	c := &cobra.Command{
		Use:   "reap-volumes",
		Short: "delete orphaned pvc-* Block Storage Volumes (--yes to delete)",
		Long: "Native port of cleanup-orphan-volumes.sh. Deletes unattached CSI Volumes\n" +
			"(linode_id null) scoped by --volume-ids and/or --region, with an\n" +
			"optional --env so RELABELED volumes (<env>-<ns>-<pvc>) are swept too — without\n" +
			"it only the CSI default pvc-* labels match and every renamed volume leaks.\n" +
			"optional --tag-must-include constraint — the same orphan predicate as `llz\n" +
			"reap`. At least one scope is required (never an unscoped sweep).\n" +
			"--wait-detach polls until every --volume-ids Volume is unattached before\n" +
			"sweeping (cluster delete detaches them asynchronously as the LKE Linodes\n" +
			"tear down).\n" +
			"--require-empty (needs --volume-ids) re-lists after the sweep and, if any\n" +
			"tracked Volume is still present, retries up to --attempts (sleeping\n" +
			"--retry-delay s between tries) and finally EXITS NON-ZERO when orphans\n" +
			"remain — so a destroy doesn't go color.Green leaving Volumes that block the next\n" +
			"apply's preflight. Without it the sweep is single-pass and best-effort.\n" +
			"Reads LINODE_TOKEN; dry-run by default, deletes only with --yes.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIReapVolumes(cliopts.Global, env, region, volumeIDs, tagMustInclude, waitDetach, attempts, retryDelay, requireEmpty)
		},
	}
	f := c.Flags()
	f.StringVar(&region, "region", "", "scope to one Linode region (e.g. us-ord)")
	f.StringVar(&env, "env", "", "deployment name (REGION_SHORT) whose RELABELED volumes to include; without it the sweep sees only the CSI default pvc-* labels and leaks every renamed volume")
	f.StringVar(&volumeIDs, "volume-ids", "", "space-separated Volume id allowlist (the precise CI scope)")
	f.StringVar(&tagMustInclude, "tag-must-include", "", "only delete Volumes whose tags include this (e.g. block-storage)")
	f.IntVar(&waitDetach, "wait-detach", 0, "seconds to wait for the --volume-ids Volumes to detach before sweeping (0 = no wait)")
	f.BoolVar(&requireEmpty, "require-empty", false, "verify every --volume-ids Volume is gone; retry then fail if orphans remain")
	f.IntVar(&attempts, "attempts", 1, "sweep+verify attempts before failing (only with --require-empty)")
	f.IntVar(&retryDelay, "retry-delay", 30, "seconds between --require-empty retries")
	return c
}
func ReapNodeBalancersCmd() *cobra.Command {
	var clusterID, region string
	var attempts, retryDelay int
	var requireEmpty bool
	c := &cobra.Command{
		Use:   "reap-nodebalancers",
		Short: "delete orphaned NodeBalancers (--cluster-id for the CI-scoped sweep; --yes to delete)",
		Long: "Native port of cleanup-orphan-nodebalancers.sh. With --cluster-id it deletes\n" +
			"only NodeBalancers carrying that cluster's CCM tag (lke<id>) — the\n" +
			"co-located-peer-safe mode the destroy path uses. Without it, an account-wide\n" +
			"orphan sweep (CCM tag points to a gone cluster, or CCM-identified with 0\n" +
			"backends), optionally narrowed by --region. Dry-run by default; --yes to delete.\n" +
			"--require-empty (needs --cluster-id) re-lists after the sweep and, if any\n" +
			"NodeBalancer still carries the cluster's CCM tag, retries up to --attempts\n" +
			"(sleeping --retry-delay s between tries) and finally EXITS NON-ZERO when\n" +
			"orphans remain — so a destroy doesn't go color.Green leaving a NodeBalancer that\n" +
			"blocks the next apply's preflight.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIReapNodeBalancers(cliopts.Global, clusterID, region, attempts, retryDelay, requireEmpty)
		},
	}
	f := c.Flags()
	f.StringVar(&clusterID, "cluster-id", "", "scope to one cluster's CCM-tagged NodeBalancers (numeric LKE id)")
	f.StringVar(&region, "region", "", "narrow the account-wide sweep to one region (ignored with --cluster-id)")
	f.BoolVar(&requireEmpty, "require-empty", false, "verify the cluster's NodeBalancers are gone; retry then fail if orphans remain")
	f.IntVar(&attempts, "attempts", 1, "sweep+verify attempts before failing (only with --require-empty)")
	f.IntVar(&retryDelay, "retry-delay", 30, "seconds between --require-empty retries")
	return c
}
func ReapObjKeysCmd() *cobra.Command {
	var env string
	c := &cobra.Command{
		Use:   "reap-objkeys",
		Short: "delete a destroyed deployment's minted Linode obj-storage keys + in-cluster PAT (--yes to delete)",
		Long: "Teardown hygiene for the ACCOUNT-scoped Linode credentials a deployment mints\n" +
			"at bootstrap/rotation: the loki + harbor-registry Object Storage keys\n" +
			"(<objLabelPrefix>-loki-<env> / <objLabelPrefix>-harbor-registry-<env>) and the narrow in-cluster\n" +
			"PAT (llz-incluster-<objLabelPrefix>-<env>). These carry no cluster tag, so the cluster-liveness\n" +
			"sweeps (reap-volumes / reap-nodebalancers / `llz reap`) can't see them; a leaked\n" +
			"mint (failed run, failed grace-window revoke) accretes toward the account's\n" +
			"100-key / 100-PAT caps until a fresh mint 400s. Run on the destroy path with the\n" +
			"env being torn down. Exact-label match — never another env's creds, and never the\n" +
			"broad token this runs under (a different label). Reads LINODE_TOKEN; dry-run by\n" +
			"default, --yes to delete.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIReapObjKeys(cliopts.Global, env) },
	}
	c.Flags().StringVar(&env, "env", "", "deployment whose minted keys + PAT to reap (required)")
	return c
}

// Deleter returns a delete closure that honors --yes/--dry-run and tallies
// outcomes, plus a finalize func that prints the summary and errors if any delete
// failed. Mirrors the del/summary scaffolding in teardown.RunReap.
func Deleter(ctx context.Context, g cliopts.Opts, client *linode.Client) (func(path, desc string), func() error) {
	confirm := g.Yes && !g.DryRun
	if !confirm {
		fmt.Println("DRY-RUN — nothing will be deleted. Re-run with --yes to delete.")
	}
	deleted, failed := 0, 0
	del := func(path, desc string) {
		if !confirm {
			fmt.Printf("  would DELETE %s\n", desc)
			return
		}
		if err := client.DeleteResourcePath(ctx, path); err != nil {
			fmt.Fprintf(os.Stderr, "  DELETE %s FAILED: %v\n", desc, err)
			failed++
			return
		}
		fmt.Printf("  DELETE %s\n", desc)
		deleted++
	}
	fin := func() error {
		fmt.Printf("summary: deleted=%d failed=%d\n", deleted, failed)
		if failed > 0 {
			return fmt.Errorf("%d delete(s) failed", failed)
		}
		return nil
	}
	return del, fin
}
func runCIReapVolumes(g cliopts.Opts, env, region, volumeIDs, tagMustInclude string, waitDetach, attempts, retryDelay int, requireEmpty bool) error {
	if region == "" && volumeIDs == "" {
		return fmt.Errorf("--region and/or --volume-ids is required (refusing an unscoped Volume sweep)")
	}
	if requireEmpty && volumeIDs == "" {
		return fmt.Errorf("--require-empty needs --volume-ids (the precise set whose disappearance is verified)")
	}
	// Narrowed by the flags: same condition Deleter uses to decide whether a
	// DELETE is issued at all, so a dry run cannot reach the destructive verbs
	// even if that closure is wrong.
	client, ctx, err := capability.CloudFor(cloudBinding(g.Yes && !g.DryRun)).FromEnv()
	if err != nil {
		return err
	}

	// Detach is a precondition of the SWEEP, not of each retry: the retries
	// re-verify DELETION (countVolumesPresent), not detachment. Waiting inside the
	// loop made the worst case attempts × (waitDetach + retryDelay) — with the
	// destroy job's real flags that is 4 × (600s + 30s) = 42 MINUTES inside a
	// 30-minute job, so the loud terminal error this function exists to print
	// ("orphans remain; failing the destroy so they don't block the next apply's
	// preflight") was unreachable on exactly the path it was written for: the job
	// was killed first. Hoisted out, the ceiling is waitDetach + (attempts-1) ×
	// retryDelay = 11.5 minutes.
	//
	// The happy path is unchanged either way — waitVolumesDetached returns on its
	// first poll when the volumes are already detached.
	//
	// NOT ON A DRY RUN, and the reason is the capability fence rather than
	// tidiness. waitVolumesDetached POSTs /detach, and this client is built from
	// `cloudBinding(g.Yes && !g.DryRun)` — so on a dry run every POST is refused at
	// the TRANSPORT, the wait can never converge, and it burns the full --wait-detach
	// budget (600s on the destroy job's real flags) emitting a warning per volume
	// per poll before the sweep it precedes prints "would delete" anyway. A dry run
	// deletes nothing, so there is nothing for a detach to be a precondition of.
	if waitDetach > 0 && volumeIDs != "" {
		if shouldWaitForDetach(g) {
			waitVolumesDetached(ctx, client, volumeIDs, waitDetach)
		} else {
			fmt.Printf("DRY-RUN — would wait up to %ds for volume(s) %s to detach before sweeping.\n", waitDetach, volumeIDs)
		}
	}

	return sweepUntilEmpty(ctx, g, client, sweepOpts{
		cmd: "reap-volumes",
		banner: fmt.Sprintf("=== orphan Volumes (env=%q region=%q volume-ids=%q tag=%q, label prefixes %v, unattached)",
			env, region, volumeIDs, tagMustInclude, linode.VolumeLabelPrefixes(env)),
		singular:     "Volume",
		plural:       "Volumes",
		unit:         "tracked Volume(s)",
		goneMsg:      "verified: all tracked Volumes are gone.",
		attempts:     attempts,
		retryDelay:   retryDelay,
		requireEmpty: requireEmpty,
	}, func(del func(path, desc string)) error {
		// env is load-bearing, not cosmetic: VolumeLabelPrefixes(env) is what lets
		// the sweep see volumes the volume-labels reconciler has RENAMED. Left
		// empty (as it was) the predicate matches only `pvc-*`, so every relabeled
		// volume is invisible to the destroy-time sweep and leaks. Measured: 15
		// volumes survived the destroy of lke637974, then squatted their labels so
		// the NEXT cluster could not relabel 12 of its 17. `llz reap` already passed
		// env here; this path never did.
		return ReapVolumes(ctx, client, ReapOpts{Env: env, Region: region, VolumeIDs: volumeIDs, TagMustInclude: tagMustInclude}, del)
	}, func() (int, error) {
		return countVolumesPresent(ctx, client, volumeIDs)
	})
}
func runCIReapNodeBalancers(g cliopts.Opts, clusterID, region string, attempts, retryDelay int, requireEmpty bool) error {
	if clusterID != "" {
		if _, perr := strconv.ParseUint(clusterID, 10, 64); perr != nil {
			return fmt.Errorf("--cluster-id must be a numeric LKE cluster id (got %q)", clusterID)
		}
	}
	if requireEmpty && clusterID == "" {
		return fmt.Errorf("--require-empty needs --cluster-id (the scoped set whose disappearance is verified)")
	}
	// Narrowed by the flags: same condition Deleter uses to decide whether a
	// DELETE is issued at all, so a dry run cannot reach the destructive verbs
	// even if that closure is wrong.
	client, ctx, err := capability.CloudFor(cloudBinding(g.Yes && !g.DryRun)).FromEnv()
	if err != nil {
		return err
	}

	// Account-wide orphan sweep (cluster gone / 0-backend) — reuse reap's logic.
	// There's no precise scoped set to converge on, so this stays single-pass.
	if clusterID == "" {
		del, fin := Deleter(ctx, g, client)
		fmt.Printf("=== orphan NodeBalancers — account-wide (region=%q) ===\n", region)
		if err := ReapNodeBalancers(ctx, client, ReapOpts{Region: region}, del); err != nil {
			return err
		}
		return fin()
	}

	// Scoped sweep: only NodeBalancers carrying THIS cluster's CCM tag (lke<id>).
	return sweepUntilEmpty(ctx, g, client, sweepOpts{
		cmd: "reap-nodebalancers",
		banner: fmt.Sprintf("=== orphan NodeBalancers — scoped to cluster %s (lke_cluster.id or CCM tag lke%s)",
			clusterID, clusterID),
		singular:     "NodeBalancer",
		plural:       "NodeBalancers",
		unit:         "cluster NodeBalancer(s)",
		goneMsg:      "verified: the cluster's NodeBalancers are gone.",
		attempts:     attempts,
		retryDelay:   retryDelay,
		requireEmpty: requireEmpty,
	}, func(del func(path, desc string)) error {
		nbs, err := client.ListNodeBalancers(ctx)
		if err != nil {
			return fmt.Errorf("list NodeBalancers: %w", err)
		}
		matched := false
		for _, nb := range nbs {
			if !nbBelongsToCluster(nb, clusterID) {
				continue
			}
			id := linode.MapUint(nb, "id")
			del(fmt.Sprintf("/v4/nodebalancers/%d", id),
				fmt.Sprintf("nodebalancer %d (%s)", id, linode.MapString(nb, "label")))
			matched = true
		}
		if !matched {
			fmt.Println("  none matched")
		}
		return nil
	}, func() (int, error) {
		return countClusterNodeBalancersPresent(ctx, client, clusterID)
	})
}
func runCIReapObjKeys(g cliopts.Opts, env string) error {
	if env == "" {
		return fmt.Errorf("--env is required")
	}
	// Narrowed by the flags: same condition Deleter uses to decide whether a
	// DELETE is issued at all, so a dry run cannot reach the destructive verbs
	// even if that closure is wrong.
	client, ctx, err := capability.CloudFor(cloudBinding(g.Yes && !g.DryRun)).FromEnv()
	if err != nil {
		return err
	}
	prefix, err := clusterspec.LabelPrefixFor("reap-env-creds")
	if err != nil {
		return err
	}
	del, fin := Deleter(ctx, g, client)
	if err := ReapEnvObjKeys(ctx, client, prefix, env, del); err != nil {
		return err
	}
	if err := ReapEnvInclusterPAT(ctx, client, prefix, env, del); err != nil {
		return err
	}
	return fin()
}

// sweepUntilEmpty runs a delete sweep and, under --require-empty, re-verifies
// that the scoped set actually disappeared — retrying up to o.attempts times and
// ultimately failing loudly so orphans cannot block the next apply's preflight.
//
// sweep performs one pass, deleting through the supplied del closure; an error
// from it aborts the run, since a sweep that cannot enumerate has nothing to
// converge on. count reports how many scoped resources remain, returning the -1
// sentinel with an error when the list call fails. client is only used to build
// the Deleter closure, so a sweep that deletes nothing never dereferences it.
func sweepUntilEmpty(ctx context.Context, g cliopts.Opts, client *linode.Client, o sweepOpts,
	sweep func(del func(path, desc string)) error,
	count func() (int, error)) error {
	confirm := g.Yes && !g.DryRun
	if o.attempts < 1 {
		o.attempts = 1
	}

	var lastErr error
	remaining := -1
	for attempt := 1; attempt <= o.attempts; attempt++ {
		del, fin := Deleter(ctx, g, client)
		fmt.Printf("%s [attempt %d/%d] ===\n", o.banner, attempt, o.attempts)
		if err := sweep(del); err != nil {
			return err
		}
		lastErr = fin()

		// Without --require-empty (or in dry-run, where nothing was deleted)
		// keep the historical single-pass best-effort behavior.
		if !o.requireEmpty || !confirm {
			return lastErr
		}

		var verr error
		if remaining, verr = count(); verr != nil {
			fmt.Fprintf(os.Stderr, "verify %s: %v\n", o.plural, verr)
			remaining = -1
		} else if remaining == 0 {
			fmt.Println(o.goneMsg)
			return lastErr
		} else {
			fmt.Printf("verify: %d %s still present after attempt %d/%d.\n", remaining, o.unit, attempt, o.attempts)
		}
		if attempt < o.attempts {
			fmt.Printf("retrying the %s sweep in %ds...\n", o.singular, o.retryDelay)
			time.Sleep(time.Duration(o.retryDelay) * time.Second)
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("%s: %s %s still present after %d attempt(s) — orphans remain; failing the destroy so they don't block the next apply's preflight",
		o.cmd, firstNonEmpty(itoaOrUnknown(remaining), "some"), o.unit, o.attempts)
}

// countVolumesPresent reports how many of the tracked Volume ids still exist in
// the account (attached or not) — the post-sweep convergence check for
// --require-empty. A surviving id is a genuine orphan (or a delete still
// settling), so the caller retries and ultimately fails on a non-zero count.
func countVolumesPresent(ctx context.Context, client interface {
	ListVolumes(context.Context) ([]map[string]any, error)
}, volumeIDs string) (int, error) {
	tracked := map[string]bool{}
	for _, id := range strings.Fields(volumeIDs) {
		tracked[id] = true
	}
	vols, err := client.ListVolumes(ctx)
	if err != nil {
		return -1, err
	}
	n := 0
	for _, v := range vols {
		if tracked[linode.MapIDString(v)] {
			n++
		}
	}
	return n, nil
}

// waitVolumesDetached waits until none of the tracked Volume ids is still
// attached (linode_id non-null), bounded by waitSec, ASKING for the detach on
// each round rather than only watching for one. Best-effort: a list error or
// timeout just falls through to the sweep — VolumeIsCandidate skips anything
// still attached, so it is left for the next run rather than mis-deleted.
//
// Waiting alone is not enough, and that gap failed a destroy. Detachment is a
// side effect of the node Linodes being reaped after the cluster DELETE, so when
// that async reap stalls there is nothing left to wait FOR: on run 30643426633
// the LKE API 500'd during `tofu plan -destroy`, force-delete removed the
// cluster object, and 16 of 17 tracked Volumes then sat attached across all 59
// polls of the full 600s window — flat, never draining — so the sweep could
// delete only the one Volume that happened to already be detached and the
// destroy failed with 16 orphans. An explicit detach does not depend on the node
// reap making progress; it is also a no-op (400/404) on the Volumes that already
// detached, so the happy path is unchanged.
//
// Scope note: only the destroy job passes --wait-detach, and it passes the ids
// `teardown-capture` attributed to the cluster being destroyed (lke<id> tag, or
// attachment to one of its own nodes). Detaching those is the whole point of the
// step; no live peer's Volume can be in the set.
// shouldWaitForDetach reports whether the detach wait can run at all.
//
// `g.Yes && !g.DryRun` is the same expression the client is built from, so the
// wait runs exactly when the transport can carry it: waitVolumesDetached POSTs
// /detach, and on a dry run that client holds the READ binding, which refuses
// every POST. Without this the wait burned its whole budget (600s on the destroy
// job's real flags) emitting a warning per volume per poll before the sweep it
// precedes printed "would delete" anyway.
//
// A NAMED PREDICATE rather than an inline conjunction, so both arms are testable
// — the inline form had no test on either, and `if false` there silently
// resurrects the 16-orphan-volume incident the wait exists for.
func shouldWaitForDetach(g cliopts.Opts) bool { return g.Yes && !g.DryRun }

func waitVolumesDetached(ctx context.Context, client interface {
	ListVolumes(context.Context) ([]map[string]any, error)
	DetachVolume(context.Context, uint64) error
}, volumeIDs string, waitSec int) {
	tracked := map[string]bool{}
	for _, id := range strings.Fields(volumeIDs) {
		tracked[id] = true
	}
	deadline := time.Now().Add(time.Duration(waitSec) * time.Second)
	for attempt := 1; ; attempt++ {
		still := -1 // unknown on a list error
		var attached []map[string]any
		if vols, err := client.ListVolumes(ctx); err == nil {
			still = 0
			for _, v := range vols {
				if tracked[linode.MapIDString(v)] && !linode.VolumeLinodeIDNull(v) {
					still++
					attached = append(attached, v)
				}
			}
		}
		if still == 0 {
			fmt.Println("all tracked Volumes are detached.")
			return
		}
		if time.Now().After(deadline) {
			fmt.Printf("tracked Volumes still attached after %ds — sweeping what is detached; the rest is left for the next run.\n", waitSec)
			return
		}
		if still < 0 {
			fmt.Printf("tracked Volumes still attached: unknown (list error, attempt %d)\n", attempt)
		} else {
			fmt.Printf("tracked Volumes still attached: %d (attempt %d) — requesting detach\n", still, attempt)
		}
		for _, v := range attached {
			id := linode.MapUint(v, "id")
			if id == 0 {
				continue
			}
			if err := client.DetachVolume(ctx, id); err != nil {
				fmt.Fprintf(os.Stderr, "::warning::detach Volume %d (%s) failed (%v) — retrying on the next poll.\n",
					id, linode.MapString(v, "label"), err)
			}
		}
		time.Sleep(volumeDetachPollInterval)
	}
}

// countClusterNodeBalancersPresent reports how many NodeBalancers still carry
// the cluster's CCM tag (lke<id>) — the post-sweep convergence check for
// reap-nodebalancers --require-empty.
func countClusterNodeBalancersPresent(ctx context.Context, client interface {
	ListNodeBalancers(context.Context) ([]map[string]any, error)
}, clusterID string) (int, error) {
	nbs, err := client.ListNodeBalancers(ctx)
	if err != nil {
		return -1, err
	}
	n := 0
	for _, nb := range nbs {
		if nbBelongsToCluster(nb, clusterID) {
			n++
		}
	}
	return n, nil
}
