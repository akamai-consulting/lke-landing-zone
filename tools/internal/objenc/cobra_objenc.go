package objenc

// cobra_objenc.go — the object-storage encryption verbs, reduced to flag sets.
//
// Everything they do is `obj-encryption` and lives in tools/internal/objenc. What
// stays here is ObjencDeps(): the OpenBao KV pair that IS the custody, a kubectl
// read, a Secret decoder, and the log mask.
//
// THE KV PAIR IS THE POINT. Linode Object Storage implements exactly one
// server-side encryption mode — SSE-C, where the CLIENT holds the key — so the
// platform mints one, keeps it in OpenBao, and hands it to every writer. That is
// custody in the literal sense, and it is why this extension is the one that
// finally binds `transition:seeded[secret-custody]`.

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/baoseed"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

// ObjencDeps is a VAR, not a func, so a test can swap the whole capability set —
// the same seam pattern package main already uses for its kubectl and Linode
// clients. `llz ci readiness` drives objenc's registry-CA check, and its tests
// used to stub a package-level objEncKubectl that is now a Deps field.
var ObjencDeps = func() Deps {
	return Deps{
		KVGet: func(path, field string) (string, KVVerdict) {
			v, verdict := baoread.KVGetFieldOK(path, field)
			// The three-valued answer is load-bearing: only a definite ABSENT may
			// be read as "not seeded". An unknown treated as absent would mint a
			// SECOND SSE-C key and every object under the first becomes unreadable.
			switch verdict {
			case baoread.Found:
				return v, KVFound
			case baoread.Absent:
				return v, KVAbsent
			default:
				return v, KVUnknown
			}
		},
		KVPut:       func(path string, fields map[string]string) error { return baoread.KVPut(path, fields) },
		KubectlOut:  kubectlprobe.Out,
		SecretField: kube.SecretField,
		MaskGHALines: func(vals ...string) {
			for _, v := range vals {
				baoseed.MaskGHALines(v)
			}
		},
	}
}

func AssertObjEncryptionCmd() *cobra.Command {
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
			return RunAssertEncryption(ObjencDeps(), endpoint, bucket, harborBucket, region, sample)
		},
	}
	c.Flags().StringVar(&endpoint, "endpoint", "", "endpoint host override (default: derived from the env's cluster.objectStorage.cluster)")
	c.Flags().StringVar(&harborBucket, "harbor-bucket", "", "Harbor registry bucket override (default: derived from the spec). Its check is the only one that proves the CA chain")
	c.Flags().StringVar(&region, "region", os.Getenv("REGION"), "deployment whose spec.components.objProxy gates this check")
	c.Flags().IntVar(&sample, "sample", 50, "how many objects to sample for check 3. Any plaintext in the sample fails; a clean sample is evidence, not proof")
	c.Flags().StringVar(&bucket, "bucket", "", "Loki chunks bucket override (default: derived from the spec). Credentials from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY")
	return c
}

func ObjProxyCmd() *cobra.Command {
	var o ProxyOpts
	c := &cobra.Command{
		Use:   "obj-proxy",
		Short: "S3 gateway that adds SSE-C encryption to Linode Object Storage writes",
		Long: "Reverse-proxies S3 traffic to Linode Object Storage, injecting SSE-C\n" +
			"encryption headers that Loki and Harbor cannot emit themselves. Linode\n" +
			"implements no other server-side encryption mode (SSE-S3 is rejected 400;\n" +
			"PutBucketEncryption is 501), so this is what makes those buckets encrypted\n" +
			"at rest — with a key Linode never stores.\n\n" +
			"Bodies are streamed and never inspected; the proxy performs no cryptography\n" +
			"of its own and does not re-sign requests (SSE-C headers are honoured outside\n" +
			"SigV4 SignedHeaders, which is why callers must reach this at the real\n" +
			"endpoint hostname via DNS rather than at a Service name).\n\n" +
			"KEY LOSS IS DATA LOSS: Linode discards SSE-C keys on receipt, so an object\n" +
			"written under a lost key is unrecoverable by anyone. Escrow --key-file with\n" +
			"the same discipline as the OpenBao seal key.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return RunProxy(cmd.Context(), o) },
	}
	f := c.Flags()
	f.StringVar(&o.Listen, "listen", ":8443", "HTTPS listen address for S3 clients")
	f.StringVar(&o.Upstream, "upstream", "", "real Object Storage endpoint host, e.g. us-ord-10.linodeobjects.com (required)")
	f.StringVar(&o.CredsFile, "creds-file", "",
		"optional file holding the object-storage AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY this proxy may "+
			"re-sign with. Enables the #397 repair: requests sent in the aws-chunked trailer-checksum framing "+
			"Linode rejects are de-chunked and re-signed AS THE SAME ACCESS KEY. Without it the proxy never "+
			"re-signs and such requests fail upstream exactly as they do today")
	f.StringVar(&o.KeyFile, "key-file", "", "file holding the 32-byte raw SSE-C key (required)")
	f.StringVar(&o.TlsCert, "tls-cert", "", "serving certificate for the endpoint hostname (required)")
	f.StringVar(&o.TlsKey, "tls-key", "", "serving private key (required)")
	f.StringVar(&o.HealthAddr, "health-addr", ":8080", "plaintext /healthz + /stats address for kubelet probes")
	f.StringVar(&o.UpstreamCAs, "upstream-ca-file", "", "optional CA bundle for verifying the upstream (default: system roots)")
	return c
}

func SeedSSECKeyCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "seed-ssec-key",
		Short: "generate and seed the obj-proxy SSE-C key when the objProxy component is enabled",
		Long: "Bootstrap seed for the object-storage encryption key.\n\n" +
			"No-ops unless spec.components.objProxy is enabled for --region. When enabled,\n" +
			"generates a 32-byte AES-256 key and writes it to " + SSECKVPath + " (key=),\n" +
			"where the proxy's ExternalSecret reads it.\n\n" +
			"STRICTLY ADDITIVE. An existing key is never overwritten: Linode discards\n" +
			"SSE-C keys on receipt and they are per-object, so replacing this value does\n" +
			"not rotate anything — it orphans every object already written, unreadably and\n" +
			"silently. Re-runs are therefore safe and do nothing.\n\n" +
			"KEY LOSS IS DATA LOSS. Back the printed key up offline on first generation,\n" +
			"with the same discipline as OPENBAO_SEAL_KEY.\n" +
			"Env: OPENBAO_* (root-token bootstrap posture).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return RunSeedSSECKey(ObjencDeps(), region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) whose objProxy toggle gates the seed (required)")
	return c
}
