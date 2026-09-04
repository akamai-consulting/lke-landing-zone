package assertobjstore

// ci_assert_obj_roundtrip.go implements `llz ci assert-obj-roundtrip` — the gate
// that Loki and Harbor can actually WRITE to their object storage.
//
// WHAT WAS ALREADY CHECKED, AND WHY IT WAS NOT ENOUGH. `llz ci
// verify-object-storage` lists the account's buckets through the LINODE API and
// confirms the region's four exist by label. Every one of those checks passed
// while Loki and Harbor were both returning NoSuchBucket in production.
//
// The reason is that they were not looking at the same thing. Linode's
// object-storage generations are DISJOINT NAMESPACES on different hosts. An
// obj-cluster id like `us-ord-10` stripped to its region `us-ord` puts the bucket
// on gen-1 (`us-ord-1`), while the consumers address the full host (gen-2). The
// bucket genuinely exists — the API says so, truthfully — and the consumer
// genuinely 404s. Two views that agree with themselves and not with each other,
// which is the same shape as the audit-log pipeline whose NetworkPolicy pointed
// at the same empty namespace as its push URL.
//
// The only check that can tell them apart is one that speaks S3 at the endpoint
// the CONSUMER uses, with the credential the CONSUMER holds. So this gate:
//
//   1. reads each consumer's OWN credential Secret — the one its pods mount, not
//      one minted for the occasion;
//   2. reads each consumer's OWN endpoint and bucket from its live config;
//   3. PUTs a small object, GETs it back and compares the bytes, then deletes it.
//
// READING BACK MATTERS. A PUT can succeed against the wrong endpoint; fetching
// the exact bytes through the same endpoint is what proves the write landed where
// the consumer will look.
//
// IT REFUSES TO GUESS AN ENDPOINT. If a consumer's endpoint cannot be read from
// its config, the gate FAILS rather than falling back to one derived from the
// Linode API or the spec — because that derived endpoint is precisely the view
// that was already wrong. A gate that silently substitutes it would reproduce the
// bug and pass.
//
// WRITE, not read. Loki writes chunks continuously and Harbor writes on every
// push; a read-only probe would pass on a bucket that has gone read-only under a
// changed key policy, which is a full outage for both.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/objstore"
)

// objConsumer describes one writer of object storage and where its real
// configuration lives.
type objConsumer struct {
	Name string
	// Secret holding the credential the consumer's pods mount, as namespace/name.
	SecretRef string
	// AccessKeyField / SecretKeyField are the keys inside it. They differ per
	// consumer because each is shaped for the upstream chart's env contract.
	AccessKeyField string
	SecretKeyField string
	// ConfigRefs are candidate objects carrying the consumer's rendered endpoint +
	// bucket, as <kind>/<namespace>/<name>, tried in order.
	//
	// A LIST, not one name, because upstream charts move their rendered config
	// between a Secret and a ConfigMap across versions and the exact object name
	// is a chart detail this repo does not own. A single guessed name would make
	// the gate fail on a perfectly healthy cluster the first time a chart renamed
	// it — and the failure would look like "Loki cannot write", which is the worst
	// possible wrong answer. Every candidate tried is named in the failure.
	ConfigRefs []string
	// Component is the optional apl-core app this consumer belongs to, as it is
	// named in `spec.cluster.bootstrap.managedApps` and in the component registry.
	//
	// WHY THE CHECK NEEDS IT AT ALL. Both consumers here are OPTIONAL apps. An
	// instance that never enabled Harbor has no harbor namespace and no
	// registry-storage-credentials Secret, and probeObjConsumer correctly reports
	// an absent credential Secret as a failure — "this consumer cannot be writing
	// at all" is true. Run unconditionally on a weekly cron, that makes the lane
	// permanently red on every instance that does not run Harbor, and a gate
	// nobody can turn green is a gate that gets switched off. Narrowing to the
	// apps the spec says are enabled is what lets this be scheduled at all.
	Component string
}

