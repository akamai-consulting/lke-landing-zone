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
type LKEVersionLister interface {
	ListLKEVersions(ctx context.Context, tier string) ([]string, error)
}

// LKEVersionClient returns a live client, or nil when no token is configured.
// Package var so tests substitute a fake.
var LKEVersionClient = func() LKEVersionLister {
	tok := firstNonEmpty(os.Getenv("LINODE_TOKEN"), os.Getenv("LINODE_API_TOKEN"))
	if tok == "" {
		return nil
	}
	return linode.NewClient(tok, 20*time.Second)
}

// accountLKEVersions returns the LKE-Enterprise versions this account may build.
// ok is false when the answer is unknown (no token, API error, empty list) — which
// is NOT the same as "this account can build nothing", and callers must not treat
// it as one.
func accountLKEVersions() (ids []string, ok bool) {
	c := LKEVersionClient()
	if c == nil {
		// Silent on the no-token path, like objClustersInRegion: CheckRegion runs
		// first in the same command and has already said it once, and
		// reportSkippedAccountCheck is a once-per-process notice for that reason.
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	all, err := c.ListLKEVersions(ctx, linode.LKETierEnterprise)
	if err != nil || len(all) == 0 {
		reportSkippedAccountCheck("--k8s-version", firstNonNilErr(err, errEmptyAccountListing))
		return nil, false
	}
	return all, true
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
	// Note is a line worth printing about Pin; "" when there is nothing to say.
	//
	// DELIBERATELY SAYS NOTHING ABOUT Newest. Whether Newest is used at all is the
	// caller's decision, and a note printed from here announced a seeding that a
	// second `env add` never performs.
	Note string
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
func ResolveK8sVersion(want string) (K8sVersionChoice, error) {
	offered, ok := accountLKEVersions()
	c := K8sVersionChoice{Pin: strings.TrimSpace(want)}
	if !ok {
		// Unknown, not wrong — the pin (if any) survives and the caller keeps its
		// offline default.
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
	if c.Pin == "" {
		return c, nil
	}

	verdict, nearest := linode.CheckVersion(c.Pin, offered)
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
		}
		return c, nil
	case linode.VersionUnknown:
		// The catalog answered in a shape that cannot settle this pin. Say nothing
		// rather than endorse it: `llz doctor` and `llz ci assert-k8s-version` reach
		// the identical verdict later.
		return c, nil
	}
	if nearest != "" {
		//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded newline carrying the fix instructions
		return K8sVersionChoice{}, fmt.Errorf("--k8s-version %q is not offered by this Linode account — but %q is.\n"+
			"  The two are the same version spelled differently, and terraform sends cluster.k8sVersion\n"+
			"  VERBATIM, so the catalog's spelling is the one that works. Pass --k8s-version %s.",
			c.Pin, nearest, nearest)
	}
	// THE REMEDY IS WRONG FOR ONE REAL CASE AND HAS TO SAY SO. "Omit it and llz
	// picks the newest" is right for a deployment that does not exist yet, and
	// actively harmful for one being RE-SCAFFOLDED over a live cluster — the
	// operator is passing the version their cluster is already running, and taking
	// the newest instead plans a control-plane upgrade nobody asked for.
	//
	// This verb cannot tell the two apart: it deliberately makes ONE catalog
	// request and does not read the account's clusters, so it has no counterpart to
	// linode.ClusterRunsVersion. That exemption lives at the preflight, which is
	// where terraform's actual plan is about to be sent — so the escape is to
	// scaffold without the flag and put the running version in the spec, which is
	// the source of truth `env add` only seeds.
	//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded newline carrying the fix instructions
	return K8sVersionChoice{}, fmt.Errorf("--k8s-version %q is not an LKE-Enterprise version this account can build.\n"+
		"  This account offers: %s\n"+
		"  Availability is PER-ACCOUNT and rotates within hours, so a version another account\n"+
		"  can build says nothing about this one. Unchanged, the cluster apply fails ~15 minutes\n"+
		"  in with `[400] [k8s_version] k8s_version is not valid`.\n"+
		"  For a NEW deployment: omit --k8s-version and llz picks the newest your account offers.\n"+
		"  Re-scaffolding a deployment whose cluster ALREADY RUNS %[1]q: omit the flag, then set\n"+
		"  cluster.k8sVersion back to it in environments/<env>.yaml and re-render. The apply sends\n"+
		"  k8s_version only on a create or a change, so a running cluster plans no diff and\n"+
		"  `llz ci assert-k8s-version` exempts it — taking the newest here would instead plan a\n"+
		"  control-plane upgrade you did not ask for.",
		c.Pin, strings.Join(offered, ", "))
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
