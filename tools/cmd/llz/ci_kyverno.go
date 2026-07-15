package main

// ci_kyverno.go implements `llz ci apply-kyverno-policy` — the local-exec body
// the kyverno_pvc_encrypted_policy null_resource in instance-template
// cluster-bootstrap/main.tf runs (it replaced the former
// scripts/apply-kyverno-policy.sh). The low-race loki-s3 + oauth2-proxy policies
// it used to also drive now ship via the GitOps tree
// (platform-apl/manifest/kyverno-policies/); only the PVC-encryption
// policy stays imperative here, because it must beat apl-operator's non-Argo PVC
// creation — a race Argo sync-waves can't win.
//
// Flow (unchanged from the bash it replaced): write KUBECONFIG_RAW to a
// tempfile, optionally poll until Kyverno can admit a ClusterPolicy (CRD present
// AND the admission controller Available) up to a deadline, server-side-apply
// the manifest, soft-fail (warn + exit 0) on the transient kyverno-svc
// admission-webhook race, then optionally "retrofit kick" a pre-existing
// ConfigMap through admission so the just-applied policy mutates it. Soft-fails
// never fail the terraform apply.
//
// The config arrives as the same environment variables the null_resource
// `environment` blocks set, so main.tf only changed its `command`. The poll/
// apply/retrofit state machine (applyKyvernoPolicy) is driven through injected
// seams (kubectl runner, clock, sleep) so it is unit-tested without a cluster;
// the env parsing and webhook-race classification are pure functions.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type kyvernoPolicyOpts struct {
	kubeconfigRaw  string
	policyManifest string
	fieldManager   string
	waitForKyverno bool
	waitTimeout    time.Duration

	timeoutWarning     string
	crdMissingWarning  string
	webhookRaceWarning string

	retrofitConfigMap string
	retrofitNamespace string
	retrofitRollout   string
	retrofitWait      time.Duration
}

// kyvernoDeps are the seams the state machine drives. kubectl runs one kubectl
// invocation (KUBECONFIG already wired by the caller) and returns combined
// output plus whether it exited 0; now/sleep make the deadline loops testable.
type kyvernoDeps struct {
	kubectl func(args ...string) (string, bool)
	now     func() time.Time
	sleep   func(time.Duration)
}

func ciApplyKyvernoPolicyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply-kyverno-policy",
		Short: "apply a Kyverno ClusterPolicy with readiness poll + webhook-race soft-fail (terraform local-exec body)",
		Long: "Shared local-exec body for the kyverno_* null_resources in cluster-bootstrap.\n" +
			"Reads its config from the same environment variables those null_resources set\n" +
			"(KUBECONFIG_RAW, POLICY_MANIFEST, WAIT_FOR_KYVERNO, WAIT_TIMEOUT_SECONDS,\n" +
			"FIELD_MANAGER, {TIMEOUT,CRD_MISSING,WEBHOOK_RACE}_WARNING, RETROFIT_*), polls\n" +
			"until Kyverno can admit the policy, server-side-applies it, soft-fails on the\n" +
			"kyverno-svc admission-webhook race, and runs the optional retrofit kick.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIApplyKyvernoPolicy() },
	}
}

func runCIApplyKyvernoPolicy() error {
	o, err := kyvernoOptsFromEnv(os.Getenv)
	if err != nil {
		return err
	}

	kubeconfig, err := os.CreateTemp("", "llz-kyverno-kubeconfig-*")
	if err != nil {
		return fmt.Errorf("create kubeconfig tempfile: %w", err)
	}
	defer os.Remove(kubeconfig.Name())
	if _, err := kubeconfig.WriteString(o.kubeconfigRaw); err != nil {
		kubeconfig.Close()
		return fmt.Errorf("write kubeconfig: %w", err)
	}
	kubeconfig.Close()

	deps := kyvernoDeps{
		kubectl: func(args ...string) (string, bool) {
			cmd := exec.Command("kubectl", args...)
			cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig.Name())
			var buf bytes.Buffer
			cmd.Stdout, cmd.Stderr = &buf, &buf
			err := cmd.Run()
			return buf.String(), err == nil
		},
		now:   time.Now,
		sleep: time.Sleep,
	}
	return applyKyvernoPolicy(o, deps)
}