// objConsumers are the two workloads whose object-storage writes are load-bearing.
//
// Loki's credential arrives as AWS_* because the chart injects the Secret via
// extraEnvFrom straight into the AWS SDK's environment; Harbor's as
// REGISTRY_STORAGE_S3_* because the distribution registry reads its own names.
// Both are the consumer's real contract, so both are read as-is rather than
// normalized somewhere.
var objConsumers = []objConsumer{
	{
		Name:      "loki",
		Component: "loki",
		// The Secret apl-core's Loki release actually mounts. NOT
		// `loki-object-store` — that is the OPENBAO PATH name
		// (secret/loki/object-store, credpaths.CredPaths) and the two were conflated. The
		// k8s ExternalSecret that once carried that name was deleted by 52465691
		// when object storage went apl-core-native, so the old ref names an object
		// that has not existed since.
		SecretRef:      "monitoring/loki-s3-linode-credentials",
		AccessKeyField: "AWS_ACCESS_KEY_ID",
		SecretKeyField: "AWS_SECRET_ACCESS_KEY",
		ConfigRefs: []string{
			"secret/monitoring/loki",
			"configmap/monitoring/loki",
			"secret/monitoring/loki-config",
			"configmap/monitoring/loki-config",
		},
	},
	{
		Name:      "harbor",
		Component: "harbor",
		// Same correction as loki: `harbor-registry-s3` is the OpenBao path name
		// (secret/harbor/registry-s3); the registry mounts this one.
		SecretRef:      "harbor/registry-storage-credentials",
		AccessKeyField: "REGISTRY_STORAGE_S3_ACCESSKEY",
		SecretKeyField: "REGISTRY_STORAGE_S3_SECRETKEY",
		ConfigRefs: []string{
			"secret/harbor/harbor-registry",
			"configmap/harbor/harbor-registry",
			"secret/harbor/harbor-registry-config",
			"configmap/harbor/harbor-registry-config",
		},
	},
}

// objVerdict is one consumer's outcome.
type objVerdict struct {
	Consumer string
	Endpoint string
	Bucket   string
	Detail   string
	FailWhy  string
}

// Endpoint and bucket, as the consumers spell them. Loki renders YAML
// (`endpoint:` / `bucketnames:`); the distribution registry renders
// `regionendpoint:` / `bucket:`. Both are matched rather than one normalized
// form, because normalizing would mean re-deriving the value this gate exists to
// read verbatim.
var (
	// [ \t]* rather than \s* after the colon, deliberately: \s matches NEWLINES,
	// and Loki renders bucketNames as a nested mapping —
	//
	//   bucketnames:
	//     chunks: llz-loki-chunks
	//
	// so a \s*-based pattern skips the line break and captures the literal key
	// "chunks" as the bucket name. The gate would then PUT into a bucket called
	// "chunks", get NoSuchBucket, and report a healthy cluster as broken.
	// `s3:` is in the alternation because Loki spells the endpoint that way —
	// `storage.s3.s3: https://<host>` — while Harbor's registry uses
	// `regionendpoint:`. Anchoring on the key name with only leading whitespace
	// keeps `object_store: s3` and `s3forcepathstyle: true` out: neither has the
	// colon immediately after the key this matches on.
	objEndpointRe = regexp.MustCompile(`(?im)^[ \t]*(?:(?:s3_)?(?:region)?endpoint|s3)[ \t]*:[ \t]*["']?(?:https?://)?([A-Za-z0-9._-]+)`)
	objBucketRe   = regexp.MustCompile(`(?im)^[ \t]*(?:bucket|bucketnames|chunks)[ \t]*:[ \t]*["']?([A-Za-z0-9._-]+)`)
)

// parseObjConfig extracts the endpoint host and a bucket name from a consumer's
// rendered config. Pure.
//
// A miss on either is an ERROR, never a default. Substituting a derived endpoint
// here would re-create the exact blindness this gate was written for.
func parseObjConfig(cfg string) (endpoint, bucket string, err error) {
	if m := objEndpointRe.FindStringSubmatch(cfg); len(m) == 2 {
		endpoint = m[1]
	}
	if m := objBucketRe.FindStringSubmatch(cfg); len(m) == 2 {
		bucket = m[1]
	}
	switch {
	case endpoint == "" && bucket == "":
		return "", "", fmt.Errorf("found neither an endpoint nor a bucket in this consumer's config")
	case endpoint == "":
		return "", "", fmt.Errorf("found bucket %q but no endpoint — refusing to derive one, because a derived "+
			"endpoint is exactly the value that was wrong when Loki and Harbor 404'd against existing buckets", bucket)
	case bucket == "":
		return "", "", fmt.Errorf("found endpoint %q but no bucket name in this consumer's config", endpoint)
	}
	return endpoint, bucket, nil
}

// ── cluster reads (seamed) ───────────────────────────────────────────────────

