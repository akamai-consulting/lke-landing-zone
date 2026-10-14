package database

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/tofudriver"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// databaseIDsOutput renders a `terraform output -json` blob carrying the
// databases root's `database_ids` output, so the tests exercise the real
// tfOutputValue extraction rather than a hand-made inner value.
func databaseIDsOutput(inner string) string {
	return `{"database_ids":{"sensitive":false,"type":"map","value":` + inner + `}}`
}

// fakeDBAPI records what the rotator did to Linode and lets each call be failed
// independently — the failure MODE is the thing under test here, because after a
// reset every one of them means a live credential is at risk.
type fakeDBAPI struct {
	resets       []uint64
	resetErr     error
	statuses     []string // consumed one per PostgresInstance call; last value repeats
	statusErr    error
	creds        linode.DBCredentials
	credsErr     error
	instanceGets int
}

func (f *fakeDBAPI) ResetPostgresCredentials(_ context.Context, id uint64) error {
	if f.resetErr != nil {
		return f.resetErr
	}
	f.resets = append(f.resets, id)
	return nil
}

func (f *fakeDBAPI) PostgresInstance(_ context.Context, _ uint64) (map[string]any, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	f.instanceGets++
	i := f.instanceGets - 1
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	return map[string]any{"status": f.statuses[i]}, nil
}

func (f *fakeDBAPI) PostgresCredentials(_ context.Context, _ uint64) (linode.DBCredentials, error) {
	if f.credsErr != nil {
		return linode.DBCredentials{}, f.credsErr
	}
	return f.creds, nil
}

// rotateDBHarness stubs every seam RunRotateDBAdmin touches. stored maps a KV
// path+field to its value, so a test can seed a complete secret, a partial one,
// or none at all.
type rotateDBHarness struct {
	api       *fakeDBAPI
	writes    map[string]map[string]string
	stored    map[string]string // "<path>|<field>" → value
	baoStderr string
	putErr    error
	now       time.Time
}

func newRotateDBHarness(t *testing.T, outputs string, stored map[string]string, api *fakeDBAPI) *rotateDBHarness {
	t.Helper()
	h := &rotateDBHarness{
		api:    api,
		writes: map[string]map[string]string{},
		stored: stored,
		now:    time.Unix(1_800_000_000, 0),
	}

	prevTF, prevExec, prevPut := tofudriver.OutputRunFn, baoread.ExecStdin, baoread.KVPut
	prevRead := baoread.Exec
	prevNow, prevClient, prevSleep := dbAdminNow, dbAdminLinodeClient, dbAdminSleep
	t.Cleanup(func() {
		tofudriver.OutputRunFn, baoread.ExecStdin, baoread.KVPut = prevTF, prevExec, prevPut
		baoread.Exec = prevRead
		dbAdminNow, dbAdminLinodeClient, dbAdminSleep = prevNow, prevClient, prevSleep
	})

	t.Setenv("LINODE_TOKEN", "tok")
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")

	tofudriver.OutputRunFn = func() (string, error) { return outputs, nil }
	dbAdminNow = func() time.Time { return h.now }
	dbAdminSleep = func(d time.Duration) { h.now = h.now.Add(d) }
	dbAdminLinodeClient = func(string) dbAdminAPI { return h.api }

	baoread.ExecStdin = func(_, _ string, args ...string) (string, string, error) {
		if h.baoStderr != "" {
			return "", h.baoStderr, errors.New("exit 2")
		}
		// kv get -field=<f> <path>
		if len(args) >= 4 && args[0] == "kv" && args[1] == "get" {
			field := strings.TrimPrefix(args[2], "-field=")
			if v, ok := h.stored[args[3]+"|"+field]; ok && v != "" {
				return v + "\n", "", nil
			}
			return "", "No value found at " + args[3], errors.New("exit 2")
		}
		return "", "", errors.New("unexpected bao exec: " + strings.Join(args, " "))
	}

	// ALL THREE SEAMS. The carried-field reads go through baoread.KVGetFieldOK,
	// which uses baoread.Exec — NOT ExecStdin. Stubbing only the two this harness
	// names left every read hitting the erroring default, which is the double-seam
	// trap for the fifth time. Delegating keeps one fake behaviour.
	baoread.Exec = func(token string, args ...string) (string, string, error) {
		return baoread.ExecStdin(token, "", args...)
	}

	baoread.KVPut = func(path string, fields map[string]string) error {
		if h.putErr != nil {
			return h.putErr
		}
		copied := map[string]string{}
		for k, v := range fields {
			copied[k] = v
		}
		h.writes[path] = copied
		return nil
	}
	return h
}

