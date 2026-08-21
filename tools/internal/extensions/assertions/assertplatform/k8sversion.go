package assertplatform

// k8sversion.go — `llz ci assert-k8s-version`, the preflight that refuses to start
// a ~15-minute cluster apply against a `k8sVersion` THIS ACCOUNT cannot build.
//
// WHY THIS IS A GATE AND NOT A LOOKUP. LKE-Enterprise version availability is
// per-ACCOUNT, and that is the whole subtlety. Measured in the same hour on
// 2026-08-11: one Linode account still listed `v1.33.6+lke7` while the e2e account
// offered exactly two versions, `v1.34.6+lke2` and `v1.32.9+lke4`. So "is this pin
// valid" has no answer except against the account that will build the cluster —
// it cannot be settled by reading release notes, and a run that worked at 16:04
// was rejected at 17:06 on the same pin.
//
// Unchecked, the pin reaches terraform apply and dies there:
//
//	Error: failed to create LKE cluster: [400] [k8s_version] k8s_version is not valid
//
// — ~15 minutes in, and by then the VPC, object-storage and database roots have
// also applied, so the cleanup is real infrastructure rather than a red X. That
// cost a release-e2e round. `apply-vpc` already front-loads the cheap questions
// (assert-image-fresh, require-secret, validate-tokens, assert-apl-version)
// precisely so a long apply is not spent on something answerable in seconds; this
// belongs in that battery.
//
// WHAT IT ACTUALLY PREVENTS, STATED NARROWLY. It runs in apply-vpc, and apply-vpc
// is what apply-cluster `needs:` — so it stops the ~15-minute CLUSTER apply and
// nothing else. `apply-object-storage` and `apply-databases` declare no `needs:`
// and run in PARALLEL, so on `module: all` they still apply. That is deliberate
// rather than an oversight: buckets and the Managed Postgres instance are reused
// by the retry once the pin is fixed, so they are not wasted work, and serialising
// them behind this check would slow every good build to save nothing. Don't read
// the incident timeline above as a list of things this gate stops.
//
// IT DEGRADES THE WAY ITS NEIGHBOURS DO: warn and PASS when the API cannot be
// reached, the token lacks the scope, or the account answers with a shape this
// does not recognise. The e2e token 401s on /v4beta/lke/tiers/enterprise/versions
// from some contexts, and a build must not be blocked on a question nobody could
// ask. Only a definite answer — a catalog that came back, and does not contain the
// pin — fails.
//
// AND THAT LENIENCY IS NOW MEASURED, WHICH IT WAS NOT (issue #449). Warn-and-pass
// is the right rule and it has one cost: in a pipeline where the read ALWAYS
// fails, this gate is permanently inert and every run of it is green — the class
// it exists for stays open wearing a green check, in the exact environment the
// 2026-08-11 incident happened in. Two things now stop that from being silent:
//
//   - EVERY RUN EMITS A VERDICT RECORD (verdict.go) naming what was decided and
//     whether the ACCOUNT decided it. `decided=no` on every run of a cycle is a
//     countable fact rather than a warning somebody had to be looking at.
//   - `llz ci validate-tokens` PROBES THIS EXACT ROUTE and, on a refusal, asks a
//     second LKE route needing the same grant. Refused at both ⇒ the PAT is
//     under-scoped, which blocks the build (the cluster apply would fail on it
//     regardless, ~15 minutes later). Accepted at the second ⇒ the token is
//     scoped and the ROUTE refuses it, which is reported as this gate being inert
//     here. That preflight runs in the same job, a few steps before this one.
//
// WHAT IS STILL NOT MEASURED, so the next reader does not re-derive it: nothing
// outside the run asserts on those records. An e2e cycle in which every record
// says `decided=no` is visible in each run's summary and in nothing that fails.
// Closing that means the release-e2e lane reading the dispatched instance run's
// verdicts back — worth doing, and a different change from this one.
//
// `llz doctor` has reported this advisorily since the section was written, and
// still does. What was missing is that doctor is not run before an apply.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// listAccountLKEVersions asks the account which LKE-Enterprise versions it may
// build. A seam so the tests never touch the network.
//
// The client comes from THIS BINDING's grants (cloud-read), not from a bare
// constructor: the lane looks at an account and must not be able to change one.
var listAccountLKEVersions = func(ctx context.Context) ([]string, error) {
	c, err := k8sVersionClient()
	if err != nil {
		return nil, err
	}
	return c.ListLKEVersions(ctx, linode.LKETierEnterprise)
}