var (
	readObjSecret = func(ref string) ([]byte, error) {
		ns, name, _ := strings.Cut(ref, "/")
		return objstoreCluster().Run("-n", ns, "get", "secret", name, "-o", "json")
	}
	// readObjConfig returns the consumer's rendered config as text. Secrets and
	// ConfigMaps both carry it depending on the chart, so the ref names the kind.
	readObjConfig = func(ref string) (string, error) {
		parts := strings.SplitN(ref, "/", 3)
		if len(parts) != 3 {
			return "", fmt.Errorf("config ref %q is not <kind>/<namespace>/<name>", ref)
		}
		kind, ns, name := parts[0], parts[1], parts[2]
		out, err := objstoreCluster().Run("-n", ns, "get", kind, name, "-o", "json")
		if err != nil {
			return "", err
		}
		var obj struct {
			Data       map[string]string `json:"data"`
			StringData map[string]string `json:"stringData"`
		}
		if err := json.Unmarshal(out, &obj); err != nil {
			return "", fmt.Errorf("decoding %s: %w", ref, err)
		}
		var b strings.Builder
		for _, v := range obj.Data {
			// Secret values are base64; ConfigMap values are not. Try both rather
			// than branching on kind, so a chart moving its config between the two
			// does not silently stop being read.
			if dec, derr := base64.StdEncoding.DecodeString(v); derr == nil {
				b.Write(dec)
			} else {
				b.WriteString(v)
			}
			b.WriteString("\n")
		}
		for _, v := range obj.StringData {
			b.WriteString(v + "\n")
		}
		if strings.TrimSpace(b.String()) == "" {
			return "", fmt.Errorf("%s carried no data", ref)
		}
		return b.String(), nil
	}
)

// readFirstObjConfig returns the first candidate that exists and carries data,
// plus which one it was. The ref is reported so a color.Green run records WHERE the
// endpoint came from — otherwise a chart rename silently shifts which object the
// gate trusts.
func readFirstObjConfig(refs []string) (cfg, from string, err error) {
	if len(refs) == 0 {
		return "", "", fmt.Errorf("no config refs declared for this consumer")
	}
	var lastErr error
	for _, ref := range refs {
		c, e := readObjConfig(ref)
		if e == nil && strings.TrimSpace(c) != "" {
			return c, ref, nil
		}
		lastErr = e
	}
	return "", "", lastErr
}

