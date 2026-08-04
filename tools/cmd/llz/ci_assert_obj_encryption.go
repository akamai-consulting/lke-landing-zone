package main

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
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// objEncryptionNS/Selector locate the registry pods the CA must reach.
const (
	harborNS            = "harbor"
	harborRegistryLabel = "harbor-registry"
	ssecCertDirEnv      = "SSL_CERT_DIR"
	objProxyCAMount     = "/etc/llz/ca"
	objProxyCAVolume    = "llz-obj-proxy-ca"
	objProxyService     = "obj-proxy.obj-proxy.svc.cluster.local"
)

// objEncKubectl is the read seam. The checks below are the whole value of this
// gate, and a check that can only run against a live cluster is a check that is
// never exercised until the night it matters.
var objEncKubectl = kubectlOut

type objEncryptionFinding struct {
	check   string
	problem string
	fix     string
}

func ciAssertObjEncryptionCmd() *cobra.Command {
	var endpoint, bucket, harborBucket, region string
	var sample int
	c := &cobra.Command{
		Use:   "assert-obj-encryption",
		Short: "fail unless objects written to Object Storage are actually encrypted",
		Long: "Runtime gate on the SSE-C object-storage gateway (docs/designs/obj-sse-c-gateway.md).\n\n" +
			"Checks three things that a manifest diff cannot see:\n" +
			"  1. harbor-registry pods carry the CA mount + SSL_CERT_DIR (checked on the\n" +
			"     RUNNING pod — the Kyverno webhook is failurePolicy: Ignore, so a pod\n" +
			"     admitted while Kyverno was down silently has neither)\n" +
			"  2. the endpoint hostname resolves to the obj-proxy Service (i.e. the\n" +
			"     CoreDNS rewrite is actually in force)\n" +
			"  3. a live object reports SSE-C on HEAD\n\n" +
			"Check 3 is the one that observes the OUTCOME. Checks 1 and 2 are both true\n" +
			"the instant before the first write, so a gate without 3 passes on a cluster\n" +
			"writing pure plaintext.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runAssertObjEncryption(endpoint, bucket, harborBucket, region, sample)
		},
	}
	c.Flags().StringVar(&endpoint, "endpoint", "", "endpoint host override (default: derived from the env's cluster.objectStorage.cluster)")
	c.Flags().StringVar(&harborBucket, "harbor-bucket", "", "Harbor registry bucket override (default: derived from the spec). Its check is the only one that proves the CA chain")
	c.Flags().StringVar(&region, "region", os.Getenv("REGION"), "deployment whose spec.components.objProxy gates this check")
	c.Flags().IntVar(&sample, "sample", 50, "how many objects to sample for check 3. Any plaintext in the sample fails; a clean sample is evidence, not proof")
	c.Flags().StringVar(&bucket, "bucket", "", "Loki chunks bucket override (default: derived from the spec). Credentials from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY")
	return c
}

func runAssertObjEncryption(endpoint, bucket, harborBucket, region string, sample int) error {
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
	// has no encrypted objects to find, and failing it would red every cluster for a
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
			bucket = clusterspec.ObjLokiChunksBucket(region)
		}
		if harborBucket == "" {
			harborBucket = clusterspec.ObjHarborRegistryBucket(region)
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

	var findings []objEncryptionFinding
	findings = append(findings, checkRegistryPodsCarryCA()...)
	findings = append(findings, checkEndpointResolvesToProxy(endpoint)...)
	findings = append(findings, checkObjectsAreEncrypted(endpoint, bucket, sample)...)
	findings = append(findings, checkHarborPath(endpoint, harborBucket)...)

	if len(findings) == 0 {
		fmt.Printf("assert-obj-encryption: registry pods carry the CA, the endpoint resolves to the proxy, "+
			"and a live object in %s is encrypted.\n", bucket)
		return nil
	}
	for _, f := range findings {
		fmt.Printf("::error::[%s] %s\n  fix: %s\n", f.check, f.problem, f.fix)
	}
	return fmt.Errorf("assert-obj-encryption: %d check(s) failed — objects may be landing in PLAINTEXT", len(findings))
}

