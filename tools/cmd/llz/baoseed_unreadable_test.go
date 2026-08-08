package main

// baoseed_unreadable_test.go — the two tests that STAYED, and the sixth stranded-
// test find.
//
// They lived in bao_read_test.go, whose subject is the OpenBao read classifier —
// but they drive openbao.RunSeed and the objkey mint paths, which are package main's.
// The filename named the module they exercise THROUGH, not the code they test. Two
// of the five tests in that file were about the classifier; three were about its
// callers.
//
// The pattern is the fourth this campaign has recorded, after files named for a
// coverage METRIC, for the COMMAND that calls the code, and for the BATCH they
// were written in. This one is named for the DEPENDENCY the tests share — which is
// the most plausible-looking of the four, and still wrong for the same reason:
// nothing in the name points at a subject.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/openbao"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
)

// The seeder must not overwrite a path it could not read. This is the bug:
// a sealed pod read "" and the guard let fresh crypto/rand bytes land on a live
// credential.
func TestBaoSeedRefusesToWriteOnUnreadablePath(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	withBaoReadSeam(t, "Vault is sealed", false)

	wrote := false
	prevPut := baoread.KVPut
	baoread.KVPut = func(string, map[string]string) error { wrote = true; return nil }
	t.Cleanup(func() { baoread.KVPut = prevPut })

	err := openbao.RunSeed(openbao.Opts{
		Path:          "secret/grafana/admin",
		FieldSpecs:    []string{"password=gen:hex:16"},
		SkipIfPresent: "password",
		OnMissing:     "error",
	})
	if err == nil {
		t.Fatal("an unreadable path must fail the seed, not silently overwrite it")
	}
	if wrote {
		t.Fatal("a credential was overwritten on the strength of a failed read")
	}
	if !strings.Contains(err.Error(), "NOT evidence") {
		t.Errorf("the error must say what it did not conclude: %v", err)
	}
}

// The mint paths create real cloud resources on the same "" — a Linode
// object-storage key and an in-cluster PAT.
// The mint paths create real cloud resources on the same "" — a Linode
// object-storage key and an in-cluster PAT.
func TestMintPathsRefuseOnUnreadablePath(t *testing.T) {
	// mint-bootstrap-pat resolves the instance-scoped PAT label from the spec
	// before it touches OpenBao; this test is about the fail-closed read.
	dir := chdirTempDir(t)
	mustWrite(t, filepath.Join(dir, "landingzone.yaml"),
		"apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: acme\nspec:\n  instance:\n    repo: o/acme\n")
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	t.Setenv("LINODE_API_TOKEN", "broad")
	withBaoReadSeam(t, "connection refused", false)

	if err := credrotate.RunMintBootstrapPAT("primary"); err == nil ||
		!strings.Contains(err.Error(), "NOT evidence") {
		t.Errorf("mint-bootstrap-pat on an unreadable path: err = %v, want a fail-closed refusal", err)
	}
}

// withBaoReadSeam swaps internal/baoread's TWO seams so these package main tests
// can drive an unreadable path.
//
// It is a COPY of the package's own fixture, not an export: a test helper that
// exists to be reachable from two packages is a symbol in an API for no runtime
// reason. Both seams are swapped — leaving PodStatusUnsealed at its default
// (false) would resolve every unrecognised stderr to Unknown regardless of the
// fake pod, and the tests would pass while asserting nothing. That double-seam
// mistake has cost this campaign twice already.
func withBaoReadSeam(t *testing.T, stderr string, podHealthy bool) {
	t.Helper()
	prevExec, prevStatus := baoread.Exec, baoread.PodStatusUnsealed
	baoread.Exec = func(_ string, args ...string) (string, string, error) {
		if args[0] == "status" {
			if !podHealthy {
				return "", "connection refused", errors.New("exit 2")
			}
			return `{"initialized":true,"sealed":false}`, "", nil
		}
		return "", stderr, errors.New("exit 2")
	}
	baoread.PodStatusUnsealed = func(out string) bool {
		return strings.Contains(out, `"sealed":false`)
	}
	t.Cleanup(func() { baoread.Exec, baoread.PodStatusUnsealed = prevExec, prevStatus })
}
