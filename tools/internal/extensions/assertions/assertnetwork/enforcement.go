package assertnetwork

// ci_assert_network_enforcement.go implements `llz ci assert-network-enforcement`
// — the runtime proof that NetworkPolicy and mTLS are actually ENFORCED, not just
// declared.
//
// These are the two Tier-4 checks that cannot be server-dry-run. Admission
// policies can: submit an object, read the API server's answer. Enforcement of
// network policy happens in the DATA PLANE — in Cilium's BPF programs and in
// Istio's sidecars — and the only thing that knows whether a packet is dropped is
// a packet. So this gate opens real connections from a real pod.
//
// EVERY NEGATIVE TEST HERE CARRIES A POSITIVE CONTROL, and that is the whole
// design. "The connection failed" is nearly worthless on its own: it is equally
// consistent with the policy working, the image failing to pull, DNS being down,
// the pod not being scheduled, or the target simply not existing. A gate built on
// bare failure would report enforcement it never observed — the same vacuous pass
// the admission gates refuse, in a form that is much easier to write by accident.
//
// So each check dials TWO addresses from the SAME pod in the SAME run:
//
//	ALLOWED  must connect  — proves the pod has working networking, DNS and
//	                         scheduling, so a failure on the other dial means
//	                         something denied it rather than nothing worked
//	DENIED   must be blocked
//
// The verdict is the CONJUNCTION. If the allowed dial fails, the run is
// INCONCLUSIVE and fails as such — explicitly not reported as "enforcement
// working", because we learned nothing about the denied path.
//
// The two checks:
//
//	netpol  a scratch namespace with a default-deny-egress NetworkPolicy that
//	        allows DNS and the apiserver only. The pod must reach the apiserver
//	        (allowed) and must NOT reach an arbitrary in-cluster Service (denied).
//	        This proves the CNI enforces NetworkPolicy at all — on LKE-E that is
//	        Cilium, and a cluster where it silently does not is one where every
//	        default-deny in this repo is decorative.
//
//	mtls    the same pod, deliberately NOT in the mesh, dialing a STRICT-mesh
//	        workload's port in plaintext. Istio must refuse it. The positive
//	        control is the apiserver again.
//
//	        IT SKIPS UNLESS THE TARGET NAMESPACE IS ACTUALLY STRICT, read from its
//	        PeerAuthentications at run time. A PERMISSIVE workload accepts
//	        plaintext by design, so the dial succeeding there is correct behaviour
//	        and not a finding — harbor ships PERMISSIVE deliberately (ADR 0010
//	        step 3). Asserting STRICT before the flip is the "aspirational claim"
//	        harbor-peerauthentication.yaml warns about having already caught twice.
//
// IT CLEANS UP AFTER ITSELF, including on failure: the scratch namespace is
// deleted in a defer. A gate that leaks a namespace on every color.Red run makes the
// next run's cluster dirtier, and this one runs on a cluster that is about to be
// asserted against by ten other lanes.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/harborauth"
)

const (
	// netEnforceNamespace is created and destroyed by this gate. The name is
	// distinctive so a leaked one is obviously ours.
	netEnforceNamespace = "llz-net-enforce-probe"
	// netEnforceAllowedTarget is the positive control: the in-cluster apiserver
	// Service, which exists on every cluster and which the probe policy allows.
	netEnforceAllowedTarget = "kubernetes.default.svc.cluster.local:443"
	// netEnforceDeniedTarget is an in-cluster Service the probe policy does NOT
	// allow. Loki's gateway is present on every landing zone and is plain HTTP, so
	// a successful dial is unambiguous.
	netEnforceDeniedTarget = "loki-gateway.monitoring.svc.cluster.local:80"
	// netEnforceMTLSTarget is the port the mtls check dials in plaintext.
	//
	// harbor is NOT STRICT today: all three documents in
	// harbor-peerauthentication.yaml ship mode: PERMISSIVE on purpose — ADR 0010
	// step 3, the measurement phase that watches for clients which would break
	// under STRICT before the flip. So this check SKIPS here until those three
	// lines change, and the mode is read off the cluster rather than assumed.
	netEnforceMTLSTarget = "harbor-core.harbor.svc.cluster.local:80"
)

