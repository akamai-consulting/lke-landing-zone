package instanceresolve

// k8sversion_resolve.go — seed `cluster.k8sVersion` from the ACCOUNT at
// `llz env add`, instead of from a literal baked into the scaffold.
//
// WHY A LITERAL CANNOT WORK HERE, AND THIS IS THE WHOLE ARGUMENT. LKE-Enterprise
// version availability is per-ACCOUNT and rotates fast: measured on 2026-08-11,
// one account still listed `v1.33.6+lke7` while the e2e account offered exactly
// `v1.34.6+lke2` and `v1.32.9+lke4` — in the same hour. A pin accepted at 16:04
// was rejected at 17:06. So a version compiled into the scaffold is not stale
// eventually, it is stale by construction, and refreshing it is a manual edit in
// several files that nothing gates.
//
// WHY IT GOT WORSE RATHER THAN BETTER WHEN THE GATE LANDED. `llz ci
// assert-k8s-version` and `llz doctor` now hard-fail on a pin the account cannot
// build, which is the improvement — but it means a fresh `llz new` → `llz env add`
// → `llz up` could stop at the doctor stage on a version THE OPERATOR NEVER CHOSE.
// The failure names the versions the account does offer, so it is recoverable; it
// is still the tool asking the operator to fix its own default. `llz env add`
// already asks this account about `--region` and `--obj-cluster` in the same
// breath (see region_resolve.go), so the answer was one request away.
//
// BEST-EFFORT, EXACTLY LIKE ITS NEIGHBOURS. `llz env add` has never needed a
// Linode token or a network and must not start: with no token, an unreachable API,
// or a catalog shape this cannot read, the caller keeps its baked default and the
// run proceeds as it always did. The one thing that DOES fail is an explicit
// `--k8s-version` the account definitely cannot build — the same licence
// CheckRegion and ResolveOBJCluster already take, and the same one
// `linode.CheckVersion` grants: only a catalog that came back naming full builds
// may reject anything.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// LKEVersionLister is the slice of the Linode client this needs — a seam so the
// tests never touch the network.
// LKEVersionLister is EXPORTED for the same reason RegionLister is: the gate that
// proves `llz env add` still asks the account lives in the package that WIRES this
// (extensions/lifecycle/environments), and a fake it cannot name is a seam only
// this package can test through — which is how the wiring half goes unwatched.
//
// BOTH READS ARE ON ONE INTERFACE, not two seams over one account. #453 added the
// cluster read, and the tempting shape was a second package var with its own token
// discovery — which is two places to wire, and a fake that satisfies one of them
// silently answers "no clusters" for the other. That is the exact failure mode this
// repo already paid for once (a seam whose default said "the caller assigns it" and
// no caller did). One client, one substitution point, both questions.
type LKEVersionLister interface {
	ListLKEVersions(ctx context.Context, tier string) ([]string, error)
	// ListClusters answers "does a cluster for this deployment already exist, and
	// what does it run?" — the question linode.ClusterVersionFor matches on.
	ListClusters(ctx context.Context) ([]map[string]any, error)
}

// LKEVersionClient returns a live client, or nil when no token is configured.
// Package var so tests substitute a fake.
var LKEVersionClient = func() LKEVersionLister {
	tok := firstNonEmpty(os.Getenv("LINODE_TOKEN"), os.Getenv("LINODE_API_TOKEN"))
	if tok == "" {
		return nil
	}
	// accountRequestTimeout, NOT a second 20s: the read budget below is SIZED from
	// this client's per-request timeout, so two spellings of one number would let a
	// change to the constant silently desynchronize the two.
	return linode.NewClient(tok, accountRequestTimeout)
}

// accountRequestTimeout bounds one HTTP call; accountReadBudget bounds one READ,
// and clusterProbeTimeout bounds the whole retried cluster read.
//
// A READ IS NOT A REQUEST. Both routes this file calls go through listAllPages,
// which issues one request per 100-item page, so a budget sized from the
// per-request timeout under-counts any account big enough to paginate. The two
// callers that already read /v4beta/lke/clusters carry these same three constants
// and this same reasoning (verbs/doctor/linode.go, assertplatform/k8sversion.go);
// this is the third copy on purpose, because the alternative is a fourth spelling
// of the sizing rule.
//
// THE READ BUDGET IS APPLIED PER ATTEMPT, NOT MERELY USED TO SIZE THE PARENT, and
// the first cut of the retry below got this wrong in the way that makes a retry
// decorative: one 20s context wrapped the whole loop while the HTTP client's own
// timeout was also 20s, so a single SLOW request exhausted the shared budget,
// tripped the ctx.Err() break, and attempt 2 never ran. The retry existed and
// could not fire.
const (
	accountRequestTimeout = 20 * time.Second
	accountReadPages      = 5
	accountReadBudget     = accountReadPages * accountRequestTimeout
	clusterProbeTimeout   = linode.ClusterReadAttempts*accountReadBudget +
		linode.ClusterReadAttempts*linode.ClusterReadRetryPause + 10*time.Second
)