// checkHarborPath runs the push check when Harbor is on this cluster.
//
// An ABSENT harbor namespace is not a finding: managed App Platform renders a
// minimal app set and simply may not include it, and failing there would red a
// cluster for a component it never ran. An absent --harbor-bucket while the
// namespace IS present is a finding, because that is the check being skipped on a
// cluster that needs it — and it is the ONLY check that proves the CA chain.
func checkHarborPath(endpoint, harborBucket string) []objEncryptionFinding {
	present, err := namespaceExists(harborNS)
	if err != nil {
		return []objEncryptionFinding{{
			check: "harbor-push", problem: "could not determine whether the harbor namespace exists: " + err.Error(),
			fix: "this gate cannot decide whether to run the CA-chain check without it",
		}}
	}
	if !present {
		fmt.Println("  harbor-push: the harbor namespace is absent — Harbor is not deployed here, so the CA chain has nothing to prove")
		return nil
	}
	if harborBucket == "" {
		return []objEncryptionFinding{{
			check:   "harbor-push",
			problem: "the harbor namespace exists but --harbor-bucket was not given, so the CA-chain check did not run",
			fix: "pass --harbor-bucket. Every other check passes with a broken CA: the pod check only proves the " +
				"mutation LANDED, and the object check samples Loki, which reaches the proxy with no CA at all",
		}}
	}
	raw, err := readHarborRobotSecret(harborRobotSecretNS, harborRobotSecretName)
	if err != nil || len(raw) == 0 {
		return []objEncryptionFinding{{
			check:   "harbor-push",
			problem: fmt.Sprintf("could not read the Harbor robot credential %s/%s", harborRobotSecretNS, harborRobotSecretName),
			fix:     "assert-harbor-roundtrip covers this credential path; fix it there first",
		}}
	}
	creds, err := decodeRobotSecret(raw)
	if err != nil {
		return []objEncryptionFinding{{
			check: "harbor-push", problem: "the Harbor robot credential is unusable: " + err.Error(),
			fix: "assert-harbor-roundtrip covers this credential path; fix it there first",
		}}
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return []objEncryptionFinding{{
			check: "harbor-push", problem: "could not generate probe bytes: " + err.Error(), fix: "retry",
		}}
	}
	return checkHarborBlobIsEncrypted(endpoint, harborBucket, creds, nonce)
}

// registryPodView is the slice of a Pod this check needs. Decoded from JSON rather
// than scraped from a jsonpath template, because the template could not attribute a
// value to the container it came from — see checkRegistryPodsCarryCA.
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

