package database

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/tofudriver"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
)

// connectionsOutput renders a `terraform output -json` blob carrying the
// databases root's `connections` output, so the tests exercise the real
// tfOutputValue extraction rather than a hand-made inner value.
func connectionsOutput(inner string) string {
	return `{"connections":{"sensitive":true,"type":"map","value":` + inner + `}}`
}

// seedDBAdminHarness stubs every seam RunSeedDBAdmin touches and records the
// OpenBao writes. seededEndpoints maps a KV path to the ENDPOINT already stored
// there ("" = unseeded) — the identity field the command now compares on, since
// OpenBao (not Terraform state) owns the password. baoStderr, when set, is
// returned for every read so a test can drive the fail-closed path.
type seedDBAdminHarness struct {
	writes          map[string]map[string]string
	writeOrder      []string
	seededEndpoints map[string]string
	baoStderr       string
	putErr          error
}

func newSeedDBAdminHarness(t *testing.T, outputs string, seeded map[string]string) *seedDBAdminHarness {
	t.Helper()
	h := &seedDBAdminHarness{
		writes:          map[string]map[string]string{},
		seededEndpoints: seeded,
	}

	prevTF, prevExec, prevPut, prevNow := tofudriver.OutputRunFn, baoread.ExecStdin, baoread.KVPut, seedDBAdminNow
	prevRead := baoread.Exec
	t.Cleanup(func() {
		tofudriver.OutputRunFn, baoread.ExecStdin, baoread.KVPut, seedDBAdminNow = prevTF, prevExec, prevPut, prevNow
		baoread.Exec = prevRead
	})

	tofudriver.OutputRunFn = func() (string, error) { return outputs, nil }
	seedDBAdminNow = func() time.Time { return time.Unix(1700000000, 0) }

	baoread.ExecStdin = func(_, _ string, args ...string) (string, string, error) {
		if h.baoStderr != "" {
			return "", h.baoStderr, errors.New("exit 2")
		}
		// kv get -field=endpoint <path>
		if len(args) >= 4 && args[0] == "kv" && args[1] == "get" {
			if ep := h.seededEndpoints[args[3]]; ep != "" {
				return ep + "\n", "", nil
			}
			return "", "No value found at " + args[3], errors.New("exit 2")
		}
		return "", "", errors.New("unexpected bao exec: " + strings.Join(args, " "))
	}

	// The seeded-path check reads through baoread.Exec, not ExecStdin. Delegating
	// keeps one fake behaviour — see the rotate harness for the same note.
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
		h.writeOrder = append(h.writeOrder, path)
		return nil
	}
	return h
}

const twoClusterConnections = `{
  "shared":    {"endpoint":"shared.vpc.internal",   "port":5432,"username":"akmadmin","password":"pw-shared",   "ca":"Y2Etc2hhcmVk"},
  "analytics": {"endpoint":"analytics.vpc.internal","port":5432,"username":"akmadmin","password":"pw-analytics","ca":"Y2EtYW5hbHl0aWNz"}
}`

func TestSeedDBAdminSeedsEveryClusterUnderItsOwnPath(t *testing.T) {
	h := newSeedDBAdminHarness(t, connectionsOutput(twoClusterConnections), nil)

	if err := RunSeedDBAdmin("prod"); err != nil {
		t.Fatalf("seed-db-admin: %v", err)
	}
	if len(h.writes) != 2 {
		t.Fatalf("expected 2 writes, got %d: %v", len(h.writes), h.writeOrder)
	}

	// Sorted, not Go map order — the command must be byte-stable across runs.
	if want := []string{"secret/infra/db-admin/analytics", "secret/infra/db-admin/shared"}; !equalStrs(h.writeOrder, want) {
		t.Errorf("write order = %v, want sorted %v", h.writeOrder, want)
	}

	got := h.writes["secret/infra/db-admin/shared"]
	for field, want := range map[string]string{
		"endpoint":   "shared.vpc.internal",
		"port":       "5432",
		"username":   "akmadmin",
		"password":   "pw-shared",
		"ca":         "Y2Etc2hhcmVk",
		"sslmode":    "require",
		"rotated_at": "1700000000",
	} {
		if got[field] != want {
			t.Errorf("shared[%s] = %q, want %q", field, got[field], want)
		}
	}

	// The whole point of reading ONE output: a cluster's endpoint can never be
	// paired with a sibling's password.
	if a := h.writes["secret/infra/db-admin/analytics"]; a["endpoint"] != "analytics.vpc.internal" || a["password"] != "pw-analytics" {
		t.Errorf("analytics entry crossed with another cluster: %v", a)
	}
}