// accountLKEVersions returns the LKE-Enterprise versions this account may build.
// ok is false when the answer is unknown (no token, API error, empty list) — which
// is NOT the same as "this account can build nothing", and callers must not treat
// it as one.
//
// TAKES THE CLIENT RATHER THAN BUILDING ONE. Both reads in this file are about the
// SAME account, and calling LKEVersionClient() twice would have let one question be
// answered by a token and the other by nothing (or, after a test substituted the
// var mid-run, by two different fakes).
func accountLKEVersions(c LKEVersionLister) (ids []string, ok bool) {
	if c == nil {
		// Silent on the no-token path, like objClustersInRegion: CheckRegion runs
		// first in the same command and has already said it once, and
		// reportSkippedAccountCheck is a once-per-process notice for that reason.
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountReadBudget)
	defer cancel()
	all, err := c.ListLKEVersions(ctx, linode.LKETierEnterprise)
	if err != nil || len(all) == 0 {
		reportSkippedAccountCheck("--k8s-version", firstNonNilErr(err, errEmptyAccountListing))
		return nil, false
	}
	return all, true
}

// Deployment names the cluster `llz env add` is about to author, so the resolver
// can ask the account the question the preflight already asks: does a cluster for
// this deployment exist, and what does it run?
//
// THE TWO KEYS ARE THE ONES linode.ClusterVersionFor MATCHES ON, and both are
// known before anything is written — ClusterLabel is `<instanceName>-<env>`, which
// is what envdef.WriteEnvDefinition is about to author into
// `cluster.clusterLabel`, and Region is a required flag. So the resolver looks up
// the SAME cluster `llz ci assert-k8s-version` will later look up for this
// deployment, by construction rather than by two derivations agreeing.
//
// A ZERO Deployment DISABLES THE READ, which is what every caller outside `env
// add` wants: no label, nothing to match, no request.
type Deployment struct {
	ClusterLabel string
	Region       string
}

// clusterReadSleep is the pause between cluster-read attempts. Package var so a
// test does not pay linode.ClusterReadRetryPause of real time; the doctor's
// lkeSleep is the same seam for the same reason.
var clusterReadSleep = time.Sleep

// clusterLookup is everything the account said about THIS deployment's cluster.
//
// IT IS A STRUCT BECAUSE FIVE OUTCOMES WERE BEING SQUEEZED INTO A STRING, AND THE
// MESSAGES LIED. Successive reviews found three separate false statements, all the
// same root cause: "no version to adopt" was the only thing the caller could see,
// and it is true of five situations that need five different sentences —
//
//	skipped     no token, or no label to match on: llz never looked
//	failed      the read errored after its retries
//	none        the account answered and holds no such cluster  ← evidence
//	ambiguous   several share the label+region: llz will not guess
//	unreadable  exactly one matched but reports no k8s_version
//	found       exactly one matched and reports a version        ← Running
//
// Only `none` licenses "there is nothing on this account to exempt your pin", and
// llz was saying it for all five.
type clusterLookup struct {
	// Running is the version, set only in the `found` case.
	Running string
	// ID is that cluster's Linode id, for a message that has to name it. Zero when
	// no single cluster was identified.
	//
	// THROUGH linode.MatchClusterIDs, WHICH DECODES IT. `id` arrives from the API as
	// a json.Number or a float64, so formatting the raw map value with %v prints
	// `1.234567e+06` for any account whose ids are seven digits — an id nobody can
	// paste into a delete command. An earlier revision did exactly that, and its
	// test hid it by writing int literals into the fixture.
	ID uint64
	// Matches is how many clusters carried this deployment's label+region. Only
	// meaningful when Asked.
	Matches int
	// Asked is whether the account actually answered. False for skipped and failed.
	Asked bool
}

// Unreadable reports the one match that could not say what it runs.
func (l clusterLookup) Unreadable() bool { return l.Asked && l.Matches == 1 && l.Running == "" }

