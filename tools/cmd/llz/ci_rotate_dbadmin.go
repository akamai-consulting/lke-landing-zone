package main

// ci_rotate_dbadmin.go implements `llz ci rotate-db-admin` — admin-credential
// rotation for the Linode Managed PostgreSQL clusters the `databases` root
// provisions (docs/designs/shared-managed-postgres.md).
//
// This closes the gap docs/secrets.md called out: every cluster's admin
// credential was seeded once by `llz ci seed-db-admin` and then held forever.
// The DB admin is the highest-value credential in the deployment — it owns every
// logical database Crossplane carves out — and it had no rotation path at all.
//
// ── Why this rotator is shaped differently from the others ───────────────────
//
// Every other rotator in this repo is MINT → VERIFY → SWAP → DRAIN: mint a
// second credential, prove it works, publish it, and only then revoke the first.
// A bad mint can never break a consumer because the old credential is still live
// the whole time.
//
// That is impossible here. Linode fixes the admin user to `akmadmin` and offers
// exactly one mutation — POST .../credentials/reset — which regenerates the
// password IN PLACE. There is no second credential, no overlap window, and no
// way to choose the new password. The old one dies the instant the platform
// applies the reset, before anything has been verified or persisted.
//
// So the invariant flips from "never break a consumer" (unattainable) to "never
// LOSE the new credential". Consequences, all deliberate:
//
//   - --apply arms the mutation; without it this is a pure report. The default
//     is dry-run because the mutation is irreversible and unattended.
//   - The OpenBao write is the ONLY thing standing between a reset and a
//     locked-out database, so a failed write is a loud, specific error carrying
//     the exact command to re-read the credential from Linode — never the
//     credential itself, which would put a live admin password in a CI log.
//   - Rotation is SEQUENTIAL across clusters and stops at the first failure. A
//     loop that kept going would turn one lost credential into several.
//
// ── The Terraform-state hazard ───────────────────────────────────────────────
//
// `llz ci seed-db-admin` reconciles OpenBao TOWARD Terraform state: if the
// stored password differs from the state's `connections` output, it overwrites
// OpenBao. That is correct for its own job (a recreated cluster must not strand
// consumers on a dead credential) and actively dangerous here — after an
// out-of-band reset, state holds the OLD password, so a seed run against
// unrefreshed state would push the dead credential back over the live one.
//
// The rotator therefore refreshes state itself after a successful rotation, and
// treats a refresh failure as a FAILED RUN even though the credential is safely
// in OpenBao — because the deployment is left in the one configuration where a
// routine seed silently undoes the rotation. See refreshTFState.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/spf13/cobra"
)

const (
	// dbAdminRotateAfterDays is the default rotation threshold. Below the 90d
	// policy the credential inventory alerts on, so a scheduled monthly run has
	// two chances to land before LLZCredentialRotationOverdue would fire.
	dbAdminRotateAfterDays = 80
	// dbAdminActiveTimeout bounds the wait for a cluster to return to `active`
	// after a reset. Generous: the Aiven-backed platform applies a credential
	// reset in seconds, but the cluster may already be mid-maintenance.
	dbAdminActiveTimeout = 10 * time.Minute
	dbAdminPollInterval  = 10 * time.Second
)

// Seams for tests.
var (
	dbAdminNow          = func() time.Time { return time.Now() }
	dbAdminLinodeClient = func(token string) dbAdminAPI { return linode.NewClient(token, 60*time.Second) }
	dbAdminSleep        = func(d time.Duration) { time.Sleep(d) }
	// tfRefreshRunFn runs the state refresh in the databases root. Seamed so the
	// tests exercise the post-rotation hazard handling without a real backend.
	tfRefreshRunFn = func() error {
		cmd := exec.Command("terraform", "apply", "-refresh-only", "-auto-approve", "-input=false")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
)

// dbAdminAPI is the slice of the Linode client the rotator needs.
type dbAdminAPI interface {
	PostgresInstance(ctx context.Context, id uint64) (map[string]any, error)
	PostgresCredentials(ctx context.Context, id uint64) (linode.DBCredentials, error)
	ResetPostgresCredentials(ctx context.Context, id uint64) error
}

// dbAdminTarget is one cluster considered for rotation.
type dbAdminTarget struct {
	name    string // spec key — also the OpenBao path discriminator
	id      uint64 // Linode Managed Database id
	path    string // secret/platform/db-admin/<name>
	ageDays int    // -1 when rotated_at is absent/unparseable
	due     bool
}

func ciRotateDBAdminCmd() *cobra.Command {
	var region string
	var apply bool
	var afterDays int
	c := &cobra.Command{
		Use:   "rotate-db-admin",
		Short: "rotate the admin password on each Managed Postgres cluster (due-based)",
		Long: "Rotates the `akmadmin` password on every Linode Managed PostgreSQL cluster in\n" +
			"the deployment whose secret/platform/db-admin/<name> credential is older than\n" +
			"--rotate-after-days, and writes the replacement back to OpenBao.\n\n" +
			"REPORT-ONLY unless --apply is passed. The Linode API offers only an in-place\n" +
			"credential RESET — there is no second credential to verify before swapping, and\n" +
			"the old password dies immediately — so the mutation is irreversible and is not\n" +
			"armed by default.\n\n" +
			"After a successful rotation the command refreshes Terraform state, because\n" +
			"`llz ci seed-db-admin` reconciles OpenBao toward state and would otherwise push\n" +
			"the pre-rotation password back over the live one.\n\n" +
			"Run with the databases root as the working directory. Reads LINODE_TOKEN and\n" +
			"OPENBAO_ROOT_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIRotateDBAdmin(region, apply, afterDays)
		},
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) being rotated — labels the run summary (required)")
	c.Flags().BoolVar(&apply, "apply", false, "arm the rotation; without it the command only reports what is due")
	c.Flags().IntVar(&afterDays, "rotate-after-days", dbAdminRotateAfterDays, "rotate a credential older than this many days")
	return c
}

