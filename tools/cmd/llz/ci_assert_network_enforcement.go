package main

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
// IT CLEANS UP AFTER ITSELF, including on failure: the scratch namespace is
// deleted in a defer. A gate that leaks a namespace on every red run makes the
// next run's cluster dirtier, and this one runs on a cluster that is about to be
// asserted against by ten other lanes.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
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
	// netEnforceMTLSTarget is a STRICT-mesh port. harbor is namespace-wide STRICT
	// (harbor-peerauthentication.yaml), so a plaintext dial from an unmeshed pod
	// must be refused by the sidecar.
	netEnforceMTLSTarget = "harbor-core.harbor.svc.cluster.local:80"
)

func ciAssertNetworkEnforcementCmd() *cobra.Command {
	var checks, image, namespace, allowed, denied, mtlsTarget string
	var timeout int
	var keep bool
	c := &cobra.Command{
		Use:   "assert-network-enforcement",
		Short: "fail unless NetworkPolicy and mTLS are actually enforced in the data plane",
		Long: "Opens real connections from a real pod, because these are the two enforcement\n" +
			"properties that cannot be server-dry-run: admission answers from the API server,\n" +
			"but a dropped packet is only knowable by sending one.\n\n" +
			"EVERY negative carries a POSITIVE CONTROL. \"The connection failed\" is equally\n" +
			"consistent with the policy working, the image not pulling, DNS being down, or\n" +
			"the pod never being scheduled — so each check dials an ALLOWED address that\n" +
			"must connect and a DENIED one that must not, from the same pod in the same run.\n" +
			"If the allowed dial fails the run is INCONCLUSIVE and fails as such, rather\n" +
			"than being reported as enforcement it never observed.\n\n" +
			"  netpol — a scratch namespace with default-deny egress (DNS + apiserver only).\n" +
			"           Proves the CNI enforces NetworkPolicy at all; a cluster where it\n" +
			"           silently does not is one where every default-deny here is decorative.\n" +
			"  mtls   — the same pod, deliberately unmeshed, dialing a STRICT-mesh port in\n" +
			"           plaintext. Istio must refuse it.\n\n" +
			"MUTATING: creates and deletes a scratch namespace, a NetworkPolicy and a Pod.\n" +
			"Cleanup runs on failure too. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertNetworkEnforcement(netEnforceOpts{
				checks:     splitCSVList(checks),
				image:      image,
				namespace:  namespace,
				allowed:    allowed,
				denied:     denied,
				mtlsTarget: mtlsTarget,
				timeout:    time.Duration(timeout) * time.Second,
				keep:       keep,
			})
		},
	}
	c.Flags().StringVar(&checks, "checks", "netpol,mtls", "comma-separated checks to run (netpol, mtls)")
	c.Flags().StringVar(&image, "image", "", "llz image for the probe pod (default: the image this binary's reconciler Deployment runs)")
	c.Flags().StringVar(&namespace, "namespace", netEnforceNamespace, "scratch namespace to create and delete")
	c.Flags().StringVar(&allowed, "allowed-target", netEnforceAllowedTarget, "host:port the probe MUST reach (the positive control)")
	c.Flags().StringVar(&denied, "denied-target", netEnforceDeniedTarget, "host:port the NetworkPolicy must block")
	c.Flags().StringVar(&mtlsTarget, "mtls-target", netEnforceMTLSTarget, "host:port on a STRICT-mesh workload that must refuse plaintext")
	c.Flags().IntVar(&timeout, "timeout", 8, "seconds each dial waits")
	c.Flags().BoolVar(&keep, "keep", false, "keep the scratch namespace for debugging (default: always delete)")
	return c
}

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
	v.Detail = fmt.Sprintf("control %s connected; %s blocked (%s)",
		allowedRes.Target, deniedRes.Target, deniedRes.Reason)
	return v
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
    # The positive control's target: the apiserver, reachable by CIDR because it
    # is outside the pod network.
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
      ports:
        - protocol: TCP
          port: 443
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
`, ns, image, allowed, denied, mtlsTarget, secs)
}

// ── cluster I/O (seamed) ─────────────────────────────────────────────────────

var (
	applyProbeManifest = func(manifest string) (string, error) {
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	deleteProbeNamespace = func(ns string) {
		_ = execCombined("kubectl", "delete", "namespace", ns, "--ignore-not-found", "--wait=false")
	}
	waitProbePod = func(ns string, timeout time.Duration) error {
		_, err := execOutput("kubectl", "-n", ns, "wait", "--for=jsonpath={.status.phase}=Succeeded",
			"pod/net-probe", fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())))
		if err != nil {
			// A pod whose containers all exited is Succeeded only when every exit
			// code is 0; the denied dial exits 1 by design, so Failed is the normal
			// terminal state. Fall through to reading the statuses either way.
			_, err2 := execOutput("kubectl", "-n", ns, "wait", "--for=jsonpath={.status.phase}=Failed",
				"pod/net-probe", fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())))
			if err2 != nil {
				return fmt.Errorf("the probe pod reached neither Succeeded nor Failed: %w", err)
			}
		}
		return nil
	}
	readProbeStatuses = func(ns string) ([]byte, error) {
		return execOutput("kubectl", "-n", ns, "get", "pod", "net-probe", "-o", "json")
	}
	// readProbeLog returns one probe container's stdout — the line net-probe
	// prints naming WHY the dial failed. Best-effort: a log that cannot be read
	// must never change a verdict, only how well it is explained.
	readProbeLog = func(ns, container string) string {
		out, err := execOutput("kubectl", "-n", ns, "logs", "net-probe", "-c", container)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	// resolveProbeImage reads the image the reconciler Deployment runs, so the
	// probe uses an image already present and already signature-gated rather than
	// one this file guesses at.
	resolveProbeImage = func() (string, error) {
		out, err := execOutput("kubectl", "-n", "llz-reconciler", "get", "deploy", "llz-reconciler",
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
		return fmt.Errorf("creating the network probe: %w (%s)", err, truncateForError([]byte(out)))
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
	for _, v := range vs {
		switch {
		case v.FailWhy != "":
			fmt.Printf("FAIL: %s — %s\n", v.Check, v.FailWhy)
			bad = append(bad, v.Check)
		default:
			fmt.Printf("OK: %s — %s\n", v.Check, v.Detail)
		}
	}
	if len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "::error::network enforcement not observed: %s\n", strings.Join(bad, ", "))
		return fmt.Errorf("network enforcement not observed: %s", strings.Join(bad, ", "))
	}
	fmt.Printf("All %d network-enforcement check(s) observed enforcing.\n", len(vs))
	return nil
}
