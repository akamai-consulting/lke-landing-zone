package objenc

// ci_assert_obj_encryption.go implements `llz ci assert-obj-encryption` — the
// RUNTIME gate that objects landing in Object Storage are actually encrypted.
//
// WHY A RUNTIME GATE AND NOT A CONFIG CHECK. This is the lesson PVC encryption
// taught expensively: `assert-volume-encryption` exists because the config said
// "encrypted" while the Linode API said otherwise, and only the API was telling the
// truth. Every link in this chain can be individually correct and the result still
// plaintext:
//
//   - the Kyverno policy exists, but the registry pod was admitted while Kyverno
//     was down (the live webhook is failurePolicy: Ignore) so it carries no CA
//   - the DaemonSet is Ready, but the CoreDNS rewrite was never applied, so traffic
//     goes straight past the proxy to Linode
//   - the rewrite is applied, but this env's endpoint host does not match the one
//     the certificate names, so clients fail the handshake and nothing is written
//
// None of those show up in a manifest diff. All of them show up here.
//
// THE THREE CHECKS, and why each is the one that catches its own failure:
//
//  1. POD — the registry pods carry SSL_CERT_DIR and the CA mount. Checked on the
//     RUNNING pod, never on the policy: a ClusterPolicy that exists proves nothing
//     about a pod admitted before it, or admitted while the webhook was failing
//     open. This is the check that catches the Ignore-policy race.
//  2. DNS — the endpoint hostname resolves to the obj-proxy Service inside the
//     cluster. Catches "the switch was never flipped", which is the failure that
//     otherwise looks exactly like success: everything Healthy, everything
//     plaintext.
//  3. OBJECT — a real object in the live bucket reports SSE-C on HEAD. The only
//     check that observes the outcome rather than the configuration.
//
// A gate that ran 1 and 2 and skipped 3 would have passed on every plaintext
// cluster this design has ever run on, because 1 and 2 are both true the instant
// before the first write.

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/objstore"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/harborauth"
)

// objEncryptionNS/Selector locate the registry pods the CA must reach.
const (
	HarborNS            = "harbor"
	HarborRegistryLabel = "harbor-registry"
	SsecCertDirEnv      = "SSL_CERT_DIR"
	ObjProxyCAMount     = "/etc/llz/ca"
	ObjProxyCAVolume    = "llz-obj-proxy-ca"
	objProxyService     = "obj-proxy.obj-proxy.svc.cluster.local"
)

// LokiObjSecretRef is the Secret apl-core's Loki release mounts for object storage —
// the same one assert-obj-roundtrip reads, kept in one place so the two cannot drift.
const LokiObjSecretRef = "monitoring/loki-s3-linode-credentials"

// ObjEncConsumerCreds reads an access/secret pair out of a namespaced Secret.
var ObjEncConsumerCreds = func(d Deps, ref, akField, skField string) (string, string, error) {
	ns, name, ok := strings.Cut(ref, "/")
	if !ok {
		return "", "", fmt.Errorf("secret ref %q is not namespace/name", ref)
	}
	raw, err := d.KubectlOut("-n", ns, "get", "secret", name, "-o", "json")
	if err != nil {
		return "", "", err
	}
	ak, err := d.SecretField([]byte(raw), akField)
	if err != nil {
		return "", "", err
	}
	sk, err := d.SecretField([]byte(raw), skField)
	if err != nil {
		return "", "", err
	}
	return ak, sk, nil
}

// The read seam is now Deps.KubectlOut. The checks below are the whole value of
// this gate, and a check that can only run against a live cluster is a check that
// is never exercised until the night it matters.

// Finding is one failed check. Exported with its fields because `llz ci
// readiness` renders the registry-CA check's findings in its own report — the
// second consumer, and the reason this type is not internal.
type Finding struct {
	Check   string
	Problem string
	Fix     string
}

