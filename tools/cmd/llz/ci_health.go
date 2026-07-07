package main

// ci_health.go implements `llz ci health` and `llz ci converge` — the native
// ports of check-cluster-health.sh and converge.sh. Every classification is the
// tested internal/health predicate; this file is the kubectl orchestration that
// feeds them and the convergence-contract exit code (1 hard-failed / 2 in-progress
// / 0 converged). `converge` polls `health` until it converges or the budget runs out.

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/spf13/cobra"
)

// healthNamespaces are the namespaces this repo touches (matches the script's
// NAMESPACES) — iterated for per-namespace checks.
var healthNamespaces = []string{
	"argocd", "kube-system", "cert-manager", "cert-automation", "external-secrets",
	"openbao", "observability", "harbor", "istio-system",
}

const openbaoNamespace = "llz-openbao"

func ciHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "cluster convergence health check (exit 0 converged / 2 in-progress / 1 hard-failed / 3 unreachable)",
		Long: "Native port of check-cluster-health.sh — the single source of truth for \"is\n" +
			"the cluster converged?\". Runs every in-cluster check (foundations, OpenBao,\n" +
			"cert-manager, ESO, Argo apps, workloads, storage, jobs, …) against the cluster\n" +
			"$KUBECONFIG points at, classifying each via the unit-tested internal/health\n" +
			"predicates, and exits per the convergence contract: 1 hard-failed, 2 in-\n" +
			"progress (poll), 0 converged, 3 apiserver unreachable (an infrastructure\n" +
			"transient, retried against the budget — never a hard strike).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			os.Exit(healthExitCode())
			return nil
		},
	}
}

func ciConvergeCmd() *cobra.Command {
	var budget, interval, retryDelay int
	c := &cobra.Command{
		Use:   "converge",
		Short: "poll `llz ci health` until the cluster converges or the budget runs out",
		Long: "Native port of converge.sh. Polls `llz ci health` (exit 0/1/2/3): converged\n" +
			"-> exit 0; in-progress -> sleep --interval and re-run until --budget elapses\n" +
			"(then exit 1); hard-failed -> re-run once after --retry-delay to absorb a\n" +
			"transient, and exit 1 only if it hard-fails twice in a row; apiserver\n" +
			"unreachable -> re-run after --retry-delay against the budget without spending\n" +
			"a hard strike (a blip can't trip the twice-in-a-row abort).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			os.Exit(runConverge(budget, interval, retryDelay))
			return nil
		},
	}
	c.Flags().IntVar(&budget, "budget", 1800, "total elapsed-time budget in seconds")
	c.Flags().IntVar(&interval, "interval", 30, "seconds between in-progress polls")
	c.Flags().IntVar(&retryDelay, "retry-delay", 60, "seconds before re-running a hard-fail check")
	return c
}

// ── converge loop ────────────────────────────────────────────────────────────

func runConverge(budget, interval, retryDelay int) int {
	deadline := time.Now().Add(time.Duration(budget) * time.Second)
	for attempt := 1; ; attempt++ {
		fmt.Fprintf(os.Stderr, "::notice::convergence poll attempt %d\n", attempt)
		switch health.ConvergeStep(healthExitCode()) {
		case health.ConvergeDone:
			return 0
		case health.ConvergePoll:
			if time.Now().After(deadline) {
				fmt.Fprintf(os.Stderr, "::error::budget of %ds exhausted with the cluster still in-progress.\n", budget)
				return 1
			}
			time.Sleep(time.Duration(interval) * time.Second)
		case health.ConvergeRetryHard:
			fmt.Fprintf(os.Stderr, "::warning::hard failure reported — re-checking after %ds to absorb transients.\n", retryDelay)
			time.Sleep(time.Duration(retryDelay) * time.Second)
			if health.ConvergeStep(healthExitCode()) == health.ConvergeRetryHard {
				fmt.Fprintln(os.Stderr, "::error::cluster hard-failed twice in a row — operator intervention required.")
				return 1
			}
			// recovered to converged/in-progress — keep polling
		case health.ConvergeUnreachable:
			// The apiserver was unreachable — an infrastructure transient, not a
			// cluster verdict. Retry against the budget WITHOUT spending a hard
			// strike, so a konnectivity/apiserver blip on one poll can't combine
			// with a later real hard-fail to trip the twice-in-a-row abort. A
			// genuinely unreachable cluster simply exhausts the budget below.
			if time.Now().After(deadline) {
				fmt.Fprintf(os.Stderr, "::error::budget of %ds exhausted with the apiserver still unreachable — check KUBECONFIG and cluster reachability.\n", budget)
				return 1
			}
			fmt.Fprintf(os.Stderr, "::warning::apiserver unreachable — transient; re-checking after %ds (not counted as a hard failure).\n", retryDelay)
			time.Sleep(time.Duration(retryDelay) * time.Second)
		default:
			fmt.Fprintln(os.Stderr, "::error::health check returned an exit code outside the 0/1/2/3 contract.")
			return 1
		}
	}
}

// ── health orchestrator ──────────────────────────────────────────────────────