// The opt-in needs no flag, so the command must run clean on every deployment
// that declared no databases — including one whose state predates the root and
// therefore has no `connections` output at all.
func TestSeedDBAdminIsANoOpWithoutClusters(t *testing.T) {
	for _, tc := range []struct{ name, outputs string }{
		{"empty map", connectionsOutput(`{}`)},
		{"null value", connectionsOutput(`null`)},
		{"output absent entirely", `{}`},
		{"state has no outputs", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSeedDBAdminHarness(t, tc.outputs, nil)
			if err := RunSeedDBAdmin("prod"); err != nil {
				t.Fatalf("expected a clean no-op, got %v", err)
			}
			if len(h.writes) != 0 {
				t.Errorf("expected no writes, got %v", h.writeOrder)
			}
		})
	}
}

// A path already pointing at THIS cluster is left completely alone — even
// though the password OpenBao holds differs from the one in Terraform state.
// That difference is the NORMAL steady state after rotate-on-create: state holds
// the dead provisioning credential. Rewriting here would push it back over the
// live one and strand every consumer, which is exactly what comparing on
// identity instead of on the password prevents.
func TestSeedDBAdminLeavesALiveCredentialAlone(t *testing.T) {
	h := newSeedDBAdminHarness(t, connectionsOutput(twoClusterConnections), map[string]string{
		"secret/infra/db-admin/shared":    "shared.vpc.internal",
		"secret/infra/db-admin/analytics": "analytics.vpc.internal",
	})
	if err := RunSeedDBAdmin("prod"); err != nil {
		t.Fatalf("seed-db-admin: %v", err)
	}
	if len(h.writes) != 0 {
		t.Errorf("a path pointing at this cluster must not be rewritten (its password is the ROTATED one, not state's), got %v", h.writeOrder)
	}
}

// A RECREATED cluster is the one case that still re-seeds. Its endpoint carries
// the cluster id, so a destroy/create changes it — and the credential OpenBao
// holds belongs to a database that no longer exists. Blind-skipping here would
// strand every consumer on a credential for a dead cluster.
func TestSeedDBAdminReseedsARecreatedCluster(t *testing.T) {
	h := newSeedDBAdminHarness(t, connectionsOutput(twoClusterConnections), map[string]string{
		"secret/infra/db-admin/shared":    "shared-OLD.vpc.internal",
		"secret/infra/db-admin/analytics": "analytics.vpc.internal",
	})
	if err := RunSeedDBAdmin("prod"); err != nil {
		t.Fatalf("seed-db-admin: %v", err)
	}
	if len(h.writes) != 1 || h.writes["secret/infra/db-admin/shared"]["password"] != "pw-shared" {
		t.Fatalf("expected only the recreated cluster to be re-seeded, got %v", h.writes)
	}
	if h.writes["secret/infra/db-admin/shared"]["endpoint"] != "shared.vpc.internal" {
		t.Error("the re-seeded path must point at the new cluster")
	}
}

// An unreadable path is not an absent one. Writing over a live credential we
// failed to read is indistinguishable from a successful rotation, so the command
// must fail before writing anything.
func TestSeedDBAdminFailsClosedOnAnUnreadablePath(t *testing.T) {
	h := newSeedDBAdminHarness(t, connectionsOutput(twoClusterConnections), nil)
	h.baoStderr = "Vault is sealed"

	err := RunSeedDBAdmin("prod")
	if err == nil {
		t.Fatal("expected an error when the KV read is unknown")
	}
	if len(h.writes) != 0 {
		t.Errorf("must not write when the read verdict is unknown, got %v", h.writeOrder)
	}
}

// A half-populated entry means that cluster's apply did not finish; seeding it
// publishes a credential that cannot connect.
func TestSeedDBAdminRejectsAnIncompleteConnection(t *testing.T) {
	h := newSeedDBAdminHarness(t, connectionsOutput(
		`{"shared":{"endpoint":"shared.vpc.internal","port":5432,"username":"akmadmin","password":"","ca":"x"}}`), nil)

	err := RunSeedDBAdmin("prod")
	if err == nil || !strings.Contains(err.Error(), "apply did not complete") {
		t.Fatalf("expected an incomplete-apply error, got %v", err)
	}
	if len(h.writes) != 0 {
		t.Errorf("must not seed an incomplete connection, got %v", h.writeOrder)
	}
}

func TestSeedDBAdminRequiresRegion(t *testing.T) {
	newSeedDBAdminHarness(t, connectionsOutput(`{}`), nil)
	if err := RunSeedDBAdmin(""); err == nil {
		t.Fatal("expected --region to be required")
	}
}

func TestDBAdminSummaryReportsEachDisposition(t *testing.T) {
	got := strings.Join(dbAdminSummary("prod", []string{"shared"}, []string{"analytics"}, []string{"legacy"}), "\n")
	for _, want := range []string{
		"### Database admin credentials (prod)",
		"**Seeded:** `shared`",
		"**Updated:** `analytics`",
		"**Already current:** `legacy`",
		"base64-encoded",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
	// A disposition with no clusters must not render an empty bullet.
	if none := strings.Join(dbAdminSummary("prod", []string{"shared"}, nil, nil), "\n"); strings.Contains(none, "Updated") {
		t.Errorf("empty disposition should be omitted:\n%s", none)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