func RunAssertEncryption(d Deps, endpoint, bucket, harborBucket, region string, sample int) error {
	// NO REGION MEANS SKIP, NOT FAIL. The first revision fell through to the flag
	// checks here, so a run without --region hard-failed on an empty --endpoint. That
	// inverted the safe default: a cluster that cannot tell us which deployment it is
	// certainly cannot tell us whether the SSE-C gateway is enabled on it, and redding
	// the battery for that reports an encryption failure where there is only a missing
	// argument.
	if region == "" {
		fmt.Println("::notice::assert-obj-encryption: no --region, so this gate cannot tell whether " +
			"spec.components.objProxy is enabled here. Checked NOTHING.")
		return nil
	}
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return err
	}
	// Component-gated, like seed-ssec-key. A deployment that does not run the proxy
	// has no encrypted objects to find, and failing it would color.Red every cluster for a
	// component it was never asked to run.
	//
	// The skip is LOUD. A gate that quietly passes when disabled is a gate that can
	// be silenced by disabling the thing it guards, so this prints what it did not
	// check rather than printing nothing.
	if !ssecSeedEnabled(lz, region) {
		fmt.Printf("::notice::assert-obj-encryption: spec.components.%s is NOT enabled for %q — "+
			"object storage on this deployment is UNENCRYPTED by design, and this gate checked nothing.\n",
			objProxyComponent, region)
		return nil
	}

	// Derive from the SPEC rather than from flags/env. The endpoint and both bucket
	// names come from the same functions the renderer and the overlay use, so this
	// gate cannot be pointed at a bucket the deployment does not write to. Explicit
	// flags still win, for probing a specific bucket by hand.
	if e, ok := lz.Env(region); ok {
		if endpoint == "" {
			endpoint = clusterspec.ObjEndpointHost(e.Cluster.ObjectStorage.Cluster)
		}
		if bucket == "" {
			bucket = clusterspec.ObjLokiChunksBucket(lz.ObjLabelPrefix(), region)
		}
		if harborBucket == "" {
			harborBucket = clusterspec.ObjHarborRegistryBucket(lz.ObjLabelPrefix(), region)
		}
	}
	if endpoint == "" {
		return fmt.Errorf("could not determine the object-storage endpoint for %q: the env declares no "+
			"cluster.objectStorage.cluster, so there is nothing for the gateway to front", region)
	}
	if bucket == "" {
		return fmt.Errorf("could not determine the Loki chunks bucket for %q — without it check 3 (the "+
			"OUTCOME check) cannot run, and checks 1 and 2 pass on a cluster that has never encrypted anything", region)
	}

	var findings []Finding
	findings = append(findings, CheckRegistryPodsCarryCA(d)...)
	findings = append(findings, checkEndpointResolvesToProxy(d, endpoint)...)
	// HARBOR BEFORE THE SAMPLE, and the ordering is load-bearing. checkHarborPath
	// pushes a blob through Harbor's registry, which is the one post-cutover object
	// this gate can GUARANTEE exists — see checkObjectsAreEncrypted for why that
	// matters. Sampling first meant sampling a bucket nothing had written to yet.
	findings = append(findings, checkHarborPath(d, endpoint, harborBucket)...)
	findings = append(findings, checkObjectsAreEncrypted(d, endpoint, []string{harborBucket, bucket}, sample)...)

	if len(findings) == 0 {
		fmt.Printf("assert-obj-encryption: registry pods carry the CA, the endpoint resolves to the proxy, "+
			"and a live object in %s is encrypted.\n", harborBucket)
		return nil
	}
	for _, f := range findings {
		fmt.Printf("::error::[%s] %s\n  Fix:     %s\n", f.Check, f.Problem, f.Fix)
	}
	return fmt.Errorf("assert-obj-encryption: %d check(s) failed — objects may be landing in PLAINTEXT", len(findings))
}