type netEnforceOpts struct {
	checks     []string
	image      string
	namespace  string
	allowed    string
	denied     string
	mtlsTarget string
	timeout    time.Duration
	keep       bool
}

// netEnforceVerdict is one check's outcome.
type netEnforceVerdict struct {
	Check    string
	Detail   string
	FailWhy  string
	Inconclu bool // the positive control failed: we learned nothing
	Skipped  bool // the property is not in force on this cluster yet
}

// evalEnforcementProbe is the verdict logic, and the reason it is a separate pure
// function is that this is exactly where a negative test goes wrong. Pure.
//
//	allowed connected + denied blocked → enforcement observed
//	allowed connected + denied connected → NOT enforced (the real finding)
//	allowed blocked                     → INCONCLUSIVE, whatever denied did
//
// The last arm is the one that matters. If the control could not connect, the
// pod's networking is broken and "denied was blocked" is not evidence of
// anything — reporting it as a pass is how a gate certifies a policy it never
// exercised.
func evalEnforcementProbe(check string, allowedRes, deniedRes netProbeResult, whatDenied string) netEnforceVerdict {
	v := netEnforceVerdict{Check: check}
	if !allowedRes.Connected {
		v.Inconclu = true
		v.FailWhy = fmt.Sprintf("INCONCLUSIVE: the positive control %s did not connect (%s). "+
			"The probe pod has no working path to a target that is supposed to be allowed, so nothing can be "+
			"concluded about %s — this run did NOT observe enforcement, and must not be read as if it had. "+
			"Check the probe pod's scheduling, image pull and DNS before trusting any verdict here",
			allowedRes.Target, allowedRes.Reason, deniedRes.Target)
		return v
	}
	if deniedRes.Connected {
		v.FailWhy = fmt.Sprintf("NOT ENFORCED: %s connected from the probe pod, and it must not. %s",
			deniedRes.Target, whatDenied)
		return v
	}
	// WHICH KIND OF BLOCK, for the mtls check only.
	//
	// A sidecar refusing plaintext RESETS the connection; a NetworkPolicy drop
	// blackholes it and shows up as a timeout. ci_net_probe.go draws that
	// distinction on purpose and this is the check that has to act on it: if the
	// dial timed out, the CNI dropped the packet and Istio was never consulted, so
	// a "blocked" verdict here would be the netpol check a second time wearing the
	// mesh's name. That is a pass on a property the run never exercised.
	//
	// The netpol check is deliberately NOT held to this: a drop is exactly what it
	// is asserting, and a reset there is a fine outcome too.
	if check == "mtls" && strings.Contains(deniedRes.Reason, "timeout") {
		v.Inconclu = true
		v.FailWhy = fmt.Sprintf("INCONCLUSIVE: %s was blocked by a TIMEOUT (%s), not a refusal. A timeout is a "+
			"packet drop — the CNI discarded it and Istio never saw the connection, so this run did not observe the "+
			"mesh refusing anything. Istio resets a plaintext dial to a STRICT port, which surfaces as `refused`. "+
			"Check that the probe namespace's egress actually ALLOWS this target, so that the mesh is the only "+
			"thing left that can deny it",
			deniedRes.Target, deniedRes.Reason)
		return v
	}
	v.Detail = fmt.Sprintf("control %s connected; %s blocked (%s)",
		allowedRes.Target, deniedRes.Target, deniedRes.Reason)
	return v
}