// healthExitCode runs every check against $KUBECONFIG, prints the report, and
// returns the convergence-contract exit code (0/2/1).
func healthExitCode() int {
	if !kubectlReachable() {
		// Exit 3 (not 1): an unreachable apiserver is an infrastructure transient,
		// not a cluster hard-failure. The converge loop retries it against the
		// budget instead of counting it as a hard strike (see runConverge).
		fmt.Fprintln(os.Stderr, "::error::kubectl cannot reach the apiserver — check KUBECONFIG and cluster reachability.")
		return 3
	}

	// Phase 0: pre-bootstrap (Argo CRD / platform-bootstrap App not present yet)
	// is in-progress, not converged — poll.
	if !kExists("get", "crd", "applications.argoproj.io") ||
		!kExists("-n", "argocd", "get", "application", "platform-bootstrap") {
		fmt.Println(bold("== pre-bootstrap phase detected — apl-core helmfile likely still running =="))
		fmt.Printf("  %s applications.argoproj.io CRD or platform-bootstrap Application not yet present\n", cyan("PENDING"))
		return 2
	}
	// Phase 1: cluster-bootstrap ran but bootstrap-openbao has not completed yet.
	// Historically this was keyed only on cert-manager/platform-app-ca being absent,
	// but apl-core 5.x no longer emits that Secret while the replacement CA chain can
	// already be healthy. Once the openbao ClusterSecretStore is Ready, OpenBao has
	// been unsealed/configured and later failures must fail fast instead of being
	// masked as "still installing" until the converge budget expires.
	phase1 := phase1OpenBaoBootstrapPending()

	var r health.Report
	checkNodes(&r)
	checkNamespaces(&r)
	checkAPIServices(&r)
	checkRequiredCRDs(&r)
	checkStorageClasses(&r)
	checkFirewallBootstrap(&r)
	checkOpenBao(&r, phase1)
	checkReadyResources(&r, phase1)
	checkWebhooks(&r)
	checkAppProjects(&r)
	checkLeases(&r)
	checkArgoApps(&r, phase1)
	checkWorkloads(&r, phase1)
	checkPVCs(&r)
	checkPVs(&r)
	checkNetworkPolicies(&r)
	checkJobs(&r, phase1)
	checkCronWorkflows(&r)
	checkServices(&r, phase1)
	checkPDBs(&r, phase1)
	checkIngresses(&r, phase1)
	checkWorkflows(&r, phase1)
	checkStuckFinalizers(&r)
	checkPods(&r, phase1)

	printHealthSummary(&r)

	// In phase1 the support plane is still installing (apl-core's CRDs, webhook
	// Services, and endpoints land in later helmfile phases), so a hard-fail here
	// is "not yet installed", not terminal — downgrade it to in-progress so
	// converge keeps polling until the cluster advances past phase1 instead of
	// aborting on still-installing infra. See health.PhaseAwareExitCode.
	code := health.PhaseAwareExitCode(r.ExitCode(), phase1)
	if phase1 && code != r.ExitCode() {
		fmt.Println(bold("== phase1 (support plane still installing) — hard failures above are treated as in-progress; converge will keep polling =="))
	}
	return code
}

func printHealthSummary(r *health.Report) {
	fmt.Println()
	for _, c := range r.Drift {
		fmt.Println("  " + yellow("drift:   ") + " " + c)
	}
	for _, c := range r.Deferred {
		fmt.Println("  " + cyan("deferred:") + " " + c)
	}
	for _, c := range r.Pending {
		fmt.Println("  " + cyan("pending: ") + " " + c)
	}
	for _, c := range r.Failed {
		fmt.Println("  " + red("FAILED:  ") + " " + c)
	}
	switch r.Verdict() {
	case health.HardFailed:
		fmt.Printf("%s\n", red(fmt.Sprintf("%d check(s) hard-failed.", len(r.Failed))))
	case health.InProgress:
		fmt.Println(yellow("Cluster is still converging — re-run after a backoff."))
	default:
		if len(r.Deferred) > 0 {
			fmt.Printf("%s %s\n", green("✓"), fmt.Sprintf("Cluster converged — %d operator-deferred item(s) remain, platform healthy.", len(r.Deferred)))
		} else {
			fmt.Printf("%s Cluster converged.\n", green("✓"))
		}
	}
}

// ── kubectl helpers ──────────────────────────────────────────────────────────

func kubectlReachable() bool {
	_, err := execOutput("kubectl", "version", "--request-timeout=10s")
	return err == nil
}

// kExists reports whether `kubectl <args>` exits 0.
func kExists(args ...string) bool { _, err := execOutput("kubectl", args...); return err == nil }

// phase1ProbeRetries / phase1ProbeDelay bound secretPresentWithRetry's retry
// loop. A package var so tests can zero the delay.
var (
	phase1ProbeRetries = 3
	phase1ProbeDelay   = 3 * time.Second
)

// secretPresentWithRetry reports whether `kubectl <args>` (an existence probe)
// succeeds on any of a few attempts. kExists collapses every non-zero exit to
// "missing", so a transient API/ACL blip looks identical to a genuine NotFound;
// retrying lets a one-off blip recover (present wins) while a real absence still
// fails every attempt. Used for the phase1 platform-app-ca probe, where a false
// "absent" would mislabel the cluster phase.
func secretPresentWithRetry(args ...string) bool {
	for attempt := 0; attempt < phase1ProbeRetries; attempt++ {
		if kExists(args...) {
			return true
		}
		if attempt < phase1ProbeRetries-1 {
			time.Sleep(phase1ProbeDelay)
		}
	}
	return false
}

func phase1OpenBaoBootstrapPending() bool {
	if secretPresentWithRetry("-n", "cert-manager", "get", "secret", "platform-app-ca") {
		return false
	}
	return !openBaoClusterSecretStoreReadyWithRetry()
}

func openBaoClusterSecretStoreReadyWithRetry() bool {
	for attempt := 0; attempt < phase1ProbeRetries; attempt++ {
		if openBaoClusterSecretStoreReady() {
			return true
		}
		if attempt < phase1ProbeRetries-1 {
			time.Sleep(phase1ProbeDelay)
		}
	}
	return false
}

