package main

// ci_readiness.go implements `llz ci assert-loki` and `llz ci wait-harbor` — the
// native ports of assert-loki-bootstrapped.sh and wait-for-harbor.sh. The Loki
// classification (pod readiness, S3-config detection) is the tested internal/
// health predicates; this file is the kubectl orchestration + Harbor poll loops.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/objenc"
	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/harborauth"
)

func ciAssertLokiCmd() *cobra.Command {
	var nameMatch, region string
	var settle, interval int
	var noFlushProbe bool
	c := &cobra.Command{
		Use:   "assert-loki",
		Short: "fail unless Loki is bootstrapped (workloads Ready + S3-backed) on the current cluster",
		Long: "Native port of assert-loki-bootstrapped.sh. Asserts Loki's workloads are Ready\n" +
			"AND its config references S3 object storage (the kyverno loki-s3-object-store\n" +
			"policy mutates object_store filesystem→s3 — \"s3-backed\" is the real signal log\n" +
			"persistence works). Best-effort reports the Loki Argo Application status\n" +
			"(non-gating). Polls for a short settle budget so a transient kubectl/apiserver\n" +
			"blip (or a brief readiness / kyverno-mutation lag) doesn't flake the gate — the\n" +
			"same treatment assert-scrape-targets/assert-reconciler already carry. Exit 0\n" +
			"bootstrapped, 1 otherwise.\n\n" +
			"PROVES the write path rather than inferring it: if nothing has reached the chunks\n" +
			"bucket since the ingesters started, it POSTs /flush to each ingester and waits for a\n" +
			"chunk to land. Every other check here passes on a Loki that has never written a byte\n" +
			"(#397), and two observational attempts to catch that both passed vacuously on a real\n" +
			"cluster. --no-flush-probe keeps the gate strictly read-only at the cost of the proof.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertLoki(nameMatch, region, time.Duration(settle)*time.Second, time.Duration(interval)*time.Second, !noFlushProbe)
		},
	}
	c.Flags().StringVar(&nameMatch, "name-match", "loki", "substring/regex identifying Loki workloads/objects")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling for Loki to bootstrap before failing (rides out a transient kubectl blip / readiness lag)")
	c.Flags().IntVar(&interval, "interval", 10, "seconds between poll attempts")
	c.Flags().StringVar(&region, "region", "",
		"deployment whose spec names the Loki chunks bucket the write proof reads. Threaded in rather than "+
			"read from $REGION here, for the reason assertSuiteLanes documents: a call site that reaches for "+
			"the environment turns a missing value into a silent skip instead of a visible one")
	c.Flags().BoolVar(&noFlushProbe, "no-flush-probe", false,
		"do not POST /flush to the ingesters to force a write. The lane then reports UNPROVEN instead of "+
			"proving the write path, because nothing else on a young cluster makes Loki write on demand")
	return c
}

func ciWaitHarborCmd() *cobra.Command {
	var harborURL string
	var registryOnly bool
	c := &cobra.Command{
		Use:   "wait-harbor",
		Short: "wait for the harbor-registry rollout (the post-S3-seed gate)",
		Long: "Waits for harbor-registry to roll out. It mounts the harbor-registry-s3\n" +
			"Secret via secretKeyRef, so it stays in CreateContainerConfigError until that\n" +
			"Secret exists — seeded mid-bootstrap, then synced when the es-store-recovery\n" +
			"lane sees the store go Ready. Exit 0 rolled out, 1 on timeout.\n\n" +
			"This verb used to carry a second, PRE-seed half (admin Secret + control-plane\n" +
			"Deployments/StatefulSets + an API ping). That half gated the workflow's\n" +
			"`harbor` job, whose robot provisioning moved in-cluster in f0aa68f; the job\n" +
			"went with it and took the gate's only caller, leaving the code unreachable.\n" +
			"kick-harbor-provisioner now does its own harbor-core Available wait.\n\n" +
			"--registry-only and --harbor-url are accepted and IGNORED. Instance repos\n" +
			"vendor their workflows, so a rendered-but-not-yet-upgraded instance can still\n" +
			"pass --registry-only; rejecting it would break those instances on image bump\n" +
			"alone. They go once `llz upgrade` has carried the new call site everywhere.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIWaitHarbor(harborURL, registryOnly)
		},
	}
	c.Flags().StringVar(&harborURL, "harbor-url", os.Getenv("HARBOR_URL"), "accepted and ignored (vendored-workflow compatibility)")
	c.Flags().BoolVar(&registryOnly, "registry-only", false, "accepted and ignored — the registry rollout is now the only behavior (vendored-workflow compatibility)")
	_ = c.Flags().MarkDeprecated("harbor-url", "it is ignored; the API ping was retired with the pre-seed gate")
	_ = c.Flags().MarkDeprecated("registry-only", "it is ignored; the registry rollout is now the only behavior")
	return c
}