// seededDBSecret is a complete secret/infra/db-admin/<name>, `age` days old.
func seededDBSecret(path string, ageDays int, now time.Time, password string) map[string]string {
	stamp := now.Add(-time.Duration(ageDays) * 24 * time.Hour).Unix()
	return map[string]string{
		path + "|endpoint":   "db.vpc.internal",
		path + "|port":       "5432",
		path + "|ca":         "YmFzZTY0Q0E=",
		path + "|sslmode":    "require",
		path + "|password":   password,
		path + "|username":   "akmadmin",
		path + "|rotated_at": strconv.FormatInt(stamp, 10),
	}
}

// A credential inside its window is left completely alone — no reset, no write.
func TestRotateDBAdminSkipsWhenNotDue(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := dbAdminSeedRoot + "shared"
	api := &fakeDBAPI{statuses: []string{"active"}}
	h := newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), seededDBSecret(path, 10, now, "old-pw"), api)

	if err := RunRotateDBAdmin("prod", true, false, 80); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if len(api.resets) != 0 {
		t.Errorf("a 10d-old credential must not be reset, got resets %v", api.resets)
	}
	if len(h.writes) != 0 {
		t.Errorf("nothing should be written, got %v", h.writes)
	}
}

// The default is a report. An overdue credential is NOT touched without --apply,
// because the reset is irreversible.
func TestRotateDBAdminReportOnlyDoesNotMutate(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := dbAdminSeedRoot + "shared"
	api := &fakeDBAPI{statuses: []string{"active"}}
	h := newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), seededDBSecret(path, 200, now, "old-pw"), api)

	if err := RunRotateDBAdmin("prod", false, false, 80); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if len(api.resets) != 0 {
		t.Errorf("report-only must not reset, got %v", api.resets)
	}
	if len(h.writes) != 0 {
		t.Errorf("report-only must not write, got %v", h.writes)
	}
}

// The happy path: reset, wait out the `updating` window, persist the new
// credential WITH the fields a reset does not change, then refresh state.
func TestRotateDBAdminRotatesAndCarriesFields(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := dbAdminSeedRoot + "shared"
	api := &fakeDBAPI{
		statuses: []string{"updating", "updating", "active"},
		creds:    linode.DBCredentials{Username: "akmadmin", Password: "brand-new-pw"},
	}
	h := newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), seededDBSecret(path, 200, now, "old-pw"), api)

	if err := RunRotateDBAdmin("prod", true, false, 80); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if len(api.resets) != 1 || api.resets[0] != 12345 {
		t.Fatalf("want one reset of id 12345, got %v", api.resets)
	}
	got := h.writes[path]
	if got == nil {
		t.Fatalf("nothing written to %s", path)
	}
	// A KV v2 put REPLACES the secret, so the untouched members have to be
	// carried across or consumers lose the host they connect to.
	for k, want := range map[string]string{
		"password": "brand-new-pw",
		"username": "akmadmin",
		"endpoint": "db.vpc.internal",
		"port":     "5432",
		"ca":       "YmFzZTY0Q0E=",
		"sslmode":  "require",
	} {
		if got[k] != want {
			t.Errorf("field %s = %q, want %q", k, got[k], want)
		}
	}
	stamp, perr := strconv.ParseInt(got["rotated_at"], 10, 64)
	if perr != nil {
		t.Fatalf("rotated_at = %q, want a unix stamp: %v", got["rotated_at"], perr)
	}
	if stamp < now.Unix() || stamp > now.Add(dbAdminActiveTimeout).Unix() {
		t.Errorf("rotated_at = %d, want a stamp between the start of the rotation (%d) and its wait budget (%d)",
			stamp, now.Unix(), now.Add(dbAdminActiveTimeout).Unix())
	}
}

// A reset that leaves the password unchanged must NOT be recorded as a rotation:
// stamping rotated_at would hide the stale credential for another full window.
func TestRotateDBAdminRefusesUnchangedPassword(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := dbAdminSeedRoot + "shared"
	api := &fakeDBAPI{
		statuses: []string{"active"},
		creds:    linode.DBCredentials{Username: "akmadmin", Password: "old-pw"},
	}
	h := newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), seededDBSecret(path, 200, now, "old-pw"), api)

	err := RunRotateDBAdmin("prod", true, false, 80)
	if err == nil {
		t.Fatal("an unchanged password after a reset must be an error")
	}
	if !strings.Contains(err.Error(), "did not take effect") {
		t.Errorf("error should name the cause, got: %v", err)
	}
	if len(h.writes) != 0 {
		t.Errorf("must not stamp rotated_at on a rotation that did not happen, wrote %v", h.writes)
	}
}

