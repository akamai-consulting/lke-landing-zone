package doctor

// linode.go — the one thing `llz doctor` never asked: can this LINODE
// ACCOUNT actually build what the spec describes?
//
// doctor is billed as the single "am I ready to build?" gate, but every check it
// ran was local or GitHub — tooling on PATH, gh auth, spec files, repo config. The
// three prerequisites the quickstart opens with (a Linode account WITH
// LKE-Enterprise, an APL entitlement, a GitHub org) were exactly the ones it could
// not see, and `cluster.k8sVersion` was validated for non-emptiness only.
//
// So an account without LKE-E entitlement, or a spec pinning a `+lke` version that
// has since been retired, got a GREEN doctor — and then `llz up` provisioned real
// cloud resources (state bucket + scoped OBJ key), dispatched a workflow, and
// failed inside terraform apply. Worse, per docs/lessons-learned.md, an LKE-E
// create that cannot be envreq.Satisfied does not always fail: it can hang on
// "Still creating..." to the job timeout, a mode that doc explicitly calls not
// reliably diagnosable. This turns the most expensive failure in the flow into a
// line of local output before anything is provisioned.
//
// IT REPORTS AT FULL VOLUME AND BLOCKS NOTHING, and getting to that took an
// argument in both directions — so here is the whole of it, because the next
// person will be tempted to move it again.
//
// It began advisory under a stated uncertainty: the route's existence was
// verified but "its response BODY has not been seen against an entitled account",
// so a check that could not be fully verified must not block a build that would
// have worked. That uncertainty is now GONE — the catalog has been measured (see
// linode.ListLKEVersions) — and the same question HARD FAILS on the apply path as
// `llz ci assert-k8s-version`, added after a bad pin cost a release-e2e round on
// 2026-08-11. On that basis it was briefly made fatal here too, to avoid the
// doctor-green/build-red pattern onboard/doctor_build_preflights.go exists to
// eliminate.
//
// THE ARGUMENT FOR BLOCKING DIED WITH THE COST IT WAS BUYING DOWN. Doctor blocks
// to save an EXPENSIVE failure — the whole file it borrows that reasoning from is
// about a ~15-minute cluster apply. The CI gate now catches this in the first job,
// in seconds, naming the versions the account does offer. What a green doctor
// costs an operator here is therefore one dispatch that fails almost immediately
// with a better message than doctor could print.
//
// AND THE VERDICT IS NOT ABOUT THE ACCOUNT THAT WILL BUILD. Availability is
// per-ACCOUNT, and this reads whatever token is in the operator's shell —
// `llz tokens` PROMPTS for the PAT it pushes as the LINODE_API_TOKEN repo secret
// rather than reading $LINODE_TOKEN, so the two really can differ. Blocking made
// `llz up` abort on a spec CI would have built fine, on the primary onboarding
// path, and the only way past it was to unset the token — which also silently
// disabled objlabel_preflight, the OTHER account-reading check.
//
// objlabel_preflight is the precedent, in this same tree and for this same
// reason: "it can only see this account's buckets, so it never gates". A check
// that reads a different system than the one CI will read reports; it does not
// decide.
//
// WHAT KEEPS THE ORIGINAL COMPLAINT ANSWERED is volume, not exit status. A
// definite mismatch prints a red ✗ and says in as many words that
// `llz ci assert-k8s-version` will fail the build — so an operator who reads
// doctor is never surprised by CI, which was the actual harm. Uncertainty (no
// token, an auth failure, an empty or unparseable list, a catalog too coarse to
// settle the pin) is reported as UNKNOWN and never as "not offered".
//
// THE VERDICT IS SHARED WITH THE GATE. Both call linode.CheckVersion, so the
// two cannot reach different conclusions about one spec — and now they cannot
// reach different CONSEQUENCES either, which is what "one signal" has to mean.
// Presentation stays local: this prints a compact report, the CI verb prints
// remediation prose.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// lkeRequestTimeout bounds one HTTP call; lkeProbeTimeout bounds the whole
// section, which makes up to two (the version catalog, then the cluster list).
// A READ IS NOT A REQUEST: http.Client.Timeout bounds one HTTP call, and both of
// these reads go through listAllPages, which issues one per 100-item page. The
// probe budget is therefore sized from a per-READ allowance (several pages) and
// the attempt count — deriving it from the per-request timeout under-counts any
// account big enough to paginate.
//
// The read budget is also APPLIED per read, not merely used to size the parent:
// otherwise a slow catalog read starves the exemption read, which is the regression
// the constant exists to prevent. See assertplatform/k8sversion.go, which carries
// the same three constants and the same reasoning.
const (
	lkeRequestTimeout = 20 * time.Second
	lkeReadPages      = 5
	lkeReadBudget     = lkeReadPages * lkeRequestTimeout
	lkeProbeTimeout   = (1+linode.ClusterReadAttempts)*lkeReadBudget +
		linode.ClusterReadAttempts*linode.ClusterReadRetryPause + 10*time.Second
)

