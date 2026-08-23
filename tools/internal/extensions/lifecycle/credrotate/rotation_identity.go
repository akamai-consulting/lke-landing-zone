package credrotate

// rotation_identity.go — resolve the two identifiers the scheduled credential
// rotation used to carry as literals in the workflow: WHICH object-storage
// cluster the TF-state key is scoped to, and WHAT label the rotated credentials
// are minted and drained under.
//
// Both were hardcoded in instance-template/.github/workflows/llz-secret-rotation.yml
// and neither was ever correct for anyone but the maintainers' account.
//
// THE CLUSTER. `bucket-cluster: "us-ord-10"` sat in the caller since the initial
// public release — the literal from the action's own `e.g. us-ord-10` input
// docs. The monthly cron APPLIES (routeRotation sets TFStateApply on
// cronMonthlyRotate), so every instance whose state bucket lives elsewhere minted
// a key against the wrong cluster and wrote it over TF_STATE_ACCESS_KEY /
// TF_STATE_SECRET_KEY for every deployment. A bucket is reachable ONLY at the
// endpoint it was created against — the disjoint-generation rule that produced
// the Loki/Harbor NoSuchBucket class — so that pair cannot read the state it is
// supposed to unlock. It fires ~30 days after a successful first build, with no
// operator action involved. The instance already records the right answer:
// `llz tokens` writes TF_STATE_ENDPOINT as https://<cluster>.linodeobjects.com.
//
// THE LABEL. `gha-platform-platform_LINODE_API_TOKEN` and
// `gha-platform-platform_TF_STATE_KEY` carry no instance discriminator, and
// revoke-old drains every same-labelled credential in the ACCOUNT down to
// --keep-newest. Two LLZ instances on one Linode account therefore drain each
// other's live credentials — including the broad account PAT the whole platform
// runs on. This is the identical collision the objLabelPrefix work fixed for the
// in-cluster PAT and the OBJ keys; it did not reach these four literals because
// they live in YAML rather than in the spec-driven render path.
//
// MIGRATION IS DELIBERATELY ONE-WAY. Once create mints under the namespaced
// label, the legacy shared-label credentials stop being drained. That is the
// safe direction: draining them is the cross-instance revocation this fixes, and
// an un-drained leftover costs a manual revoke where a wrongly-drained one costs
// an outage. reportLegacyRotationLabels names them once so the leftover is
// visible rather than silent.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// legacyRotationLabels are the pre-namespacing literals, kept ONLY so the
// migration notice can name what an instance may still be holding. Nothing
// mints or drains under them.
var legacyRotationLabels = map[string]string{
	rotationKindPAT:        "gha-platform-platform_LINODE_API_TOKEN",
	rotationKindTFStateKey: "gha-platform-platform_TF_STATE_KEY",
}

// rotation kinds — the credential families the scheduled rotation owns.
const (
	rotationKindPAT        = "linode-api-token"
	rotationKindTFStateKey = "tf-state-key"
)

// rotationLabel is the instance-scoped label for a rotated credential family.
//
// Shape mirrors inclusterPATLabel's (`llz-incluster-<prefix>-<region>`): an `llz-`
// namespace so the account owner can tell what minted it, then the per-instance
// prefix that makes the drain safe. These two are instance-wide rather than
// per-deployment — one broad PAT and one TF-state key serve every deployment —
// so no region participates.
func rotationLabel(prefix, kind string) string {
	return "llz-" + prefix + "-" + kind
}