// listAccountClusters is the second read, and it only happens on the failing
// path — see linode.ClusterRunsVersion for why a cluster already at the pin
// exempts the deployment. A good build pays for one request, not two.
var listAccountClusters = func(ctx context.Context) ([]map[string]any, error) {
	c, err := k8sVersionClient()
	if err != nil {
		return nil, err
	}
	return c.ListClusters(ctx)
}

// The three budgets, and they are three because A READ IS NOT A REQUEST.
//
// http.Client.Timeout bounds ONE HTTP call, and both of these reads go through
// listAllPages, which issues one call per 100-item page. Deriving the whole-probe
// budget from the per-request timeout therefore under-counts any account big
// enough to paginate: on one with a few hundred LKE clusters the exemption read
// would be cancelled by the parent context, `clustersForExemption` would report an
// error, and the catalog verdict would stand — failing an apply for a deployment
// whose cluster is running the pin. The arithmetic test asserted the same
// under-counting formula, so it could not have caught it.
//
// So the middle term is explicit: one READ is allowed several pages' worth of
// requests, and the probe is sized from that and the attempt count. Beyond
// k8sVersionReadPages the read is still cut off — that residue is real and
// bounded, and it degrades the safe way for the catalog read (UNCHECKED) and the
// unsafe way for the exemption read, which is why the page allowance is generous
// rather than tight.
const (
	k8sVersionRequestTimeout = 20 * time.Second
	// Pages one read may spend before the parent context cuts it off — 500 clusters
	// or versions on one account.
	k8sVersionReadPages  = 5
	k8sVersionReadBudget = k8sVersionReadPages * k8sVersionRequestTimeout
	// One catalog read plus every attempt the exemption is allowed, plus the pauses
	// between them, plus slack.
	k8sVersionProbeTimeout = (1+linode.ClusterReadAttempts)*k8sVersionReadBudget +
		linode.ClusterReadAttempts*linode.ClusterReadRetryPause + 10*time.Second
)

// k8sVersionSleep is the pause between exemption attempts. A seam so the tests do
// not spend it.
var k8sVersionSleep = time.Sleep

func k8sVersionClient() (*linode.Client, error) {
	token, err := linode.TokenFromEnv()
	if err != nil {
		return nil, err
	}
	return capability.CloudFor(k8sVersionBinding()).Client(token, k8sVersionRequestTimeout), nil
}