// checkHarborPath runs the push check when Harbor is on this cluster.
//
// An ABSENT harbor namespace is not a finding: managed App Platform renders a
// minimal app set and simply may not include it, and failing there would color.Red a
// cluster for a component it never ran. An absent --harbor-bucket while the
// namespace IS present is a finding, because that is the check being skipped on a
// cluster that needs it — and it is the ONLY check that proves the CA chain.
func checkHarborPath(d Deps, endpoint, harborBucket string) []Finding {
	// Guard on HARBOR, not on the namespace that happens to hold a credential.
	//
	// This guarded on llz-cert-automation for two revisions, and the reasoning was
	// sound for the bug it fixed (reading a robot Secret out of a namespace a minimal
	// managed render never creates, then reporting the miss as an encryption finding).
	// It was wrong about which namespace is the SUBJECT. llz-cert-automation is
	// LLZ's own cert component, and on a managed cluster cert-automation is
	// apl-core's — see components.go — so that namespace is not merely "sometimes
	// absent", it is NEVER present on the entire cluster class this gate exists to
	// protect. The check skipped 100% of the time on managed, printed a reassuring
	// line explaining why that was fine, and left the CA chain unproven on every
	// cluster we actually run.
	//
	// The question is "is there a Harbor whose writes must be encrypted", so the
	// guard is the harbor namespace. The credential is a detail, and there is one
	// available wherever Harbor is (see harborProbeCreds).
	present, err := harborauth.NamespaceExists(HarborNS)
	if err != nil {
		return []Finding{{
			Check: "harbor-push", Problem: "could not determine whether the harbor namespace exists: " + err.Error(),
			Fix: "this gate cannot decide whether to run the CA-chain check without it",
		}}
	}
	if !present {
		fmt.Printf("  harbor-push: namespace %s is absent — no Harbor on this cluster, so there is no "+
			"push path to prove the CA chain with\n", HarborNS)
		return nil
	}
	if harborBucket == "" {
		return []Finding{{
			Check:   "harbor-push",
			Problem: "the harbor namespace exists but --harbor-bucket was not given, so the CA-chain check did not run",
			Fix: "pass --harbor-bucket. Every other check passes with a broken CA: the pod check only proves the " +
				"mutation LANDED, and the object check samples Loki, which reaches the proxy with no CA at all",
		}}
	}
	creds, findings := harborProbeCreds(d)
	if len(findings) > 0 {
		return findings
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return []Finding{{
			Check: "harbor-push", Problem: "could not generate probe bytes: " + err.Error(), Fix: "retry",
		}}
	}
	return checkHarborBlobIsEncrypted(d, endpoint, harborBucket, creds, nonce)
}

// harborProbeCreds returns a credential that can push one blob into Harbor.
//
// PREFERS THE LLZ ROBOT where LLZ's cert-automation is deployed, and keeps the old
// strictness there: if that namespace exists, a missing or malformed robot Secret is
// still a finding rather than a quiet downgrade, because on those clusters it is a
// real break in a path assert-harbor-roundtrip owns.
//
// FALLS BACK TO HARBOR'S ADMIN only when that namespace is absent — which on managed
// is always, since cert-automation there is apl-core's. Admin is a heavier credential
// than a scoped robot and would be the wrong default; it is used here because it is
// the one credential that exists wherever Harbor does, and a gate that cannot
// authenticate is a gate that proves nothing. It is the same credential the
// in-cluster harbor provisioner already runs on, and this reads it with the cluster
// access the gate already holds — so it grants no reach it did not have.
func harborProbeCreds(d Deps) (harborauth.RobotCreds, []Finding) {
	robotNS, err := harborauth.NamespaceExists(harborauth.RobotSecretNS)
	if err != nil {
		return harborauth.RobotCreds{}, []Finding{{
			Check: "harbor-push", Problem: "could not determine whether " + harborauth.RobotSecretNS + " exists: " + err.Error(),
			Fix: "this gate cannot choose a credential without it",
		}}
	}
	if !robotNS {
		return harborAdminProbeCreds(d)
	}
	raw, err := harborauth.ReadRobotSecret(harborauth.RobotSecretNS, harborauth.RobotSecretName)
	if err != nil || len(raw) == 0 {
		return harborauth.RobotCreds{}, []Finding{{
			Check:   "harbor-push",
			Problem: fmt.Sprintf("could not read the Harbor robot credential %s/%s", harborauth.RobotSecretNS, harborauth.RobotSecretName),
			Fix:     "assert-harbor-roundtrip covers this credential path; fix it there first",
		}}
	}
	creds, err := harborauth.DecodeRobotSecret(raw)
	if err != nil {
		return harborauth.RobotCreds{}, []Finding{{
			Check: "harbor-push", Problem: "the Harbor robot credential is unusable: " + err.Error(),
			Fix: "assert-harbor-roundtrip covers this credential path; fix it there first",
		}}
	}
	fmt.Printf("  harbor-push: pushing as robot %q (from %s/%s)\n", creds.Username, harborauth.RobotSecretNS, harborauth.RobotSecretName)
	return creds, nil
}