func openBaoClusterSecretStoreReady() bool {
	out, err := execOutput("kubectl", "get", "clustersecretstore", defaultSecretStore, "-o", "json")
	if err != nil {
		return false
	}
	var item readyResourceItem
	if err := json.Unmarshal(out, &item); err != nil {
		return false
	}
	status, _, _ := health.FindReady(item.Status.Conditions)
	return status == "True"
}

// kItems runs `kubectl get <args> -o json` and returns its .items[] as raw
// messages, or nil on any error (a missing/unreachable resource => empty, so the
// section is a no-op rather than a false failure). Routes through the execOutput
// seam so the section orchestrators are unit-testable with stubbed kubectl JSON.
func kItems(args ...string) []json.RawMessage {
	out, err := execOutput("kubectl", append(args, "-o", "json")...)
	if err != nil {
		return nil
	}
	var body struct {
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(out, &body) != nil {
		return nil
	}
	return body.Items
}

// record prints a labeled line for a finding and routes it into the report
// (CatOK/CatWarn print but never affect the verdict).
func record(r *health.Report, cat health.Category, msg string) {
	label := map[health.Category]string{
		health.CatOK: "OK", health.CatWarn: "WARN", health.CatFail: "FAIL",
		health.CatPending: "PENDING", health.CatDeferred: "DEFERRED", health.CatDrift: "DRIFT",
	}[cat]
	// Pad to the fixed column on the PLAIN label, then color — the ANSI escapes are
	// zero-width, so the columns stay aligned (color.go).
	fmt.Printf("  %s %s\n", catColor(cat, fmt.Sprintf("%-8s", label)), msg)
	r.Add(cat, msg)
}

// catColor tints a health-category label by severity, degrading to plain off a TTY.
func catColor(cat health.Category, s string) string {
	switch cat {
	case health.CatOK:
		return green(s)
	case health.CatFail:
		return red(s)
	case health.CatWarn, health.CatDrift:
		return yellow(s)
	case health.CatPending, health.CatDeferred:
		return cyan(s)
	}
	return s
}

func hdr(s string) { fmt.Printf("\n%s\n", bold("== "+s+" ==")) }

// metaName / nsName extract common metadata for inline-typed items.
type meta struct {
	Metadata struct {
		Namespace         string            `json:"namespace"`
		Name              string            `json:"name"`
		Annotations       map[string]string `json:"annotations"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
		Finalizers        []string          `json:"finalizers"`
	} `json:"metadata"`
}

// ── sections ─────────────────────────────────────────────────────────────────

func checkNodes(r *health.Report) {
	hdr("node health")
	for _, raw := range kItems("get", "nodes") {
		var n health.Node
		if json.Unmarshal(raw, &n) != nil {
			continue
		}
		ok, ready, mem, disk, pid := health.NodeHealthy(n)
		if ok {
			record(r, health.CatOK, fmt.Sprintf("Node %s (Ready, no pressure)", n.Name()))
		} else {
			record(r, health.CatFail, fmt.Sprintf("Node %s (Ready=%s MemPressure=%s DiskPressure=%s PIDPressure=%s)", n.Name(), ready, mem, disk, pid))
		}
		for _, t := range health.UnexpectedTaints(n) {
			val := ""
			if t.Value != "" {
				val = "=" + t.Value
			}
			record(r, health.CatFail, fmt.Sprintf("Node %s has unexpected taint %s%s:%s (blocks scheduling)", n.Name(), t.Key, val, t.Effect))
		}
	}
}

func checkNamespaces(r *health.Report) {
	hdr("namespaces (stuck Terminating)")
	stuck := false
	for _, raw := range kItems("get", "ns") {
		var m struct {
			meta
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if health.NamespaceTerminating(m.Status.Phase) {
			record(r, health.CatFail, fmt.Sprintf("Namespace %s stuck Terminating (check .spec.finalizers and stuck CRs)", m.Metadata.Name))
			stuck = true
		}
	}
	if !stuck {
		record(r, health.CatOK, "no namespaces in Terminating state")
	}
}

func checkAPIServices(r *health.Report) {
	hdr("APIService availability")
	for _, raw := range kItems("get", "apiservices") {
		var a health.APIService
		if json.Unmarshal(raw, &a) != nil {
			continue
		}
		if bad, msg := health.APIServiceUnavailable(a); bad {
			record(r, health.CatFail, fmt.Sprintf("APIService %s not Available — %s", a.Metadata.Name, msg))
		}
	}
}

func checkRequiredCRDs(r *health.Report) {
	hdr("required CRDs")
	for _, crd := range health.RequiredCRDs() {
		if kExists("get", "crd", crd) {
			record(r, health.CatOK, "CRD "+crd+" installed")
		} else {
			record(r, health.CatFail, "CRD "+crd+" missing — owning ArgoCD Application has not installed it")
		}
	}
}

func checkStorageClasses(r *health.Report) {
	hdr("StorageClasses")
	for _, sc := range health.RequiredStorageClasses() {
		if kExists("get", "storageclass", sc) {
			record(r, health.CatOK, "StorageClass "+sc+" present")
		} else {
			record(r, health.CatFail, "StorageClass "+sc+" missing")
		}
	}
	var classes []health.StorageClass
	for _, raw := range kItems("get", "storageclass") {
		var c health.StorageClass
		if json.Unmarshal(raw, &c) == nil {
			classes = append(classes, c)
		}
	}
	switch def := health.DefaultStorageClasses(classes); len(def) {
	case 1:
		record(r, health.CatOK, "exactly one default StorageClass ("+def[0]+")")
	case 0:
		record(r, health.CatFail, "no default StorageClass — PVCs without an explicit storageClassName will stay Pending")
	default:
		// Two defaults is the transient cold-start state, NOT a terminal failure:
		// LKE's Flux-managed workload HelmRelease ships linode-block-storage-retain
		// as a default, and the sc-demote reconciler (leader-gated, watch + resync
		// floor) demotes it so block-storage-retain is the sole default. On a fresh
		// cluster that demote lands within the reconciler's resync floor (~120s),
		// which can exceed a single converge poll's hard-fail tolerance — so classify
		// it as in-progress (poll against the budget) rather than CatFail. A genuinely
		// stuck duplicate (reconciler down/never-leader) still fails, but on budget
		// exhaustion instead of a fast hard-fail that races the self-heal. See
		// reconcile_sc_demote.go + the leader-election re-fire in reconcile.go.
		record(r, health.CatPending, fmt.Sprintf("%d default StorageClasses (%s) — non-deterministic; awaiting sc-demote reconciler", len(def), strings.Join(def, ",")))
	}
}

func checkFirewallBootstrap(r *health.Report) {
	hdr("cloud-firewall bootstrap (kube-system)")
	// The firewall controller is optional (the private llz-linode-cidr-firewall
	// chart + the cidrFirewall component that feeds it). When neither the
	// controller Deployment nor its ConfigMap exists the component is simply not
	// enabled on this instance — skip instead of failing every public adopter.
	// (Before the cidrFirewall component, `llz ci bootstrap-cloud-firewall`
	// seeded the ConfigMap unconditionally on every apply, so its absence WAS a
	// bootstrap failure; now the ConfigMap only exists where the component runs.)
	if !kExists("-n", "kube-system", "get", "deployment", firewallDeploymentName) &&
		!kExists("-n", "kube-system", "get", "configmap", firewallConfigMapName) {
		record(r, health.CatOK, "firewall-controller not installed (cidrFirewall component disabled) — skipped")
		return
	}
	exists := kExists("-n", "kube-system", "get", "secret", "linode")
	token := ""
	if exists {
		token = kJSONPath("-n", "kube-system", "get", "secret", "linode", "-o", "jsonpath={.data.token}")
	}
	cat, msg := health.ClassifyFirewallToken(exists, token)
	record(r, cat, msg)

	// firewallConfigMapName (ci_firewall.go) is the single source of truth for the
	// ConfigMap name the private chart renders (<fullname>-config =
	// llz-linode-cidr-firewall-config) and `bootstrap-cloud-firewall` patches.
	if !kExists("-n", "kube-system", "get", "configmap", firewallConfigMapName) {
		record(r, health.CatFail, "ConfigMap kube-system/"+firewallConfigMapName+" missing")
		return
	}
	record(r, health.CatOK, "ConfigMap kube-system/"+firewallConfigMapName+" exists")
	for _, key := range []string{"LINODE_FIREWALL_ID", "LKE_CLUSTER_ID", "FIREWALL_TEMPLATE_ID", "RECONCILE_INTERVAL_SECS", "VPC_CIDR"} {
		val := kJSONPath("-n", "kube-system", "get", "configmap", firewallConfigMapName, "-o", "jsonpath={.data."+key+"}")
		cat := health.ClassifyFirewallConfigKey(key, val)
		if cat == health.CatOK {
			record(r, health.CatOK, "  "+key+" = "+val)
		} else {
			record(r, health.CatDeferred, "  "+key+" empty (set when the firewall bootstrap / Argo app runs)")
		}
	}
}

func checkReadyResources(r *health.Report, phase1 bool) {
	// cert-manager ClusterIssuers / Certificates / CertificateRequests + ESO.
	readyKind(r, "ClusterIssuer", []string{"get", "clusterissuers.cert-manager.io"}, false,
		func(key string) bool { return phase1 && health.MatchPrefix(key, health.Phase1PendingIssuers()) },
		health.ExternalDepIssuers())
	readyKind(r, "Certificate", []string{"get", "certificates.cert-manager.io", "-A"}, true,
		func(key string) bool { return phase1 && health.MatchPrefix(key, health.Phase1PendingCerts()) },
		health.ExternalDepCerts())
	certRequests(r, phase1)
	readyKind(r, "ClusterSecretStore", []string{"get", "clustersecretstores.external-secrets.io"}, false,
		func(string) bool { return phase1 }, nil)
	readyKind(r, "ExternalSecret", []string{"get", "externalsecrets.external-secrets.io", "-A"}, true,
		func(string) bool { return phase1 }, health.ExternalDepExternalSecrets())
}

// readyResourceItem is a resource with a Ready condition.
type readyResourceItem struct {
	meta
	Status struct {
		Conditions []health.Condition `json:"conditions"`
	} `json:"status"`
}

func readyKind(r *health.Report, kind string, getArgs []string, namespaced bool, phase1Pending func(key string) bool, extDep []health.DepEntry) {
	hdr(kind + "s")
	for _, raw := range kItems(getArgs...) {
		var it readyResourceItem
		if json.Unmarshal(raw, &it) != nil {
			continue
		}
		key := it.Metadata.Name
		if namespaced {
			key = it.Metadata.Namespace + "/" + it.Metadata.Name
		}
		status, reason, msg := health.FindReady(it.Status.Conditions)
		cat, line := health.ClassifyReady(kind, key, status, reason, msg, phase1Pending(key), extDep)
		record(r, cat, line)
	}
}

func certRequests(r *health.Report, phase1 bool) {
	hdr("CertificateRequests")
	for _, raw := range kItems("get", "certificaterequests.cert-manager.io", "-A") {
		var it readyResourceItem
		if json.Unmarshal(raw, &it) != nil {
			continue
		}
		key := it.Metadata.Namespace + "/" + it.Metadata.Name
		status, reason, msg := health.FindReady(it.Status.Conditions)
		p1 := phase1 && health.MatchPrefix(key, health.Phase1PendingCerts())
		cat, line := health.ClassifyCertificateRequest(key, status, reason, msg, p1, health.ExternalDepCerts())
		record(r, cat, line)
	}
}

func checkOpenBao(r *health.Report, phase1 bool) {
	hdr("openbao seal / HA")
	replicas, err := strconv.Atoi(strings.TrimSpace(kJSONPath("-n", openbaoNamespace, "get", "sts", "platform-openbao", "-o", "jsonpath={.spec.replicas}")))
	if err != nil || replicas == 0 {
		record(r, health.CatWarn, "OpenBao StatefulSet not present — skipping seal check")
		return
	}
	active := 0
	for i := 0; i < replicas; i++ {
		pod := fmt.Sprintf("platform-openbao-%d", i)
		if !kExists("-n", openbaoNamespace, "get", "pod", pod) {
			record(r, health.CatFail, "Pod openbao/"+pod+" missing")
			continue
		}
		ready := kJSONPath("-n", openbaoNamespace, "get", "pod", pod, "-o", `jsonpath={.status.containerStatuses[?(@.name=="openbao")].ready}`)
		if ready != "true" {
			record(r, health.CatPending, "Pod openbao/"+pod+" (openbao container not Ready — can't query seal status)")
			continue
		}
		out, _ := execOutput("kubectl", "-n", openbaoNamespace, "exec", pod, "-c", "openbao", "--",
			"env", "VAULT_ADDR=https://127.0.0.1:8200", "VAULT_SKIP_VERIFY=true", "bao", "status", "-format=json")
		st, perr := health.ParseBaoStatus(out)
		if perr != nil {
			record(r, health.CatFail, "Pod openbao/"+pod+" (could not parse bao status JSON)")
			continue
		}
		cat, msg := health.ClassifyBaoSeal(st)
		record(r, cat, "Pod openbao/"+pod+" ("+msg+")")
		if cat == health.CatOK && st.HAMode == "active" {
			active++
		}
		if cat == health.CatOK && !phase1 {
			if kExists("-n", openbaoNamespace, "exec", pod, "-c", "openbao", "--", "test", "-s", "/openbao/audit/audit.log") {
				record(r, health.CatOK, "  audit device active on "+pod)
			} else {
				record(r, health.CatFail, "  audit device inactive on "+pod+" — /openbao/audit/audit.log missing or empty")
			}
		}
	}
	if cat, msg := health.ClassifyLeaderCount(replicas, active); cat != health.CatOK {
		record(r, cat, msg)
	}
}

func checkWebhooks(r *health.Report) {
	hdr("admission webhooks (Validating + Mutating)")
	type webhookCfg struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Webhooks []struct {
			ClientConfig struct {
				Service *struct {
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
				} `json:"service"`
			} `json:"clientConfig"`
		} `json:"webhooks"`
	}
	for _, kind := range []string{"validatingwebhookconfigurations", "mutatingwebhookconfigurations"} {
		for _, raw := range kItems("get", kind) {
			var cfg webhookCfg
			if json.Unmarshal(raw, &cfg) != nil {
				continue
			}
			for _, wh := range cfg.Webhooks {
				if wh.ClientConfig.Service == nil {
					continue
				}
				ns, svc := wh.ClientConfig.Service.Namespace, wh.ClientConfig.Service.Name
				if ns == "" || svc == "" {
					continue
				}
				exists := kExists("-n", ns, "get", "svc", svc)
				ready := countReadyEndpoints(ns, svc)
				cat, msg := health.ClassifyWebhookBackend(exists, ready)
				record(r, cat, fmt.Sprintf("%s %s → %s/%s %s", kind, cfg.Metadata.Name, ns, svc, msg))
			}
		}
	}
}

func checkAppProjects(r *health.Report) {
	hdr("ArgoCD AppProjects")
	if !kExists("get", "crd", "appprojects.argoproj.io") {
		return
	}
	// platform-support is the only per-domain AppProject the support-plane
	// Applications reference.
	for _, ap := range []string{"platform-support"} {
		if kExists("-n", "argocd", "get", "appproject", ap) {
			record(r, health.CatOK, "AppProject argocd/"+ap+" present")
		} else {
			record(r, health.CatFail, "AppProject argocd/"+ap+" missing — child Applications will ComparisonError 'project not found'")
		}
	}
}

func checkLeases(r *health.Report) {
	hdr("controller Lease freshness")
	now := time.Now()
	stale := false
	for _, ns := range []string{"argocd", "cert-manager", "external-secrets", "cert-automation", "openbao", "kube-system"} {
		if !kExists("get", "ns", ns) {
			continue
		}
		for _, raw := range kItems("-n", ns, "get", "leases.coordination.k8s.io") {
			var it struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Spec struct {
					HolderIdentity       string `json:"holderIdentity"`
					LeaseDurationSeconds int    `json:"leaseDurationSeconds"`
					RenewTime            string `json:"renewTime"`
				} `json:"spec"`
			}
			if json.Unmarshal(raw, &it) != nil || it.Spec.RenewTime == "" {
				continue
			}
			renew, err := time.Parse(time.RFC3339, it.Spec.RenewTime)
			if err != nil {
				continue
			}
			if health.LeaseStale(renew, now, it.Spec.LeaseDurationSeconds) {
				record(r, health.CatFail, fmt.Sprintf("Lease %s/%s stale (holder=%s) — leader-elected controller silently stopped", ns, it.Metadata.Name, it.Spec.HolderIdentity))
				stale = true
			}
		}
	}
	if !stale {
		record(r, health.CatOK, "all controller Leases renewed within 4× leaseDuration")
	}
}

func checkArgoApps(r *health.Report, phase1 bool) {
	hdr("ArgoCD Applications")
	for _, raw := range kItems("-n", "argocd", "get", "applications.argoproj.io") {
		a, err := health.ParseArgoApp(raw)
		if err != nil {
			continue
		}
		cat, msg := health.ClassifyArgoApp(a, phase1)
		record(r, cat, msg)
	}
}

func checkWorkloads(r *health.Report, phase1 bool) {
	hdr("Deployments / StatefulSets / DaemonSets")
	for _, ns := range healthNamespaces {
		if !kExists("get", "ns", ns) {
			continue
		}
		for _, raw := range kItems("-n", ns, "get", "deploy") {
			var d struct {
				meta
				Spec struct {
					Replicas int `json:"replicas"`
				} `json:"spec"`
				Status struct {
					ReadyReplicas int                `json:"readyReplicas"`
					Conditions    []health.Condition `json:"conditions"`
				} `json:"status"`
			}
			if json.Unmarshal(raw, &d) != nil {
				continue
			}
			preason, pmsg := progressingCondition(d.Status.Conditions)
			cat, msg := health.ClassifyWorkload("Deployment", ns, d.Metadata.Name, d.Spec.Replicas, d.Status.ReadyReplicas, preason, pmsg, phase1)
			record(r, cat, msg)
		}
		for _, raw := range kItems("-n", ns, "get", "sts") {
			var s struct {
				meta
				Spec struct {
					Replicas int `json:"replicas"`
				} `json:"spec"`
				Status struct {
					ReadyReplicas int `json:"readyReplicas"`
				} `json:"status"`
			}
			if json.Unmarshal(raw, &s) != nil {
				continue
			}
			cat, msg := health.ClassifyWorkload("StatefulSet", ns, s.Metadata.Name, s.Spec.Replicas, s.Status.ReadyReplicas, "", "", phase1)
			record(r, cat, msg)
		}
		for _, raw := range kItems("-n", ns, "get", "ds") {
			var ds struct {
				meta
				Status struct {
					DesiredNumberScheduled int `json:"desiredNumberScheduled"`
					NumberReady            int `json:"numberReady"`
					UpdatedNumberScheduled int `json:"updatedNumberScheduled"`
					NumberMisscheduled     int `json:"numberMisscheduled"`
				} `json:"status"`
			}
			if json.Unmarshal(raw, &ds) != nil {
				continue
			}
			cat, msg := health.ClassifyDaemonSet(ns, ds.Metadata.Name, ds.Status.DesiredNumberScheduled, ds.Status.NumberReady, ds.Status.UpdatedNumberScheduled, ds.Status.NumberMisscheduled)
			record(r, cat, msg)
		}
	}
}

func checkPVCs(r *health.Report) {
	hdr("PersistentVolumeClaim binding")
	for _, raw := range kItems("get", "pvc", "-A") {
		var p struct {
			meta
			Spec struct {
				StorageClassName string `json:"storageClassName"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		}
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		cat, msg := health.ClassifyPVC(p.Metadata.Namespace, p.Metadata.Name, p.Status.Phase, p.Spec.StorageClassName)
		record(r, cat, msg)
	}
}