// specK8sVersion resolves the pin this preflight is about.
//
// THE TWO SIDES OF THIS GATE FAIL IN OPPOSITE DIRECTIONS, and that is the whole
// design rather than an inconsistency. The CATALOG is a third party's answer over
// the network, so every uncertainty there warns and passes — a Linode blip must
// not fail a build on a good spec. The SPEC is the corpus this gate owns and can
// read with certainty, so vacuity there FAILS: a preflight that examined nothing
// and said "ok" is indistinguishable from one that verified something, which is
// the failure docs/e2e-gates.md is written around.
//
// Concretely, `--env` naming a deployment the spec does not define was the live
// hole: an empty $REGION or a typo printed "no k8sVersion pinned for deployment
// "prd"" and exited 0, and since validate.go REQUIRES cluster.k8sVersion on every
// defined deployment, that was the only way the quiet arm could fire at all. The
// gate went dead in exactly the state where its output claimed it had looked.
//
// (nothing, nil) is reserved for the one honest skip: no spec on disk. `llz ci`
// verbs run in the template repo too, and there is genuinely nothing to check.
func specK8sVersion(env string) (want, label, region string, shared, noSpec bool, err error) {
	lz, present, lerr := deps.LoadSpec()
	switch {
	case lerr != nil:
		// UNCHECKED, not "bad pin": an unreadable spec is a different failure with
		// its own gates (`llz validate`), and this one has no verdict to offer.
		return "", "", "", false, false, fmt.Errorf("could not read the LandingZone spec, so the k8sVersion pin is UNCHECKED: %w", lerr)
	case !present || lz == nil:
		return "", "", "", false, true, nil
	}
	e, ok := lz.Env(env)
	if !ok {
		//lint:ignore ST1005 multi-line operator diagnostic: the trailing period ends a sentence of remediation prose that continues on the next line
		return "", "", "", false, false, fmt.Errorf(`the spec defines no deployment %q, so this preflight checked NOTHING (it defines: %s).

An empty or mistyped --env is the one way this gate can pass having examined
nothing — cluster.k8sVersion is required on every deployment the spec DOES define,
so a named deployment always has a pin to check.`,
			env, strings.Join(lz.EnvNames(), ", "))
	}
	if v := strings.TrimSpace(e.Cluster.K8sVersion); v != "" {
		// `shared` decides which FILE the remediation names. applyInheritance folds
		// spec.defaults into every environment before Env() sees it, so the merged
		// value cannot say where it came from — but a pin equal to the shared default
		// is one landingzone.yaml plausibly owns, and that is the distinction the
		// operator needs. Telling someone to edit environments/<env>.yaml when the
		// value lives in spec.defaults unblocks ONE deployment and leaves the real
		// stale value un-named, so the rest fail one dispatch at a time.
		shared := v == strings.TrimSpace(lz.Spec.Defaults.Cluster.K8sVersion)
		return v, strings.TrimSpace(e.Cluster.ClusterLabel), strings.TrimSpace(e.Cluster.Region), shared, false, nil
	}
	return "", "", "", false, false, fmt.Errorf("deployment %q pins no cluster.k8sVersion — it is a required field, "+
		"and an empty version is not one any account can build", env)
}

// k8sPreflight is what the decision function hands back: the line to print, and
// the RECORD of what was decided.
//
// The record travels WITH the message rather than being re-derived from it by the
// caller. A caller that inspected the note to work out whether the account had
// answered would be a second implementation of the same rule, disagreeing with
// this one the first time a message is reworded — and the thing it would get
// wrong is precisely "did this gate decide anything", which is the question
// issue #449 exists because nobody could answer.
type k8sPreflight struct {
	note   string
	kind   k8sVerdictKind
	reason string
}