// lkeSleep is the pause between exemption attempts. A seam so the tests do not
// spend it.
var lkeSleep = time.Sleep

// lkeVersionLister is the slice of the Linode client this needs — a seam so the
// tests never touch the network.
type lkeVersionLister interface {
	ListLKEVersions(ctx context.Context, tier string) ([]string, error)
	ListClusters(ctx context.Context) ([]map[string]any, error)
}

// linodeTokenVars is the fallback order, written down once.
var linodeTokenVars = []string{"LINODE_TOKEN", "LINODE_API_TOKEN"}

// doctorLinodeToken names the environment variable that supplied the token, or ""
// when none did. Split out because the report has to say WHOSE answer it is: the
// question is per-ACCOUNT, and the operator's shell need not hold the credential
// the LINODE_API_TOKEN repo secret does. Naming the variable is what turns a
// confusing disagreement into a one-line diagnosis.
func doctorLinodeToken() (name, tok string) {
	for _, n := range linodeTokenVars {
		if v := os.Getenv(n); v != "" {
			return n, v
		}
	}
	return "", ""
}

// doctorLinodeClient returns a live client, or nil when no token is configured.
var doctorLinodeClient = func() lkeVersionLister {
	_, tok := doctorLinodeToken()
	if tok == "" {
		return nil
	}
	// Fenced: doctor reports, it does not change the account. See
	// capability.ReadOnlyCloud for why this tree takes a handle rather than a
	// binding.
	return capability.ReadOnlyCloud().Client(tok, lkeRequestTimeout)
}

// K8sPin is one deployment's k8sVersion together with the identity needed to ask
// whether a cluster is ALREADY running it. The label and region are what make the
// existing-cluster exemption possible; a bare version string cannot express it.
type K8sPin struct {
	Env, Version, ClusterLabel, Region string
	// Shared marks a pin equal to spec.defaults.cluster.k8sVersion — i.e. one
	// landingzone.yaml plausibly owns. It decides which FILE the remediation names,
	// and naming the wrong one sends the operator to fix a value that is not there:
	// they unblock one deployment and the rest fail one dispatch at a time.
	Shared bool
}