// harborAdminSecret is Harbor's own admin credential, created by its Helm release in
// the harbor namespace — present on managed and self-installed alike.
const (
	harborAdminSecretName = "harbor-admin-password"
	harborAdminSecretKey  = "HARBOR_ADMIN_PASSWORD"
	harborAdminUser       = "admin"
)

func harborAdminProbeCreds(d Deps) (harborauth.RobotCreds, []Finding) {
	fail := func(problem, fix string) (harborauth.RobotCreds, []Finding) {
		return harborauth.RobotCreds{}, []Finding{{Check: "harbor-push", Problem: problem, Fix: fix}}
	}
	raw, err := harborauth.ReadRobotSecret(HarborNS, harborAdminSecretName)
	if err != nil || len(raw) == 0 {
		return fail(
			fmt.Sprintf("%s is absent, and Harbor's own %s/%s could not be read either", harborauth.RobotSecretNS, HarborNS, harborAdminSecretName),
			"Harbor's Helm release creates this Secret; if it is missing, Harbor is not installed yet — "+
				"re-run once it is, rather than accepting an unproven CA chain")
	}
	pass, err := d.SecretField(raw, harborAdminSecretKey)
	if err != nil || pass == "" {
		return fail(
			fmt.Sprintf("%s/%s has no usable %s", HarborNS, harborAdminSecretName, harborAdminSecretKey),
			"the CA chain cannot be proven without a credential that can push")
	}
	host, ferr := harborPublicHost(d)
	if ferr != nil {
		return fail("could not discover Harbor's public registry host: "+ferr.Error(),
			"the push must go through the registry's own S3 client to prove anything, so it needs the host "+
				"`docker push` targets. On managed there is no spec domainSuffix to render it from, so it is read "+
				"from Harbor's route")
	}
	fmt.Printf("  harbor-push: %s is absent (cert-automation is apl-core's here) — pushing as %s to %s\n",
		harborauth.RobotSecretNS, harborAdminUser, host)
	return harborauth.RobotCreds{Username: harborAdminUser, Password: pass, RegistryHost: host}, nil
}

// harborPublicHost reads the host `docker push` targets off Harbor's own route.
//
// Not from the spec: on managed there is no domainSuffix, which is the whole reason
// HarborHost("") once rendered the string "harbor." — non-empty, so it satisfied
// every `== ""` guard on the way to 401ing every push (see PR #342). Reading the
// cluster avoids re-deriving a value the cluster already states.
func harborPublicHost(d Deps) (string, error) {
	out, err := d.KubectlOut("-n", HarborNS, "get", "httproute,ingress", "-o", "json")
	if err != nil {
		return "", err
	}
	var view struct {
		Items []struct {
			Spec struct {
				Hostnames []string `json:"hostnames"` // HTTPRoute
				Rules     []struct {
					Host string `json:"host"` // Ingress
				} `json:"rules"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		return "", fmt.Errorf("decoding Harbor's routes: %w", err)
	}
	for _, it := range view.Items {
		for _, h := range it.Spec.Hostnames {
			if harborauth.UsableRegistryHost(h) {
				return h, nil
			}
		}
		for _, r := range it.Spec.Rules {
			if harborauth.UsableRegistryHost(r.Host) {
				return r.Host, nil
			}
		}
	}
	return "", fmt.Errorf("no HTTPRoute or Ingress in the %s namespace declares a usable hostname", HarborNS)
}

// bucketCounts renders "harbor=12, loki=30" for the sample line, so a reader can see
// WHICH bucket supplied the evidence rather than a bare total.
func bucketCounts(d Deps, per map[string]int) string {
	names := make([]string, 0, len(per))
	for b := range per {
		names = append(names, b)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, b := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", b, per[b]))
	}
	return strings.Join(parts, ", ")
}

// objProxyCutoverTime is when object-storage traffic began flowing through the
// gateway: the creation of the CoreDNS rewrite, which is the switch that routes it.
//
// Read from the cluster rather than tracked separately because the ConfigMap IS the
// cutover — it cannot drift from the thing it dates. Deleting and re-adding the
// rewrite moves it forward, which is the correct behaviour: writes during the gap
// went direct and are genuinely plaintext.
var objProxyCutoverTime = func(d Deps) (time.Time, error) {
	out, err := d.KubectlOut("-n", "kube-system", "get", "configmap", objProxyRewriteConfigMap,
		"-o", "jsonpath={.metadata.creationTimestamp}")
	if err != nil {
		return time.Time{}, fmt.Errorf("reading %s: %w", objProxyRewriteConfigMap, err)
	}
	ts, perr := time.Parse(time.RFC3339, strings.TrimSpace(out))
	if perr != nil {
		return time.Time{}, fmt.Errorf("%s has an unparseable creationTimestamp %q", objProxyRewriteConfigMap, out)
	}
	return ts, nil
}

const objProxyRewriteConfigMap = "coredns-custom"

// registryPodView is the slice of a Pod this check needs. Decoded from JSON rather
// than scraped from a jsonpath template, because the template could not attribute a
// value to the container it came from — see CheckRegistryPodsCarryCA.
type registryPodView struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Name string `json:"name"`
				Env  []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"env"`
				VolumeMounts []struct {
					Name string `json:"name"`
				} `json:"volumeMounts"`
			} `json:"containers"`
			Volumes []struct {
				Name string `json:"name"`
			} `json:"volumes"`
		} `json:"spec"`
	} `json:"items"`
}