// k8sVersionVerdict is the whole decision, split from the I/O so both arms are
// unit-testable: the line to print, and the error that fails the build.
//
// EVERY UNCERTAIN ARM RETURNS A NIL ERROR, and that is the property to protect. A
// list that could not be fetched, an empty list, and a list too coarse to settle
// the pin are all "this check could not be answered" — not "the pin is bad".
// Fail-closed is the right default for a gate reading a corpus it OWNS; this one
// reads a third party's catalog over the network, where the same instinct would
// convert a Linode blip into a failed build on a perfectly good spec. #426 is
// explicit about it: the e2e token 401s on the versions route from some contexts
// and "that must not become a hard failure".
//
// SO EVERY ONE OF THEM ANNOUNCES ITSELF AS AN ANNOTATION. The cost of that rule
// is that a token which ALWAYS 401s leaves the gate permanently inert, and a
// warning buried in a green job's log is indistinguishable from a real pass —
// which is how the class this gate exists for comes back wearing a green check.
// `::warning::` puts it in the run summary instead, where "this check did not
// run" is visible without reading the step, and every arm now also emits a
// machine-readable verdict record (verdict.go) so "did not run" is countable
// rather than merely visible.
//
// WHETHER SUCH A TOKEN SHOULD BE A HARD FAILURE IS A DECISION ABOUT CREDENTIALS,
// AND IT HAS BEEN MADE — in `llz ci validate-tokens`, which is where a credential
// verdict belongs. It probes linode.LKEVersionsPath directly and separates the two
// 401s this verb cannot tell apart: a PAT missing the LKE grant now BLOCKS there,
// while a correctly-scoped PAT refused at this route is reported as this gate
// being inert. Neither decision moved into this file, and that is the point —
// this verb still judges a pin, not a token.
//
// THROUGH cigate.Warning, which decides the rendering. Under Actions a workflow
// command ends at the first raw newline, so written directly these kept their
// headline in the annotation and dropped the API error and the PAT-scope hint into
// plain step-log text — the half that says WHY, missing from the one place the
// annotation exists to put it. And OUTSIDE Actions the escaping is a
// disfigurement: this verb runs on laptops, where `%0A` in place of the remedy is
// its own way of hiding the reason. One helper, so a caller cannot get the prefix
// right and the escaping wrong.
func k8sVersionVerdict(env, want string, shared bool, offered []string, listErr error) (k8sPreflight, error) {
	switch {
	case listErr != nil:
		return k8sPreflight{
			note: cigate.Warning(fmt.Sprintf("could not list the account's LKE-Enterprise versions, so %q is UNCHECKED — "+
				"if it is a retired or mistyped pin the cluster apply will fail with `[400] [k8s_version] k8s_version is not valid`.\n"+
				"  %s\n"+
				"  `llz ci validate-tokens` probes this exact route and says whether the PAT is scoped for it; a refusal there "+
				"that reproduces every run means this preflight is INERT in this pipeline, not that it passed.", want, cigate.FirstLine(listErr.Error()))),
			kind:   k8sUndecided,
			reason: reasonCatalogUnreadable,
		}, nil
	case len(offered) == 0:
		return k8sPreflight{
			note: cigate.Warning(fmt.Sprintf("the account reported NO LKE-Enterprise versions, so %q is UNCHECKED — "+
				"an empty catalog is an unexpected answer, not proof the pin is wrong.", want)),
			kind:   k8sUndecided,
			reason: reasonCatalogEmpty,
		}, nil
	}

	verdict, nearest := linode.CheckVersion(want, offered)
	switch verdict {
	case linode.VersionOffered:
		return k8sPreflight{
			note: fmt.Sprintf("k8sVersion %s (deployment %q) is offered by this Linode account.", want, env),
			kind: k8sOffered,
		}, nil
	case linode.VersionUnknown:
		// The catalog answered, but not in a spelling that can disprove this pin —
		// a list of bare major.minor cannot reject a `+lke` build. Guessing at it is
		// how a preflight turns into a build blocked on a spelling difference.
		return k8sPreflight{
			note: cigate.Warning(fmt.Sprintf("the account's version list (%s) cannot settle %q, so it is UNCHECKED — "+
				"this is not the catalog shape this gate was measured against.", strings.Join(offered, ", "), want)),
			kind:   k8sUndecided,
			reason: reasonCatalogCoarse,
		}, nil
	}
	if nearest != "" {
		// One character apart, so name it instead of printing the whole catalog and
		// letting the operator find the difference.
		return k8sPreflight{kind: k8sNotOffered}, fmt.Errorf(`k8sVersion %q (deployment %q) is NOT offered by this Linode account — but %q is.

The two are the same version spelled differently, and terraform sends
cluster.k8sVersion VERBATIM, so the catalog's spelling is the one that works.

Fix: set cluster.k8sVersion to %q %s`, want, env, nearest, nearest, k8sVersionFixIn(env, shared))
	}
	return k8sPreflight{kind: k8sNotOffered}, fmt.Errorf(`k8sVersion %q (deployment %q) is NOT offered by this Linode account.

  offered: %s

LKE-Enterprise version availability is PER-ACCOUNT, so a version another account
can build — or one that was valid yesterday — is not evidence about this one.
Unchanged, the cluster apply fails ~15 minutes in with:

  Error: failed to create LKE cluster: [400] [k8s_version] k8s_version is not valid

Fix: %s`,
		want, env, strings.Join(offered, ", "), k8sVersionFixHint(env, shared))
}