// ── assert-loki ──────────────────────────────────────────────────────────────

type lokiPod struct {
	ns, name string
	status   health.PodStatus
}

// runCIAssertLoki returns nil when Loki is bootstrapped and an error otherwise
// (cobra exits 1 on it). The ::error:: annotation is still written directly —
// GitHub parses an annotation only at the start of a line, and a returned error
// reaches stderr behind main.go's "llz: " prefix.
func runCIAssertLoki(nameMatch, region string, settle, interval time.Duration, allowFlush bool) error {
	fmt.Println("## Loki bootstrap assertion")

	// Poll the two gating conditions for a settle budget. lokiBootstrapped reads the
	// cluster via kItems, which collapses a kubectl/apiserver error to "no items" —
	// indistinguishable from a genuine absence. A single one-shot read therefore
	// turned a transient blip (an apiserver 5xx/429, an LKE-E HA control-plane replica
	// dropping seconds after wait-cluster-ready — a documented transient) into
	// "no Loki pods → exit 1". Re-evaluating until it passes (or the budget elapses)
	// rides out that blip and any brief readiness / kyverno-mutation lag, the same as
	// assert-scrape-targets. settle<=0 → a single evaluation (used by tests).
	var ok bool
	var msgs []string
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		ok, msgs = lokiBootstrapped(nameMatch, region, allowFlush)
		if ok || !time.Now().Before(deadline) {
			break
		}
		fmt.Printf("attempt %d: Loki not bootstrapped yet — retrying in %s\n", attempt, interval)
		time.Sleep(interval)
	}
	for _, m := range msgs {
		fmt.Println(m)
	}

	// Best-effort Argo CD Application status (non-gating).
	if kExists("get", "crd", "applications.argoproj.io") {
		re := regexp.MustCompile(nameMatch)
		for _, raw := range kItems("get", "applications.argoproj.io", "-A") {
			a, err := health.ParseArgoApp(raw)
			if err != nil || !re.MatchString(a.Name) {
				continue
			}
			if a.Sync == "Synced" && a.Health == "Healthy" {
				fmt.Printf("OK: Argo Application %s Synced + Healthy\n", a.Name)
			} else {
				fmt.Printf("WARN: Argo Application %s sync=%s health=%s (not gating on this)\n", a.Name, a.Sync, a.Health)
			}
			break
		}
	}

	if !ok {
		fmt.Fprintln(os.Stderr, "::error::Loki is not bootstrapped")
		return fmt.Errorf("loki is not bootstrapped")
	}
	fmt.Println("Loki is bootstrapped.")
	return nil
}