// lookupCluster asks the account what THIS deployment's cluster is, if anything.
//
// IT RETRIES, ON THE SAME BUDGET AS THE TWO CALLERS THAT ALREADY READ THIS ROUTE
// (linode.ClusterReadAttempts). An earlier revision was single-attempt on the
// grounds that the fallback is "behave as `llz env add` did before" — true for the
// no-pin arm, and false for the one that matters: an explicit --k8s-version is
// REJECTED unless this read acquits it, so one 503 turns a deployment whose cluster
// is running the pin into a hard failure, on exactly the remedy
// `llz ci assert-k8s-version` recommends. The preflight refuses to let an
// unprovable exemption decide that, and so does this.
//
// WHAT IT DOES NOT DO IS GO QUIET on the two outcomes that are neither evidence nor
// an answer: a failed read and an ambiguous account both fall through to seeding
// whatever the caller would otherwise have seeded, which against a live cluster is
// the unrequested control-plane upgrade this whole exemption exists to prevent.
func lookupCluster(c LKEVersionLister, d Deployment) clusterLookup {
	if c == nil || d.ClusterLabel == "" {
		return clusterLookup{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), clusterProbeTimeout)
	defer cancel()
	var err error
	for attempt := 1; attempt <= linode.ClusterReadAttempts; attempt++ {
		var clusters []map[string]any
		// A SUB-BUDGET PER ATTEMPT, carved out of the probe budget. Without it the
		// two attempts share one deadline and a slow first request spends the lot.
		rctx, rcancel := context.WithTimeout(ctx, accountReadBudget)
		clusters, err = c.ListClusters(rctx)
		rcancel()
		if err == nil {
			return classifyClusters(clusters, d)
		}
		// OUR OWN DEADLINE IS NOT A TRANSIENT ERROR, asked of ctx and never of the
		// error — an http.Client.Timeout unwraps to context.DeadlineExceeded, so
		// classifying on the error would read one slow request as the whole budget
		// running out. Same arm, and same reasoning, as the preflight's.
		if ctx.Err() != nil {
			break
		}
		if attempt < linode.ClusterReadAttempts {
			clusterReadSleep(linode.ClusterReadRetryPause)
		}
	}
	// IT NAMES NO FALLBACK, and an earlier revision named the wrong one. This one
	// warning is reached from three callers whose fallbacks differ — the catalog was
	// read (seed the newest), the catalog was NOT read (seed llz's compiled literal),
	// or an explicit pin is about to be REJECTED outright and nothing is seeded at
	// all. Asserting "llz falls back to seeding the newest version this account
	// offers" was false in two of the three, in the same run as the message that
	// said what actually happened.
	fmt.Fprintln(os.Stderr, cigate.Warning(fmt.Sprintf(
		"whether a cluster named %q already exists could not be checked: %s\n"+
			"  llz therefore cannot tell a RE-SCAFFOLD over a live cluster from a first run, and this run's\n"+
			"  cluster.k8sVersion is decided without that answer. If that cluster does exist and runs a\n"+
			"  DIFFERENT version than the spec ends up pinning, set cluster.k8sVersion to the running one\n"+
			"  in environments/<env>.yaml before the next apply — otherwise terraform plans a\n"+
			"  control-plane upgrade nobody asked for.",
		d.ClusterLabel, firstLine(err.Error()))))
	return clusterLookup{}
}

// classifyClusters turns one cluster listing into the outcome the messages branch
// on, and says the two things worth saying out loud.
func classifyClusters(clusters []map[string]any, d Deployment) clusterLookup {
	// linode.MatchingClusters, NOT a loop over label+region here. It is the same
	// matching rule linode.ClusterRunsVersion is written in terms of, so this
	// command and the preflight cannot come to different conclusions about which
	// cluster belongs to a deployment.
	m := linode.MatchingClusters(clusters, d.ClusterLabel, d.Region)
	ids := linode.MatchClusterIDs(clusters, d.ClusterLabel, d.Region)
	l := clusterLookup{Matches: len(m), Asked: true}
	switch {
	case len(m) > 1:
		// AMBIGUITY IS NOT SILENCE. Several clusters at one label+region is an account
		// this must not guess about — and it is also the shape a failed cycle leaves,
		// an orphan sitting beside the live deployment, where falling through plans a
		// control-plane upgrade on whichever one is real.
		//
		// THE REMEDY MUST NOT BE `llz reap --cluster-label`, and the first cut of this
		// warning said exactly that. That flag is LABEL-scoped: it lists every cluster
		// carrying the label and DELETEs each one (teardown/reap.go), which is correct
		// for a sweep after a deployment is gone and catastrophic here — this warning
		// fires precisely BECAUSE two clusters share the label, so the advice, followed
		// with --yes, destroys the live deployment alongside the orphan.
		fmt.Fprintln(os.Stderr, cigate.Warning(fmt.Sprintf(
			"%d clusters on this account are labelled %q in %s, so llz cannot tell which one is this\n"+
				"  deployment's and will not guess. cluster.k8sVersion is decided without them — which,\n"+
				"  against a cluster that is already running, plans a control-plane upgrade nobody asked for.\n"+
				"%s"+
				"  Identify which is live, then delete the OTHER one by id. Do NOT reach for\n"+
				"  `llz reap --cluster-label %[2]s`: it is label-scoped and would delete both.\n"+
				"  Or pin the running version with --k8s-version and re-run.",
			len(m), d.ClusterLabel, orAnyRegion(d.Region), describeClusters(ids, m))))
		return l
	case len(m) == 1:
		l.ID = ids[0]
		l.Running = strings.TrimSpace(mapString(m[0], "k8s_version"))
		if l.Running == "" {
			// FOUND IT, AND IT WILL NOT SAY WHAT IT RUNS. Indistinguishable from "no such
			// cluster" until this case existed, so llz seeded the newest over a live
			// deployment in silence and then told the operator, in the rejection message,
			// that no cluster of that name was on the account — about a listing that
			// contained one.
			fmt.Fprintln(os.Stderr, cigate.Warning(fmt.Sprintf(
				"cluster %q (id %d) exists in %s but reports no k8s_version, so llz cannot adopt what it\n"+
					"  runs and decides cluster.k8sVersion without it. If that cluster is live, read its version\n"+
					"  (`linode-cli lke cluster-view %[2]d`) and pass it as --k8s-version — otherwise terraform\n"+
					"  may plan a control-plane upgrade nobody asked for.",
				d.ClusterLabel, l.ID, orAnyRegion(d.Region))))
		}
		return l
	}
	return l
}