func runCIRotateDBAdmin(region string, apply bool, afterDays int) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	if afterDays <= 0 {
		return fmt.Errorf("--rotate-after-days must be positive, got %d", afterDays)
	}

	targets, err := dbAdminTargets(afterDays)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Println("rotate-db-admin: no database clusters in this deployment — nothing to rotate.")
		return nil
	}

	var due []dbAdminTarget
	for _, t := range targets {
		if t.due {
			due = append(due, t)
		}
	}
	for _, t := range targets {
		fmt.Printf("%s: %s (%s)\n", t.name, dbAdminAgeText(t), map[bool]string{true: "DUE", false: "within window"}[t.due])
	}
	if len(due) == 0 {
		fmt.Printf("rotate-db-admin: nothing due (threshold %dd).\n", afterDays)
		return appendGHAFile("GITHUB_STEP_SUMMARY", dbAdminRotateSummary(region, apply, afterDays, targets, nil)...)
	}
	if !apply {
		fmt.Printf("rotate-db-admin: %d credential(s) due — NOT rotating (--apply not set).\n", len(due))
		return appendGHAFile("GITHUB_STEP_SUMMARY", dbAdminRotateSummary(region, apply, afterDays, targets, nil)...)
	}

	token := os.Getenv("LINODE_TOKEN")
	if token == "" {
		return fmt.Errorf("LINODE_TOKEN must be set to rotate database admin credentials")
	}
	api := dbAdminLinodeClient(token)
	ctx := context.Background()

	var rotated []string
	for _, t := range due {
		// SEQUENTIAL and fail-fast on purpose: each reset is irreversible, so a
		// failure that might indicate a lost credential must not be followed by
		// another reset on a different cluster.
		if err := rotateOneDBAdmin(ctx, api, t); err != nil {
			return err
		}
		rotated = append(rotated, t.name)
		fmt.Printf("%s: rotated; %s updated.\n", t.name, t.path)
	}

	// The rotation is already durable in OpenBao at this point. This is about the
	// NEXT seed run, not this one — see the file header.
	if err := refreshTFState(rotated); err != nil {
		return err
	}
	return appendGHAFile("GITHUB_STEP_SUMMARY", dbAdminRotateSummary(region, apply, afterDays, targets, rotated)...)
}