func checkPVs(r *health.Report) {
	hdr("PersistentVolume hygiene")
	released := 0
	for _, raw := range kItems("get", "pv") {
		var p struct {
			meta
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		}
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		switch health.ClassifyPVPhase(p.Status.Phase) {
		case health.CatFail:
			record(r, health.CatFail, fmt.Sprintf("PV %s %s — provisioner/CSI issue; dependent PVC will stay Pending", p.Metadata.Name, p.Status.Phase))
		case health.CatWarn:
			record(r, health.CatWarn, fmt.Sprintf("PV %s unrecognized phase=%s", p.Metadata.Name, p.Status.Phase))
		default:
			if p.Status.Phase == "Released" {
				released++
			}
		}
	}
	if released > 0 {
		record(r, health.CatWarn, fmt.Sprintf("%d Released PV(s) — expected with Retain; run orphan-cleanup so leaked Volumes don't count against quota", released))
	} else {
		record(r, health.CatOK, "no Released/Failed/Pending PVs")
	}
}

func checkNetworkPolicies(r *health.Report) {
	hdr("NetworkPolicy presence per namespace")
	for _, ns := range healthNamespaces {
		if !kExists("get", "ns", ns) || health.NetpolExemptNamespace(ns) {
			continue
		}
		cat, msg := health.ClassifyNamespaceNetpol(ns, len(kItems("-n", ns, "get", "networkpolicies")))
		record(r, cat, msg)
	}
}