// Everything after the reset is past the point of no return, so each failure has
// to hand the operator the recovery path — and must never print the credential.
func TestRotateDBAdminPostResetFailuresAreLoud(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := dbAdminSeedRoot + "shared"
	cases := []struct {
		name    string
		mutate  func(*fakeDBAPI, *rotateDBHarness)
		wantErr string
	}{
		{"credential re-read fails", func(a *fakeDBAPI, _ *rotateDBHarness) {
			a.credsErr = errors.New("500")
		}, "re-read credentials"},
		{"empty password returned", func(a *fakeDBAPI, _ *rotateDBHarness) {
			a.creds = linode.DBCredentials{Username: "akmadmin"}
		}, "empty password"},
		{"openbao write fails", func(_ *fakeDBAPI, h *rotateDBHarness) {
			h.putErr = errors.New("sealed")
		}, "write secret/infra/db-admin/shared"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := &fakeDBAPI{statuses: []string{"active"}, creds: linode.DBCredentials{Username: "akmadmin", Password: "new-pw"}}
			h := newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), seededDBSecret(path, 200, now, "old-pw"), api)
			c.mutate(api, h)

			err := RunRotateDBAdmin("prod", true, false, 80)
			if err == nil {
				t.Fatal("want an error")
			}
			msg := err.Error()
			if !strings.Contains(msg, "RESET BUT NOT PERSISTED") {
				t.Errorf("error must flag the lost-credential state, got: %v", err)
			}
			if !strings.Contains(msg, c.wantErr) {
				t.Errorf("error should name the cause %q, got: %v", c.wantErr, err)
			}
			if !strings.Contains(msg, "postgresql-creds-view 12345") {
				t.Errorf("error must carry the recovery command, got: %v", err)
			}
			if strings.Contains(msg, "new-pw") {
				t.Errorf("the error must NOT contain the credential itself: %v", err)
			}
		})
	}
}

// Rotating a path that was never seeded would store a password with no endpoint
// to use it against — refuse, and do it BEFORE the irreversible reset.
func TestRotateDBAdminRefusesUnseededPath(t *testing.T) {
	api := &fakeDBAPI{statuses: []string{"active"}}
	newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), map[string]string{}, api)

	err := RunRotateDBAdmin("prod", true, false, 80)
	if err == nil {
		t.Fatal("an unseeded path must not be rotated")
	}
	if !strings.Contains(err.Error(), "seed-db-admin") {
		t.Errorf("error should point at the seed command, got: %v", err)
	}
	if len(api.resets) != 0 {
		t.Errorf("the refusal must happen BEFORE the reset, got resets %v", api.resets)
	}
}

// An OpenBao that will not answer is not an unseeded path. Fail closed rather
// than resetting a credential we cannot prove anything about.
func TestRotateDBAdminFailsClosedOnUnreadableBao(t *testing.T) {
	api := &fakeDBAPI{statuses: []string{"active"}}
	h := newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), map[string]string{}, api)
	h.baoStderr = "Error making API request: connection refused"

	err := RunRotateDBAdmin("prod", true, false, 80)
	if err == nil {
		t.Fatal("an unreadable OpenBao must fail the run")
	}
	if len(api.resets) != 0 {
		t.Errorf("must not reset when the stored credential could not be read, got %v", api.resets)
	}
}

// A deployment that declared no databases: the command runs clean and does
// nothing, so it can sit unconditionally in a scheduled workflow.
func TestRotateDBAdminNoDatabasesIsNoOp(t *testing.T) {
	api := &fakeDBAPI{statuses: []string{"active"}}
	newRotateDBHarness(t, `{}`, map[string]string{}, api)

	if err := RunRotateDBAdmin("prod", true, false, 80); err != nil {
		t.Fatalf("a deployment with no databases must be a clean no-op: %v", err)
	}
	if len(api.resets) != 0 {
		t.Error("nothing should have happened")
	}
}

// A credential with no parseable rotated_at is DUE — an admin password of
// unknown age is exactly what rotation is for.
func TestRotateDBAdminMissingStampIsDue(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := dbAdminSeedRoot + "shared"
	stored := seededDBSecret(path, 1, now, "old-pw")
	delete(stored, path+"|rotated_at")
	api := &fakeDBAPI{statuses: []string{"active"}, creds: linode.DBCredentials{Username: "akmadmin", Password: "new-pw"}}
	h := newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), stored, api)

	if err := RunRotateDBAdmin("prod", true, false, 80); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if len(api.resets) != 1 {
		t.Errorf("an unstamped credential should be treated as due, got resets %v", api.resets)
	}
	if h.writes[path]["password"] != "new-pw" {
		t.Error("the new credential should have been persisted")
	}
}