// objEncCAContainers are the containers that must carry the CA. BOTH speak S3:
// registryctl runs garbage collection against the same backend, so a registryctl
// without the CA fails GC while pushes and pulls look perfectly healthy.
var objEncCAContainers = map[string]bool{"registry": true, "registryctl": true}

// CheckRegistryPodsCarryCA reads the RUNNING pods, not the policy.
//
// EVERY registry pod must carry it, not merely one: pods are admitted
// independently, so a fleet where one pod missed the mutation is a fleet where some
// fraction of writes fail (or, before the rewrite is live, go direct in plaintext).
// Reporting "at least one is fine" would hide exactly the intermittent case the
// Ignore webhook produces.
//
// AND EVERY CONTAINER, which the first revision could not see. It asked kubectl for
// `.spec.containers[*].env[?(@.name=='SSL_CERT_DIR')].value` and tested the joined
// result with strings.Contains — so a pod where `registry` was mutated and
// `registryctl` was not passed cleanly, because the one value that came back
// contained the mount path. jsonpath cannot say WHICH container a value came from,
// which is the whole question here, so this decodes the objects instead.
func CheckRegistryPodsCarryCA(d Deps) []Finding {
	out, err := d.KubectlOut("-n", HarborNS, "get", "pods", "-o", "json")
	if err != nil {
		return []Finding{{
			Check:   "pod",
			Problem: "could not list pods in the harbor namespace",
			Fix:     "check cluster access and RBAC; this gate cannot pass without reading the running registry pods",
		}}
	}
	var view registryPodView
	if jerr := json.Unmarshal([]byte(out), &view); jerr != nil {
		return []Finding{{
			Check:   "pod",
			Problem: "could not decode the harbor pod list: " + jerr.Error(),
			Fix:     "a gate that cannot read the pods must not report color.Green",
		}}
	}

	var findings []Finding
	seen := 0
	for _, pod := range view.Items {
		name := pod.Metadata.Name
		if !strings.HasPrefix(name, HarborRegistryLabel) {
			continue
		}
		seen++
		hasVolume := false
		for _, v := range pod.Spec.Volumes {
			if v.Name == ObjProxyCAVolume {
				hasVolume = true
			}
		}
		if !hasVolume {
			findings = append(findings, Finding{
				Check:   "pod",
				Problem: fmt.Sprintf("%s has no %s volume — any SSL_CERT_DIR on it points at an empty directory", name, ObjProxyCAVolume),
				Fix:     "confirm the obj-proxy-ca Secret exists IN THE HARBOR NAMESPACE (a Pod can only mount Secrets from its own namespace), then `kubectl -n harbor rollout restart deploy/harbor-registry`",
			})
		}
		for _, c := range pod.Spec.Containers {
			if !objEncCAContainers[c.Name] {
				continue // istio-proxy and friends have no business reaching S3
			}
			certDir := ""
			for _, e := range c.Env {
				if e.Name == SsecCertDirEnv {
					certDir = e.Value
				}
			}
			mounted := false
			for _, m := range c.VolumeMounts {
				if m.Name == ObjProxyCAVolume {
					mounted = true
				}
			}
			if !strings.Contains(certDir, ObjProxyCAMount) || !mounted {
				findings = append(findings, Finding{
					Check: "pod",
					Problem: fmt.Sprintf("%s container %q does not trust the obj-proxy CA (%s=%q, ca volume mounted=%v)",
						name, c.Name, SsecCertDirEnv, certDir, mounted),
					Fix: "this container was admitted without the Kyverno mutation (the mutate webhook is " +
						"failurePolicy: Ignore, so a pod admitted while Kyverno was down silently has neither). " +
						"Confirm ClusterPolicy harbor-obj-proxy-ca exists, then " +
						"`kubectl -n harbor rollout restart deploy/harbor-registry` and re-run. " +
						"registryctl matters as much as registry: it runs GC against the same S3 backend",
				})
			}
		}
	}
	if seen == 0 {
		findings = append(findings, Finding{
			Check:   "pod",
			Problem: "no harbor-registry pods found",
			Fix:     "a gate that examined nothing must not report color.Green — check the harbor namespace is deployed",
		})
	}
	return findings
}