func checkJobs(r *health.Report, phase1 bool) {
	hdr("Jobs (failed or stuck)")
	type jobItem struct {
		Metadata struct {
			Namespace         string            `json:"namespace"`
			Name              string            `json:"name"`
			CreationTimestamp string            `json:"creationTimestamp"`
			OwnerReferences   []health.OwnerRef `json:"ownerReferences"`
		} `json:"metadata"`
		Status struct {
			Succeeded  int                `json:"succeeded"`
			Failed     int                `json:"failed"`
			Active     int                `json:"active"`
			Conditions []health.Condition `json:"conditions"`
		} `json:"status"`
	}
	var items []jobItem
	var runs []health.JobRun
	for _, raw := range kItems("get", "jobs", "-A") {
		var j jobItem
		if json.Unmarshal(raw, &j) != nil {
			continue
		}
		items = append(items, j)
		key := j.Metadata.Namespace + "/" + j.Metadata.Name
		complete, failed := false, false
		for _, c := range j.Status.Conditions {
			if c.Type == "Complete" && c.Status == "True" {
				complete = true
			}
			if c.Type == "Failed" && c.Status == "True" {
				failed = true
			}
		}
		var cronOwner string
		for _, o := range j.Metadata.OwnerReferences {
			if o.Kind == "CronJob" {
				cronOwner = o.Name
			}
		}
		created, _ := time.Parse(time.RFC3339, j.Metadata.CreationTimestamp)
		runs = append(runs, health.JobRun{Key: key, CronOwner: cronOwner, Created: created, Complete: complete, Failed: failed})
	}
	// An early CronJob tick that failed before its backing service was up, then
	// superseded by a later successful tick, must not fail the gate (see
	// health.SupersededFailedJobs).
	superseded := health.SupersededFailedJobs(runs)
	for i, j := range items {
		run := runs[i]
		if run.Failed && !run.Complete && superseded[run.Key] {
			record(r, health.CatOK, "Job "+run.Key+" Failed but superseded by a newer successful "+run.CronOwner+" CronJob run")
			continue
		}
		p1 := phase1 && health.MatchPrefix(run.Key, health.Phase1PendingWorkloads())
		cat, msg := health.ClassifyJob(run.Key, run.Complete, run.Failed, j.Status.Active, j.Status.Succeeded, j.Status.Failed, p1)
		record(r, cat, msg)
	}
}