// describeClusters lists matched clusters as indented "cluster id N — runs V"
// lines, so an operator told two clusters share a label can tell them apart without
// going to the Console. ids and clusters are parallel: both come from the same
// matcher, in the same order.
func describeClusters(ids []uint64, clusters []map[string]any) string {
	var b strings.Builder
	for i, m := range clusters {
		v := strings.TrimSpace(mapString(m, "k8s_version"))
		if v == "" {
			v = "an unreported version"
		}
		fmt.Fprintf(&b, "  cluster id %d — runs %s\n", ids[i], v)
	}
	return b.String()
}

// mapString is the linode package's mString, which is unexported. Three lines, and
// a shared package for them would cost more than the duplication — the same call
// this repo makes for firstNonEmpty and firstLine.
func mapString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// K8sVersionChoice is everything `llz env add` needs in order to decide a
// deployment's version, out of ONE catalog read.
//
// IT IS A STRUCT BECAUSE THE COMMAND ASKS TWO QUESTIONS, and the first cut
// answered only the one the operator asked out loud. `llz env add` seeds
// spec.defaults on the FIRST deployment; every later one INHERITS a pin chosen
// when the instance was scaffolded — possibly months and several catalog
// rotations ago. Returning a single string meant the second `env add` printed
// "derived" about a value it then discarded, and the deployment went on to fail
// `llz ci assert-k8s-version` on the pin this command had just claimed to choose.
type K8sVersionChoice struct {
	// Pin is the operator's --k8s-version, checked against the account. "" when
	// none was given.
	//
	// IT IS PER-DEPLOYMENT AND NEVER BECOMES spec.defaults, which is the rule every
	// other flag here already follows: --region, --node-type and --node-count all
	// write environments/<env>.yaml alone. Seeding the shared default from it made
	// one deliberately-pinned deployment silently decide the version of every
	// deployment added afterwards.
	Pin string
	// Newest is the newest full build id the account offers — what a NEW
	// landingzone.yaml is seeded with. "" when the catalog could not be read or
	// holds nothing that could be sent to the create API.
	Newest string
	// Offered is the catalog itself; nil when the answer is unknown.
	//
	// It is here so a caller can judge a pin this function was never asked about —
	// the shared default a later `env add` inherits — without a second request, and
	// through the SAME matcher rather than a second opinion about the same spec.
	Offered []string
	// Running is what the account's ONE cluster for this deployment already runs;
	// "" when none matched, several did, or the account was not (or could not be)
	// asked. See linode.ClusterVersionFor.
	//
	// IT IS THE ANSWER TO "IS THIS A RE-SCAFFOLD?", which nothing on disk can give.
	// A re-scaffold that removes landingzone.yaml, environments/<env>.yaml and the
	// overlay together is byte-for-byte a fresh instance — so every guard keyed off
	// the tree is blind to precisely the case that matters most, and the account is
	// the only witness left.
	Running string
	// Note is a line worth printing about Pin; "" when there is nothing to say.
	//
	// DELIBERATELY SAYS NOTHING ABOUT Newest. Whether Newest is used at all is the
	// caller's decision, and a note printed from here announced a seeding that a
	// second `env add` never performs.
	Note string
	// Warning is a consequence the operator must not be able to miss — rendered
	// through cigate so it reaches an Actions run summary rather than line 400 of a
	// step log. "" when there is none.
	//
	// SEPARATE FROM Note BECAUSE THE TWO ARMS ARE NOT THE SAME NEWS. "Your pin is in
	// the catalog" is a receipt; "llz pinned a version the account can no longer
	// build, because your cluster is running it" is a state the operator has to
	// carry forward — that deployment cannot be RE-CREATED until the pin moves.
	Warning string
}

