package database

// ci_seed_dbadmin.go implements `llz ci seed-db-admin` — the OpenBao half of the
// `databases` root (docs/designs/shared-managed-postgres.md). The root provisions
// 0-n VPC-attached Linode Managed PostgreSQL clusters; this copies each cluster's
// admin connection from Terraform state into secret/infra/db-admin/<name>, so
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
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/tofudriver"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
)

// dbAdminSeedRoot is the KV v2 prefix each cluster's admin credential lands under.
// The cluster NAME is the discriminator, not the username: Linode fixes the admin
// user to `akmadmin` on every Managed Postgres, so with 0-n clusters the username
// no longer says which cluster you are holding.
const dbAdminSeedRoot = "secret/infra/db-admin/"

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

func RunSeedDBAdmin(region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	raw, err := tofudriver.OutputRunFn()
	if err != nil {
		return fmt.Errorf("seed-db-admin: terraform output -json: %w", err)
	}
	// allowMissing: a state predating the databases root has no `connections`
	// output at all. That is the same "nothing to seed" as an empty map, and it
	// must not fail a bootstrap that never declared a
	blob, err := tofudriver.OutputValue(raw, "connections", true, true)
	if err != nil {
		return err
	}
	conns, err := parseDBConnections(blob)
	if err != nil {
		return err
	}
	if len(conns) == 0 {
		// EMPTY IS ONLY OK IF NOTHING WAS DECLARED, and this is the whole reason the
		// check exists. `terraform output connections` on a databases root that has
		// not been applied yet is indistinguishable, at this line, from one applied
		// against a deployment that declares no clusters — both are an empty map.
		//
		// The delivered pipeline used to make that difference reachable: on
		// `module=all`, bootstrap-openbao did not depend on apply-databases, so it
		// ran WHILE Postgres was provisioning. The seed steps are gated on
		// `llz ci db-declared`, which reads the SPEC, so they ran; this line then
		// reported "nothing to seed" and exited 0. OpenBao finished the bootstrap
		// with no db-admin credential, rotate-on-create had nothing to rotate, and
		// the PROVISIONING password Terraform had just written into state stayed
		// live — with the run green from end to end. llz-terraform.yml now orders
		// the two jobs; this refuses the contradiction from the other side, which is
		// the half that also covers a hand-run seed and a standalone
		// llz-bootstrap-openbao.yml dispatch.
		//
		// It names what IS there (the tfvars it read) rather than only what is
		// missing, because "declared" and "applied" are different repairs.
		if d := dbDeclares(region); d.Declared || !d.Answered {
			why := fmt.Sprintf("%s declares database clusters", d.Path)
			if !d.Answered {
				why = fmt.Sprintf("%s could not be read, so an empty output cannot be confirmed harmless", d.Path)
			}
			cwd, _ := os.Getwd()
			return fmt.Errorf("seed-db-admin: %s, but `terraform output -json connections` (run in %s) is empty — "+
				"the databases root has not been applied for %q yet, so there is nothing to copy into OpenBao. "+
				"Run the databases apply first (Terraform Infrastructure → action=apply, module=databases or all) "+
				"and re-run the bootstrap. Exiting 0 here would leave OpenBao with no db-admin credential while the "+
				"PROVISIONING password stayed live in Terraform state", why, cwd, region)
		}
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

		// Compare on the cluster's IDENTITY (endpoint), never on the password.
		//
		// This used to compare passwords and rewrite OpenBao whenever it differed
		// from Terraform state. That made STATE the source of truth for the
		// credential, which is wrong once anything rotates it: after `llz ci
		// rotate-db-admin` resets the password out of band, state holds the old
		// one, and a seed run would push that dead credential back over the live
		// one and strand every consumer. (The rotator used to defend against this
		// by refreshing state; comparing on identity removes the hazard at its
		// source instead of racing it.)
		//
		// OpenBao is now authoritative for the password. Seeding is for two cases
		// only: the path does not exist yet, or it belongs to a DIFFERENT cluster.
		// The endpoint carries the cluster id, so a recreated cluster changes it —
		// which is the case the old password comparison was really there to catch,
		// and it catches it without ever second-guessing a live credential.
		//
		// An unreadable path is NOT an absent one — writing over a live credential
		// we failed to read is indistinguishable from a successful rotation, so
		// fail closed.
		existingEndpoint, verdict := capability.For(namedBinding("seed-admin")).Secrets.Get(path, "endpoint")
		if verdict == baoread.Unknown {
			return baoread.ErrReadUnknown(path, "endpoint", "seed the admin credential for database cluster "+name)
		}
		switch {
		case existingEndpoint == "":
			fields["rotated_at"] = stamp
			if err := capability.For(namedBinding("seed-admin")).Custodian.Put(path, fields); err != nil {
				return fmt.Errorf("seed %s: %w", path, err)
			}
			seeded = append(seeded, name)
			fmt.Printf("%s: seeded %s (%s).\n", name, path, c.Endpoint)
		case existingEndpoint != c.Endpoint:
			fields["rotated_at"] = stamp
			if err := capability.For(namedBinding("seed-admin")).Custodian.Put(path, fields); err != nil {
				return fmt.Errorf("update %s: %w", path, err)
			}
			updated = append(updated, name)
			fmt.Printf("%s: %s pointed at a different cluster (%s -> %s) — re-seeded from the current one.\n",
				name, path, existingEndpoint, c.Endpoint)
		default:
			// Same cluster. Whatever password OpenBao holds is the live one (the
			// rotator owns it from here), so leave the secret completely alone.
			unchanged = append(unchanged, name)
			fmt.Printf("%s: %s already points at this cluster — leaving its credential alone.\n", name, path)
		}
	}

	return ghaout.Append("GITHUB_STEP_SUMMARY", dbAdminSummary(region, seeded, updated, unchanged)...)
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