func checkCronWorkflows(r *health.Report) {
	hdr("CronWorkflows")
	if !kExists("get", "crd", "cronworkflows.argoproj.io") {
		return
	}
	now := time.Now()
	for _, raw := range kItems("get", "cronworkflows.argoproj.io", "-A") {
		var cw struct {
			meta
			Spec struct {
				Suspend bool `json:"suspend"`
			} `json:"spec"`
			Status struct {
				Conditions        []health.Condition `json:"conditions"`
				LastScheduledTime string             `json:"lastScheduledTime"`
			} `json:"status"`
		}
		if json.Unmarshal(raw, &cw) != nil {
			continue
		}
		key := cw.Metadata.Namespace + "/" + cw.Metadata.Name
		submissionErr := ""
		for _, c := range cw.Status.Conditions {
			if c.Type == "SubmissionError" {
				submissionErr = c.Message
			}
		}
		ageDays := -1
		if cw.Status.LastScheduledTime != "" {
			if last, err := time.Parse(time.RFC3339, cw.Status.LastScheduledTime); err == nil {
				ageDays = int(now.Sub(last).Hours() / 24)
			}
		}
		cat, msg := health.ClassifyCronWorkflow(key, submissionErr, cw.Spec.Suspend, ageDays, 30)
		record(r, cat, msg)
	}
}