// readPeerAuthModes returns every mtls mode declared by the PeerAuthentications
// in a namespace — the workload defaults and the portLevelMtls entries alike.
// Seamed for tests.
var readPeerAuthModes = func(ns string) ([]string, error) {
	out, err := deps.Exec("kubectl", "-n", ns, "get", "peerauthentication",
		"-o", `jsonpath={range .items[*]}{.spec.mtls.mode}{" "}{range .spec.portLevelMtls.*}{.mode}{" "}{end}{end}`)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// meshEnforcesSTRICT reports whether a namespace is actually refusing plaintext.
// Pure.
//
// Absence of any STRICT mode — including no PeerAuthentication at all — means the
// mesh default applies, and this platform's default is PERMISSIVE
// (llz-cluster-foundation values.yaml). A PERMISSIVE workload ACCEPTS plaintext
// by design, so a plaintext dial succeeding against it is correct behaviour and
// not a finding.
func meshEnforcesSTRICT(modes []string) bool {
	for _, m := range modes {
		if strings.EqualFold(strings.TrimSpace(m), "STRICT") {
			return true
		}
	}
	return false
}

// serviceNamespaceOf returns the namespace label of a cluster-local
// "<svc>.<ns>.svc.cluster.local[:port]" target, or "" if it is not that shape.
// Pure.
func serviceNamespaceOf(hostPort string) string {
	host, _, ok := strings.Cut(hostPort, ":")
	if !ok {
		host = hostPort
	}
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// probePodManifest renders the scratch namespace, the default-deny-egress policy
// and the probe pod.
//
// The pod is restricted-PSS compliant and carries sidecar.istio.io/inject=false:
// the mtls check REQUIRES it to be outside the mesh, because a meshed pod would
// have its plaintext transparently upgraded to mTLS by its own sidecar and the
// denied dial would succeed — proving the mesh works while asserting the
// opposite.
func probePodManifest(ns, image, allowed, denied, mtlsTarget string, timeout time.Duration) string {
	secs := int(timeout.Seconds())
	// THE MTLS TARGET'S NAMESPACE IS ALLOWED, on every port, and that is the point
	// rather than a hole. The mtls check claims to observe Istio refusing
	// plaintext; if this NetworkPolicy also denies the dial, the CNI drops the
	// packet first and Istio is never consulted — the check would then be a second
	// copy of the netpol check, passing on a property it never exercised. Opening
	// the path leaves the mesh as the only thing that can refuse it, which is what
	// the check has to isolate. (The denied target for the NETPOL check stays
	// unallowed — that one is about the CNI.)
	mtlsNS := serviceNamespaceOf(mtlsTarget)
	mtlsAllow := ""
	if mtlsNS != "" {
		mtlsAllow = fmt.Sprintf(`
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: %s`, mtlsNS)
	}
	// One container per dial so each exit code is separately readable from the
	// pod's containerStatuses. initContainers run in order and would stop at the
	// first failure, which is precisely what must NOT happen here: the denied dial
	// is EXPECTED to fail and the control still has to run.
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
  labels:
    pod-security.kubernetes.io/enforce: restricted
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-egress
  namespace: %[1]s
spec:
  podSelector: {}
  policyTypes: [Egress]
  egress:
    # DNS, or every dial fails on resolution and the control cannot distinguish
    # "policy blocked it" from "the name never resolved".
    #
    # BOTH label values, because the upstream convention is not universal. Stock
    # kube-dns/CoreDNS carries k8s-app=kube-dns, but LKE-Enterprise installs
    # CoreDNS from its "workload" Helm release labelled k8s-app=coredns and
    # publishes it as the "coredns" Service — there is no kube-dns Service on the
    # cluster at all. Selecting only kube-dns matched NOTHING there, so DNS egress
    # was denied, every dial failed to resolve, and the positive control failed —
    # which this gate correctly reported as INCONCLUSIVE rather than as
    # enforcement. matchExpressions/In is the one selector that covers both
    # without a second rule.
    - to:
        - namespaceSelector: {}
          podSelector:
            matchExpressions:
              - key: k8s-app
                operator: In
                values: [kube-dns, coredns]
      ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
    # The positive control's target: the apiserver.
    #
    # NO "to:" SELECTOR, DELIBERATELY — a bare port allow, matching any
    # destination. This rule is copied in shape from the llz-openbao-platform
    # policy, which reaches the apiserver successfully on these clusters every
    # bootstrap; the difference between them was the whole bug.
    #
    # "to: ipBlock: 0.0.0.0/0" reads like "anywhere" and is not. Cilium turns an
    # ipBlock into a CIDR rule, and CIDR rules match entities OUTSIDE the cluster;
    # the kube-apiserver carries a cluster identity, so the rule never applied to
    # it and the dial was blackholed. Two rounds on real clusters both reported
    # the control as blocked by TIMEOUT, a drop rather than a refusal — including
    # one round where 6443 had been added alongside 443, which ruled the port out
    # as the cause.
    #
    # Both ports stay: on LKE-Enterprise the "kubernetes" Service DNATs 443 to 6443
    # and Cilium evaluates egress on the post-DNAT port. The OpenBao policy
    # carries both for exactly this reason.
    - ports:
        - protocol: TCP
          port: 443
        - protocol: TCP
          port: 6443%[7]s
---
apiVersion: v1
kind: Pod
metadata:
  name: net-probe
  namespace: %[1]s
  annotations:
    sidecar.istio.io/inject: "false"
  labels:
    app: llz-net-probe
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    seccompProfile: {type: RuntimeDefault}
  containers:
    - name: control
      image: %[2]s
      args: ["ci", "net-probe", "%[3]s", "--timeout", "%[6]d"]
      securityContext: &sec
        allowPrivilegeEscalation: false
        capabilities: {drop: ["ALL"]}
        readOnlyRootFilesystem: true
    - name: denied
      image: %[2]s
      args: ["ci", "net-probe", "%[4]s", "--timeout", "%[6]d"]
      securityContext: *sec
    - name: mtls
      image: %[2]s
      args: ["ci", "net-probe", "%[5]s", "--timeout", "%[6]d"]
      securityContext: *sec
`, ns, image, allowed, denied, mtlsTarget, secs, mtlsAllow)
}

// ── cluster I/O (seamed) ─────────────────────────────────────────────────────

var (
	// THROUGH THE DECLARED GRANT, not a raw exec. This call is why the first
	// capability census undercounted: it never touched a Deps seam, so a
	// seam-based count saw an extension that only reads while it was creating a
	// namespace and a probe pod.
	applyProbeManifest = func(manifest string) (string, error) {
		out, err := deps.W().ApplyStdin(manifest, "llz-net-probe")
		return string(out), err
	}
	deleteProbeNamespace = func(ns string) {
		_, _ = deps.W().Delete("", "namespace", ns, "--wait=false")
	}
	waitProbePod = func(ns string, timeout time.Duration) error {
		_, err := deps.Exec("kubectl", "-n", ns, "wait", "--for=jsonpath={.status.phase}=Succeeded",
			"pod/net-probe", fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())))
		if err != nil {
			// A pod whose containers all exited is Succeeded only when every exit
			// code is 0; the denied dial exits 1 by design, so Failed is the normal
			// terminal state. Fall through to reading the statuses either way.
			_, err2 := deps.Exec("kubectl", "-n", ns, "wait", "--for=jsonpath={.status.phase}=Failed",
				"pod/net-probe", fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())))
			if err2 != nil {
				return fmt.Errorf("the probe pod reached neither Succeeded nor Failed: %w", err)
			}
		}
		return nil
	}
	readProbeStatuses = func(ns string) ([]byte, error) {
		return deps.Exec("kubectl", "-n", ns, "get", "pod", "net-probe", "-o", "json")
	}
	// readProbeLog returns one probe container's stdout — the line net-probe
	// prints naming WHY the dial failed. Best-effort: a log that cannot be read
	// must never change a verdict, only how well it is explained.
	readProbeLog = func(ns, container string) string {
		out, err := deps.Exec("kubectl", "-n", ns, "logs", "net-probe", "-c", container)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	// resolveProbeImage reads the image the reconciler Deployment runs, so the
	// probe uses an image already present and already signature-gated rather than
	// one this file guesses at.
	resolveProbeImage = func() (string, error) {
		out, err := deps.Exec("kubectl", "-n", "llz-reconciler", "get", "deploy", "llz-reconciler",
			"-o", "jsonpath={.spec.template.spec.containers[0].image}")
		if err != nil {
			return "", fmt.Errorf("reading the llz image from the reconciler Deployment: %w", err)
		}
		img := strings.TrimSpace(string(out))
		if img == "" {
			return "", fmt.Errorf("the reconciler Deployment reported no image")
		}
		return img, nil
	}
)

// containerExit maps a container name to its terminated exit code.
func containerExit(raw []byte) (map[string]int, error) {
	var pod struct {
		Status struct {
			ContainerStatuses []struct {
				Name  string `json:"name"`
				State struct {
					Terminated *struct {
						ExitCode int `json:"exitCode"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &pod); err != nil {
		return nil, fmt.Errorf("decoding the probe pod: %w", err)
	}
	out := map[string]int{}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated == nil {
			return nil, fmt.Errorf("probe container %q has not terminated — the pod did not finish", cs.Name)
		}
		out[cs.Name] = cs.State.Terminated.ExitCode
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the probe pod reported no container statuses")
	}
	return out, nil
}

