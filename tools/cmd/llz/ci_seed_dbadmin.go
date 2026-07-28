package main

// ci_seed_dbadmin.go implements `llz ci seed-db-admin` — the OpenBao half of the
// `databases` root (docs/designs/shared-managed-postgres.md). The root provisions
// 0-n VPC-attached Linode Managed PostgreSQL clusters; this copies each cluster's
// admin connection from Terraform state into secret/platform/db-admin/<name>, so
// ESO can publish it to the consumer that carves per-app logical databases out of
// the cluster (Crossplane provider-sql, in the first instance).
//
// ONE read of ONE output. The root exposes a single `connections` map keyed by
// cluster name, each entry {endpoint, port, username, password, ca}, rather than
// five parallel per-field maps. That is not a convenience: with 0-n clusters,
// reading fields separately makes it possible to pair one cluster's endpoint with
// another's password, and the resulting secret is wrong in a way nothing detects
// until an app connects to the wrong database with credentials that happen to work.
//
// Runs with the databases root as its working directory (the workflow sets it,
// same as the object-storage summary step), so it reads the state the apply just
// wrote.
//
// Env: OPENBAO_ROOT_TOKEN (seed), GITHUB_STEP_SUMMARY.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// dbAdminSeedRoot is the KV v2 prefix each cluster's admin credential lands under.
// The cluster NAME is the discriminator, not the username: Linode fixes the admin
// user to `akmadmin` on every Managed Postgres, so with 0-n clusters the username
// no longer says which cluster you are holding.
const dbAdminSeedRoot = "secret/platform/db-admin/"

// dbAdminSSLMode is seeded alongside the connection because the consumer must not
// have to know it. `require` is the floor: the endpoint is VPC-internal but the
// Aiven-backed platform still terminates TLS, and a client that omits sslmode
// negotiates plaintext where the server permits it. `verify-full` additionally
// needs the CA below, decoded.
const dbAdminSSLMode = "require"

// dbConnection is one cluster's admin connection as the root's `connections`
// output renders it. Port is json.Number because Terraform emits it unquoted and
// the seed writes it back as a string field.
type dbConnection struct {
	Endpoint string      `json:"endpoint"`
	Port     json.Number `json:"port"`
	Username string      `json:"username"`
	Password string      `json:"password"`
	CA       string      `json:"ca"`
}

// seedDBAdminNow is a seam for tests (the rotated_at stamp).
var seedDBAdminNow = func() time.Time { return time.Now() }

func ciSeedDBAdminCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "seed-db-admin",
		Short: "seed each Managed Postgres cluster's admin connection into OpenBao",
		Long: "Reads the `databases` root's single `connections` output (a map keyed by\n" +
			"cluster name) and writes one secret/platform/db-admin/<name> per entry —\n" +
			"endpoint, port, username, password, ca, sslmode. Reading ONE output keeps a\n" +
			"cluster's endpoint and password from ever being paired across clusters.\n\n" +
			"A no-op when the map is empty, so it runs unconditionally on a deployment\n" +
			"that declared no databases. Idempotent: a path whose fields already match is\n" +
			"left alone; one that differs is UPDATED (a recreated cluster changes the\n" +
			"password, and a stale credential would strand every consumer). Paths are\n" +
			"never deleted — removing a cluster from the spec leaves its credential for\n" +
			"an operator to reap, because the cluster usually still exists at that point\n" +
			"and this is the only way back into it.\n\n" +
			"Run with the databases root as the working directory. Reads\n" +
			"OPENBAO_ROOT_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCISeedDBAdmin(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) being seeded — labels the run summary (required)")
	return c
}