func checkServices(r *health.Report, phase1 bool) {
	hdr("Service endpoints (repo namespaces)")
	for _, ns := range healthNamespaces {
		if !kExists("get", "ns", ns) {
			continue
		}
		for _, raw := range kItems("-n", ns, "get", "svc") {
			var s struct {
				meta
				Spec struct {
					Type      string `json:"type"`
					ClusterIP string `json:"clusterIP"`
				} `json:"spec"`
			}
			if json.Unmarshal(raw, &s) != nil || s.Spec.Type == "ExternalName" || s.Spec.ClusterIP == "None" {
				continue
			}
			key := ns + "/" + s.Metadata.Name
			p1 := phase1 && health.MatchPrefix(key, health.Phase1PendingWorkloads())
			cat, msg := health.ClassifyServiceEndpoints(key, countReadyEndpoints(ns, s.Metadata.Name), p1)
			if cat != health.CatOK { // only surface non-OK to cut noise (matches script's VERBOSE-gated pass)
				record(r, cat, msg)
			}
		}
	}
}

func checkPDBs(r *health.Report, phase1 bool) {
	hdr("PodDisruptionBudgets")
	for _, raw := range kItems("get", "pdb", "-A") {
		var p struct {
			meta
			Status struct {
				CurrentHealthy     int `json:"currentHealthy"`
				DesiredHealthy     int `json:"desiredHealthy"`
				DisruptionsAllowed int `json:"disruptionsAllowed"`
				ExpectedPods       int `json:"expectedPods"`
			} `json:"status"`
		}
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		key := p.Metadata.Namespace + "/" + p.Metadata.Name
		cat, msg := health.ClassifyPDB(key, p.Status.CurrentHealthy, p.Status.DesiredHealthy, p.Status.DisruptionsAllowed, p.Status.ExpectedPods, phase1)
		if cat != health.CatOK {
			record(r, cat, msg)
		}
	}
}

