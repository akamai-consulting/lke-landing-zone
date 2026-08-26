package upgradeplan

// census.go — the one impure part of this gate, kept in its own file so the
// judgement in upgradeplan.go stays a pure function over parsed input.
//
// WHAT IT COSTS. One GET of /v4/object-storage/buckets, which returns every
// bucket on the account with an `objects` count already computed. No S3
// credentials, no per-bucket listing, no pagination of object keys — the
// expensive-sounding question ("is this bucket empty?") is a field on a response
// the API hands over in one call.
//
// ONLY ON A DESTRUCTIVE PLAN. Run() calls this only when there is something to
// weigh, so the ordinary clean plan — every plan on a settled instance — makes no
// network call at all and this gate stays offline in the case that runs most.
//
// SILENT ON FAILURE, BECAUSE THE FAILURE IS ALREADY SAFE. No token, an expired
// one, a network error and an account with no buckets all produce a nil census,
// and a nil census exempts nothing: every destructive finding blocks, which is
// precisely the behaviour this gate had before the census existed. So a broken
// lookup costs an operator the exemption, never their data — and there is nothing
// to warn about, because the strict answer is the correct one.

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfenc"
)

// bucketLookupTimeout bounds the one request. Short on purpose: this runs between
// a plan and an apply, and a gate that hangs on a slow API is a gate someone
// disables. Falling back to "no census" costs only the exemption.
const bucketLookupTimeout = 20 * time.Second

// LookupBuckets returns how many objects each Object Storage bucket on this
// account holds.
//
// A package var so tests drive the whole verdict without a token or a network —
// the alternative is threading a client through Run, Evaluate and RenameRemedy to
// reach one call site.
var LookupBuckets = func() BucketCensus {
	token := linodeToken()
	if token == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), bucketLookupTimeout)
	defer cancel()
	// THROUGH capability, NOT linode.NewClient. An unpoliced client here would carry
	// a token that can delete buckets into the step whose whole job is stopping
	// buckets being deleted; the policy built from this extension's cloud-read grant
	// refuses every mutating method at the one place all fifty-two client methods
	// funnel through. TestNoNewUnpolicedLinodeClients holds it.
	client := capability.CloudFor(Extension().MustBindingOf(extension.Assertion, extension.Configured)).
		Client(token, bucketLookupTimeout)
	buckets, err := client.ListObjectStorageBuckets(ctx)
	if err != nil {
		return nil
	}
	// The KEY labels carry the same prefix, and a recommendation that ignores them
	// is half an answer: `llz reap` and the rotation table match key labels exactly,
	// so a prefix that is right for the buckets and wrong for the keys silently
	// breaks rotation. A failed keys call leaves the slice empty, which the caller
	// reads as "no evidence" rather than "no keys".
	keys, err := client.ListObjectStorageKeys(ctx)
	if err != nil {
		keys = nil
	}
	lastKeyLabels = keyLabelsFrom(keys)
	return censusFrom(buckets)
}

// lastKeyLabels carries the key half of the same lookup to the caller without
// changing BucketCensus into a struct — the map's nil-means-unknown is what the
// whole exemption rests on, and it is not worth losing for a second field.
// Written by LookupBuckets and read by KeyLabels immediately after, on one
// goroutine, in one command invocation.
var lastKeyLabels []string

// KeyLabels returns the Object Storage key labels seen by the most recent
// LookupBuckets. Empty means no evidence, never "no keys".
func KeyLabels() []string { return lastKeyLabels }

// keyLabelsFrom pulls the OBJ key labels out of a keys listing.
func keyLabelsFrom(keys []map[string]any) []string {
	var out []string
	for _, k := range keys {
		if label, ok := k["label"].(string); ok && label != "" {
			out = append(out, label)
		}
	}
	sort.Strings(out)
	return out
}

// censusFrom turns the API's bucket list into the census.
//
// SPLIT FROM THE REQUEST so it can be tested, and it is the half that was wrong:
// the decode dropped every bucket against the real API while every test passed,
// because the tests built a BucketCensus by hand and nothing exercised this loop.
func censusFrom(buckets []map[string]any) BucketCensus {
	census := BucketCensus{}
	for _, b := range buckets {
		label, ok := b["label"].(string)
		if !ok || label == "" {
			continue
		}
		n, ok := objectCountOf(b["objects"])
		if !ok {
			continue
		}
		census[label] = n
	}
	return census
}

// linodeToken finds the credential this lookup needs, in CI and on a workstation.
//
// THE CACHE FALLBACK IS NOT A CONVENIENCE. In the pipeline the job exports
// LINODE_TOKEN and the environment is the whole answer. Run by hand it is not:
// this is a SEPARATE PROCESS from the `llz tofu` that produced the plan, so none
// of that command's hydrated environment reaches it, and an operator following the
// documented local check got "unknown" for every bucket — the fail-closed answer,
// which is safe and also exactly no help. The credential is sitting in the same
// `.llz/secrets.env` that `llz tofu` reads.
//
// The ENVIRONMENT STILL WINS, so CI is unaffected and an operator who exported a
// token deliberately keeps it.
func linodeToken() string {
	// ONE RESOLUTION, tfenc's, rather than a second opinion here. Reading the
	// environment first looked obviously right and was wrong for the same reason it
	// was wrong inside `llz tofu`: LINODE_TOKEN is a GENERIC name, an operator with
	// any Linode account has one exported, and it is not necessarily the account
	// holding this instance's buckets. Preferring it produced a failed lookup, a nil
	// census, and "no evidence" — the fail-closed answer, which is safe and exactly
	// no help — on a workstation whose `.llz` had the right credential all along.
	//
	// Hydrate handles the whole precedence: this instance's cached value beats an
	// ambient generic name, an explicitly exported LINODE_API_TOKEN beats the cache,
	// and OUTSIDE an instance it contributes nothing so Value() falls through to the
	// environment — which is the CI case, where the job exports the token and no
	// cache exists.
	local, err := tfenc.Hydrate(".")
	if err == nil {
		if t := local.Value("LINODE_TOKEN"); t != "" {
			return t
		}
	}
	// The workflow spells it LINODE_API_TOKEN everywhere except the env it hands to
	// Terraform, and this step reads whichever the job happened to set.
	return os.Getenv("LINODE_API_TOKEN")
}

// objectCountOf reads Linode's per-bucket `objects` field.
//
// json.Number, NOT float64. The client decodes with UseNumber() — every numeric
// field in this package arrives as json.Number — so a float64 assertion matches
// NOTHING and silently produced an empty census against the real API: every
// bucket dropped, every replace blocked, the exemption dead on arrival while all
// the unit tests passed. They passed because they build a BucketCensus directly
// and never decode a response, which is the third time in this change that a gate
// over the parts missed a broken join between them.
//
// float64 and string are accepted too. Neither is what the API sends today, and
// both are one decoder setting away from being what it sends tomorrow — the cost
// is two cases, and the failure mode they prevent is silent.
//
// A count that cannot be read is NOT ZERO. Returning false leaves the bucket out
// of the census, and an absent bucket blocks: the one guess here that could cost
// data is guessing "empty".
func objectCountOf(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	case float64:
		return int(n), true
	case string:
		i, err := strconv.Atoi(n)
		if err != nil {
			return 0, false
		}
		return i, true
	}
	return 0, false
}
