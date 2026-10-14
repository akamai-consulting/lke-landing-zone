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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
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

// accountCheckSkipReported keeps the notice from repeating. checkRegion,
// resolveOBJCluster and accountLKEVersions all run in a single `llz env add`, and
// printing the same paragraph three times reads like three different problems.
//
// THE GUARD IS PER CAUSE CLASS, NOT ONE FLAG FOR EVERYTHING, and a single global
// bool was wrong in a way that took a live reproduction to see:
//
//   - NO TOKEN is one fact about the ENVIRONMENT. It is equally true of every
//     lookup, so it is said once — the noise this guard was built to stop. Key "".
//   - AN API ERROR IS PER ROUTE. /v4/regions is unauthenticated and passes on a
//     bad token, /v4/object-storage/clusters 401s, and /v4beta/lke/tiers/…/versions
//     401s separately. Under one flag the obj-cluster failure consumed the guard
//     and the VERSIONS failure went silent — losing exactly the per-route
//     diagnostic #426 argues for, in the run where the operator most needs it.
//     Key the flag name, so each route gets one say.
//
// Process-global, which is right for a CLI (one `env add` per process) and a
// hazard in tests: the first case to trip it silences every later one, so an
// assertion on the notice would pass or fail by test ORDER. resetAccountCheckSkip
// exists so a test can state its precondition instead of inheriting one.
var accountCheckSkipReported = map[string]bool{}

// resetAccountCheckSkip re-arms the once-guard. Test-only in practice; kept
// beside the flag rather than in a _test.go file so the reason travels with it.
func resetAccountCheckSkip() { accountCheckSkipReported = map[string]bool{} }

// reportSkippedAccountCheck tells the operator that a validation they were
// promised did not happen. cause is nil for "no token configured".
//
// IT SPEAKS ONLY FOR THE LOOKUP IT WAS GIVEN, and a k8sVersion sentence briefly
// lived here that broke that. This notice is SHARED: `llz reap` reaches it through
// AccountRegions to sweep orphaned resources, and it seeds no spec at all — so a
// line about cluster.k8sVersion falling back to a compiled default was simply
// untrue there. `llz env add` reports its own version consequence in its banner
// ("scaffold default — the account could not be asked"), which is the right place:
// beside the value, in the one command that writes one.
//
// THROUGH cigate, SO CI SEES IT TOO — and that is not cosmetic. On a laptop this
// is a yellow paragraph nobody can miss. In a workflow it was plain step-log text
// inside a green step, which is precisely the state it exists to make visible: a
// run that validated nothing is indistinguishable from one that validated
// everything. `llz ci assert-k8s-version` made this same call for the same reason,
// and the repo's own e2e lane is the case that forced it here — its scaffold runs
// in the template repo, where a Linode token is optional, so "this check did not
// run" has to reach the run summary rather than line 400 of a step log.
//
// The alternative was three lines of `if [[ -z "$TOKEN" ]]` in the workflow, which
// is the untestable-inline-bash the budget gate exists to refuse — and it would
// have covered ONE caller, while this covers every one.
func reportSkippedAccountCheck(what string, cause error) {
	// "" for the no-token case — one environment fact, said once however many
	// lookups it stops. The flag name otherwise, so each ROUTE that failed gets to
	// say so. See accountCheckSkipReported.
	key := ""
	if cause != nil {
		key = what
	}
	if accountCheckSkipReported[key] {
		return
	}
	accountCheckSkipReported[key] = true
	if cause == nil {
		// ONE cigate.Warning CALL, NOT FOUR PRINTLNS. Under Actions a workflow
		// command ends at the first raw newline, so a multi-line message keeps its
		// headline in the annotation and drops the reason into step-log text — the
		// half that says what to do about it. cigate.Annotation escapes them.
		fmt.Fprintln(os.Stderr, cigate.Warning(fmt.Sprintf(
			"%s was NOT checked against your Linode account — no LINODE_TOKEN is set.\n"+
				"  Region and object-storage-cluster ids overlap (`us-sea` is a region, `us-sea-1` is an\n"+
				"  OBJ cluster), so a swapped or typo'd value is caught by nothing local — the first thing\n"+
				"  to notice is `terraform apply`, ~20 minutes in.\n"+
				"  Export one and re-run to get the check: export LINODE_TOKEN=…", what)))
		return
	}
	fmt.Fprintln(os.Stderr, cigate.Warning(fmt.Sprintf(
		"%s was NOT checked against your Linode account — the API did not answer.\n"+
			"  %s\n"+
			"  A token that is set but expired, revoked, or under-scoped looks exactly like a validated\n"+
			"  run from here. It will also fail the build: the same credential is what CI uses. Check it,\n"+
			"  then re-run to get the check: llz doctor  # reports the same lookup, with the status code",
		what, firstLine(cause.Error()))))
}

// firstLine is a local three-line copy. internal/extensions/clusteraccess has one
// too; a shared package for three lines would cost more than the duplication.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