// resultFromExit turns a net-probe exit code back into a result. Exit 2 (the
// probe could not run) is NOT "blocked" — it is an inconclusive control.
//
// `log` is the probe container's own stdout, which already names the distinction
// that matters: net-probe classifies every dial as refused / timeout / dns
// precisely so a reader need not guess, and collapsing all of them to "blocked"
// threw away the only evidence the gate had. A failed positive control used to
// print "check the probe pod's scheduling, image pull and DNS" while holding the
// answer to that question — "dns" points at CoreDNS or the policy's DNS allow,
// "timeout" at a policy drop, "refused" at a closed port or a sidecar reset.
// Empty when the log could not be read, in which case the verdict is unchanged.
func resultFromExit(target string, code int, ok bool, log string) netProbeResult {
	if !ok {
		return netProbeResult{Target: target, Connected: false, Reason: "the probe container did not report an exit code"}
	}
	switch code {
	case 0:
		return netProbeResult{Target: target, Connected: true, Reason: "connected"}
	case 2:
		return netProbeResult{Target: target, Connected: false, Reason: withProbeLog("the probe could not run (bad address)", log)}
	default:
		return netProbeResult{Target: target, Connected: false, Reason: withProbeLog("blocked", log)}
	}
}

// withProbeLog appends the probe's own explanation to a reason. Pure.
func withProbeLog(reason, log string) string {
	if log == "" {
		return reason
	}
	return reason + ": " + log
}