// lokiBootstrapped evaluates the two gating conditions — Loki workloads exist and
// are Ready, and the config is S3-backed — returning whether both hold plus the
// OK/FAIL lines to print. It is a pure read (no side effects), so the caller can
// re-run it across a settle budget: a transient kubectl failure surfaces here as
// "not bootstrapped" (kItems → empty) and is ridden out by the poll rather than
// hard-failing the gate on one blip.
func lokiBootstrapped(nameMatch, region string, allowFlush bool) (bool, []string) {
	ok := true
	var msgs []string

	// 1. Loki workloads exist and are Ready.
	pods := lokiPods(nameMatch)
	if len(pods) == 0 {
		msgs = append(msgs, fmt.Sprintf("FAIL: no Loki pods found (matched name~=%q)", nameMatch))
		ok = false
	} else {
		var notReady []string
		for _, p := range pods {
			if !health.LokiPodReady(p.status) {
				notReady = append(notReady, fmt.Sprintf("%s/%s phase=%s", p.ns, p.name, p.status.Phase))
			}
		}
		if len(notReady) > 0 {
			msgs = append(msgs, "FAIL: Loki pods not Ready:")
			for _, n := range notReady {
				msgs = append(msgs, "  "+n)
			}
			ok = false
		} else {
			msgs = append(msgs, fmt.Sprintf("OK: %d Loki pod(s) Ready", len(pods)))
		}
	}

	// 2. Loki is configured for S3 object storage (not the filesystem default).
	if health.LokiConfigUsesS3(lokiConfigText(nameMatch)) {
		msgs = append(msgs, "OK: Loki config references S3 object storage")
	} else {
		msgs = append(msgs, "FAIL: Loki config does not reference S3 — still on the filesystem default? (kyverno loki-s3-object-store may not have applied)")
		ok = false
	}

	// 3. Loki's writes are actually LANDING.
	//
	// Checks 1 and 2 are both properties of the cluster's INTENT — pods scheduled,
	// config pointing at S3 — and #397 is what it looks like when intent is perfect
	// and the outcome is nothing. Loki was Running, Ready, Synced, Healthy, S3
	// configured, and every PutObject returned 403 AccessDenied: 238 flush failures
	// on one ingester and a chunks bucket whose newest object predated the cluster
	// by ten days. This lane was color.Green throughout, because nothing here had ever
	// asked whether a byte was written.
	//
	// TWO SIGNALS, because neither alone is enough. Flush ERRORS catch active
	// breakage but only once Loki has tried, and on a fresh cluster it has not: the
	// first version of this scanned a 60s window and passed on a cluster that had
	// written nothing at all, which is precisely the blindness it was added to
	// remove. The OUTCOME signal — has anything reached the bucket since the
	// ingesters started — covers that, but only after enough time has passed that a
	// healthy Loki would have flushed. Together they cover both.
	failed, lines := applyLokiWriteVerdict(lokiWriteFindings(nameMatch, region, allowFlush))
	msgs = append(msgs, lines...)
	if failed {
		ok = false
	}
	return ok, msgs
}

// applyLokiWriteVerdict turns write findings into lines plus a pass/fail, applying
// the #397 deferral. Split out from lokiBootstrapped so the deferral rule can be
// tested on its own: through lokiBootstrapped it is invisible, because the pod and
// config checks fail first without a cluster and mask whatever this decides.
func applyLokiWriteVerdict(findings []lokiWriteMsg) (bool, []string) {
	var failed bool
	var lines []string
	for _, m := range findings {
		lines = append(lines, m.text)
		if !m.fatal {
			continue
		}
		if lokiWriteChecksGating {
			failed = true
			continue
		}
		lines = append(lines, "  ^ REPORTED, NOT GATING: this is "+lokiWriteChecksOpenIssue+", a known platform "+
			"defect this repo cannot fix — see lokiWriteChecksGating. The finding is real; it is not failing "+
			"the lane because there is no action available to whoever reads it.")
	}
	return failed, lines
}

// lokiWriteChecksGating decides whether a proven write outage FAILS the lane.
//
// TRUE. A cluster that cannot persist a log line is not a passing cluster, and this
// gate says so even though nobody reading the failure can fix it today.
//
// THE CONSEQUENCE IS DELIBERATE: while #397 is open, every e2e run is RED here. That
// is the point. The alternative — reporting the outage and passing anyway — buys a
// color.Green pipeline by making the pipeline mean less, and this repo has just spent three
// check designs learning where that leads. The first two versions of this check
// PASSED on clusters that had persisted nothing at all, and each looked reasonable
// while doing it.
//
// It went in as false for one run on the argument that an unsatisfiable gate is one
// people route around. That was overruled, and the overrule is right: a gate nobody
// can satisfy is a problem with the PLATFORM, not with the gate, and hiding it
// relocates the problem to somewhere nobody is looking.
//
// WHAT IT TAKES TO GO GREEN, so this is actionable rather than merely loud. The cause
// is Linode's Ceph RGW rejecting the AWS SDK's default aws-chunked trailer-checksum
// framing; the remedy is Loki's thanos client (use_thanos_objstore +
// send_content_md5), whose config apl-core owns on a managed App Platform. Measured,
// not assumed: LLZ's one admission-time lever does NOT reach it — a Kyverno mutation
// setting AWS_REQUEST_CHECKSUM_CALCULATION landed cleanly on an ingester and it still
// failed every flush and wrote nothing.
//
// Set to false only as a temporary, explicitly-owned exception with a date and a
// reason — never as a way to get a merge through.
var lokiWriteChecksGating = true

