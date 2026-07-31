package main

// ci_assert_database.go implements `llz ci assert-database` — the gate that each
// declared Managed Postgres accepts the admin credential the platform holds.
//
// `db-declared` and `db-summary` report what the spec asks for. `seed-db-admin`
// writes the credential into OpenBao. Nothing checked that the credential still
// WORKS, and that is the one thing that breaks here.
//
// `rotate-db-admin` resets the password IN PLACE with no overlap window — which
// is exactly why it is classed on-demand and kept off a cron
// (docs/designs/shared-managed-postgres.md). So the failure mode is not a dead
// endpoint. It is a live endpoint that answers every TCP connect cheerfully and
// rejects the credential every consumer is carrying: a rotation that half
// landed, an OpenBao copy that was never re-seeded, or a reset performed outside
// the tooling. A reachability check passes through all of that.
//
// So the assertion is authentication, not connectivity. No query is ever issued
// — reaching ReadyForQuery is the whole answer, and it is also the least
// invasive thing that can supply it.
//
// SELF-SKIPS when no cluster is declared, because most deployments declare none
// and a gate that failed on their absence would be turned off everywhere. It does
// NOT skip when a cluster is declared and its credential is missing: that is the
// half-seeded state, and it is a finding.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
)

// dbProbeDatabase is the database every Managed Postgres exposes, used purely as
// a startup target. Nothing is read from it.
const dbProbeDatabase = "defaultdb"