func checkIngresses(r *health.Report, phase1 bool) {
	hdr("Ingress addresses")
	for _, raw := range kItems("get", "ingress", "-A") {
		var ing struct {
			meta
			Status struct {
				LoadBalancer struct {
					Ingress []json.RawMessage `json:"ingress"`
				} `json:"loadBalancer"`
			} `json:"status"`
		}
		if json.Unmarshal(raw, &ing) != nil {
			continue
		}
		key := ing.Metadata.Namespace + "/" + ing.Metadata.Name
		cat, msg := health.ClassifyIngress(key, len(ing.Status.LoadBalancer.Ingress), phase1)
		record(r, cat, msg)
	}
}

func checkWorkflows(r *health.Report, phase1 bool) {
	hdr("Argo Workflows (recent Failed / Error)")
	if !kExists("get", "crd", "workflows.argoproj.io") {
		return
	}
	for _, raw := range kItems("get", "workflows.argoproj.io", "-A") {
		var wf struct {
			meta
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		}
		if json.Unmarshal(raw, &wf) != nil {
			continue
		}
		key := wf.Metadata.Namespace + "/" + wf.Metadata.Name
		if cat, msg := health.ClassifyWorkflowPhase(key, wf.Status.Phase, phase1); cat != health.CatOK {
			record(r, cat, msg)
		}
	}
}

func checkStuckFinalizers(r *health.Report) {
	hdr("stuck-finalizer deletions")
	now := time.Now()
	found := false
	for _, spec := range health.StuckResourceKinds() {
		parts := strings.SplitN(spec, "|", 2)
		kind, scope := parts[0], parts[1]
		if kind != "pv" && kind != "pvc" && !kExists("get", "crd", kind) {
			continue
		}
		args := []string{"get"}
		if scope == "-A" {
			args = append(args, "-A")
		}
		args = append(args, kind)
		for _, raw := range kItems(args...) {
			var m meta
			if json.Unmarshal(raw, &m) != nil || m.Metadata.DeletionTimestamp == "" {
				continue
			}
			del, err := time.Parse(time.RFC3339, m.Metadata.DeletionTimestamp)
			if err != nil {
				continue
			}
			if health.StuckFinalizer(true, len(m.Metadata.Finalizers), now.Sub(del).Seconds()) {
				ns := m.Metadata.Namespace
				if ns == "" {
					ns = "<cluster>"
				}
				record(r, health.CatFail, fmt.Sprintf("%s %s/%s stuck Terminating (finalizers: %s)", kind, ns, m.Metadata.Name, strings.Join(m.Metadata.Finalizers, ",")))
				found = true
			}
		}
	}
	if !found {
		record(r, health.CatOK, "no resources stuck Terminating (>5min with non-empty finalizers)")
	}
}

func checkPods(r *health.Report, phase1 bool) {
	hdr("unhealthy pods (all namespaces)")
	bad := false
	for _, raw := range kItems("get", "pods", "-A") {
		var p struct {
			Metadata struct {
				Namespace       string            `json:"namespace"`
				Name            string            `json:"name"`
				OwnerReferences []health.OwnerRef `json:"ownerReferences"`
			} `json:"metadata"`
			Status health.PodStatus `json:"status"`
		}
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		// Job/CronJob pods are ephemeral and self-completing — their health is
		// the Job section's (checkJobs/ClassifyJob), not this steady-state
		// workload gate. Skip them so a short-lived CronJob pod caught
		// mid-creation (e.g. argo-resync-nudger) can't flunk the gate.
		if health.IsJobControlled(p.Metadata.OwnerReferences) {
			continue
		}
		key := p.Metadata.Namespace + "/" + p.Metadata.Name
		if health.PodIsFailing(p.Status) {
			detail := fmt.Sprintf("Pod %s phase=%s ready=%s state=%s", key, p.Status.Phase, health.ReadyRatio(p.Status), health.SummarizeStates(p.Status))
			switch {
			case phase1 && health.MatchPrefix(key, health.Phase1PendingWorkloads()):
				record(r, health.CatPending, detail+" — waiting on OpenBao bootstrap")
			case extDepMatch(key):
				reason, _ := health.MatchExternalDep(key, health.ExternalDepWorkloads())
				record(r, health.CatDeferred, detail+" — "+reason)
			default:
				record(r, health.CatFail, detail)
			}
			bad = true
		}
		if hot := health.FlappingContainers(p.Status, 5); hot != "" {
			record(r, health.CatWarn, fmt.Sprintf("Pod %s has flapping containers: %s", key, hot))
		}
	}
	if !bad {
		record(r, health.CatOK, "no pods in a failing state")
	}
}

// ── small helpers ────────────────────────────────────────────────────────────

func extDepMatch(key string) bool {
	_, ok := health.MatchExternalDep(key, health.ExternalDepWorkloads())
	return ok
}

// progressingCondition returns a Deployment's Progressing condition reason/message.
func progressingCondition(conds []health.Condition) (reason, message string) {
	for _, c := range conds {
		if c.Type == "Progressing" {
			return c.Reason, c.Message
		}
	}
	return "", ""
}

// countReadyEndpoints sums ready endpoints across a Service's EndpointSlices.
func countReadyEndpoints(ns, svc string) int {
	var slices []health.EndpointSlice
	for _, raw := range kItems("-n", ns, "get", "endpointslices", "-l", "kubernetes.io/service-name="+svc) {
		var s health.EndpointSlice
		if json.Unmarshal(raw, &s) == nil {
			slices = append(slices, s)
		}
	}
	return health.CountReadyEndpoints(slices)
}

// kJSONPath runs a kubectl get with a -o jsonpath=... arg and returns trimmed stdout.
func kJSONPath(args ...string) string {
	out, err := execOutput("kubectl", args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