// lokiWriteChecksOpenIssue is the defect this gate currently catches. Named in the
// output so the failure carries its own diagnosis, and so that if anyone ever does
// flip lokiWriteChecksGating to false, the reason they are not failing is on record.
const lokiWriteChecksOpenIssue = "#397"

// lokiWriteMsg is one line of the write-path verdict; fatal ones fail the lane.
type lokiWriteMsg struct {
	text  string
	fatal bool
}

// lokiWriteFindings answers "is Loki actually persisting anything".
func lokiWriteFindings(nameMatch, region string, allowFlush bool) []lokiWriteMsg {
	var out []lokiWriteMsg

	fails := lokiFlushFailures(nameMatch)
	if len(fails) > 0 {
		out = append(out,
			lokiWriteMsg{fmt.Sprintf("FAIL: Loki is failing to flush chunks to object storage (%d error(s) in the last %s):", len(fails), lokiFlushWindow), true},
			lokiWriteMsg{"  " + fails[0], false},
			lokiWriteMsg{"  Loki being Ready and S3-configured does not mean it can WRITE. A 403 AccessDenied " +
				"here is usually NOT the credential — check it against a plain signed PUT before chasing the key. " +
				"Linode's Ceph RGW rejects the AWS SDK's default PutObject framing (content-encoding: aws-chunked " +
				"with an x-amz-trailer CRC32); the same credential succeeds without it. Loki's legacy S3 client " +
				"offers no knob for that and ignores AWS_REQUEST_CHECKSUM_CALCULATION — the remedy is Loki's " +
				"thanos client (use_thanos_objstore: true + send_content_md5: true), which apl-core owns. See #397", false})
		return out
	}
	out = append(out, lokiWriteMsg{fmt.Sprintf("OK: no chunk-flush failures in the last %s", lokiFlushWindow), false})

	// The PROOF half. This used to compare the bucket's newest object against the
	// ingesters' start time and, before enough time had passed for a healthy Loki to
	// flush, report UNPROVEN. That was honest but useless on the only timeline this
	// runs on: the e2e suite fires ~9 minutes after bootstrap, Loki holds a chunk for
	// up to chunk_idle_period, so the answer was always "too early to tell" and the
	// lane never actually confirmed anything. Two revisions of observational checking
	// both passed vacuously on a real cluster before this.
	//
	// So it asks Loki to write instead of waiting to see whether it did.
	return append(out, lokiProveWrites(nameMatch, region, allowFlush)...)
}

// lokiNow is the clock seam.
var lokiNow = time.Now