func kyvernoOptsFromEnv(getenv func(string) string) (kyvernoPolicyOpts, error) {
	o := kyvernoPolicyOpts{
		kubeconfigRaw:      getenv("KUBECONFIG_RAW"),
		policyManifest:     getenv("POLICY_MANIFEST"),
		fieldManager:       envOrDefault(getenv, "FIELD_MANAGER", "cluster-bootstrap-tf"),
		waitForKyverno:     getenv("WAIT_FOR_KYVERNO") != "false", // default true
		timeoutWarning:     getenv("TIMEOUT_WARNING"),
		crdMissingWarning:  getenv("CRD_MISSING_WARNING"),
		webhookRaceWarning: getenv("WEBHOOK_RACE_WARNING"),
		retrofitConfigMap:  getenv("RETROFIT_CONFIGMAP"),
		retrofitNamespace:  envOrDefault(getenv, "RETROFIT_NAMESPACE", "monitoring"),
		retrofitRollout:    getenv("RETROFIT_ROLLOUT"),
	}
	if o.kubeconfigRaw == "" {
		return o, fmt.Errorf("KUBECONFIG_RAW must be set")
	}
	if o.policyManifest == "" {
		return o, fmt.Errorf("POLICY_MANIFEST must be set")
	}
	secs, err := envSecondsOrDefault(getenv, "WAIT_TIMEOUT_SECONDS", 900)
	if err != nil {
		return o, err
	}
	o.waitTimeout = time.Duration(secs) * time.Second
	rsecs, err := envSecondsOrDefault(getenv, "RETROFIT_WAIT_SECONDS", 60)
	if err != nil {
		return o, err
	}
	o.retrofitWait = time.Duration(rsecs) * time.Second
	return o, nil
}

func envOrDefault(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

func envSecondsOrDefault(getenv func(string) string, key string, def int) (int, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer number of seconds, got %q", key, v)
	}
	return n, nil
}

// kyvernoWebhookRaceRE matches the transient kyverno-svc admission errors that
// mean "Kyverno is up but its webhook endpoint/cert isn't reachable yet" — a
// 30-90s race that re-running terraform apply clears, so it must not fail the
// whole apply.
var kyvernoWebhookRaceRE = regexp.MustCompile(`failed calling webhook|connect: operation not permitted|connection refused|no endpoints available`)

func isKyvernoWebhookRace(out string) bool { return kyvernoWebhookRaceRE.MatchString(out) }