// ResolveK8sVersion asks the account what it can build, once.
//
// A ZERO Newest MEANS "KEEP YOUR OWN DEFAULT", NOT "no version". The caller
// (envdef.EnsureLandingZone) owns an offline fallback — the cluster root's
// terraform.tfvars.example, then a literal — and this must not overwrite it with
// emptiness when the account could not be asked. `llz env add` works on a laptop
// with no token, and that is a requirement rather than a nicety.
//
// A supplied want is checked, and rejected ONLY when the catalog is entitled to
// reject (see linode.CheckVersion); otherwise it passes through untouched,
// because an unanswerable question is not evidence against the operator's pin.
//
// Newest is chosen over "newest within a tracked minor" because a tracked minor is
// exactly the rotting literal this file exists to delete, one precision level
// coarser. It is written ONCE, into landingzone.yaml's spec.defaults on the first
// `llz env add`, so the choice cannot move under a live instance — only two
// instances scaffolded months apart differ, which is the point.
//
// A DEPLOYMENT WHOSE CLUSTER ALREADY EXISTS GETS PINNED TO WHAT IT RUNS, and that
// is the second question this function asks (#453). Everything above reasons about
// a deployment that does not exist yet; a RE-SCAFFOLD over a live cluster wants the
// opposite answer, because any version other than the running one is an
// LKE-Enterprise control-plane upgrade nobody asked for. The two cases are
// indistinguishable on disk — the operator may have deleted the spec, the env file
// and the overlay together, which is what this repo's own e2e lane does — so the
// account is asked instead. One match wins; zero or several fall through to
// everything above, unchanged.
func ResolveK8sVersion(want string, d Deployment) (K8sVersionChoice, error) {
	client := LKEVersionClient()
	offered, ok := accountLKEVersions(client)
	c := K8sVersionChoice{Pin: strings.TrimSpace(want)}
	var lk clusterLookup
	if !ok {
		// Unknown, not wrong — the pin (if any) survives and the caller keeps its
		// offline default.
		//
		// BUT THE CLUSTER READ STILL HAPPENS, and an earlier revision skipped it on
		// the reasoning that "a catalog that could not be answered means no token or
		// no reachable API, so the second request would fail the same way". That is
		// three different failures collapsed into one, and two of them are measured
		// shapes in this repo:
		//
		//   - the two endpoints are not the same endpoint. #426 recorded the e2e token
		//     401ing on the VERSIONS route from some contexts; /v4beta/lke/clusters is
		//     a different path with its own PAT scope, and a read-only Kubernetes scope
		//     answers it fine.
		//   - an EMPTY catalog also lands here (accountLKEVersions folds it in), and an
		//     account listing no LKE-E versions says nothing whatever about whether it
		//     has clusters.
		//
		// The cost of getting this wrong is the whole feature: a re-scaffold over a
		// live cluster would silently re-seed today's newest and plan the upgrade #453
		// exists to prevent, with nothing on disk left to warn from. No token still
		// costs nothing — runningVersionFor returns immediately on a nil client.
		lk = lookupCluster(client, d)
		c.Running = lk.Running
		if c.Pin == "" && c.Running != "" {
			c.Pin = c.Running
			// offered is nil here, so CheckVersion answers Unknown and adoptionMessage
			// degrades to the plain note — which claims nothing about a catalog llz
			// could not read. `llz doctor` reaches that verdict later, with its own
			// diagnostics for why the read failed.
			c.Note, c.Warning = adoptionMessage(d, lk, nil)
		}
		return c, nil
	}
	c.Offered = offered
	c.Newest = linode.NewestVersion(offered)
	if c.Newest == "" {
		// The catalog came back, but nothing in it is a full build id — the shape
		// this was measured against. Seeding a coarse entry would write a pin the
		// LKE-E create API rejects, which is worse than the stale literal.
		// NOT "falls back to the scaffold default": only the FIRST `llz env add`
		// seeds anything. A later one leaves the deployment inheriting
		// spec.defaults, and nothing falls back at all — this function cannot tell
		// which, so it says what it actually knows.
		//
		// THROUGH cigate FOR THE SAME REASON reportSkippedAccountCheck is. This is
		// also "a check you were promised did not produce an answer", and as raw
		// stderr it was plain text inside a green step — invisible in exactly the
		// CI runs where nobody re-reads a scaffold log.
		fmt.Fprintln(os.Stderr, cigate.Warning(fmt.Sprintf(
			"the account's LKE-Enterprise catalog (%s) names no full build id, so llz cannot derive\n"+
				"  cluster.k8sVersion and leaves the spec to decide it. Check it with `llz doctor` before building.",
			strings.Join(offered, ", "))))
	}
	verdict, nearest := linode.VersionUnknown, ""
	if c.Pin != "" {
		verdict, nearest = linode.CheckVersion(c.Pin, offered)
	}

	// THE SECOND REQUEST, ASKED ONLY WHEN ITS ANSWER CAN CHANGE THE WRITE. A pin the
	// catalog CONFIRMS needs no exemption and is not up for adoption — --k8s-version
	// is the operator saying the version out loud, and it wins. A pin the catalog
	// cannot settle passes through untouched for the same reason. What is left is
	// the two arms this read exists for: no pin at all (adopt what the cluster runs
	// instead of seeding today's newest), and a pin the catalog REJECTS (exempt it
	// if that is precisely what the cluster runs).
	//
	// SO A GENUINELY FRESH `llz new` STILL PAYS ONE REQUEST, and the alternative was
	// worse. Gating on "landingzone.yaml exists or environments/ is non-empty" was
	// considered and rejected: the single-deployment re-scaffold — spec, env file and
	// overlay deleted together — leaves NOTHING on disk, so a disk-shaped gate is
	// blind in exactly the case the read was added for. One list call against an
	// account with no clusters is the price of that case being covered at all.
	if c.Pin == "" || verdict == linode.VersionNotOffered {
		lk = lookupCluster(client, d)
		c.Running = lk.Running
	}

	if c.Pin == "" {
		if c.Running != "" {
			// RE-SCAFFOLD OVER A LIVE CLUSTER. Pin what it runs, per-deployment (never
			// spec.defaults — see K8sVersionChoice.Pin), so terraform plans no change
			// to k8s_version. Newest is still returned for the shared default, because
			// a deployment added LATER to this instance genuinely should get today's
			// newest rather than this cluster's.
			c.Pin = c.Running
			c.Note, c.Warning = adoptionMessage(d, lk, offered)
		}
		return c, nil
	}

	switch verdict {
	case linode.VersionOffered:
		// "CONFIRMED" ONLY WHEN THE PIN IS A BUILD ID — the PIN, which is the string
		// terraform sends, not the catalog it matched. CheckVersion confirms on a
		// byte-exact match, so `--k8s-version 1.33` against a catalog holding a bare
		// `1.33` row comes back Offered, and the LKE-E create API rejects it.
		//
		// THE FIRST CUT TESTED c.Newest — i.e. "does the catalog name any build
		// anywhere" — which reads like the same question and is not. Against a MIXED
		// catalog (`["v1.34.6+lke2", "1.33"]`) c.Newest is non-empty, so a coarse pin
		// still got a confidently-worded confirmation and was written to the spec.
		//
		// The verdict stays CheckVersion's (both callers share it, and #443 is
		// explicit that a coarse catalog is an unmeasured shape rather than a
		// falsehood); what this must not do is tell the operator it checked out.
		//
		// It also produced two contradictory lines in ONE run: the "names no full
		// build id" warning above and a confidently-worded confirmation here, about
		// the same catalog.
		if linode.NamesABuild(c.Pin) {
			c.Note = fmt.Sprintf("k8s-version %s confirmed against the account's LKE-Enterprise catalog.", c.Pin)
			return c, nil
		}
		// SUPPRESSING THE CONFIRMATION IS NOT ENOUGH, and for one revision that was all
		// this did. A coarse pin byte-matches a coarse catalog row, so CheckVersion
		// says Offered — and then `llz doctor` and `llz ci assert-k8s-version` say
		// Offered too, for the identical reason. Every gate the feature added is
		// GREEN, the spec carries the pin, and the apply dies ~15 minutes in on
		// `[400] k8s_version is not valid`: the exact failure this feature exists to
		// remove, wearing a full set of passing checks.
		//
		// A WARNING RATHER THAN A REJECTION, deliberately. #443 is explicit that a
		// catalog spelled in bare major.minor is an UNMEASURED shape rather than a
		// falsehood, and CheckVersion owns the verdict — nothing here may become the
		// stricter of two checks that are supposed to be one. What this may do is
		// refuse to let the operator walk away believing it checked out.
		c.Warning = fmt.Sprintf(
			"--k8s-version %q matches a row in this account's catalog, but it is not a full LKE-E build id\n"+
				"  (those carry an `+lke` suffix, e.g. v1.34.6+lke2) — and terraform sends cluster.k8sVersion\n"+
				"  VERBATIM. The catalog row it matched is just as coarse, so `llz doctor` and\n"+
				"  `llz ci assert-k8s-version` will both pass it, and the cluster apply is what discovers the\n"+
				"  problem ~15 minutes in with `[400] [k8s_version] k8s_version is not valid`.\n"+
				"  Pass a build id from this list instead: %s",
			c.Pin, strings.Join(offered, ", "))
		return c, nil
	case linode.VersionUnknown:
		// The catalog answered in a shape that cannot settle this pin. Say nothing
		// rather than endorse it: `llz doctor` and `llz ci assert-k8s-version` reach
		// the identical verdict later.
		return c, nil
	}
	// THE PREFLIGHT'S OWN RULE, MOVED TO WHERE THE PIN IS CHOSEN. `k8s_version`
	// reaches the API only on a create or a change, so a cluster ALREADY RUNNING
	// this pin plans no diff and the catalog's opinion of it is irrelevant — which
	// is exactly what linode.ClusterRunsVersion exempts at
	// `llz ci assert-k8s-version`. Without the same arm here, the remedy that
	// function's own error recommends (pin the version your cluster is running)
	// stopped working the day that version left the catalog, which for LKE-E is a
	// matter of hours.
	//
	// EXACT, AND THAT IS linode.ClusterRunsVersion's COMPARISON, not a looser one:
	// Running came from ClusterVersionFor and the pin is the string terraform will
	// send, so `1.34.6+lke2` against a cluster reporting `v1.34.6+lke2` is a diff and
	// must NOT be exempted here.
	if c.Running != "" && c.Running == c.Pin {
		_, c.Warning = adoptionMessage(d, lk, offered)
		return c, nil
	}
	if nearest != "" {
		//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded newline carrying the fix instructions
		return K8sVersionChoice{}, fmt.Errorf("--k8s-version %q is not offered by this Linode account — but %q is.\n"+
			"  The two are the same version spelled differently, and terraform sends cluster.k8sVersion\n"+
			"  VERBATIM, so the catalog's spelling is the one that works. Pass --k8s-version %s.",
			c.Pin, nearest, nearest)
	}
	// THE REMEDY USED TO BE WRONG FOR ONE REAL CASE, AND NOW IT IS REACHABLE. "Omit
	// it and llz picks the newest" is right for a deployment that does not exist yet
	// and actively harmful for one being RE-SCAFFOLDED over a live cluster — so this
	// verb now asks the account (runningVersionFor) before getting here, and the
	// exemption above has already returned for the case where the pin IS what the
	// cluster runs. What is left is a genuinely unbuildable pin, and the advice
	// depends on what the account actually said.
	if d.ClusterLabel != "" && c.Running != "" {
		// A cluster for this deployment exists and runs something ELSE. Naming it is
		// the whole answer: keeping the pin plans an upgrade to a version the account
		// cannot build, and omitting the flag now pins the running one automatically.
		//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded newline carrying the fix instructions
		return K8sVersionChoice{}, fmt.Errorf("--k8s-version %q is not an LKE-Enterprise version this account can build.\n"+
			"  This account offers: %s\n"+
			"  Cluster %q (%s) already exists and runs %s — NOT the version you passed. Keeping this pin\n"+
			"  would plan a control-plane upgrade to a version the account cannot build, and the apply\n"+
			"  fails ~15 minutes in with `[400] [k8s_version] k8s_version is not valid`.\n"+
			"  Omit --k8s-version: llz pins %[5]s, the version that cluster is running, so terraform plans\n"+
			"  no change to k8s_version at all.",
			c.Pin, strings.Join(offered, ", "), d.ClusterLabel, orAnyRegion(d.Region), c.Running)
	}
	// NOTHING TO EXEMPT IT. Either no cluster for this deployment exists on the
	// account (so terraform will CREATE, and a create sends exactly this string), or
	// the caller passed no label and this verb was never in a position to look.
	// Which of the two it is changes the sentence, because "we checked and there is
	// no such cluster" and "we did not check" are different claims and only one of
	// them is evidence.
	// WHAT llz ACTUALLY LEARNED, IN ITS OWN WORDS. The verdict is the same in every
	// arm — the catalog answered and the pin is not in it, and no amount of doubt
	// about the escape hatch is evidence that a cluster exists (the preflight's own
	// rule). What differs is the CLAIM, and collapsing these is how llz came to say
	// "No single cluster named X is on this account" about a listing it never read,
	// and about one that contained exactly that cluster.
	lookedUp := "  Availability is PER-ACCOUNT and rotates within hours, so a version another account\n" +
		"  can build says nothing about this one.\n"
	switch {
	case !lk.Asked && d.ClusterLabel != "":
		lookedUp = fmt.Sprintf("  Whether a cluster named %q already runs it could not be checked (the read above says\n"+
			"  why), so the catalog verdict stands. Re-run if that read was transient.\n", d.ClusterLabel)
	case lk.Unreadable():
		lookedUp = fmt.Sprintf("  Cluster %q (id %d) exists but reports no k8s_version, so llz cannot tell whether it is\n"+
			"  already on this pin. Read its version and pass that, or move the pin to a listed one.\n",
			d.ClusterLabel, lk.ID)
	case lk.Matches > 1:
		lookedUp = fmt.Sprintf("  %d clusters share the label %q here, so llz will not decide which one this pin\n"+
			"  belongs to (the listing above names them).\n", lk.Matches, d.ClusterLabel)
	case lk.Asked:
		lookedUp = fmt.Sprintf("  No single cluster named %q is on this account, so nothing exempts this pin: k8s_version\n"+
			"  reaches the API on a create, and a create sends exactly this string.\n", d.ClusterLabel)
	}
	//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded newline carrying the fix instructions
	return K8sVersionChoice{}, fmt.Errorf("--k8s-version %q is not an LKE-Enterprise version this account can build.\n"+
		"  This account offers: %s\n"+
		"%s"+
		"  Unchanged, the cluster apply fails ~15 minutes in with\n"+
		"  `[400] [k8s_version] k8s_version is not valid`.\n"+
		"  Omit --k8s-version: llz picks the newest your account offers, or — if a cluster for this\n"+
		"  deployment does exist — the version it is ALREADY RUNNING, which plans no diff at all.",
		c.Pin, strings.Join(offered, ", "), lookedUp)
}