func ciAssertDatabaseCmd() *cobra.Command {
	var settle, interval, timeout int
	c := &cobra.Command{
		Use:   "assert-database",
		Short: "fail unless every declared Managed Postgres accepts its seeded admin credential",
		Long: "Connects to each Managed Postgres declared under " + dbAdminRoot + " and completes\n" +
			"the TLS + authentication handshake with the seeded admin credential. No query is\n" +
			"issued — reaching ReadyForQuery is the whole answer.\n\n" +
			"db-declared and db-summary report what the spec asks for; seed-db-admin writes\n" +
			"the credential. Nothing checked it still WORKS, and rotate-db-admin resets the\n" +
			"password IN PLACE with no overlap window, so the failure mode is a live endpoint\n" +
			"that answers every TCP connect and rejects the credential every consumer holds —\n" +
			"a rotation that half landed, or an OpenBao copy never re-seeded. A reachability\n" +
			"check passes straight through that; only authentication separates them.\n\n" +
			"Self-skips when no cluster is declared. Does NOT skip when one is declared and\n" +
			"its credential is missing — that is the half-seeded state, and it is a finding.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertDatabase(time.Duration(timeout)*time.Second,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().IntVar(&timeout, "timeout", 20, "seconds for the TLS + auth handshake")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}

// dbVerdict is one cluster's outcome.
type dbVerdict struct {
	Cluster string
	Detail  string
	FailWhy string
}

// dbAdminCreds is the connection record seed-db-admin writes.
type dbAdminCreds struct {
	Endpoint string
	Port     string
	Username string
	Password string
}

// missingFields names the empty required fields. Pure.
//
// A half-populated record is its own failure and must not be probed: dialing
// with an empty password would produce a "rejected" verdict that blames the
// database for a seeding problem.
func (c dbAdminCreds) missingFields() []string {
	var missing []string
	for _, f := range []struct{ name, val string }{
		{"endpoint", c.Endpoint}, {"port", c.Port},
		{"username", c.Username}, {"password", c.Password},
	} {
		if strings.TrimSpace(f.val) == "" {
			missing = append(missing, f.name)
		}
	}
	return missing
}

// evalDBProbe turns a probe verdict into a gate verdict. Pure.
//
// UNREACHABLE and REJECTED are kept apart deliberately. Rejected is a real
// finding about the credential — the state this gate exists for. Unreachable is
// a statement about the network or the probe, and reporting it as a credential
// failure would send someone to rotate a password that is perfectly good.
func evalDBProbe(cluster string, v pgVerdict, msg string) dbVerdict {
	d := dbVerdict{Cluster: cluster}
	switch v {
	case pgAuthenticated:
		d.Detail = "admin credential accepted (" + msg + ")"
	case pgRejected:
		d.FailWhy = "the server REJECTED the seeded admin credential: " + msg +
			". The endpoint is live, so this is the credential — most likely a rotate-db-admin that reset the " +
			"password in place without the OpenBao copy being re-seeded, or a reset performed outside the tooling"
	default:
		d.FailWhy = "could not complete the handshake (" + msg +
			"). This is a network/TLS problem, NOT evidence about the credential — check the endpoint, the " +
			"firewall allowlist and sslmode before touching the password"
	}
	return d
}

// ── OpenBao reads (seamed) ───────────────────────────────────────────────────

// dbBao is the ONE OpenBao connection every read in this gate shares, opened
// lazily and torn down by runCIAssertDatabase.
//
// It goes through openbaoClientForward, not openbaoClient, and that is the whole
// point. OpenBao has no external ingress, so NOTHING in the bootstrap workflow
// sets OPENBAO_ADDR_ACTIVE — every CI-side caller reaches it either by `kubectl
// exec` (baoKVPutFn) or by ephemeral port-forward (team-login-smoke). This gate
// shipped calling the plain constructor and died in 70ms on "OPENBAO_ADDR_ACTIVE
// is not set", failing a whole release-e2e for want of an address rather than
// for anything about a database.
//
// Shared rather than per-read because the settle loop re-reads every cluster on
// every attempt: a client per read would open, warm up and tear down a
// port-forward up to (settle/interval)×clusters times.
var dbBao struct {
	client  *openbao.Client
	cleanup func()
	err     error
	opened  bool
}

func dbBaoClient() (*openbao.Client, error) {
	if !dbBao.opened {
		dbBao.client, dbBao.cleanup, dbBao.err = openbaoClientForward(roleActive)
		dbBao.opened = true
	}
	return dbBao.client, dbBao.err
}

// closeDBBao tears the connection down and resets it, so a second call in the
// same process (and every test) starts from a closed state.
func closeDBBao() {
	if dbBao.cleanup != nil {
		dbBao.cleanup()
	}
	dbBao.client, dbBao.cleanup, dbBao.err, dbBao.opened = nil, nil, nil, false
}

// listDBClusters returns the cluster names declared under dbAdminRoot.
var listDBClusters = func(ctx context.Context) ([]string, error) {
	bao, err := dbBaoClient()
	if err != nil {
		return nil, err
	}
	names, ok, err := bao.MetadataList(ctx, dbAdminRoot)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil // nothing declared
	}
	sort.Strings(names)
	return names, nil
}

// readDBCreds fetches one cluster's admin connection record.
var readDBCreds = func(ctx context.Context, cluster string) (dbAdminCreds, error) {
	bao, err := dbBaoClient()
	if err != nil {
		return dbAdminCreds{}, err
	}
	get := func(key string) string {
		v, _, _ := bao.Get(ctx, dbAdminRoot+"/"+cluster, key)
		return v
	}
	return dbAdminCreds{
		Endpoint: get("endpoint"), Port: get("port"),
		Username: get("username"), Password: get("password"),
	}, nil
}

func probeDBClusters(ctx context.Context, clusters []string, timeout time.Duration) []dbVerdict {
	out := make([]dbVerdict, 0, len(clusters))
	for _, name := range clusters {
		creds, err := readDBCreds(ctx, name)
		if err != nil {
			out = append(out, dbVerdict{Cluster: name,
				FailWhy: fmt.Sprintf("could not read its admin credential from %s/%s: %v", dbAdminRoot, name, err)})
			continue
		}
		if missing := creds.missingFields(); len(missing) > 0 {
			out = append(out, dbVerdict{Cluster: name,
				FailWhy: fmt.Sprintf("its credential record is missing %s — the cluster is DECLARED but only "+
					"half-seeded, so nothing can be connecting to it", strings.Join(missing, ", "))})
			continue
		}
		v, msg := pgProbeCredential(creds.Endpoint, creds.Port, creds.Username, creds.Password, dbProbeDatabase, timeout)
		out = append(out, evalDBProbe(name, v, msg))
	}
	return out
}

func failedDBs(vs []dbVerdict) []string {
	var out []string
	for _, v := range vs {
		if v.FailWhy != "" {
			out = append(out, v.Cluster)
		}
	}
	return out
}

func runCIAssertDatabase(timeout, settle, interval time.Duration) error {
	fmt.Println("## Managed Postgres credential assertion")
	defer closeDBBao()
	ctx := context.Background()

	clusters, err := listDBClusters(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::could not list declared databases under %s (%v)\n", dbAdminRoot, err)
		return fmt.Errorf("could not list declared databases under %s: %w", dbAdminRoot, err)
	}
	if len(clusters) == 0 {
		fmt.Printf("SKIP: no Managed Postgres declared under %s on this deployment.\n", dbAdminRoot)
		return nil
	}
	fmt.Printf("declared clusters: %s\n", strings.Join(clusters, ", "))

	var last []dbVerdict
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		last = probeDBClusters(ctx, clusters, timeout)
		if len(failedDBs(last)) == 0 || time.Now().After(deadline) {
			break
		}
		fmt.Printf("attempt %d: %v not authenticating yet — retrying in %s\n", attempt, failedDBs(last), interval)
		time.Sleep(interval)
	}

	for _, v := range last {
		if v.FailWhy != "" {
			fmt.Printf("FAIL: %s — %s\n", v.Cluster, v.FailWhy)
		} else {
			fmt.Printf("OK: %s — %s\n", v.Cluster, v.Detail)
		}
	}
	if bad := failedDBs(last); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "::error::database credential not usable for: %s\n", strings.Join(bad, ", "))
		return fmt.Errorf("database credential not usable for: %s", strings.Join(bad, ", "))
	}
	fmt.Printf("All %d declared database(s) accept their seeded admin credential.\n", len(last))
	return nil
}