// lokiOldestIngesterStart is when the longest-running ingester came up.
func lokiOldestIngesterStart(nameMatch string) (time.Time, error) {
	var oldest time.Time
	for _, p := range lokiPodsFn(nameMatch) {
		if !strings.Contains(p.name, "ingester") {
			continue
		}
		t, err := lokiPodStart(p.ns, p.name)
		if err != nil {
			continue
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	if oldest.IsZero() {
		return oldest, fmt.Errorf("no ingester pod reported a start time")
	}
	return oldest, nil
}

// Seams for the outcome half.
var (
	lokiPodStart = func(ns, pod string) (time.Time, error) {
		out, err := kubectlOut("-n", ns, "get", "pod", pod, "-o", "jsonpath={.status.startTime}")
		if err != nil {
			return time.Time{}, err
		}
		return time.Parse(time.RFC3339, strings.TrimSpace(out))
	}
	// lokiChunksTarget resolves the bucket + endpoint from the deployment spec, the
	// same derivation assert-obj-encryption uses, so the two cannot disagree about
	// which bucket "Loki's" means.
	lokiChunksTarget = func(region string) (string, string) {
		if region == "" {
			return "", ""
		}
		lz, err := clusterspec.LoadInstance(".")
		if err != nil {
			return "", ""
		}
		e, ok := lz.Env(region)
		if !ok {
			return "", ""
		}
		return clusterspec.ObjLokiChunksBucket(lz.ObjLabelPrefix(), region), clusterspec.ObjEndpointHost(e.Cluster.ObjectStorage.Cluster)
	}
)

// lokiFlushWindow is how far back the flush-failure scan looks.
//
// SHORT ON PURPOSE. assert-loki polls for a settle budget (120s by default) and
// re-evaluates this each attempt, so the window must be short enough that a single
// transient S3 blip AGES OUT inside that budget — otherwise one hiccup during
// bootstrap reds the lane for the whole settle. A minute does that, and the failure
// this catches recurs every few seconds, so it cannot hide in the gap.
var lokiFlushWindow = time.Minute

// lokiFlushFailures returns recent chunk-flush errors from Loki's writers.
//
// LOGS, NOT METRICS, deliberately. The equivalent metric lives behind Prometheus or
// a port-forward to each pod, and a check that needs a working monitoring stack to
// tell you the monitoring stack is broken is the wrong shape. The log line is
// unambiguous, already the thing an operator greps for, and needs nothing but the
// cluster access this gate already has.
//
// This proves FAILURE, not success: a quiet window can also mean Loki simply had
// nothing to flush yet, which on a fresh bootstrap is normal. That asymmetry is why
// assert-obj-encryption separately reports a bucket with no writes since the cutover
// — between them, "nothing written" cannot pass as "everything fine".
func lokiFlushFailures(nameMatch string) []string {
	var out []string
	for _, p := range lokiPodsFn(nameMatch) {
		// Only the components that WRITE. Queriers and gateways read, and scanning
		// them adds log volume without adding signal.
		if !strings.Contains(p.name, "ingester") && !strings.Contains(p.name, "compactor") {
			continue
		}
		raw, err := lokiLogs(p.ns, p.name, lokiFlushWindow)
		if err != nil {
			continue // a pod that just restarted / is terminating is not evidence
		}
		for _, line := range strings.Split(raw, "\n") {
			if strings.Contains(line, "failed to flush") {
				out = append(out, strings.TrimSpace(harborauth.TruncateForError([]byte(p.ns+"/"+p.name+": "+line))))
			}
		}
	}
	return out
}

// lokiLogs and lokiPodsFn are the seams for the outcome check. The flush-failure
// scan is the one check in this lane that reads a RUNNING process's behaviour rather
// than the cluster's declared intent, which is exactly what made it the check that
// was missing — and exactly why it needs to be exercisable without a cluster.
var (
	lokiLogs = func(ns, pod string, since time.Duration) (string, error) {
		return kubectlOut("-n", ns, "logs", pod, "--since="+since.String(), "--tail=200")
	}
	lokiPodsFn = lokiPods
)

// lokiPods returns the Loki pods, preferring the app.kubernetes.io/name label and
// falling back to a name-regex match over all pods (so it doesn't depend on one
// labelling convention).
func lokiPods(match string) []lokiPod {
	items := kItems("get", "pods", "-A", "-l", "app.kubernetes.io/name="+match)
	filterByName := false
	if len(items) == 0 {
		items = kItems("get", "pods", "-A")
		filterByName = true
	}
	re := regexp.MustCompile(match)
	var out []lokiPod
	for _, raw := range items {
		var p struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
			Status health.PodStatus `json:"status"`
		}
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		if filterByName && !re.MatchString(p.Metadata.Name) {
			continue
		}
		out = append(out, lokiPod{p.Metadata.Namespace, p.Metadata.Name, p.Status})
	}
	return out
}

// lokiConfigText concatenates the data values of every name-matching ConfigMap
// (where the rendered Loki config lives) so the S3 detection can scan it.
func lokiConfigText(match string) string {
	re := regexp.MustCompile(match)
	var b strings.Builder
	for _, raw := range kItems("get", "configmap", "-A") {
		var cm struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Data map[string]string `json:"data"`
		}
		if json.Unmarshal(raw, &cm) != nil || !re.MatchString(cm.Metadata.Name) {
			continue
		}
		for _, v := range cm.Data {
			b.WriteString(v)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ── wait-harbor ──────────────────────────────────────────────────────────────

// runCIWaitHarbor waits for the harbor-registry rollout — the post-S3-seed gate.
// harbor-registry mounts the harbor-registry-s3 Secret via secretKeyRef, so it
// stays in CreateContainerConfigError until that Secret exists (seeded
// mid-bootstrap, then synced by the es-store-recovery lane when the store goes
// Ready).
//
// The harborURL / registryOnly parameters are vestigial and ignored; see the
// command's help for why they are still accepted.
//
// It polls a REAL budget and self-reports non-fatally rather than running a
// single 2m `kubectl rollout status`. harbor-registry only becomes schedulable
// once its ExternalSecret syncs — a few minutes after the KV seed on a fresh
// bootstrap — so that single 2m wait predictably timed out, and the caller's
// `continue-on-error` then painted a color.Green check over a scary
// "error: timed out … exit code 1" that carried no signal (it "passed" whether
// or not the registry came up). Now a healthy bootstrap goes color.Green honestly, and
// a genuine stall is a visible ::warning:: — the convergence gate is the hard
// check — never a masked error. That is what lets the caller drop
// continue-on-error.
func runCIWaitHarbor(_ string, _ bool) error {
	allRolled := true
	for _, d := range health.HarborRegistryDeployments() {
		if waitPoll(harborWaitBudget, 10*time.Second, func() bool { return deploymentRolledOut("harbor", d) }) {
			fmt.Printf("harbor deployment %q rolled out.\n", d)
			continue
		}
		allRolled = false
		fmt.Fprintf(os.Stderr, "::warning::harbor deployment %q not rolled out within %s — its harbor-registry-s3 ExternalSecret may still be syncing; the convergence gate will catch a genuine stall.\n", d, harborWaitBudget)
	}
	if allRolled {
		fmt.Println("harbor-registry rolled out.")
	}
	return nil
}

func ciHarborTrustObjProxyCACmd() *cobra.Command {
	return &cobra.Command{
		Use:   "harbor-trust-obj-proxy-ca",
		Short: "roll harbor-registry if its pods predate the obj-proxy CA policy (no-op when objProxy is off)",
		Long: "Closes the admission race between apl-core's Harbor install and the Kyverno\n" +
			"policy that mounts the obj-proxy CA. The policy mutates on ADMISSION, so pods\n" +
			"that already existed when it landed carry no CA — and once the CoreDNS rewrite\n" +
			"is live those pods cannot complete a single S3 call, without crashing and\n" +
			"without reporting anything.\n\n" +
			"MUST RUN AFTER CONVERGE. It keys off the ClusterPolicy's existence to decide\n" +
			"whether objProxy is enabled here, and the policy arrives with the component's\n" +
			"Argo sync — so running it earlier (it used to be folded into wait-harbor) sees\n" +
			"no policy, concludes the component is off, and silently does nothing.\n\n" +
			"Always exits 0: a failure to repair is a ::warning::, and\n" +
			"`llz ci assert-obj-encryption` is the gate that fails.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			retrofitHarborObjProxyCA()
			return nil
		},
	}
}

// retrofitHarborObjProxyCA closes the admission race that would otherwise make the
// obj-proxy CoreDNS rewrite a Harbor outage.
//
// THE RACE. The Kyverno policy that mounts the obj-proxy CA into registry pods is a
// MUTATE-ON-ADMISSION rule, so it only ever affects pods created after it exists.
// harbor-registry is installed by apl-core, and the objProxy component syncs
// alongside it — so whether the registry's running pods carry the CA is decided by
// which of the two won a race nobody controls. Once the DNS rewrite is live, a pod
// that lost that race cannot complete a single S3 call: it dials the proxy, fails to
// verify a certificate from a CA it does not trust, and every push and pull fails.
// It does not crash, so nothing restarts it and nothing reports it — Harbor is
// simply broken until a human notices.
//
// A previous run passing proves only that the race was won that time. This makes it
// deterministic: check what is actually running, and roll the deployment if any pod
// is missing the mutation.
//
// Best-effort by construction — it warns and returns rather than failing. The
// convergence gate and `llz ci assert-obj-encryption`'s [pod] check are the hard
// checks; this is the repair that stops them from having to fire.
//
// ORDERING IS LOAD-BEARING: see ciHarborTrustObjProxyCACmd. This reads the
// ClusterPolicy to decide whether the component is enabled, so it is only correct
// once that policy has synced.
func retrofitHarborObjProxyCA() {
	// Absent policy = the objProxy component is not enabled on this cluster. Not a
	// problem to repair; nothing here applies.
	if _, err := harborCARetrofitKubectl("get", "clusterpolicy", objProxyCAPolicy); err != nil {
		return
	}
	findings := objenc.CheckRegistryPodsCarryCA(objencDeps())
	if len(findings) == 0 {
		fmt.Printf("harbor-registry pods already trust the obj-proxy CA (clusterpolicy/%s).\n", objProxyCAPolicy)
		return
	}
	for _, f := range findings {
		fmt.Printf("retrofit: %s\n", f.Problem)
	}

	fmt.Printf("::notice::harbor-registry pods predate clusterpolicy/%s — rolling them so the mutation applies.\n", objProxyCAPolicy)
	for _, d := range health.HarborRegistryDeployments() {
		if _, err := harborCARetrofitKubectl("-n", objenc.HarborNS, "rollout", "restart", "deploy/"+d); err != nil {
			fmt.Fprintf(os.Stderr, "::warning::could not roll harbor/%s to pick up the obj-proxy CA (%v) — `kubectl -n %s rollout restart deploy/%s` by hand before relying on Harbor.\n", d, err, objenc.HarborNS, d)
			return
		}
		if !waitPoll(harborWaitBudget, 10*time.Second, func() bool { return harborCARetrofitRolledOut(objenc.HarborNS, d) }) {
			fmt.Fprintf(os.Stderr, "::warning::harbor/%s did not finish rolling within %s after the obj-proxy CA retrofit.\n", d, harborWaitBudget)
			return
		}
	}

	// Re-read rather than assume: the restart is only a fix if the new pods actually
	// came back mutated. If Kyverno was down, they did not, and saying so here is the
	// difference between a warning and a silent Harbor outage.
	if findings := objenc.CheckRegistryPodsCarryCA(objencDeps()); len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintf(os.Stderr, "::warning::obj-proxy CA retrofit did not take: %s\n", f.Problem)
		}
		return
	}
	fmt.Println("retrofit: harbor-registry now trusts the obj-proxy CA.")
}