// checkEndpointResolvesToProxy confirms the CoreDNS rewrite is in force.
//
// This is the check for the failure that looks most like success: without the
// rewrite every component is Healthy, the proxy is Ready, the policy is applied —
// and every byte goes straight to Linode in the clear, because nothing is routing
// through the gateway.
func checkEndpointResolvesToProxy(d Deps, endpoint string) []Finding {
	out, err := d.KubectlOut("-n", "kube-system", "get", "configmap", "coredns-custom",
		"-o", "jsonpath={.data}")
	// AN UNREADABLE ConfigMap IS NOT A MISSING REWRITE — and A MISSING ONE IS NOT
	// UNREADABLE. Folding `err != nil` into the absent branch reported an RBAC
	// denial or an apiserver blip as "object-storage traffic is going DIRECT to
	// Linode, unencrypted": a specific, alarming claim about live traffic made on
	// the strength of not having looked, whose remedy is to apply the rewrite —
	// at best a no-op on a cluster that already has one, at worst the "flip it
	// before the registry trusts the CA" ordering the Fix text itself warns about.
	//
	// But treating EVERY error as unreadable swallowed the case this check exists
	// for. kube-system/coredns-custom is an OPTIONAL CoreDNS extension point, and
	// coredns-rewrite.yaml records that no LLZ-managed cluster ships one: on a
	// cluster that never applied objProxy the get exits 1 with NotFound, and the
	// one cluster genuinely writing plaintext was the one told "UNKNOWN — do NOT
	// apply the rewrite". ClassifyErr is the seam that already knows the
	// difference, and using it here is what keeps the two answers apart.
	if err != nil && kubectlprobe.ClassifyErr(err) != kubectlprobe.Absent {
		return []Finding{{
			Check: "dns",
			Problem: fmt.Sprintf("could not read kube-system/coredns-custom (%v) — whether %s is rewritten "+
				"to the obj-proxy is UNKNOWN. This is not evidence that traffic is unencrypted, and not "+
				"evidence that it is encrypted either", err, endpoint),
			Fix: "check RBAC on kube-system/configmaps and that the apiserver is reachable, then re-run. " +
				"Do NOT apply the rewrite on the strength of this finding — see the ordering warning on the " +
				"absent-rewrite case",
		}}
	}
	// err != nil past the guard above means a classified ABSENCE: the ConfigMap is
	// genuinely not there, `out` is empty, and that falls through to the
	// missing-rewrite verdict below exactly as an empty ConfigMap would.
	if !strings.Contains(out, endpoint) {
		return []Finding{{
			Check: "dns",
			Problem: fmt.Sprintf("the CoreDNS rewrite for %s is not present in kube-system/coredns-custom — "+
				"object-storage traffic is going DIRECT to Linode, unencrypted, while every component reports Healthy", endpoint),
			Fix: "apply platform-apl/components/objProxy/obj-proxy/coredns-rewrite.yaml with the endpoint " +
				"host filled in. Do this LAST, after the pod check above passes: flipping it before the registry " +
				"trusts the CA turns an unencrypted-but-working registry into a broken one",
		}}
	}
	if !strings.Contains(out, objProxyService) {
		return []Finding{{
			Check:   "dns",
			Problem: fmt.Sprintf("kube-system/coredns-custom mentions %s but does not rewrite it to %s", endpoint, objProxyService),
			Fix:     "the rewrite target must be the obj-proxy Service; check the generated rewrite line",
		}}
	}
	return nil
}