// orAnyRegion names the region a lookup was scoped to, for a message. `llz env
// add` always has one (--region is required), so this only guards a caller passing
// a zero Deployment.
func orAnyRegion(r string) string {
	if r == "" {
		return "any region"
	}
	return r
}

// adoptionMessage says what llz did when it pinned the version a deployment's
// cluster is ALREADY RUNNING, and how loud to say it.
//
// TWO ARMS, BECAUSE THE TWO OUTCOMES ARE NOT EQUALLY CONSEQUENTIAL. When the
// running version is still in the catalog, adopting it changed nothing an operator
// has to remember — a receipt line is enough. When it has ROTATED OUT, the
// deployment is now pinned to a version the account can no longer build: every
// routine apply is fine (terraform plans no diff), and a REBUILD is not. That is
// the state `llz doctor` and `llz ci assert-k8s-version` will both annotate later,
// and it must not be first heard about during a rebuild.
//
// IT ALSO NAMES THE ORPHAN CASE, which is the one way this can be wrong. A cluster
// left at this label by a failed cycle satisfies the match while the tfstate that
// would adopt it is gone, so terraform plans a CREATE — and a create sends the pin,
// rotated-out and all. That is the same false-pass linode.ClusterRunsVersion
// documents at the preflight, bounded the same way (`llz reap` sweeps orphans), and
// it is named here because this is the one caller that WRITES the pin rather than
// judging one.
// TENSE MATTERS HERE AND THE FIRST CUT GOT IT WRONG. These lines are printed on
// the `--dry-run` path too, where nothing is written — so "pinned it" and "can no
// longer be RE-CREATED" were past-tense claims about a write that never happened,
// which is the exact defect printK8sVersionConsequences was extracted to fix for
// the other two version consequences. Both are phrased as the DECISION rather than
// the deed, which is true before the write and still true after it.
func adoptionMessage(d Deployment, lk clusterLookup, offered []string) (note, warning string) {
	if verdict, _ := linode.CheckVersion(lk.Running, offered); verdict != linode.VersionNotOffered {
		return fmt.Sprintf("cluster %s (%s) already runs %s — pinning it for this deployment, so terraform plans no control-plane change.",
			d.ClusterLabel, orAnyRegion(d.Region), lk.Running), ""
	}
	// THE ORPHAN REMEDY NAMES AN ID, NOT THE LABEL. `llz reap --cluster-label` sweeps
	// by label ACCOUNT-WIDE — ClustersWithLabel does not filter by region — while this
	// adoption matched label AND region, so a same-labelled cluster in another region
	// is a live deployment the advice would delete. Same hazard the ambiguity warning
	// had to have removed from it; it survived here because the two messages were
	// written a round apart.
	return "", fmt.Sprintf(
		"cluster %s (%s, id %d) already runs %s, which this account no longer offers — pinning it anyway.\n"+
			"  That is deliberate: k8s_version reaches the API only on a create or a change, so a running\n"+
			"  cluster plans no diff and every routine apply is unaffected. This deployment cannot then be\n"+
			"  RE-CREATED until the pin moves to a listed version (%s).\n"+
			"  If id %[3]d is instead an ORPHAN from a failed cycle, terraform will plan a CREATE and send\n"+
			"  this pin: delete that cluster BY ID and re-run `llz env add`. Not\n"+
			"  `llz reap --cluster-label %[1]s`, which sweeps every region and would take live deployments\n"+
			"  sharing the label.",
		d.ClusterLabel, orAnyRegion(d.Region), lk.ID, lk.Running, strings.Join(offered, ", "))
}