// ReportLinodeAccount prints doctor's "Linode account" section for the k8s
// versions the spec pins. Returns nothing: it reports a different account than
// the one CI builds under, so it decides nothing. See the header.
func ReportLinodeAccount(want []K8sPin) {
	fmt.Println("\n" + color.Bold("Linode account (advisory — CI decides, with the credential that will build):"))

	if len(want) == 0 {
		// NOTHING TO CHECK ⇒ NO REQUEST. The sibling CI verb states this rule and
		// follows it ("a verb that reaches for a token it does not need fails wherever
		// there is none"); this used to make the catalog request first and then report
		// that it had no pins, spending a round-trip in a spec-less checkout to learn
		// what it already knew.
		//
		// REACHABLE IS NOT CHECKED, which is the other half. Printing a green
		// "LKE-Enterprise reachable" line and stopping reads as a clean bill of health
		// for a deployment nobody looked at — and the input that most often produces
		// an empty set is an `--env` the spec does not define, on which
		// `llz ci assert-k8s-version` HARD FAILS.
		fmt.Printf("  %s  no k8sVersion pins to check — no spec here, the spec defines no such deployment, or none pins a version\n", color.Dim("–"))
		return
	}

	c := doctorLinodeClient()
	if c == nil {
		fmt.Printf("  %s  LKE-Enterprise check skipped — set LINODE_TOKEN to verify the account offers your k8sVersion\n", color.Dim("–"))
		return
	}
	// COVERS BOTH READS. The section used to make one request and the probe budget
	// equalled the per-request one; the existing-cluster exemption added a second,
	// so a slow catalog read would leave the cluster read to be cancelled by the
	// parent — reporting "could not check" for a deployment that is running fine,
	// on the one path where that answer is load-bearing.
	ctx, cancel := context.WithTimeout(context.Background(), lkeProbeTimeout)
	defer cancel()

	cctx, ccancel := context.WithTimeout(ctx, lkeReadBudget)
	got, err := c.ListLKEVersions(cctx, linode.LKETierEnterprise)
	ccancel()
	switch {
	case err != nil:
		// Includes 401/403 — which is the interesting case, because it is what a
		// token without the scope, or an account without the entitlement, looks
		// like. Never fatal: it is also what a network blip looks like.
		fmt.Printf("  %s  could not list LKE-Enterprise versions — check the PAT's scope and that the account is LKE-Enterprise entitled\n", color.Yellow("!"))
		fmt.Printf("      %s\n", color.Dim(cigate.FirstLine(err.Error())))
		return
	case len(got) == 0:
		fmt.Printf("  %s  the account reports NO LKE-Enterprise versions — an apply would have nothing to create\n", color.Yellow("!"))
		return
	}

	if name, _ := doctorLinodeToken(); name != "" {
		// WHICH ACCOUNT ANSWERED IS PART OF THE ANSWER, and it is the reason this
		// section reports rather than decides. Availability is per-account, so a
		// verdict from the operator's shell credential says nothing about the account
		// CI builds under if the two differ — and without this line a red ✗ against a
		// spec CI builds fine looks simply broken.
		fmt.Printf("  %s\n", color.Dim("answered by the account behind $"+name+"; CI uses the LINODE_API_TOKEN repo secret, which may be a different account"))
	}
	fmt.Printf("  %s  LKE-Enterprise reachable — %d version(s) offered\n", color.Green("✓"), len(got))

	// Only read once, and only if something is actually unoffered — the common
	// case is every pin offered, and that must cost one request.
	var clusters []map[string]any
	var clusterErr error
	clustersRead := false

	for _, p := range want {
		verdict, nearest := linode.CheckVersion(p.Version, got)
		switch verdict {
		case linode.VersionOffered:
			fmt.Printf("  %s  k8sVersion %s is offered\n", color.Green("✓"), p.Version)
			continue
		case linode.VersionUnknown:
			// The catalog answered in a spelling that cannot disprove this pin. Report
			// it and move on — a list of bare major.minor has no standing to reject a
			// `+lke` build, and the CI gate declines the same shape, so doctor must not
			// become the stricter of two checks that are supposed to be one.
			fmt.Printf("  %s  k8sVersion %s is UNCHECKED — the account's list (%s) is not the catalog shape this was measured against\n",
				color.Yellow("!"), p.Version, strings.Join(got, ", "))
			continue
		}
		if !clustersRead {
			clusters, clusterErr = clustersForExemption(ctx, c)
			clustersRead = true
		}
		switch {
		case clusterErr != nil:
			// THIS ARM MUST NOT PREDICT CI'S VERDICT. The pin is not in the catalog, but
			// whether a cluster already runs it — which would exempt the deployment
			// entirely — could not be read HERE, and CI makes that read itself, with its
			// own retries. Saying "CI will fail this" off a local 503 sends an operator
			// to bump cluster.k8sVersion for a deployment that never needed it.
			fmt.Printf("  %s  k8sVersion %s is NOT in the account's list, and whether %s already runs it could not be checked here\n",
				color.Yellow("!"), p.Version, p.ClusterLabel)
			fmt.Printf("      %s\n", color.Dim(cigate.FirstLine(clusterErr.Error())))
			fmt.Printf("      %s\n", color.Dim("offered: "+strings.Join(got, ", ")))
			fmt.Printf("      %s\n", color.Dim("`llz ci assert-k8s-version` re-runs this read; if no cluster is on that version it will fail the build. Fix: "+pinSource(p)))
		case linode.ClusterRunsVersion(clusters, p.ClusterLabel, p.Region, p.Version):
			// The pin left the catalog under a cluster that is already on it.
			// terraform plans no change to k8s_version, so nothing is blocked — but
			// the deployment cannot be RE-CREATED until the pin moves, and that is
			// worth saying out loud before someone finds out during a rebuild.
			fmt.Printf("  %s  k8sVersion %s has left the account's list, but %s already runs it — no apply is blocked; it cannot be re-created until the pin moves\n",
				color.Yellow("!"), p.Version, p.ClusterLabel)
		default:
			fmt.Printf("  %s  k8sVersion %s is NOT in the account's list — a retired or mistyped pin fails (or HANGS) at apply\n", color.Red("✗"), p.Version)
			if nearest != "" {
				// One character apart: name it rather than making them diff the list.
				fmt.Printf("      %s\n", color.Cyan("the account offers "+nearest+" — the same version spelled differently, and terraform sends the pin verbatim"))
			} else {
				fmt.Printf("      %s\n", color.Dim("offered: "+strings.Join(got, ", ")))
			}
			reportWillFailCI(p)
		}
	}
}

// reportWillFailCI is what replaces the exit code. The complaint that made this
// section briefly fatal was never really about doctor's status — it was that a
// green doctor let an operator walk into a red build having been told they were
// ready. Saying "CI will fail this, here is the fix" answers that, from a check
// that read a DIFFERENT ACCOUNT than the one CI will read and therefore has no
// business deciding. See the header.
func reportWillFailCI(p K8sPin) {
	fmt.Printf("      %s\n", color.Red("`llz ci assert-k8s-version` will FAIL the build on this — the pipeline stops in its first job."))
	fmt.Printf("      %s\n", color.Cyan("fix: "+pinSource(p)))
	fmt.Printf("      %s\n", color.Dim("verdict from "+answeringAccount()))
}