// A cluster stuck in `updating` must time out rather than reading back a
// pre-reset credential pair and storing it as the rotation result.
func TestRotateDBAdminTimesOutWaitingForActive(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := dbAdminSeedRoot + "shared"
	api := &fakeDBAPI{statuses: []string{"updating"}, creds: linode.DBCredentials{Username: "akmadmin", Password: "new-pw"}}
	h := newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), seededDBSecret(path, 200, now, "old-pw"), api)
	// Advance the clock past the timeout on every poll.
	calls := 0
	dbAdminNow = func() time.Time {
		calls++
		return now.Add(time.Duration(calls) * dbAdminActiveTimeout)
	}

	err := RunRotateDBAdmin("prod", true, false, 80)
	if err == nil {
		t.Fatal("a cluster that never returns to active must fail the run")
	}
	if !strings.Contains(err.Error(), "RESET BUT NOT PERSISTED") {
		t.Errorf("a timeout after the reset is a lost-credential state, got: %v", err)
	}
	if len(h.writes) != 0 {
		t.Errorf("must not persist a credential read from a non-active cluster, wrote %v", h.writes)
	}
}

func TestParseDBIDs(t *testing.T) {
	cases := []struct {
		name, in string
		want     int
		wantErr  string
	}{
		{"empty output", "", 0, ""},
		{"null output", "null", 0, ""},
		{"two clusters", `{"shared":1,"analytics":2}`, 2, ""},
		// A fractional id decodes into json.Number cleanly and only fails at
		// ParseUint — the branch that catches an output whose SHAPE is right but
		// whose value could never be a Linode id.
		{"fractional id", `{"shared":1.5}`, 0, "non-numeric database id"},
		{"quoted id", `{"shared":"abc"}`, 0, "parse the database_ids output"},
		{"not a map", `["shared"]`, 0, "parse the database_ids output"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseDBIDs(c.in)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("want error containing %q, got %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDBIDs: %v", err)
			}
			if len(got) != c.want {
				t.Errorf("got %d ids, want %d", len(got), c.want)
			}
		})
	}
}

func TestRotateDBAdminRejectsBadFlags(t *testing.T) {
	if err := RunRotateDBAdmin("", true, false, 80); err == nil {
		t.Error("--region is required")
	}
	if err := RunRotateDBAdmin("prod", true, false, 0); err == nil {
		t.Error("--rotate-after-days must be positive")
	}
}

// rotate-on-create: --rotate-now ignores the age check entirely. Bootstrap uses
// it to burn the PROVISIONING credential — the one Terraform just handed over
// and is still holding in state — within the same run that created it.
func TestRotateDBAdminRotateNowIgnoresAge(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := dbAdminSeedRoot + "shared"
	// Freshly seeded: zero days old, nowhere near the 80d threshold.
	api := &fakeDBAPI{statuses: []string{"active"}, creds: linode.DBCredentials{Username: "akmadmin", Password: "post-bootstrap-pw"}}
	h := newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), seededDBSecret(path, 0, now, "provisioning-pw"), api)

	if err := RunRotateDBAdmin("prod", true, true, 80); err != nil {
		t.Fatalf("rotate-now: %v", err)
	}
	if len(api.resets) != 1 {
		t.Fatalf("--rotate-now must rotate a 0d-old credential, got resets %v", api.resets)
	}
	if got := h.writes[path]["password"]; got != "post-bootstrap-pw" {
		t.Errorf("password = %q, want the post-rotation one", got)
	}
	// The provisioning credential Terraform holds in state is now dead.
	if h.writes[path]["password"] == "provisioning-pw" {
		t.Error("the provisioning credential must not survive rotate-on-create")
	}
}

// --rotate-now still respects the --apply gate: it changes WHICH clusters are
// due, not whether the irreversible reset is armed.
func TestRotateDBAdminRotateNowStillNeedsApply(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := dbAdminSeedRoot + "shared"
	api := &fakeDBAPI{statuses: []string{"active"}}
	h := newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), seededDBSecret(path, 0, now, "provisioning-pw"), api)

	if err := RunRotateDBAdmin("prod", false, true, 80); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if len(api.resets) != 0 || len(h.writes) != 0 {
		t.Error("--rotate-now without --apply must still be report-only")
	}
}