// ReplacementForInheritedPin returns the version a NEW deployment must pin for
// itself instead of inheriting inherited, or "" to inherit it unchanged.
//
// THIS IS THE HALF THE FIRST CUT MISSED, and it is the same failure as the one
// this whole file exists for, moved one deployment along. spec.defaults is seeded
// once, at scaffold time; the SECOND `llz env add` — a new region, a DR peer, a
// deployment added a quarter later — inherits it. LKE-E availability rotates
// within hours (#426 measured a version leaving an account's catalog inside one),
// so by then the shared pin may be one the account can no longer build, and the
// new deployment is created against it and fails `llz ci assert-k8s-version`.
//
// ONLY A DEFINITE NEGATIVE OVERRIDES. VersionUnknown, an unread catalog, or a
// catalog too coarse to disprove all inherit unchanged: divergence between
// deployments is a real cost, and this may only impose it on evidence. The
// deployments already running the old pin are untouched either way — terraform
// plans no change to k8s_version for them, which is what linode.ClusterRunsVersion
// exempts.
func (c K8sVersionChoice) ReplacementForInheritedPin(inherited string) string {
	if c.Newest == "" || len(c.Offered) == 0 || strings.TrimSpace(inherited) == "" {
		return ""
	}
	if verdict, _ := linode.CheckVersion(inherited, c.Offered); verdict != linode.VersionNotOffered {
		return ""
	}
	return c.Newest
}