// clustersForExemption reads the account's clusters, retrying a bounded number of
// times. The exemption is a PROOF that terraform will not send the pin, and this
// refuses to let an unprovable one acquit — so a single 503 must not be what
// decides that a running deployment is unbuildable. Mirrors the CI gate's arm; see
// linode.ClusterReadAttempts.
func clustersForExemption(ctx context.Context, c lkeVersionLister) ([]map[string]any, error) {
	var err error
	for attempt := 1; attempt <= linode.ClusterReadAttempts; attempt++ {
		var clusters []map[string]any
		rctx, rcancel := context.WithTimeout(ctx, lkeReadBudget)
		clusters, err = c.ListClusters(rctx)
		rcancel()
		if err == nil {
			return clusters, nil
		}
		// Our own deadline is not a transient error — retrying against an expired
		// context burns the pause and fails again for the same reason. Same arm as the
		// CI gate's, INCLUDING that it asks ctx and never the error: an
		// http.Client.Timeout unwraps to context.DeadlineExceeded, so classifying on
		// the error would read one slow request as the whole probe running out. ctx is
		// read after the call returns, so it cannot be nil here.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("the probe budget expired before the account's clusters could be listed (%v): %w", ctx.Err(), err)
		}
		if attempt < linode.ClusterReadAttempts {
			lkeSleep(linode.ClusterReadRetryPause)
		}
	}
	return nil, err
}

// answeringAccount names the credential the verdict came from.
//
// WHOSE ANSWER IS IT. The question is per-ACCOUNT, and this reads whatever token
// is in the operator's shell — which is NOT the account CI builds under by
// construction: `llz tokens` PROMPTS for the PAT it pushes as the
// LINODE_API_TOKEN repo secret rather than reading $LINODE_TOKEN, so someone
// working across accounts can hold one locally and ship another.
//
// That is the reason this section reports rather than decides (see the header),
// and it is also why the report has to name the credential: an operator seeing a
// red ✗ against a spec CI builds fine needs one line to tell them which of the two
// accounts disagreed, not an afternoon.
func answeringAccount() string {
	name, _ := doctorLinodeToken()
	if name == "" {
		return "this Linode account"
	}
	return "$" + name + "'s Linode account (CI builds under the LINODE_API_TOKEN repo secret, " +
		"which may be a different account — CI's answer is the one that decides)"
}

// pinSource names the file to edit. Kept in step with assertplatform's
// k8sVersionFixHint deliberately — the two are the same instruction, and an
// operator meeting one after the other must not be sent to different files.
func pinSource(p K8sPin) string {
	if p.Shared {
		return fmt.Sprintf("landingzone.yaml (spec.defaults.cluster.k8sVersion, inherited by every "+
			"deployment) or override it in environments/%s.yaml", p.Env)
	}
	return fmt.Sprintf("environments/%s.yaml (cluster.k8sVersion)", p.Env)
}

// SpecK8sPins returns the pins doctor should check against the account: just the
// named deployment's when one is given, otherwise every deployment's. Silent on a
// repo with no spec — doctor runs there too.
//
// A NAMED-BUT-UNKNOWN DEPLOYMENT RETURNS NOTHING. It used to fall THROUGH to every
// env, so `llz doctor --env prd` (a typo) reported on pins the operator never
// asked about, in deployments they may not own — and `llz ci assert-k8s-version`
// hard-fails on that same input rather than widening. Scope follows the flag.
func SpecK8sPins(env string) []K8sPin {
	lz, present, err := caps.LoadSpec()
	if !present || err != nil || lz == nil {
		return nil
	}
	names := lz.EnvNames()
	if env != "" {
		if _, ok := lz.Env(env); !ok {
			return nil
		}
		names = []string{env}
	}
	// No dedup: EnvNames() is a map's key set, so the names are already unique, and
	// one pin per DEPLOYMENT is what the exemption needs — two deployments sharing a
	// version still have different clusters to ask about. (The predecessor keyed a
	// `seen` map on the version and collapsed them, which is why a vestigial
	// name-keyed one survived here for a while, unable to fire.)
	var out []K8sPin
	for _, n := range names {
		e, ok := lz.Env(n)
		if !ok {
			continue
		}
		v := strings.TrimSpace(e.Cluster.K8sVersion)
		if v == "" {
			continue
		}
		out = append(out, K8sPin{
			Env:          n,
			Version:      v,
			ClusterLabel: strings.TrimSpace(e.Cluster.ClusterLabel),
			Region:       strings.TrimSpace(e.Cluster.Region),
			Shared:       v == strings.TrimSpace(lz.Spec.Defaults.Cluster.K8sVersion),
		})
	}
	return out
}