// EACH READ IS BOUNDED ON ITS OWN, not just the probe as a whole. The read budget
// used to size the parent deadline and nothing else — so a slow catalog read could
// spend most of the probe and hand the exemption read a nearly-dead context, which
// is the exact regression that constant and its arithmetic test claim to prevent.
// (The test checks the SUM, so it could not notice.) The parent still wins when it
// is nearer, which is what makes these slices of the budget rather than extensions
// of it.

// errProbeBudget marks the exemption read having been cut off by OUR OWN
// deadline rather than by the API. The two want opposite advice — one is "re-run,
// it may have been transient", the other "a re-run expires identically" — and
// telling an operator to retry a deterministic timeout is how a real problem gets
// four wasted dispatches.
var errProbeBudget = errors.New("the probe budget expired before the account's clusters could be listed")

// clustersForExemption reads the account's clusters, retrying a bounded number of
// times. The exemption is a PROOF that terraform will not send the pin, and the
// caller refuses to let an unprovable one acquit — so a single 503 must not be
// what decides that a running deployment is unbuildable. See
// linode.ClusterReadAttempts.
func clustersForExemption(ctx context.Context) ([]map[string]any, error) {
	var err error
	for attempt := 1; attempt <= linode.ClusterReadAttempts; attempt++ {
		var clusters []map[string]any
		rctx, rcancel := context.WithTimeout(ctx, k8sVersionReadBudget)
		clusters, err = listAccountClusters(rctx)
		rcancel()
		if err == nil {
			return clusters, nil
		}
		// OUR OWN DEADLINE IS NOT A TRANSIENT ERROR. Retrying against an expired
		// context burns the pause and fails again for the same reason, and the caller
		// then tells the operator to "re-run if that read was transient" about a budget
		// that will expire identically next time. Stop and say which it was.
		//
		// ASKED OF ctx ONLY, NEVER OF THE ERROR — and the first version of this did
		// both, which broke the retry it was added next to. `http.Client.Timeout`
		// reports as a WRAPPED context.DeadlineExceeded (measured: `Get "…": context
		// deadline exceeded (Client.Timeout exceeded while awaiting headers)`, and
		// errors.Is says true), so classifying on the error made one slow request
		// indistinguishable from the whole probe running out. A single slow
		// /lke/clusters call then short-circuited the retry with ~90% of the budget
		// unspent, failed apply-vpc for a deployment whose cluster is running the pin,
		// and told the operator a re-run was pointless.
		//
		// ctx.Err() is read AFTER the call returns, so there is nothing to race: if the
		// parent expired, it is set.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w after %v: %v", errProbeBudget, k8sVersionProbeTimeout, cigate.FirstLine(err.Error()))
		}
		if attempt < linode.ClusterReadAttempts {
			fmt.Printf("  listing the account's clusters failed (attempt %d/%d), retrying: %s\n",
				attempt, linode.ClusterReadAttempts, cigate.FirstLine(err.Error()))
			k8sVersionSleep(linode.ClusterReadRetryPause)
		}
	}
	return nil, err
}

// k8sVersionFixHint names the file that actually holds the pin. See specK8sVersion
// for why the merged spec cannot say for certain, and why naming the wrong one is
// worse than naming both.
func k8sVersionFixHint(env string, shared bool) string {
	return "set cluster.k8sVersion to one of the versions above " + k8sVersionFixIn(env, shared)
}

// k8sVersionFixIn is the FILE half on its own, so a caller that already named the
// value to use does not have to say "one of the versions above" about a list it
// deliberately did not print — which is what the near-miss message did, leaving
// the phrase with no referent.
func k8sVersionFixIn(env string, shared bool) string {
	if shared {
		return fmt.Sprintf("in landingzone.yaml (spec.defaults.cluster.k8sVersion, which EVERY deployment "+
			"inherits — this deployment's pin matches it), or in environments/%s.yaml to override just this one.", env)
	}
	return fmt.Sprintf("in environments/%s.yaml.", env)
}