// checkObjectsAreEncrypted is CHECK 3 — the only one that observes the outcome
// rather than the configuration.
//
// An EMPTY bucket is a failure, not a pass. "Nothing has been written yet" and
// "everything written is encrypted" produce the same color.Green from a checker that
// shrugs at zero objects, and the first is the state every misconfigured cluster
// passes through on its way to writing plaintext.
//
// SAMPLED, and it says so. Any plaintext in the sample is proof of failure; a clean
// sample is evidence of success, not proof of it. The success line prints the
// sample size for exactly that reason — "0 of 50 sampled objects were plaintext" is
// an auditable claim, "encrypted" is not.
// checkObjectsAreEncrypted samples REAL objects and fails on any plaintext among
// the ones written since the gateway went live.
//
// EVERY BUCKET, not just Loki's, because a bucket only proves something if it has
// data from AFTER the cutover — and pinning this to Loki made the check unsatisfiable
// on the e2e timeline. Loki does not flush its first chunk until chunk_idle_period,
// so on a cluster the gateway went live on nine minutes earlier the bucket holds
// nothing but a previous cluster's objects, and the check reported "nothing has been
// written since the gateway went live" on a fleet that was encrypting correctly.
// That is the same failure as the pre-cutover sampling this replaced: a check whose
// precondition the timeline cannot meet.
//
// Harbor's bucket always has fresh material, because checkHarborPath pushes a blob
// into it moments before this runs. A check that can CAUSE the data it judges never
// waits on a background consumer — the same reason the harbor push exists at all.
// Loki's bucket is still sampled, so its writes are covered whenever it has made
// any; "has Loki written at all" belongs to assert-loki, which owns Loki.
func checkObjectsAreEncrypted(d Deps, endpoint string, buckets []string, sample int) []Finding {
	// The CONSUMER's own credentials, read from the Secret it mounts — not the
	// ambient AWS_* env.
	//
	// In the assert-suite those env vars are the TERRAFORM STATE bucket's key, which
	// has no access to the data buckets: the first revision failed with
	// "403 AccessDenied listing platform-loki-chunks-e2e" and reported it as an
	// encryption finding. A gate that reads whatever credentials happen to be in
	// scope is a gate that reports its own misconfiguration as a breach.
	// assert-obj-roundtrip already reads each consumer's own key for exactly this
	// reason; this uses the same Secret.
	ak, sk, cerr := ObjEncConsumerCreds(d, LokiObjSecretRef, "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY")
	if cerr != nil {
		return []Finding{{
			Check:   "object",
			Problem: "could not read the Loki object-store credentials from " + LokiObjSecretRef + ": " + cerr.Error(),
			Fix: "this is the same Secret assert-obj-roundtrip reads; if that lane is also failing, fix it " +
				"there first. The probe needs only LIST and HEAD on the bucket, and NOT the SSE-C key",
		}}
	}
	var keys []objstore.ObjectRef
	perBucket := map[string]int{}
	for _, b := range buckets {
		if b == "" {
			continue
		}
		got, err := objstore.SampleObjectKeys(ak, sk, endpoint, b, sample)
		if err != nil {
			return []Finding{{
				Check:   "object",
				Problem: fmt.Sprintf("could not list %s: %v", b, err),
				Fix:     "check the key is scoped to this bucket and the endpoint host is the one the bucket was created against",
			}}
		}
		for _, r := range got {
			r.Bucket = b
			keys = append(keys, r)
		}
		perBucket[b] = len(got)
	}
	if len(keys) == 0 {
		return []Finding{{
			Check:   "object",
			Problem: strings.Join(buckets, " and ") + " are EMPTY, so nothing proves writes are encrypted",
			Fix: "write something through the real path (push an image / let Loki flush a chunk) and re-run. " +
				"An empty bucket and a correctly-encrypted bucket look identical to every check except this one",
		}}
	}

	// JUDGE ONLY WHAT WAS WRITTEN SINCE THE GATEWAY WENT LIVE.
	//
	// Objects written before the cutover are plaintext and stay plaintext — that is
	// a documented, irreversible property of turning this on over an existing
	// bucket, not a finding. Counting them made this check UNPASSABLE on any bucket
	// with history, which for e2e is every bucket, since they are reused across
	// clusters. It reported "20 of 20 sampled objects are stored in PLAINTEXT" on a
	// cluster where the proxy had encrypted every write it received, and the fix
	// text underneath explained why that might be fine — a gate whose own failure
	// message argues that the failure may not be real is one people learn to skip.
	cutover, cerr := objProxyCutoverTime(d)
	if cerr != nil {
		return []Finding{{
			Check:   "object",
			Problem: "could not determine when the gateway went live: " + cerr.Error(),
			Fix: "without it, objects that PREDATE the cutover cannot be told from writes that bypassed the " +
				"proxy — and reporting the first as the second is how this check cried wolf",
		}}
	}
	var fresh []objstore.ObjectRef
	for _, k := range keys {
		if k.LastModified.After(cutover) {
			fresh = append(fresh, k)
		}
	}
	if len(fresh) == 0 {
		return []Finding{{
			Check: "object",
			Problem: fmt.Sprintf("nothing has been written to %s since the gateway went live at %s "+
				"(newest of %d sampled: %s), so this proves nothing about encryption",
				strings.Join(buckets, " or "), cutover.Format(time.RFC3339), len(keys), keys[0].LastModified.Format(time.RFC3339)),
			Fix: "a check that examined no relevant object must not report color.Green. The Harbor push above " +
				"normally guarantees at least one post-cutover object, so if THAT skipped or failed this is " +
				"downstream of it. Otherwise no consumer has written since the cutover — check their logs " +
				"for S3 errors before suspecting the proxy",
		}}
	}

	var plaintext, unknown []string
	for _, k := range fresh {
		switch verdict, _ := ObjectSSECProbe(ak, sk, endpoint, k.Bucket, k.Key); verdict {
		case ssecPlaintext:
			plaintext = append(plaintext, k.Bucket+"/"+k.Key)
		case ssecEncrypted, ssecAbsent:
			// absent: raced a compaction/delete between LIST and HEAD. Not evidence
			// of plaintext, and not worth failing a gate over.
		default:
			unknown = append(unknown, k.Bucket+"/"+k.Key)
		}
	}
	fmt.Printf("  object: %d of %d sampled objects (%s) were written since the cutover at %s; classifying those\n",
		len(fresh), len(keys), bucketCounts(d, perBucket), cutover.Format(time.RFC3339))

	var out []Finding
	if len(plaintext) > 0 {
		out = append(out, Finding{
			Check: "object",
			Problem: fmt.Sprintf("%d of %d sampled objects in %s are stored in PLAINTEXT (e.g. %s)",
				len(plaintext), len(fresh), strings.Join(buckets, "+"), plaintext[0]),
			Fix: "these were written AFTER the gateway went live, so they are not cutover residue — the " +
				"traffic is bypassing obj-proxy. Check the CoreDNS rewrite resolves the endpoint to the " +
				"proxy Service and that llz_objproxy_ssec_injected_total is climbing",
		})
	}
	if len(unknown) > 0 {
		out = append(out, Finding{
			Check:   "object",
			Problem: fmt.Sprintf("%d of %d sampled objects could not be classified (e.g. %s)", len(unknown), len(fresh), unknown[0]),
			Fix:     "an unclassifiable response is not a pass; check the key's permissions on this bucket",
		})
	}
	if len(out) == 0 {
		// len(fresh), not len(keys): the number that means anything is how many
		// objects were actually JUDGED. Reporting the whole sample here overstated
		// the coverage — "0 of 12" when only 4 were post-cutover and examined.
		fmt.Printf("  object: 0 of %d judged objects in %s were plaintext (a clean sample is evidence, not proof of full coverage)\n",
			len(fresh), strings.Join(buckets, "+"))
	}
	return out
}