// checkRegistryPodsCarryCA reads the RUNNING pods, not the policy.
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
func checkRegistryPodsCarryCA() []objEncryptionFinding {
	out, err := objEncKubectl("-n", harborNS, "get", "pods", "-o", "json")
	if err != nil {
		return []objEncryptionFinding{{
			check:   "pod",
			problem: "could not list pods in the harbor namespace",
			fix:     "check cluster access and RBAC; this gate cannot pass without reading the running registry pods",
		}}
	}
	var view registryPodView
	if jerr := json.Unmarshal([]byte(out), &view); jerr != nil {
		return []objEncryptionFinding{{
			check:   "pod",
			problem: "could not decode the harbor pod list: " + jerr.Error(),
			fix:     "a gate that cannot read the pods must not report green",
		}}
	}

	var findings []objEncryptionFinding
	seen := 0
	for _, pod := range view.Items {
		name := pod.Metadata.Name
		if !strings.HasPrefix(name, harborRegistryLabel) {
			continue
		}
		seen++
		hasVolume := false
		for _, v := range pod.Spec.Volumes {
			if v.Name == objProxyCAVolume {
				hasVolume = true
			}
		}
		if !hasVolume {
			findings = append(findings, objEncryptionFinding{
				check:   "pod",
				problem: fmt.Sprintf("%s has no %s volume — any SSL_CERT_DIR on it points at an empty directory", name, objProxyCAVolume),
				fix:     "confirm the obj-proxy-ca Secret exists IN THE HARBOR NAMESPACE (a Pod can only mount Secrets from its own namespace), then `kubectl -n harbor rollout restart deploy/harbor-registry`",
			})
		}
		for _, c := range pod.Spec.Containers {
			if !objEncCAContainers[c.Name] {
				continue // istio-proxy and friends have no business reaching S3
			}
			certDir := ""
			for _, e := range c.Env {
				if e.Name == ssecCertDirEnv {
					certDir = e.Value
				}
			}
			mounted := false
			for _, m := range c.VolumeMounts {
				if m.Name == objProxyCAVolume {
					mounted = true
				}
			}
			if !strings.Contains(certDir, objProxyCAMount) || !mounted {
				findings = append(findings, objEncryptionFinding{
					check: "pod",
					problem: fmt.Sprintf("%s container %q does not trust the obj-proxy CA (%s=%q, ca volume mounted=%v)",
						name, c.Name, ssecCertDirEnv, certDir, mounted),
					fix: "this container was admitted without the Kyverno mutation (the mutate webhook is " +
						"failurePolicy: Ignore, so a pod admitted while Kyverno was down silently has neither). " +
						"Confirm ClusterPolicy harbor-obj-proxy-ca exists, then " +
						"`kubectl -n harbor rollout restart deploy/harbor-registry` and re-run. " +
						"registryctl matters as much as registry: it runs GC against the same S3 backend",
				})
			}
		}
	}
	if seen == 0 {
		findings = append(findings, objEncryptionFinding{
			check:   "pod",
			problem: "no harbor-registry pods found",
			fix:     "a gate that examined nothing must not report green — check the harbor namespace is deployed",
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
func checkEndpointResolvesToProxy(endpoint string) []objEncryptionFinding {
	out, err := objEncKubectl("-n", "kube-system", "get", "configmap", "coredns-custom",
		"-o", "jsonpath={.data}")
	if err != nil || !strings.Contains(out, endpoint) {
		return []objEncryptionFinding{{
			check: "dns",
			problem: fmt.Sprintf("the CoreDNS rewrite for %s is not present in kube-system/coredns-custom — "+
				"object-storage traffic is going DIRECT to Linode, unencrypted, while every component reports Healthy", endpoint),
			fix: "apply platform-apl/components/objProxy/obj-proxy/coredns-rewrite.yaml with the endpoint " +
				"host filled in. Do this LAST, after the pod check above passes: flipping it before the registry " +
				"trusts the CA turns an unencrypted-but-working registry into a broken one",
		}}
	}
	if !strings.Contains(out, objProxyService) {
		return []objEncryptionFinding{{
			check:   "dns",
			problem: fmt.Sprintf("kube-system/coredns-custom mentions %s but does not rewrite it to %s", endpoint, objProxyService),
			fix:     "the rewrite target must be the obj-proxy Service; check the generated rewrite line",
		}}
	}
	return nil
}

// checkObjectsAreEncrypted is CHECK 3 — the only one that observes the outcome
// rather than the configuration.
//
// An EMPTY bucket is a failure, not a pass. "Nothing has been written yet" and
// "everything written is encrypted" produce the same green from a checker that
// shrugs at zero objects, and the first is the state every misconfigured cluster
// passes through on its way to writing plaintext.
//
// SAMPLED, and it says so. Any plaintext in the sample is proof of failure; a clean
// sample is evidence of success, not proof of it. The success line prints the
// sample size for exactly that reason — "0 of 50 sampled objects were plaintext" is
// an auditable claim, "encrypted" is not.
func checkObjectsAreEncrypted(endpoint, bucket string, sample int) []objEncryptionFinding {
	ak, sk := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
	if ak == "" || sk == "" {
		return []objEncryptionFinding{{
			check:   "object",
			problem: "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY are not set, so the outcome check cannot run",
			fix:     "supply a key scoped to " + bucket + "; the probe needs only LIST and HEAD, and NOT the SSE-C key",
		}}
	}
	keys, err := s3SampleObjectKeys(ak, sk, endpoint, bucket, sample)
	if err != nil {
		return []objEncryptionFinding{{
			check:   "object",
			problem: fmt.Sprintf("could not list %s: %v", bucket, err),
			fix:     "check the key is scoped to this bucket and the endpoint host is the one the bucket was created against",
		}}
	}
	if len(keys) == 0 {
		return []objEncryptionFinding{{
			check:   "object",
			problem: bucket + " is EMPTY, so nothing proves writes are encrypted",
			fix: "write something through the real path (push an image / let Loki flush a chunk) and re-run. " +
				"An empty bucket and a correctly-encrypted bucket look identical to every check except this one",
		}}
	}

	var plaintext, unknown []string
	for _, k := range keys {
		switch verdict, _ := s3ObjectSSECProbe(ak, sk, endpoint, bucket, k); verdict {
		case ssecPlaintext:
			plaintext = append(plaintext, k)
		case ssecEncrypted, ssecAbsent:
			// absent: raced a compaction/delete between LIST and HEAD. Not evidence
			// of plaintext, and not worth failing a gate over.
		default:
			unknown = append(unknown, k)
		}
	}

	var out []objEncryptionFinding
	if len(plaintext) > 0 {
		out = append(out, objEncryptionFinding{
			check: "object",
			problem: fmt.Sprintf("%d of %d sampled objects in %s are stored in PLAINTEXT (e.g. %s)",
				len(plaintext), len(keys), bucket, plaintext[0]),
			fix: "if this is a FRESH bucket, traffic is bypassing obj-proxy — check the CoreDNS rewrite is " +
				"live and llz_objproxy_ssec_injected_total is climbing. If the proxy was turned on over an " +
				"EXISTING bucket, these predate it: objects written before the cutover stay plaintext " +
				"forever. For Loki that resolves itself once a full retention period has passed; for a " +
				"registry it needs a re-push or a rewrite",
		})
	}
	if len(unknown) > 0 {
		out = append(out, objEncryptionFinding{
			check:   "object",
			problem: fmt.Sprintf("%d of %d sampled objects could not be classified (e.g. %s)", len(unknown), len(keys), unknown[0]),
			fix:     "an unclassifiable response is not a pass; check the key's permissions on this bucket",
		})
	}
	if len(out) == 0 {
		fmt.Printf("  object: 0 of %d sampled objects in %s were plaintext (a clean sample is evidence, not proof of full coverage)\n",
			len(keys), bucket)
	}
	return out
}