func assertK8sVersion(env string) error {
	// ONE DEFERRED EMIT, ON EVERY PATH. Recording at each return instead would put
	// the fail-closed property in the hands of whoever adds the next arm — and the
	// arm most likely to be added is another soft pass, which is the one that must
	// not be able to go unrecorded. The closure reads v when the function returns,
	// so every branch below only has to fill it in.
	v := k8sVerdict{env: env}
	defer func() { emitK8sVerdict(v) }()

	want, label, region, shared, noSpec, err := specK8sVersion(env)
	v.pin = want
	switch {
	case err != nil:
		v.kind, v.reason = k8sSpecRejected, reasonSpecUnusable
		return err
	case noSpec:
		// The one honest skip, and it stops WITHOUT calling the API: a verb that
		// reaches for a token it does not need fails wherever there is none.
		v.kind, v.reason = k8sNoSpec, reasonNoSpec
		fmt.Println("no LandingZone spec here — nothing to check against the account.")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), k8sVersionProbeTimeout)
	defer cancel()

	cctx, ccancel := context.WithTimeout(ctx, k8sVersionReadBudget)
	offered, lerr := listAccountLKEVersions(cctx)
	ccancel()
	p, fatal := k8sVersionVerdict(env, want, shared, offered, lerr)
	v.kind, v.reason = p.kind, p.reason
	if fatal == nil {
		fmt.Println(p.note)
		return nil
	}

	// LAST QUESTION BEFORE FAILING: is terraform even going to send this string?
	// A cluster already running the pin plans no diff, so a version that has since
	// left the account's catalog is not this apply's problem — see
	// linode.ClusterRunsVersion. Asked only here, so a good build pays nothing.
	clusters, cerr := clustersForExemption(ctx)
	if errors.Is(cerr, errProbeBudget) {
		fmt.Println(cigate.Warning(fmt.Sprintf("%s is not in the account's catalog, and whether a cluster "+
			"already runs it could not be checked: %s.\n"+
			"  A re-run expires the same way — this account's cluster list needs more than the probe budget.\n"+
			"  The catalog verdict stands.", want, cigate.FirstLine(cerr.Error()))))
		return fatal
	}
	if cerr != nil {
		// AN UNPROVABLE EXEMPTION DOES NOT DISCHARGE A PROVEN FAILURE. This used to
		// warn and return nil, on the "uncertainty never blocks" rule — but that rule
		// is about whether the pin is buildable, and that question is already
		// ANSWERED here: the catalog read succeeded and the pin is not in it. The only
		// thing in doubt is an escape hatch, and an unreadable cluster list is not
		// evidence that a cluster exists. Passing here let the original incident
		// through — retired pin, no cluster yet, one 503 on /lke/clusters — and the
		// apply still died fifteen minutes in.
		fmt.Printf("NOTE: whether a cluster already runs %s could not be checked, so the catalog "+
			"verdict stands. Re-run if that read was transient.\n  %s\n",
			want, cigate.FirstLine(cerr.Error()))
		return fatal
	}
	if linode.ClusterRunsVersion(clusters, label, region, want) {
		v.kind, v.reason = k8sExempt, ""
		// THE ONE ARM THAT WAVES A DEFINITELY-RETIRED PIN THROUGH, so it is the last
		// one that may render as a silent green step. Every other soft pass here
		// annotates; this one did not, which made the most consequential "yes, but"
		// in the verb the quietest thing it prints.
		fmt.Println(cigate.Warning(fmt.Sprintf("k8sVersion %s (deployment %q) is no longer in the account's catalog, "+
			"but the cluster already runs it — terraform plans no change to k8s_version, so this apply is unaffected.\n"+
			"  This deployment can no longer be RE-CREATED until the pin moves to a listed version.", want, env)))
		return nil
	}
	return fatal
}
