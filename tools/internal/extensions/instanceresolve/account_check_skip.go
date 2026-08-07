package instanceresolve

// account_check_skip.go — say when an account-side validation did not run.
//
// `llz env add` checks --region and --obj-cluster against the Linode account
// before authoring a spec against them, because the two identifier spaces overlap
// (`us-sea` is a region, `us-sea-1` is an OBJ cluster, `de-fra-2` is a region) so
// no local rule can tell a swap from a typo. Both checks are best-effort by
// design and must never fail a run that used to work.
//
// The bug was not the degradation, it was the SILENCE. AccountRegions and
// objClustersInRegion folded "no token" and "the API rejected our token" into one
// unannounced `ok=false`, so:
//
//	LINODE_TOKEN=<expired> llz env add lab --region us-sea --obj-cluster us-sea-1
//
// exited 0 having validated nothing, looking exactly like a run that validated
// everything. The quickstart promises these checks in two places and explains
// that they are what stands between a typo and a 20-minute apply failure, and
// `llz doctor` reports the identical 401 loudly — so the tooling already knew;
// it just did not say so at the step that commits the value.
//
// The two cases need different words because they have different fixes: no token
// means "you skipped a quickstart step", a rejected token means "your PAT is
// expired or under-scoped, and it will fail the build too".

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// errEmptyAccountListing stands in for an API call that succeeded but returned
// nothing — indistinguishable from a broken lookup for our purposes, and equally
// not evidence that the value is wrong.
var errEmptyAccountListing = errors.New("the Linode API returned an empty list")

// firstNonNilErr picks err when non-nil, else fallback. Lets a caller collapse
// "call failed" and "call returned nothing" into one report without losing which
// one happened.
func firstNonNilErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

// accountCheckSkipReported keeps the notice to once per process. checkRegion and
// resolveOBJCluster both run in a single `llz env add`, and printing the same
// "your token did not work" paragraph twice reads like two different problems.
//
// Process-global, which is right for a CLI (one `env add` per process) and a
// hazard in tests: the first case to trip it silences every later one, so an
// assertion on the notice would pass or fail by test ORDER. resetAccountCheckSkip
// exists so a test can state its precondition instead of inheriting one.
var accountCheckSkipReported bool

// resetAccountCheckSkip re-arms the once-guard. Test-only in practice; kept
// beside the flag rather than in a _test.go file so the reason travels with it.
func resetAccountCheckSkip() { accountCheckSkipReported = false }

// reportSkippedAccountCheck tells the operator that a validation they were
// promised did not happen. cause is nil for "no token configured".
func reportSkippedAccountCheck(what string, cause error) {
	if accountCheckSkipReported {
		return
	}
	accountCheckSkipReported = true
	if cause == nil {
		fmt.Fprintf(os.Stderr, "%s %s was NOT checked against your Linode account — no LINODE_TOKEN is set.\n",
			color.Yellow("!"), what)
		fmt.Fprintln(os.Stderr, "  Region and object-storage-cluster ids overlap (`us-sea` is a region, `us-sea-1`")
		fmt.Fprintln(os.Stderr, "  is an OBJ cluster), so a swapped or typo'd value is caught by nothing local —")
		fmt.Fprintln(os.Stderr, "  the first thing to notice is `terraform apply`, ~20 minutes in.")
		fmt.Fprintf(os.Stderr, "  Export one and re-run to get the check: %s\n", color.Cyan("export LINODE_TOKEN=…"))
		return
	}
	fmt.Fprintf(os.Stderr, "%s %s was NOT checked against your Linode account — the API did not answer.\n",
		color.Yellow("!"), what)
	fmt.Fprintf(os.Stderr, "  %s\n", color.Dim(firstLine(cause.Error())))
	fmt.Fprintln(os.Stderr, "  A token that is set but expired, revoked, or under-scoped looks exactly like a")
	fmt.Fprintln(os.Stderr, "  validated run from here. It will also fail the build: the same credential is")
	fmt.Fprintln(os.Stderr, "  what CI uses. Check it, then re-run to get the check:")
	fmt.Fprintf(os.Stderr, "    %s\n", color.Cyan("llz doctor        # reports the same lookup, with the status code"))
}

// firstLine is a local three-line copy. internal/extensions/clusteraccess has one
// too; a shared package for three lines would cost more than the duplication.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