// applyKyvernoPolicy runs the poll/apply/retrofit state machine. It returns a
// non-nil error ONLY on a hard apply failure (a non-race kubectl-apply error);
// every readiness timeout, missing-CRD guard, and webhook race is a soft-fail
// (::warning:: + nil) exactly as the bash exited 0.
func applyKyvernoPolicy(o kyvernoPolicyOpts, d kyvernoDeps) error {
	if o.waitForKyverno {
		// Poll until the CRD exists AND the admission controller is Available.
		ready := pollUntil(d, o.waitTimeout, func() bool {
			if _, ok := d.kubectl("get", "crd", "clusterpolicies.kyverno.io"); !ok {
				return false
			}
			_, ok := d.kubectl("-n", "kyverno", "wait", "--for=condition=Available",
				"deployment/kyverno-admission-controller", "--timeout=5s")
			return ok
		})
		if !ready {
			warn(firstNonEmpty(o.timeoutWarning,
				"Kyverno admission controller not Ready within deadline — skipping policy apply. Re-run terraform apply once Kyverno is up."))
			return nil
		}
	} else if _, ok := d.kubectl("get", "crd", "clusterpolicies.kyverno.io"); !ok {
		warn(firstNonEmpty(o.crdMissingWarning,
			"Kyverno ClusterPolicy CRD not present — skipping policy apply."))
		return nil
	}

	out, ok := d.kubectl("apply", "--server-side", "--force-conflicts",
		"--field-manager="+o.fieldManager, "-f", o.policyManifest)
	if !ok {
		if isKyvernoWebhookRace(out) {
			warn(firstNonEmpty(o.webhookRaceWarning,
				"Kyverno admission webhook not yet reachable — policy apply skipped. Re-run terraform apply once kyverno-svc has Ready endpoints."))
			fmt.Fprint(os.Stderr, out)
			return nil
		}
		fmt.Fprint(os.Stderr, out)
		return fmt.Errorf("kubectl apply %s failed", o.policyManifest)
	}

	// Confirm the policy actually reaches Ready — a ClusterPolicy can apply cleanly
	// yet sit not-Ready (webhook/cert not wired), in which case it silently mutates
	// nothing. Best-effort: surface a non-Ready policy as a ::warning:: (the
	// PVC-storageclass audit still backstops any escapees) rather than failing the
	// apply, since the cluster is otherwise functional.
	if name := policyName(o.policyManifest); name != "" {
		if _, ok := d.kubectl("wait", "--for=condition=Ready", "clusterpolicy/"+name, "--timeout=60s"); ok {
			notice(fmt.Sprintf("clusterpolicy/%s is Ready (enforcing).", name))
		} else {
			warn(fmt.Sprintf("clusterpolicy/%s applied but did not report Ready within 60s — it may not be enforcing yet; the PVC-storageclass audit will flag any escapees.", name))
		}
	}

	if o.retrofitConfigMap != "" {
		retrofitKyvernoConfigMap(o, d)
	}
	return nil
}

// retrofitKyvernoConfigMap closes the admission race for a policy that mutates a
// ConfigMap created by another controller: if the target predates the policy
// (so the admission rule never fired on it), force one UPDATE through admission
// and optionally roll the consumer. Best-effort — never returns an error.
func retrofitKyvernoConfigMap(o kyvernoPolicyOpts, d kyvernoDeps) {
	ns := o.retrofitNamespace
	present := pollUntil(d, o.retrofitWait, func() bool {
		_, ok := d.kubectl("-n", ns, "get", "configmap", o.retrofitConfigMap)
		return ok
	})
	if !present {
		notice(fmt.Sprintf("%s/%s absent after %s — it is created after this policy, so the admission CREATE rule mutates it. No retrofit needed.",
			ns, o.retrofitConfigMap, o.retrofitWait))
		return
	}
	// A changing annotation value guarantees a real UPDATE (admission fires).
	annotation := "llz.akamai.com/kyverno-retrofit=" + strconv.FormatInt(d.now().Unix(), 10)
	if _, ok := d.kubectl("-n", ns, "annotate", "configmap", o.retrofitConfigMap, annotation, "--overwrite"); ok {
		notice(fmt.Sprintf("retrofit: kicked pre-existing %s/%s through admission so %s mutates it.",
			ns, o.retrofitConfigMap, policyName(o.policyManifest)))
	}
	if o.retrofitRollout != "" {
		if _, ok := d.kubectl("-n", ns, "rollout", "restart", "deploy/"+o.retrofitRollout); ok {
			notice(fmt.Sprintf("retrofit: rolled %s/deploy/%s to reload the mutated config.", ns, o.retrofitRollout))
		}
	}
}

// pollUntil calls cond immediately then every 5s until it returns true or the
// timeout elapses (mirrors the bash `until … sleep 5` loops). now/sleep are
// injected so tests run without real waiting.
func pollUntil(d kyvernoDeps, timeout time.Duration, cond func() bool) bool {
	deadline := d.now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if !d.now().Before(deadline) {
			return false
		}
		d.sleep(5 * time.Second)
	}
}

func warn(msg string)   { fmt.Printf("::warning::%s\n", msg) }
func notice(msg string) { fmt.Printf("::notice::%s\n", msg) }

// policyName is the manifest's basename without the .yaml extension — the label
// the bash logged via `basename … .yaml`.
func policyName(manifest string) string {
	return strings.TrimSuffix(filepath.Base(manifest), ".yaml")
}
