package identityconfig

// openbao_policy_coupling_test.go — the lane's credential table against the policy
// that grants it.
//
// THIS TEST SPANS THE EXTRACTION BOUNDARY, deliberately. credpaths.CredPaths
// is what the openbao-gauges lane samples; policyReconcilerRead is the OpenBao
// policy `llz ci bao-configure` writes, and it stayed in internal/cli with the
// rest of bootstrap. One ungranted path is a 403, and the sampler treats any
// non-404 failure as fatal — so a single missing grant takes down the seal gauge
// and every other credential's age with it.
//
// It lives on this side because the policy is the larger, more core artifact and
// CredPaths is already exported. A coupling test has to be able to reach both
// halves; which package it sits in is a choice about which half is the fixture.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/credpaths"
)

// Every credpaths.CredPaths entry must have a matching metadata-read grant in
// policyReconcilerRead. A missing grant is a 403, and SampleOpenBao treats any
// non-404 failure as fatal — so one ungranted path silently takes down the seal
// gauge and every OTHER credential's age with it, which is the opposite of what
// widening the coverage was for.
func TestCredPathsAreGrantedInReconcilerPolicy(t *testing.T) {
	for _, cp := range credpaths.CredPaths {
		meta := strings.Replace(cp.Path, "secret/", "secret/metadata/", 1)
		if !strings.Contains(policyReconcilerRead, `path "`+meta+`"`) {
			t.Errorf("credpaths.CredPaths has %s but policyReconcilerRead grants no read on %q — the sampler will 403 and fail the whole lane", cp.Path, meta)
		}
		switch cp.Class {
		case credpaths.CredClassAutomated, credpaths.CredClassGenerateOnce, credpaths.CredClassTracksSource, credpaths.CredClassStatic, credpaths.CredClassOnDemand:
		default:
			t.Errorf("credpaths.CredPaths entry %s has unknown class %q; the alert rules match on the known set", cp.Cred, cp.Class)
		}
	}
}

// The db-admin paths are discovered at sample time, so the pin test above cannot
// see them — their grants are a LIST on the collection plus a metadata-read
// prefix, and BOTH are needed: without the list the sampler 403s on discovery,
// without the read it 403s on the first cluster it finds. Either one fails the
// whole lane, not just the db series.
func TestDBAdminGrantsInReconcilerPolicy(t *testing.T) {
	for _, want := range []string{
		`path "secret/metadata/infra/db-admin/" { capabilities = ["list"] }`,
		`path "secret/metadata/infra/db-admin/*" { capabilities = ["read"] }`,
	} {
		if !strings.Contains(policyReconcilerRead, want) {
			t.Errorf("policyReconcilerRead is missing the db-admin grant %q", want)
		}
	}
	// The reconciler must never be able to read a database admin password — only
	// its metadata. A data grant here would be a real privilege escalation.
	// Matched on the `path "` prefix, not the bare string: the policy's own HCL
	// comment names secret/data/infra/db-admin/* to say it is NOT granted.
	if strings.Contains(policyReconcilerRead, `path "secret/data/infra/db-admin`) {
		t.Error("policyReconcilerRead grants DATA on infra/db-admin; the age sampler needs metadata only")
	}
}

// The backup CronJob's policy, against the two things it must agree with: the KV
// path the rotation table writes the backup key to (baosnap.KeyPath — the same
// constant the CronJob reads), and the restore boundary.
//
// LEAST PRIVILEGE IS THE ASSERTION, not decoration. `update` on the snapshot path
// is RESTORE, and `sudo` would unlock root-protected operations on it; a role that
// can take a snapshot every six hours must not be able to roll the store back to
// one. snapshot-force is the forced restore and must not appear at all.
func TestOpenBaoBackupPolicyIsReadOnlyAndCoupledToTheKeyPath(t *testing.T) {
	data := strings.Replace(baosnap.KeyPath, "secret/", "secret/data/", 1)
	if !strings.Contains(policyOpenBaoBackup, `path "`+data+`"`) {
		t.Errorf("openbao-backup policy does not grant %s — the CronJob reads its object-storage key there", data)
	}
	if !strings.Contains(policyOpenBaoBackup, `path "sys/storage/raft/snapshot"          { capabilities = ["read"] }`) {
		t.Error("openbao-backup policy must grant exactly read on sys/storage/raft/snapshot")
	}
	for _, forbidden := range []string{"update", "sudo", "create", "delete", "snapshot-force", `"*"`} {
		if strings.Contains(policyOpenBaoBackup, forbidden) {
			t.Errorf("openbao-backup policy contains %q — the backup identity must not be able to restore or write", forbidden)
		}
	}
	// The rotator writes the key the CronJob reads; without this grant the first
	// rotation 403s and the reconciler's linode-creds lane fails every pass.
	if !strings.Contains(policyLinodeRotator, `path "`+data+`"     { capabilities = ["create", "update", "read"] }`) {
		t.Errorf("linode-rotator policy cannot write %s — the in-cluster rotator owns that key", data)
	}
	// The role binds the identity the CronJob runs as.
	var role []string
	for _, s := range baoConfigureSteps("", "", nil, clusterspec.OpenBaoExtensions{}) {
		if len(s.args) > 1 && s.args[1] == "auth/kubernetes/role/"+baosnap.Role {
			role = s.args
		}
	}
	want := []string{"bound_service_account_names=" + baosnap.ServiceAccount,
		"bound_service_account_namespaces=" + baosnap.Namespace, "policies=openbao-backup"}
	for _, w := range want {
		found := false
		for _, a := range role {
			found = found || a == w
		}
		if !found {
			t.Errorf("kubernetes role %s missing %q (args %v)", baosnap.Role, w, role)
		}
	}
}
