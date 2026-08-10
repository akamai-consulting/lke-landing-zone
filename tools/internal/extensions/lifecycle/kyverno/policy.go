package kyverno

// ci_kyverno.go implements `llz ci apply-kyverno-policy` — the local-exec body
// the kyverno_pvc_encrypted_policy null_resource in instance-template
// cluster-bootstrap/main.tf runs (it replaced the former
// scripts/apply-kyverno-policy.sh). The low-race loki-s3 + oauth2-proxy policies
// it used to also drive now ship via the GitOps tree
// (manifest/kyverno-policies/); only the PVC-encryption
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
// apply/retrofit state machine (Apply) is driven through injected
// seams (kubectl runner, clock, sleep) so it is unit-tested without a cluster;
// the env parsing and webhook-race classification are pure functions.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
	"sigs.k8s.io/yaml"
)

type Opts struct {
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

func Run() error {
	o, err := kyvernoOptsFromEnv(os.Getenv)
	if err != nil {
		return err
	}

	kubeconfig, cleanup, err := cigate.WriteTempKubeconfig("llz-kyverno-kubeconfig-*", []byte(o.kubeconfigRaw))
	if err != nil {
		return err
	}
	defer cleanup()

	return Apply(o, cigate.NewDepsFor(kubeconfig).GrantedBy(Extension().MustBindingOf(extension.Transition, extension.Converged)))
}

func kyvernoOptsFromEnv(getenv func(string) string) (Opts, error) {
	o := Opts{
		kubeconfigRaw:      getenv("KUBECONFIG_RAW"),
		policyManifest:     getenv("POLICY_MANIFEST"),
		fieldManager:       cigate.EnvOrDefault(getenv, "FIELD_MANAGER", "cluster-bootstrap-tf"),
		waitForKyverno:     getenv("WAIT_FOR_KYVERNO") != "false", // default true
		timeoutWarning:     getenv("TIMEOUT_WARNING"),
		crdMissingWarning:  getenv("CRD_MISSING_WARNING"),
		webhookRaceWarning: getenv("WEBHOOK_RACE_WARNING"),
		retrofitConfigMap:  getenv("RETROFIT_CONFIGMAP"),
		retrofitNamespace:  cigate.EnvOrDefault(getenv, "RETROFIT_NAMESPACE", "monitoring"),
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

// Apply runs the poll/apply/retrofit state machine. It returns a
// non-nil error ONLY on a hard apply failure (a non-race kubectl-apply error);
// every readiness timeout, missing-CRD guard, and webhook race is a soft-fail
// (::warning:: + nil) exactly as the bash exited 0.
func Apply(o Opts, d cigate.Deps) error {
	if o.waitForKyverno {
		// Poll until the CRD exists AND the admission controller is Available.
		ready := cigate.PollUntil(d.Now, d.Sleep, o.waitTimeout, 5*time.Second, func() bool {
			if _, ok := d.Kubectl("get", "crd", "clusterpolicies.kyverno.io"); !ok {
				return false
			}
			_, ok := d.Kubectl("-n", "kyverno", "wait", "--for=condition=Available",
				"deployment/kyverno-admission-controller", "--timeout=5s")
			return ok
		})
		if !ready {
			warn(firstNonEmpty(o.timeoutWarning,
				"Kyverno admission controller not Ready within deadline — skipping policy apply. Re-run terraform apply once Kyverno is up."))
			return nil
		}
	} else if _, ok := d.Kubectl("get", "crd", "clusterpolicies.kyverno.io"); !ok {
		warn(firstNonEmpty(o.crdMissingWarning,
			"Kyverno ClusterPolicy CRD not present — skipping policy apply."))
		return nil
	}

	// ApplyServerSide is the NAMED escape hatch: applying a manifest is close to
	// unrestricted in permission terms, so it is the one operation a reviewer can
	// grep for rather than an argv indistinguishable from a `get`.
	raw, applyErr := d.W().ApplyServerSide(o.policyManifest, o.fieldManager)
	out, ok := string(raw), applyErr == nil
	if !ok {
		if out == "" {
			out = applyErr.Error()
		}
		if health.IsWebhookRace(out) {
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
		if _, ok := d.Kubectl("wait", "--for=condition=Ready", "clusterpolicy/"+name, "--timeout=60s"); ok {
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
func retrofitKyvernoConfigMap(o Opts, d cigate.Deps) {
	ns := o.retrofitNamespace
	present := cigate.PollUntil(d.Now, d.Sleep, o.retrofitWait, 5*time.Second, func() bool {
		_, ok := d.Kubectl("-n", ns, "get", "configmap", o.retrofitConfigMap)
		return ok
	})
	if !present {
		notice(fmt.Sprintf("%s/%s absent after %s — it is created after this policy, so the admission CREATE rule mutates it. No retrofit needed.",
			ns, o.retrofitConfigMap, o.retrofitWait))
		return
	}
	// A changing annotation value guarantees a real UPDATE (admission fires).
	annotation := "llz.akamai.com/kyverno-retrofit=" + strconv.FormatInt(d.Now().Unix(), 10)
	if _, err := d.W().Annotate(ns, "configmap", o.retrofitConfigMap, annotation); err == nil {
		notice(fmt.Sprintf("retrofit: kicked pre-existing %s/%s through admission so %s mutates it.",
			ns, o.retrofitConfigMap, policyName(o.policyManifest)))
	}
	if o.retrofitRollout != "" {
		if _, err := d.W().RolloutRestart(ns, "deploy/"+o.retrofitRollout); err == nil {
			notice(fmt.Sprintf("retrofit: rolled %s/deploy/%s to reload the mutated config.", ns, o.retrofitRollout))
		}
	}
}

func warn(msg string)   { fmt.Printf("::warning::%s\n", msg) }
func notice(msg string) { fmt.Printf("::notice::%s\n", msg) }

// policyName is the ClusterPolicy's OWN metadata.name — the identity
// `kubectl wait clusterpolicy/<name>` addresses.
//
// It used to be the manifest's basename minus .yaml, inherited from the bash's
// `basename … .yaml` LOGGING label. That was fine as a log label and wrong the
// moment it started addressing the API, because no manifest's filename equals the
// name it declares:
//
//	kyverno-pvc-encrypted-storage-class.yaml        → pvc-force-encrypted-storage-class
//	kyverno-pvc-redirect-untagged-storage-class.yaml → pvc-redirect-untagged-storage-class
//	kyverno-sc-default-demote.yaml                  → sc-default-demote
//
// So the readiness wait always addressed a nonexistent object, always failed, and
// always degraded to the "applied but did not report Ready" warning. The one check
// that confirms a policy is actually ENFORCING has never run, on any policy.
//
// Falls back to the basename when the manifest cannot be read or declares no
// metadata.name; that only re-enters the old behaviour, which is no worse.
func policyName(manifest string) string {
	fallback := strings.TrimSuffix(filepath.Base(manifest), ".yaml")
	// Fenced at the tree containing the manifest: the path arrives from the
	// caller, absolute in tests and relative in production, and RepoContaining
	// relates the two forms rather than refusing one of them.
	repo, rel := capability.RepoContaining(repoBinding(), manifest)
	raw, err := repo.ReadFile(rel)
	if err != nil {
		return fallback
	}
	var doc struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil || doc.Metadata.Name == "" {
		return fallback
	}
	return doc.Metadata.Name
}

// firstNonEmpty returns the first non-empty string. Pure, localised — package
// main keeps its own copy for the token path, and there is no behaviour to drift.
// The fifth package in this campaign to reach that conclusion about these three
// lines.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