// resolveRotationLabel returns the label to mint/drain under: an explicit
// --label verbatim, else the instance-scoped default for kind.
//
// An explicit value still wins so an operator can drain a legacy label by hand
// (`llz credentials pat revoke-old --label <old>`) without editing the workflow.
func resolveRotationLabel(explicit, kind, what string) (string, error) {
	if l := strings.TrimSpace(explicit); l != "" {
		return l, nil
	}
	// The broad PAT has a SECOND rotator. `llz ci rotate-broad-pat` runs in-cluster
	// from the broadPatRotator component and takes its label from BROAD_PAT_LABEL,
	// which `llz render` fills from spec.components.broadPatRotator.broadPATLabel —
	// an operator-chosen string with no default.
	// THE BROAD PAT HAS ONE OWNER, AND WHEN THE SPEC NAMES IT, IT IS NOT THIS JOB.
	//
	// This used to DEFER to spec.components.broadPatRotator.broadPATLabel so both
	// rotators drained one label family, on the reasoning that two families for
	// one credential would leave a live PAT nothing reaps. The reasoning was
	// right about two families and wrong about the remedy: sharing one family
	// makes the two rotators reap EACH OTHER.
	//
	// Not because they publish to different places — RotateBroadPAT writes BOTH
	// secret/linode/broad-pat AND every infra-<env> LINODE_API_TOKEN, so its
	// coverage is a superset of the CI verb's, which writes the GitHub secrets
	// and never touches OpenBao. The race is simpler than that and does not
	// depend on the destinations at all: two independent mints against one label
	// family, each keeping "the newest" and draining the rest, means whichever
	// ran last defines the survivor and the other's freshly-published token is
	// just an older sibling waiting out its grace window.
	//
	// ADR 0001 already decided this: the broadPatRotator CronJob OWNS the broad
	// PAT's create and revoke, held "on exactly one deployment (it is
	// account-wide, so more than one owner would race mint/revoke)". So the GHA
	// path stands down rather than joining in.
	//
	// It was harmless until now only because nothing called this resolver, so the
	// label was "" and both drains matched nothing.
	if kind == rotationKindPAT {
		if l := specBroadPATLabel(); l != "" {
			return "", errBroadPATOwnedInCluster(what, l)
		}
	}
	prefix, err := clusterspec.LabelPrefixFor(what)
	if err != nil {
		return "", err
	}
	return rotationLabel(prefix, kind), nil
}

// specBroadPATLabel returns spec.components.broadPatRotator.broadPATLabel, or ""
// when there is no spec, no such component, or no label set.
//
// Scans every deployment because the component is instance-wide by construction:
// the broad PAT is an ACCOUNT credential, so the component runs on exactly one
// deployment and that deployment is not necessarily the one being rotated.
func specBroadPATLabel() string {
	lz, err := clusterspec.LoadInstance(".")
	if err != nil || lz == nil {
		return ""
	}
	for _, name := range lz.EnvNames() {
		e, ok := lz.Env(name)
		if !ok {
			continue
		}
		// ENABLED, NOT MERELY LABELLED, and reading the label alone made the
		// stand-down's own remedy a dead end. Its message says "disable
		// spec.components.broadPatRotator to hand rotation back to CI" — and a
		// disabled component keeps its broadPATLabel, which validation permits,
		// so the label still answered, CI still stood down, and NEITHER owner
		// rotated the account-wide broad PAT. An operator following the
		// instructions exactly would have watched it expire.
		//
		// ComponentEnabled is the predicate every other reader of a component
		// toggle uses; there was no reason for this one to invent a second.
		if !clusterspec.ComponentEnabled(e.Components, "broadPatRotator") {
			continue
		}
		if l := strings.TrimSpace(e.Components["broadPatRotator"].BroadPATLabel); l != "" {
			return l
		}
	}
	return ""
}

// objClusterFromEndpoint reads the cluster out of TF_STATE_ENDPOINT.
//
// A THIN ALIAS OVER linode.ObjClusterFromEndpoint, because this was a SECOND
// implementation of a rule the onboarding wizard already had — and the two
// disagreed on the virtual-host spelling, this one returning the BUCKET name as
// the cluster. See that function for what both got wrong.
func objClusterFromEndpoint(endpoint string) string {
	return linode.ObjClusterFromEndpoint(endpoint)
}

