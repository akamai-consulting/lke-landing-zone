package database

// Mutation-test gap closure for ci_rotate_dbadmin.go. The Postgres admin reset is
// irreversible, so every predicate that decides WHETHER a cluster is rotated, and
// every field the rewrite has to carry across, is a one-way door.

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// dbAdminRunStepSummary runs the report-only path and returns what landed in
// $GITHUB_STEP_SUMMARY — the operator-facing rendering of each cluster's
// credential age, which nothing else asserted end to end.
func dbAdminRunStepSummary(t *testing.T, stored map[string]string, afterDays int) string {
	t.Helper()
	sum := withGHASummaryFile(t)
	newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), stored, &fakeDBAPI{statuses: []string{"active"}})
	if err := RunRotateDBAdmin("prod", false, false, afterDays); err != nil {
		t.Fatalf("report-only run: %v", err)
	}
	b, err := os.ReadFile(sum)
	if err != nil {
		t.Fatalf("read step summary: %v", err)
	}
	return string(b)
}

// sslmode is the ONLY carried field with a safe default; every other one is a
// connection coordinate that cannot be invented. Re-asserting an absent endpoint
// as "require" would store an admin password pointing nowhere — and, worse, would
// let the irreversible reset proceed against a path that was never seeded.
func TestReadDBAdminCarriedFieldsOnlySSLModeHasADefault(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := dbAdminSeedRoot + "shared"

	t.Run("absent sslmode is re-asserted", func(t *testing.T) {
		stored := seededDBSecret(path, 200, now, "old-pw")
		delete(stored, path+"|sslmode")
		newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), stored, &fakeDBAPI{statuses: []string{"active"}})

		got, err := readDBAdminCarriedFields(path, "shared")
		if err != nil {
			t.Fatalf("a secret seeded before sslmode existed must still rotate: %v", err)
		}
		if got["sslmode"] != dbAdminSSLMode {
			t.Errorf("sslmode = %q, want the re-asserted floor %q", got["sslmode"], dbAdminSSLMode)
		}
		if got["endpoint"] != "db.vpc.internal" {
			t.Errorf("endpoint = %q, want the stored value", got["endpoint"])
		}
	})

	for _, field := range []string{"endpoint", "port", "ca"} {
		t.Run("absent "+field+" refuses", func(t *testing.T) {
			stored := seededDBSecret(path, 200, now, "old-pw")
			delete(stored, path+"|"+field)
			api := &fakeDBAPI{statuses: []string{"active"}}
			newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), stored, api)

			_, err := readDBAdminCarriedFields(path, "shared")
			if err == nil {
				t.Fatalf("a missing %q must refuse the rotation, not be defaulted away", field)
			}
			if !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "seed-db-admin") {
				t.Errorf("error should name the missing field and the seed command, got: %v", err)
			}
		})
	}
}

// The due test is `age >= threshold`, INCLUSIVE. A credential that is exactly
// --rotate-after-days old is overdue: the scheduled run that lands on the
// boundary is the one that has to act, or the credential slips a whole cadence.
func TestRotateDBAdminExactThresholdIsDue(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := dbAdminSeedRoot + "shared"
	api := &fakeDBAPI{statuses: []string{"active"}, creds: linode.DBCredentials{Username: "akmadmin", Password: "new-pw"}}
	h := newRotateDBHarness(t, databaseIDsOutput(`{"shared":12345}`), seededDBSecret(path, 80, now, "old-pw"), api)

	if err := RunRotateDBAdmin("prod", true, false, 80); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if len(api.resets) != 1 {
		t.Fatalf("a credential exactly 80d old is due at a threshold of 80d, got resets %v", api.resets)
	}
	if h.writes[path]["password"] != "new-pw" {
		t.Errorf("the replacement was not persisted: %v", h.writes[path])
	}
}

// The age column is the operator's only view of the fleet. "age unknown" and a
// real day count are DIFFERENT claims: the first says "we cannot vouch for this
// credential", the second says "we can". Pin every branch of that rendering.
func TestRotateDBAdminSummaryAgeRendering(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := dbAdminSeedRoot + "shared"

	t.Run("no rotated_at stamp reads as unknown", func(t *testing.T) {
		stored := seededDBSecret(path, 5, now, "old-pw")
		delete(stored, path+"|rotated_at")
		got := dbAdminRunStepSummary(t, stored, 80)
		if !strings.Contains(got, "age unknown (no rotated_at stamp)") {
			t.Errorf("an unstamped credential must render as unknown, got:\n%s", got)
		}
		if strings.Contains(got, "d old") {
			t.Errorf("an unstamped credential must not claim a concrete age, got:\n%s", got)
		}
	})

	t.Run("an unparseable-as-positive stamp reads as unknown", func(t *testing.T) {
		stored := seededDBSecret(path, 5, now, "old-pw")
		stored[path+"|rotated_at"] = "0" // a zero epoch is not a rotation we can date
		got := dbAdminRunStepSummary(t, stored, 80)
		if !strings.Contains(got, "age unknown (no rotated_at stamp)") {
			t.Errorf("a zero stamp must render as unknown, not as ~20000 days, got:\n%s", got)
		}
	})

	t.Run("a freshly seeded credential reads as 0d old", func(t *testing.T) {
		got := dbAdminRunStepSummary(t, seededDBSecret(path, 0, now, "provisioning-pw"), 80)
		if !strings.Contains(got, "0d old") {
			t.Errorf("a 0d-old credential has a KNOWN age and must say so, got:\n%s", got)
		}
		if strings.Contains(got, "age unknown") {
			t.Errorf("a 0d-old credential must not be reported as unknown-age, got:\n%s", got)
		}
	})

	t.Run("an aged credential reads with its day count", func(t *testing.T) {
		got := dbAdminRunStepSummary(t, seededDBSecret(path, 200, now, "old-pw"), 80)
		if !strings.Contains(got, "200d old") {
			t.Errorf("want the concrete age, got:\n%s", got)
		}
		if strings.Contains(got, "age unknown") {
			t.Errorf("a stamped credential must not be reported as unknown-age, got:\n%s", got)
		}
	})
}