func runCIAssertNetworkEnforcement(o netEnforceOpts) error {
	fmt.Println("## Network-enforcement assertion (NetworkPolicy + mTLS, in the data plane)")
	if len(o.checks) == 0 {
		fmt.Fprintln(os.Stderr, "::error::no --checks given — refusing to pass having verified nothing")
		return fmt.Errorf("no --checks given — refusing to pass vacuously")
	}

	image := o.image
	if image == "" {
		var err error
		if image, err = resolveProbeImage(); err != nil {
			fmt.Fprintf(os.Stderr, "::error::%v\n", err)
			return err
		}
	}
	fmt.Printf("probe image %s, scratch namespace %s\n", image, o.namespace)

	// Delete first in case a previous run leaked, then always clean up.
	deleteProbeNamespace(o.namespace)
	if !o.keep {
		defer deleteProbeNamespace(o.namespace)
	}

	manifest := probePodManifest(o.namespace, image, o.allowed, o.denied, o.mtlsTarget, o.timeout)
	if out, err := applyProbeManifest(manifest); err != nil {
		fmt.Fprintf(os.Stderr, "::error::could not create the probe: %v\n", err)
		return fmt.Errorf("creating the network probe: %w (%s)", err, harborauth.TruncateForError([]byte(out)))
	}

	// The pod terminates Failed by design (the denied dial exits 1), so both
	// terminal phases are acceptable — what matters is that it finished.
	if err := waitProbePod(o.namespace, o.timeout*4); err != nil {
		fmt.Fprintf(os.Stderr, "::error::%v\n", err)
		return fmt.Errorf("the network probe did not complete: %w", err)
	}
	raw, err := readProbeStatuses(o.namespace)
	if err != nil {
		return fmt.Errorf("reading the probe pod: %w", err)
	}
	exits, err := containerExit(raw)
	if err != nil {
		return err
	}

	ctrlCode, ctrlOK := exits["control"]
	control := resultFromExit(o.allowed, ctrlCode, ctrlOK, readProbeLog(o.namespace, "control"))

	var vs []netEnforceVerdict
	for _, check := range o.checks {
		switch check {
		case "netpol":
			code, ok := exits["denied"]
			vs = append(vs, evalEnforcementProbe("netpol", control, resultFromExit(o.denied, code, ok, readProbeLog(o.namespace, "denied")),
				"The scratch namespace carries a default-deny-egress NetworkPolicy that does not allow this "+
					"address, so the CNI is not enforcing NetworkPolicy — which makes every default-deny in this "+
					"repo decorative. On LKE-E that is Cilium; check the agent is healthy and the policy was programmed."))
		case "mtls":
			// IS THE MESH ACTUALLY ENFORCING HERE YET? harbor ships all three of its
			// PeerAuthentication documents at mode: PERMISSIVE — ADR 0010 step 3, the
			// measurement phase, whose whole purpose is to watch
			// istio_requests_total{connection_security_policy="none"} fall to zero
			// BEFORE flipping to STRICT. A PERMISSIVE workload accepts plaintext by
			// design, so the dial succeeding is correct behaviour, and asserting
			// otherwise reds every cluster for a rollout step nobody has taken.
			//
			// harbor-peerauthentication.yaml already names this trap: "every claim
			// resting on 'harbor is STRICT' was aspirational", listing two guards that
			// made it. This check was the third. Reading the mode off the cluster is
			// what stops there being a fourth — the moment those three lines flip to
			// STRICT, this check starts enforcing with no code change.
			if ns := serviceNamespaceOf(o.mtlsTarget); ns != "" {
				modes, merr := readPeerAuthModes(ns)
				switch {
				case merr != nil:
					vs = append(vs, netEnforceVerdict{Check: "mtls",
						FailWhy: fmt.Sprintf("could not read the PeerAuthentication mode in %s (%v) — refusing to "+
							"guess whether the mesh is enforcing", ns, merr)})
					continue
				case !meshEnforcesSTRICT(modes):
					vs = append(vs, netEnforceVerdict{Check: "mtls", Skipped: true,
						Detail: fmt.Sprintf("namespace %s declares no STRICT PeerAuthentication (modes: %v) — the mesh "+
							"is PERMISSIVE there and ACCEPTS plaintext by design. This is ADR 0010 step 3, the "+
							"measurement phase; the check begins enforcing automatically when those modes flip to STRICT",
							ns, modes)})
					continue
				}
			}
			code, ok := exits["mtls"]
			vs = append(vs, evalEnforcementProbe("mtls", control, resultFromExit(o.mtlsTarget, code, ok, readProbeLog(o.namespace, "mtls")),
				"The probe pod is deliberately OUTSIDE the mesh (sidecar.istio.io/inject=false) and dialed this "+
					"STRICT-mesh port in plaintext. Istio must refuse that. Check the namespace's PeerAuthentication "+
					"is still STRICT and has not been reverted to PERMISSIVE, and that this port is not a portLevelMtls exemption."))
		default:
			vs = append(vs, netEnforceVerdict{Check: check,
				FailWhy: "unknown check — the lane is asking for something this verb does not implement"})
		}
	}

	var bad []string
	observed := 0
	for _, v := range vs {
		switch {
		case v.FailWhy != "":
			fmt.Printf("FAIL: %s — %s\n", v.Check, v.FailWhy)
			bad = append(bad, v.Check)
		case v.Skipped:
			fmt.Printf("SKIP: %s — %s\n", v.Check, v.Detail)
		default:
			observed++
			fmt.Printf("OK: %s — %s\n", v.Check, v.Detail)
		}
	}
	if len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "::error::network enforcement not observed: %s\n", strings.Join(bad, ", "))
		return fmt.Errorf("network enforcement not observed: %s", strings.Join(bad, ", "))
	}
	// Every check skipping is not a pass. This lane exists to observe enforcement
	// in the data plane; a run that observed none of it must say so.
	if observed == 0 {
		fmt.Fprintln(os.Stderr, "::error::every requested check skipped — no enforcement was observed")
		return fmt.Errorf("every requested check skipped — refusing to pass vacuously")
	}
	fmt.Printf("All %d network-enforcement check(s) observed enforcing.\n", observed)
	return nil
}