// rotateOneDBAdmin resets one cluster's admin password and persists it.
func rotateOneDBAdmin(ctx context.Context, api dbAdminAPI, t dbAdminTarget) error {
	// Read the whole existing secret FIRST. A KV v2 put replaces the entire
	// secret, so the fields a credential reset does not change (endpoint, port,
	// ca, sslmode) have to be carried across — dropping them would leave
	// consumers with a password and no host to use it against.
	carried, err := readDBAdminCarriedFields(t.path, t.name)
	if err != nil {
		return err
	}
	oldPassword, verdict := baoKVGetFieldOK(t.path, "password")
	if verdict == baoReadUnknown {
		return errBaoReadUnknown(t.path, "password", "rotate the admin credential for database cluster "+t.name)
	}

	if err := api.ResetPostgresCredentials(ctx, t.id); err != nil {
		// Nothing has changed yet — the reset was refused, not half-applied.
		return fmt.Errorf("rotate-db-admin: %s: reset credentials: %w", t.name, err)
	}
	// PAST THE POINT OF NO RETURN. Every error below must tell the operator how
	// to recover the credential by hand.
	if err := waitDBActive(ctx, api, t); err != nil {
		return dbAdminLostCredentialErr(t, err)
	}
	creds, err := api.PostgresCredentials(ctx, t.id)
	if err != nil {
		return dbAdminLostCredentialErr(t, fmt.Errorf("re-read credentials: %w", err))
	}
	if creds.Password == "" {
		return dbAdminLostCredentialErr(t, fmt.Errorf("the API returned an empty password after the reset"))
	}
	if oldPassword != "" && creds.Password == oldPassword {
		// The reset was accepted and the cluster went active, but the password is
		// unchanged. Persisting is harmless (it is the live credential either way);
		// what must not happen is REPORTING a rotation that did not occur, because
		// the rotated_at stamp would then hide a stale credential for another 80d.
		return dbAdminLostCredentialErr(t, fmt.Errorf("the password is unchanged after the reset — the rotation did not take effect"))
	}
	maskGHA(creds.Password)

	fields := carried
	fields["username"] = creds.Username
	fields["password"] = creds.Password
	fields["rotated_at"] = strconv.FormatInt(dbAdminNow().Unix(), 10)
	if err := baoKVPutFn(t.path, fields); err != nil {
		return dbAdminLostCredentialErr(t, fmt.Errorf("write %s: %w", t.path, err))
	}
	return nil
}

// dbAdminCarriedFields are the members of the admin secret a credential reset
// does NOT change, and which therefore have to survive the KV v2 whole-secret
// replace.
var dbAdminCarriedFields = []string{"endpoint", "port", "ca", "sslmode"}

// readDBAdminCarriedFields reads those fields from the existing secret. Every one
// is required: rotating a path that was never seeded would produce a secret with
// a password and nothing to connect to, which no consumer can use and nothing
// else would flag.
func readDBAdminCarriedFields(path, name string) (map[string]string, error) {
	out := make(map[string]string, len(dbAdminCarriedFields)+3)
	for _, f := range dbAdminCarriedFields {
		v, verdict := baoKVGetFieldOK(path, f)
		switch verdict {
		case baoReadUnknown:
			return nil, errBaoReadUnknown(path, f, "rotate the admin credential for database cluster "+name)
		case baoReadAbsent:
			if f == "sslmode" {
				// Seeded before sslmode was written, or hand-created. Re-assert the
				// floor rather than refusing: omitting it lets a client negotiate
				// plaintext where the server permits it.
				out[f] = dbAdminSSLMode
				continue
			}
			return nil, fmt.Errorf("rotate-db-admin: %s: %s has no %q field — run `llz ci seed-db-admin` first; "+
				"rotating an unseeded path would store a password with no endpoint to use it against", name, path, f)
		}
		out[f] = v
	}
	return out, nil
}

// waitDBActive polls until the cluster reports `active` again. A reset is
// asynchronous, and credentials read while the cluster is still `updating` can
// be the pre-reset pair.
func waitDBActive(ctx context.Context, api dbAdminAPI, t dbAdminTarget) error {
	deadline := dbAdminNow().Add(dbAdminActiveTimeout)
	var last string
	for {
		inst, err := api.PostgresInstance(ctx, t.id)
		if err != nil {
			return fmt.Errorf("poll cluster status: %w", err)
		}
		status, _ := inst["status"].(string)
		last = status
		if status == "active" {
			return nil
		}
		if !dbAdminNow().Before(deadline) {
			return fmt.Errorf("cluster did not return to `active` within %s (last status %q)", dbAdminActiveTimeout, last)
		}
		dbAdminSleep(dbAdminPollInterval)
	}
}

// dbAdminLostCredentialErr is the error for any failure AFTER the reset landed.
// It is deliberately verbose: at this moment the live admin password exists only
// inside Linode, and the operator reading this log is the recovery path. It
// prints the command, never the credential.
func dbAdminLostCredentialErr(t dbAdminTarget, cause error) error {
	return fmt.Errorf("rotate-db-admin: %s: THE ADMIN PASSWORD WAS RESET BUT NOT PERSISTED: %w\n\n"+
		"  The credential in %s is now DEAD, and the live one exists only in Linode.\n"+
		"  Recover it before anything else — every consumer of this cluster is broken until you do:\n\n"+
		"    linode-cli databases postgresql-creds-view %d\n"+
		"    llz openbao set platform/db-admin/%s password=<password> username=<username> rotated_at=$(date +%%s)\n\n"+
		"  (Re-running this command will NOT fix it: the stored credential is stale, so the\n"+
		"  cluster would simply be reset again and the same window reopened.)",
		t.name, cause, t.path, t.id, t.name)
}