// listSecretsIn returns the Secret names in a namespace, for failure messages
// only. Seamed with the other cluster reads.
var listSecretsIn = func(ns string) ([]string, error) {
	out, err := objstoreCluster().Run("-n", ns, "get", "secret",
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// candidateSecretHint lists the Secrets that DO exist alongside a ref that did
// not resolve, so an absent-Secret failure carries its own next step.
//
// Best-effort and silent on error: a hint that cannot be gathered must never
// change the verdict or add noise to it.
func candidateSecretHint(ref string) string {
	ns, _, ok := strings.Cut(ref, "/")
	if !ok {
		return ""
	}
	names, err := listSecretsIn(ns)
	if err != nil || len(names) == 0 {
		return ""
	}
	return fmt.Sprintf(" Secrets that DO exist in %s: %s. If the chart renamed it, correct this consumer's "+
		"SecretRef — note that the OpenBao PATH name is not a Kubernetes Secret name, which is how this ref went stale.",
		ns, strings.Join(names, ", "))
}

// probeObjConsumer resolves one consumer's real config and round-trips an object.
func probeObjConsumer(c objConsumer, keyPrefix string, now time.Time) objVerdict {
	v := objVerdict{Consumer: c.Name}

	secretRaw, err := readObjSecret(c.SecretRef)
	if err != nil {
		// NAME WHAT IS ACTUALLY THERE. "Secret X is absent" is true and nearly
		// useless: the reader's next question is always "then what IS in that
		// namespace", and answering it needs a cluster they may not have. This gate
		// shipped pointing at two Secrets that had not existed since 52465691
		// renamed the mechanism, and finding the real names took a live kubectl.
		// One extra line here is the difference between reading the log and
		// standing up a cluster.
		v.FailWhy = fmt.Sprintf("credential Secret %s is absent (%v) — this consumer cannot be writing at all.%s",
			c.SecretRef, err, candidateSecretHint(c.SecretRef))
		return v
	}
	access, err := kube.SecretField(secretRaw, c.AccessKeyField)
	if err != nil {
		v.FailWhy = fmt.Sprintf("%s: %v", c.SecretRef, err)
		return v
	}
	secret, err := kube.SecretField(secretRaw, c.SecretKeyField)
	if err != nil {
		v.FailWhy = fmt.Sprintf("%s: %v", c.SecretRef, err)
		return v
	}

	cfg, from, err := readFirstObjConfig(c.ConfigRefs)
	if err != nil {
		v.FailWhy = fmt.Sprintf("could not read this consumer's own config from any of %s (%v) — without it the "+
			"endpoint would have to be derived, and a derived endpoint is the value that was already wrong. "+
			"If the chart renamed the object, add it to this consumer's ConfigRefs rather than letting the gate guess",
			strings.Join(c.ConfigRefs, ", "), err)
		return v
	}
	endpoint, bucket, err := parseObjConfig(cfg)
	if err != nil {
		v.FailWhy = fmt.Sprintf("%s: %v", from, err)
		return v
	}
	v.Endpoint, v.Bucket = endpoint, bucket

	key := fmt.Sprintf("%s%s-%d", keyPrefix, c.Name, now.UnixNano())
	payload := []byte(fmt.Sprintf("llz obj round-trip probe for %s at %s\n", c.Name, now.UTC().Format(time.RFC3339)))

	r := objstore.S3ObjectRoundTrip(access, secret, endpoint, bucket, key, payload)
	if !r.OK() {
		v.FailWhy = r.FailWhy
		return v
	}
	cleaned := "probe object deleted"
	if !r.Cleaned {
		cleaned = "probe object left behind (delete did not land; it expires with the bucket's lifecycle)"
	}
	v.Detail = fmt.Sprintf("wrote and read back %d bytes at %s/%s (config from %s) — %s",
		len(payload), endpoint, bucket, from, cleaned)
	return v
}

// enabledConsumers narrows the consumer set to the optional apl-core apps this
// deployment actually declares, so an instance that never enabled Harbor is not
// held to Harbor's write path.
//
// FAIL-OPEN ON AN UNREADABLE SPEC, and that direction is deliberate. If the spec
// cannot be loaded this returns every consumer and says so: narrowing on evidence
// we do not have is how a gate quietly stops checking the thing it exists for,
// and a false red is recoverable in a way a vacuous green is not.
//
// SELF-INSTALL CLUSTERS ARE NOT NARROWED. managedApps is meaningful only where
// Linode installs a minimal apl-core and the operator turns extras on in the
// Console; on a self-install cluster component toggles drive what exists and the
// list is documented as ignored. Reading it there would narrow on a field that
// means nothing.
func enabledConsumers(all []objConsumer, root, env string) (sel []objConsumer, skipped []string) {
	if env == "" {
		return all, nil
	}
	lz, err := clusterspec.LoadSplit(root)
	if err != nil {
		return all, nil
	}
	e, ok := lz.Env(env)
	if !ok || !e.Cluster.Bootstrap.ManagedAppPlatform {
		return all, nil
	}
	for _, c := range all {
		if c.Component == "" || e.Cluster.Bootstrap.ManagedAppEnabled(c.Component) {
			sel = append(sel, c)
			continue
		}
		skipped = append(skipped, c.Component)
	}
	return sel, skipped
}

func Run(only []string, keyPrefix, root, env string, settle, interval time.Duration) error {
	fmt.Println("## Object-storage round-trip assertion (Loki + Harbor can WRITE)")

	consumers, skipped := enabledConsumers(objConsumers, root, env)
	for _, name := range skipped {
		// PRINTED, NOT SILENT. A narrowed check that does not say what it dropped
		// reads identically to a check that covered everything, which is the shape
		// this whole lane exists to refuse.
		fmt.Printf("skip: %s is not in spec.cluster.bootstrap.managedApps for %s — not checked\n", name, env)
	}
	// --only is an OVERRIDE, applied after the spec narrowing and against the full
	// set: an operator naming a consumer explicitly has asked for that one to be
	// checked, and silently dropping it because the spec disagrees would answer a
	// different question than the one asked.
	if len(only) > 0 {
		// Built from objConsumers, NOT from the narrowed set — that is what makes
		// this an override rather than a filter on top of a filter.
		byName := map[string]objConsumer{}
		for _, c := range objConsumers {
			byName[c.Name] = c
		}
		consumers = nil
		for _, n := range only {
			c, ok := byName[n]
			if !ok {
				fmt.Fprintf(os.Stderr, "::error::unknown consumer %q\n", n)
				return fmt.Errorf("unknown consumer %q", n)
			}
			consumers = append(consumers, c)
		}
	}
	if len(consumers) == 0 {
		fmt.Fprintln(os.Stderr, "::error::no consumers selected — refusing to pass having written nothing")
		return fmt.Errorf("no consumers selected — refusing to pass vacuously")
	}

	var last []objVerdict
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		last = nil
		for _, c := range consumers {
			last = append(last, probeObjConsumer(c, keyPrefix, time.Now()))
		}
		bad := 0
		for _, v := range last {
			if v.FailWhy != "" {
				bad++
			}
		}
		if bad == 0 || time.Now().After(deadline) {
			break
		}
		fmt.Printf("attempt %d: %d consumer(s) cannot write yet — retrying in %s\n", attempt, bad, interval)
		time.Sleep(interval)
	}

	var failed []string
	for _, v := range last {
		if v.FailWhy != "" {
			fmt.Printf("FAIL: %s — %s\n", v.Consumer, v.FailWhy)
			failed = append(failed, v.Consumer)
		} else {
			fmt.Printf("OK: %s — %s\n", v.Consumer, v.Detail)
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "::error::object storage is not writable for: %s\n", strings.Join(failed, ", "))
		return fmt.Errorf("object storage is not writable for: %s", strings.Join(failed, ", "))
	}
	fmt.Printf("All %d consumer(s) wrote and read back their own object storage.\n", len(last))
	return nil
}