// objProxyCAPolicy is the ClusterPolicy whose presence means the objProxy component
// is enabled here. Must match metadata.name in
// platform-apl/components/objProxy/obj-proxy/kyverno-harbor-ca.yaml.
const objProxyCAPolicy = "harbor-obj-proxy-ca"

// harborCARetrofitKubectl is the write seam for the retrofit (checkRegistryPodsCarryCA
// brings its own read seam). harborCARetrofitRolledOut seams the rollout wait for the
// same reason, and for one more: deploymentRolledOut reaches kubectl directly, so
// without it a test would poll the real harborWaitBudget against no cluster.
var (
	harborCARetrofitKubectl   = kubectlOut
	harborCARetrofitRolledOut = deploymentRolledOut
)

// harborWaitBudget is the wall-clock deadline for each Harbor readiness poll
// (the former harborPoll's 60 attempts × 10s). waitPoll (ci_wait.go) bounds the
// total wait, so a slow probe can't stretch it the way the old attempt-count
// loop could. A var (not const) so tests can shrink it to avoid a real 10m poll.
var harborWaitBudget = 600 * time.Second

// deploymentRolledOut reports whether the deployment has all its desired replicas
// available. A quiet kubectl read (jsonpath), unlike `rollout status`, which prints
// a noisy "error: timed out" on its deadline — so the registry gate can poll a real
// budget without littering the log with per-attempt errors.
func deploymentRolledOut(namespace, name string) bool {
	out, err := kubectlOut("-n", namespace, "get", "deployment", name,
		"-o", "jsonpath={.status.availableReplicas}/{.spec.replicas}")
	if err != nil {
		return false
	}
	return replicasRolledOut(out)
}

// replicasRolledOut parses "<available>/<desired>" and reports whether every
// desired replica is available (desired>0, available>=desired). Pure — unit-tested.
func replicasRolledOut(availSlashDesired string) bool {
	parts := strings.SplitN(strings.TrimSpace(availSlashDesired), "/", 2)
	if len(parts) != 2 {
		return false
	}
	avail, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	desired, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return false
	}
	return desired > 0 && avail >= desired
}