func runCISeedDBAdmin(region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	raw, err := tfOutputRunFn()
	if err != nil {
		return fmt.Errorf("seed-db-admin: terraform output -json: %w", err)
	}
	// allowMissing: a state predating the databases root has no `connections`
	// output at all. That is the same "nothing to seed" as an empty map, and it
	// must not fail a bootstrap that never declared a database.
	blob, err := tfOutputValue(raw, "connections", true, true)
	if err != nil {
		return err
	}
	conns, err := parseDBConnections(blob)
	if err != nil {
		return err
	}
	if len(conns) == 0 {
		fmt.Println("seed-db-admin: no database clusters in this deployment — nothing to seed.")
		return nil
	}

	names := make([]string, 0, len(conns))
	for n := range conns {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic output; Go map order is randomized

	stamp := strconv.FormatInt(seedDBAdminNow().Unix(), 10)
	var seeded, updated, unchanged []string
	for _, name := range names {
		c := conns[name]
		if c.Endpoint == "" || c.Password == "" {
			// A half-populated entry means the apply did not finish for that
			// cluster. Seeding it would publish a credential that cannot connect.
			return fmt.Errorf("seed-db-admin: cluster %q has an empty endpoint or password in the connections output — the apply did not complete", name)
		}
		maskGHA(c.Password)
		path := dbAdminSeedRoot + name

		fields := map[string]string{
			"endpoint": c.Endpoint,
			"port":     c.Port.String(),
			"username": c.Username,
			"password": c.Password,
			"ca":       c.CA,
			"sslmode":  dbAdminSSLMode,
		}

		// Compare on the password alone: it is the field that changes when a
		// cluster is recreated, and reading it is one call. An unreadable path is
		// NOT an absent one — writing over a live credential we failed to read
		// would be indistinguishable from a successful rotation, so fail closed.
		existing, verdict := baoKVGetFieldOK(path, "password")
		if verdict == baoReadUnknown {
			return errBaoReadUnknown(path, "password", "seed the admin credential for database cluster "+name)
		}
		switch {
		case existing == "":
			fields["rotated_at"] = stamp
			if err := baoKVPutFn(path, fields); err != nil {
				return fmt.Errorf("seed %s: %w", path, err)
			}
			seeded = append(seeded, name)
			fmt.Printf("%s: seeded %s (%s).\n", name, path, c.Endpoint)
		case existing != c.Password:
			fields["rotated_at"] = stamp
			if err := baoKVPutFn(path, fields); err != nil {
				return fmt.Errorf("update %s: %w", path, err)
			}
			updated = append(updated, name)
			fmt.Printf("%s: %s held a different password — updated to the current cluster credential.\n", name, path)
		default:
			unchanged = append(unchanged, name)
			fmt.Printf("%s: %s already current — skipping.\n", name, path)
		}
	}

	return appendGHAFile("GITHUB_STEP_SUMMARY", dbAdminSummary(region, seeded, updated, unchanged)...)
}

// parseDBConnections decodes the `connections` output blob. An absent output
// (allowMissing) arrives as "" and means no clusters, not a malformed state.
func parseDBConnections(blob string) (map[string]dbConnection, error) {
	trimmed := strings.TrimSpace(blob)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var conns map[string]dbConnection
	if err := json.Unmarshal([]byte(trimmed), &conns); err != nil {
		return nil, fmt.Errorf("seed-db-admin: parse the connections output: %w", err)
	}
	return conns, nil
}

// dbAdminSummary renders the step summary. Split out so the wording is asserted
// directly rather than through a $GITHUB_STEP_SUMMARY round-trip.
func dbAdminSummary(region string, seeded, updated, unchanged []string) []string {
	lines := []string{fmt.Sprintf("### Database admin credentials (%s)", region), ""}
	add := func(label string, names []string) {
		if len(names) > 0 {
			lines = append(lines, fmt.Sprintf("- **%s:** `%s`", label, strings.Join(names, "`, `")))
		}
	}
	add("Seeded", seeded)
	add("Updated", updated)
	add("Already current", unchanged)
	lines = append(lines, "",
		fmt.Sprintf("Each cluster's admin connection is at `%s<name>` (endpoint, port, username, password, ca, sslmode).", dbAdminSeedRoot),
		"`ca` is base64-encoded — decode it before writing a trust file.")
	return lines
}
