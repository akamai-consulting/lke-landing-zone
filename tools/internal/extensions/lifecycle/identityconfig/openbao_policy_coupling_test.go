package identityconfig

// openbao_policy_coupling_test.go — the lane's credential table against the policy
// that grants it.
//
// THIS TEST SPANS THE EXTRACTION BOUNDARY, deliberately. credpaths.CredPaths
// is what the openbao-gauges lane samples; policyReconcilerRead is the OpenBao
// policy `llz ci openbao-configure` writes, and it stayed in internal/cli with the
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