// refreshTFState brings the databases root's state back in line with the live
// credentials. See the file header for why a failure here fails the run.
func refreshTFState(rotated []string) error {
	if err := tfRefreshRunFn(); err != nil {
		return fmt.Errorf("rotate-db-admin: rotated %s successfully, but the Terraform state refresh FAILED: %w\n\n"+
			"  OpenBao holds the new credential and consumers are fine. The hazard is the NEXT\n"+
			"  `llz ci seed-db-admin` run: state still holds the pre-rotation password, and the seed\n"+
			"  reconciles OpenBao toward state — so it would overwrite the live credential with the\n"+
			"  dead one and strand every consumer.\n\n"+
			"  Run `terraform apply -refresh-only` in the databases root before the next apply.",
			strings.Join(rotated, ", "), err)
	}
	fmt.Println("rotate-db-admin: Terraform state refreshed — seed-db-admin will not roll the rotation back.")
	return nil
}

// dbAdminTargets builds the candidate list from the databases root's
// `database_ids` output, joined against each path's rotated_at stamp.
func dbAdminTargets(afterDays int) ([]dbAdminTarget, error) {
	raw, err := tfOutputRunFn()
	if err != nil {
		return nil, fmt.Errorf("rotate-db-admin: terraform output -json: %w", err)
	}
	// allowMissing mirrors seed-db-admin: a state predating the databases root
	// has no such output, which is "no clusters", not a broken state.
	blob, err := tfOutputValue(raw, "database_ids", true, true)
	if err != nil {
		return nil, err
	}
	ids, err := parseDBIDs(blob)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ids))
	for n := range ids {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic; Go map order is randomized

	now := dbAdminNow()
	out := make([]dbAdminTarget, 0, len(names))
	for _, name := range names {
		t := dbAdminTarget{name: name, id: ids[name], path: dbAdminSeedRoot + name, ageDays: -1}
		stamp, verdict := baoKVGetFieldOK(t.path, "rotated_at")
		if verdict == baoReadUnknown {
			return nil, errBaoReadUnknown(t.path, "rotated_at", "decide whether database cluster "+name+" is due for rotation")
		}
		if secs, err := strconv.ParseInt(strings.TrimSpace(stamp), 10, 64); err == nil && secs > 0 {
			t.ageDays = int(now.Sub(time.Unix(secs, 0)).Hours() / 24)
			t.due = t.ageDays >= afterDays
		} else {
			// No parseable stamp: either never seeded by a version that stamped, or
			// hand-written. Treat as DUE — an unknown-age admin credential is the
			// case rotation exists for, and rotateOneDBAdmin still refuses if the
			// path is not properly seeded.
			t.due = true
		}
		out = append(out, t)
	}
	return out, nil
}

// parseDBIDs decodes the `database_ids` output (cluster name → Linode id).
func parseDBIDs(blob string) (map[string]uint64, error) {
	trimmed := strings.TrimSpace(blob)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var raw map[string]json.Number
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, fmt.Errorf("rotate-db-admin: parse the database_ids output: %w", err)
	}
	out := make(map[string]uint64, len(raw))
	for name, n := range raw {
		id, err := strconv.ParseUint(n.String(), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("rotate-db-admin: cluster %q has a non-numeric database id %q", name, n.String())
		}
		out[name] = id
	}
	return out, nil
}

func dbAdminAgeText(t dbAdminTarget) string {
	if t.ageDays < 0 {
		return "age unknown (no rotated_at stamp)"
	}
	return fmt.Sprintf("%dd old", t.ageDays)
}

// dbAdminRotateSummary renders the step summary. Split out so the wording is
// asserted directly rather than through a $GITHUB_STEP_SUMMARY round-trip.
func dbAdminRotateSummary(region string, apply bool, afterDays int, targets []dbAdminTarget, rotated []string) []string {
	lines := []string{fmt.Sprintf("### Database admin rotation (%s)", region), "",
		fmt.Sprintf("Threshold: **%dd**. Mode: **%s**.", afterDays, map[bool]string{true: "apply", false: "report-only"}[apply]), ""}
	lines = append(lines, "| Cluster | Credential age | Due |", "|---|---|---|")
	for _, t := range targets {
		lines = append(lines, fmt.Sprintf("| `%s` | %s | %s |", t.name, dbAdminAgeText(t),
			map[bool]string{true: "yes", false: "no"}[t.due]))
	}
	lines = append(lines, "")
	switch {
	case len(rotated) > 0:
		lines = append(lines, fmt.Sprintf("**Rotated:** `%s`", strings.Join(rotated, "`, `")),
			"", "Terraform state was refreshed so `llz ci seed-db-admin` cannot roll the rotation back.")
	case !apply:
		lines = append(lines, "_Report-only — re-run with `--apply` to rotate. The Linode API resets the admin password in place, so the rotation is irreversible and the old password stops working immediately._")
	default:
		lines = append(lines, "_Nothing was due._")
	}
	return lines
}
