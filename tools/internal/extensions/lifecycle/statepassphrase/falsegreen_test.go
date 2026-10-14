package statepassphrase

// falsegreen_test.go — the exit status that licenses deleting the old passphrase.
//
// `llz ci rotate-state-passphrase` re-keys every Terraform root onto a new
// passphrase and verifies each one decrypts with the new key ALONE. The workflow
// gates deletion of TF_STATE_ENCRYPTION_PASSPHRASE_OLD on its exit status, and
// that secret is the only thing that can read state written under the old key.
// So exit 0 here is not a status, it is an instruction to destroy a key.
//
// It said "All 0 present root(s) verified … can now be deleted" and exited 0
// whenever every root was skipped — which is what a wrong --roots-dir produces,
// and --roots-dir defaulted to `terraform` while the roots live under
// terraform-iac-bootstrap/. Two independent typos away from four permanently
// unreadable state files, with the summary telling the operator to proceed.
//
// These are the arms that make "nothing happened" distinguishable from "it
// worked". They fail closed in the direction that keeps a key nobody needs, not
// the one that discards a key everybody does.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

var errTestVerify = errors.New("state is still on the old key")

// THE FALSE GREEN. Every root skipped is not a completed rollover.
func TestAnAllSkippedRolloverFailsRatherThanLicensingDeletion(t *testing.T) {
	rotationWindowEnv(t)
	withRolloverSeams(t,
		func(string) error { t.Fatal("re-key must not run when no root is present"); return nil },
		func(string) error { t.Fatal("verify must not run when no root is present"); return nil },
		map[string]bool{}) // no root present

	err := RunRotate(true, tmpRootsDir(t))
	if err == nil {
		t.Fatal("a rollover that re-keyed NOTHING exited 0 — the workflow reads that as " +
			"permission to delete TF_STATE_ENCRYPTION_PASSPHRASE_OLD, which is the only key " +
			"that can still read every state file in the instance")
	}
	// The operator has to be told what to do, and the remedy is the flag.
	if !strings.Contains(err.Error(), "--roots-dir") {
		t.Errorf("the error should point at the likely cause: %v", err)
	}
	for _, forbidden := range []string{"can now be deleted"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("the failure must not carry the success language: %v", err)
		}
	}
}

// A ROOTS DIRECTORY THAT DOES NOT EXIST IS "COULD NOT TELL". Each root is probed
// with a stat and an absent one is a real state — an instance with no databases
// root. The PARENT being absent is not: it means nothing was ever looked for,
// and reporting four skips for a path that does not exist is how the wrong
// default stayed invisible.
func TestAnAbsentRootsDirectoryIsRefusedRatherThanReportedAsFourSkips(t *testing.T) {
	rotationWindowEnv(t)
	withRolloverSeams(t,
		func(string) error { return nil },
		func(string) error { return nil },
		allRoots())

	err := RunRotate(true, filepath.Join(t.TempDir(), "terraform")) // the retired default
	if err == nil {
		t.Fatal("a --roots-dir that does not exist was reported on as if it had been read")
	}
	if !strings.Contains(err.Error(), "terraform-iac-bootstrap") {
		t.Errorf("the error should name where the roots actually live: %v", err)
	}
}

// AND THE DEFAULT POINTS AT THE REAL LOCATION, so the two halves cannot be fixed
// apart. The flag's default is what every hand-run uses; the workflow passes it
// explicitly, and that literal is pinned separately below.
func TestTheRootsDirDefaultIsWhereTheRootsActuallyLive(t *testing.T) {
	f := RotateStatePassphraseCmd().Flags().Lookup("roots-dir")
	if f == nil {
		t.Fatal("no --roots-dir flag")
	}
	if f.DefValue != "terraform-iac-bootstrap" {
		t.Errorf("--roots-dir default = %q, want terraform-iac-bootstrap — `llz render` materialises "+
			"the roots there, and the retired default `terraform` silently produced an all-skipped run",
			f.DefValue)
	}
}

// A PARTIAL ROLLOVER STILL FAILS, which was already true and is pinned here
// beside its sibling: the new zero-verified arm must not have replaced the
// any-failed one, since a run where three roots moved and one did not is the
// case where keeping the old passphrase matters most.
func TestAPartialRolloverStillRefusesDeletion(t *testing.T) {
	rotationWindowEnv(t)
	withRolloverSeams(t,
		func(string) error { return nil },
		func(dir string) error {
			if filepath.Base(dir) == "databases" {
				return errTestVerify
			}
			return nil
		},
		allRoots())

	err := RunRotate(true, tmpRootsDir(t))
	if err == nil {
		t.Fatal("one unverified root must fail the command")
	}
	if !strings.Contains(err.Error(), "MUST be retained") {
		t.Errorf("the error should tell the operator to keep the old passphrase: %v", err)
	}
}

// A DRY RUN IS NOT A ROLLOVER AND MUST NOT FAIL, or the report-only mode the
// scheduled job runs in goes permanently red and gets switched off — taking the
// arms above with it.
func TestADryRunOverPresentRootsStillSucceeds(t *testing.T) {
	rotationWindowEnv(t)
	withRolloverSeams(t,
		func(string) error { t.Fatal("dry run must not re-key"); return nil },
		func(string) error { t.Fatal("dry run must not verify"); return nil },
		allRoots())

	if err := RunRotate(false, tmpRootsDir(t)); err != nil {
		t.Fatalf("dry run: %v", err)
	}
}