// resolveObjBucketCluster returns the object-storage cluster the TF-state key
// must be scoped to: an explicit --bucket-cluster verbatim, else the cluster
// derived from TF_STATE_ENDPOINT.
//
// Errors rather than defaulting. An empty cluster reaches the Linode API as a
// malformed create, and a guessed one mints a key against a bucket namespace the
// state does not live in — which is exactly the silent breakage this replaces.
func resolveObjBucketCluster(explicit, endpoint string) (string, error) {
	if c := strings.TrimSpace(explicit); c != "" {
		return c, nil
	}
	if c := objClusterFromEndpoint(endpoint); c != "" {
		return c, nil
	}
	//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded remediation block
	return "", fmt.Errorf("cannot determine which object-storage cluster to scope the key to.\n" +
		"  It is normally derived from TF_STATE_ENDPOINT (https://<cluster>.linodeobjects.com),\n" +
		"  which `llz tokens` sets as a repo variable when it creates the state bucket.\n" +
		"  • in CI:      make sure the job passes TF_STATE_ENDPOINT (vars.TF_STATE_ENDPOINT)\n" +
		"  • by hand:    llz credentials obj-key create --bucket-cluster <cluster> …\n" +
		"  A key scoped to the wrong cluster CANNOT read the state bucket — a bucket is\n" +
		"  reachable only at the endpoint it was created against — so this refuses to guess.")
}

// reportLegacyRotationLabels warns, once per run, when the account still holds
// credentials under the pre-namespacing shared label.
//
// Report-only by design: revoking them is what the namespacing fixed, because a
// second instance on the same account may be the one still using them. labels is
// the caller's live listing (label -> count).
func reportLegacyRotationLabels(kind string, count int) {
	legacy, ok := legacyRotationLabels[kind]
	if !ok || count == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "%s %d credential(s) remain under the pre-namespacing label %q.\n",
		color.Yellow("!"), count, legacy)
	fmt.Fprintln(os.Stderr, "  Rotation no longer mints or drains under it: that label is shared by every LLZ")
	fmt.Fprintln(os.Stderr, "  instance on this Linode account, so draining it would revoke another instance's")
	fmt.Fprintln(os.Stderr, "  live credentials. They are superseded once a rotation has run under the")
	fmt.Fprintln(os.Stderr, "  instance-scoped label, but nothing will remove them for you.")
	fmt.Fprintln(os.Stderr, "  Confirm no other instance still reads them, then revoke them in the Linode")
	fmt.Fprintf(os.Stderr, "  console (%s). Deliberately not automated — see rotation_identity.go.\n",
		color.Dim("Profile → API Tokens / Object Storage → Access Keys"))
}

// ErrBroadPATOwnedInCluster reports that the in-cluster rotator owns the broad
// PAT, so the GitHub-Actions rotation must not mint or drain it.
//
// A SENTINEL because the callers do not treat it as a failure: the monthly
// workflow runs `scope: all`, so an instance that opted into broadPatRotator
// would otherwise go red every month for being correctly configured. The verbs
// report it and stand down. It is NOT a silent skip — standing down without
// saying so is how a rotation nobody performs looks exactly like one that works.
var ErrBroadPATOwnedInCluster = errors.New("the broad PAT is owned by the in-cluster broadPatRotator")

func errBroadPATOwnedInCluster(what, label string) error {
	return fmt.Errorf("%w (label %q): %s must not mint or drain it.\n"+
		"  ADR 0001 gives create+revoke of the account-wide broad PAT to the\n"+
		"  broadPatRotator CronJob, on exactly one deployment, because two owners\n"+
		"  race each other's mint and revoke: two independent mints against one\n"+
		"  label family, each keeping the newest and draining the rest, so\n"+
		"  whichever ran last defines the survivor and the other's freshly\n"+
		"  published token is an older sibling waiting out its grace window.\n"+
		"  • leave it to the CronJob (nothing to do), or\n"+
		"  • disable spec.components.broadPatRotator to hand rotation back to CI",
		ErrBroadPATOwnedInCluster, label, what)
}
