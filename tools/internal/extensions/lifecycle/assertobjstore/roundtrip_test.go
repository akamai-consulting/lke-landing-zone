package assertobjstore

// roundtrip_test.go — the obj half of what used to be ci_assert_obj_certs_db_test.go.
//
// THAT FILENAME IS THE FOURTH PATTERN THIS CAMPAIGN HAS FOUND STRANDING TESTS, and
// the most direct about it: the file is named for THREE unrelated subjects — obj,
// certs, db — so whichever subject moves first leaves the other two behind and
// takes a build break with it. The certs tests already left with assert-identity;
// these are the obj ones; the SCRAM/Postgres tests stay in internal/cli until
// assert-database is extracted.
//
// The three patterns already recorded: named for a coverage METRIC
// (coverage_tier1/2, branch_coverage, uncovered_helpers), named for the COMMAND
// that calls the code (env_set_test.go, holding zero tests for env_set.go), and
// named for the BATCH it was written in (ci_batch2_test.go). All four share one
// property — nothing in the name points at a subject, so nothing points at where a
// test belongs.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseObjConfig(t *testing.T) {
	loki := `
      storage:
        bucketnames: llz-loki-chunks
        s3:
          endpoint: us-ord-10.linodeobjects.com
          region: us-ord-10
`
	ep, bucket, err := parseObjConfig(loki)
	if err != nil || ep != "us-ord-10.linodeobjects.com" || bucket != "llz-loki-chunks" {
		t.Fatalf("unexpected (%q,%q,%v)", ep, bucket, err)
	}

	// The distribution registry spells them differently; both must be read as-is.
	harbor := `
storage:
  s3:
    regionendpoint: https://us-ord-10.linodeobjects.com
    bucket: llz-harbor-registry
`
	ep, bucket, err = parseObjConfig(harbor)
	if err != nil || ep != "us-ord-10.linodeobjects.com" || bucket != "llz-harbor-registry" {
		t.Fatalf("unexpected harbor parse (%q,%q,%v)", ep, bucket, err)
	}

	// THE refusal that matters. A config with a bucket but no endpoint must ERROR,
	// never fall back to a derived one — the derived endpoint is the value that was
	// already wrong when Loki and Harbor 404'd against buckets that existed.
	_, _, err = parseObjConfig("bucket: only-a-bucket\n")
	if err == nil {
		t.Fatal("a config with no endpoint must fail rather than derive one")
	}
	if !strings.Contains(err.Error(), "refusing to derive") {
		t.Errorf("the failure must say it refuses to derive an endpoint, got %q", err)
	}
	if _, _, err := parseObjConfig("nothing useful here\n"); err == nil {
		t.Error("a config with neither must fail")
	}
}

// Loki renders bucketNames as a NESTED mapping:
//
//	bucketnames:
//	  chunks: llz-loki-chunks
//
// A regex using \s* after the colon spans the newline and captures the next
// key ("chunks") as the bucket name.
// Loki renders bucketNames as a NESTED mapping:
//
//	bucketnames:
//	  chunks: llz-loki-chunks
//
// A regex using \s* after the colon spans the newline and captures the next
// key ("chunks") as the bucket name.
func TestParseObjConfigNestedBucketNames(t *testing.T) {
	cfg := "storage:\n  bucketnames:\n    chunks: llz-loki-chunks\n  s3:\n    endpoint: us-ord-10.linodeobjects.com\n"
	ep, bucket, err := parseObjConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bucket == "chunks" {
		t.Fatalf("captured the KEY as the bucket name — \\s* spanned the newline (endpoint=%q)", ep)
	}
	if bucket != "llz-loki-chunks" {
		t.Errorf("bucket = %q, want llz-loki-chunks", bucket)
	}
}

func TestProbeObjConsumerFailsClosedOnMissingConfig(t *testing.T) {
	oSec, oCfg := readObjSecret, readObjConfig
	t.Cleanup(func() { readObjSecret, readObjConfig = oSec, oCfg })
	readObjSecret = func(string) ([]byte, error) { return objSecretJSON("AK", "SK"), nil }
	readObjConfig = func(string) (string, error) { return "", errors.New("NotFound") }

	v := probeObjConsumer(objConsumers[0], "p/", time.Now())
	if v.FailWhy == "" {
		t.Fatal("an unreadable consumer config must fail — deriving the endpoint is the bug")
	}
	if !strings.Contains(v.FailWhy, "derived endpoint is the value that was already wrong") {
		t.Errorf("the failure must say why it will not derive one, got %q", v.FailWhy)
	}

	// A missing credential Secret is equally fatal: the consumer cannot be writing.
	readObjSecret = func(string) ([]byte, error) { return nil, errors.New("NotFound") }
	if v := probeObjConsumer(objConsumers[0], "p/", time.Now()); v.FailWhy == "" {
		t.Error("a missing credential Secret must fail")
	}
}

// Both load-bearing writers must be covered, and each must read ITS OWN key
// names — normalizing them would mean re-deriving the consumer's contract.
// Both load-bearing writers must be covered, and each must read ITS OWN key
// names — normalizing them would mean re-deriving the consumer's contract.
func TestObjConsumersCoverLokiAndHarbor(t *testing.T) {
	byName := map[string]objConsumer{}
	for _, c := range objConsumers {
		byName[c.Name] = c
	}
	for _, want := range []string{"loki", "harbor"} {
		c, ok := byName[want]
		if !ok {
			t.Fatalf("%s is a load-bearing object-storage writer and must be gated", want)
		}
		if c.SecretRef == "" || c.AccessKeyField == "" || c.SecretKeyField == "" || len(c.ConfigRefs) == 0 {
			t.Errorf("%s consumer is under-specified: %+v", want, c)
		}
	}
	if byName["loki"].AccessKeyField == byName["harbor"].AccessKeyField {
		t.Error("Loki and Harbor spell their credential keys differently; sharing one field name means one of them is wrong")
	}
}

// ── assert-certificates ──────────────────────────────────────────────────────

// Loki spells its endpoint `storage.s3.s3:`, not `endpoint:`. The gate's regex
// only knew the `(region)endpoint` spellings, so against apl-core's real Loki
// ConfigMap it found the bucket, found no endpoint, and refused to derive one —
// reporting a healthy consumer as broken.
func TestParseObjConfigReadsLokiS3Key(t *testing.T) {
	// Trimmed from a live monitoring/loki config.
	cfg := "common:\n  storage:\n    s3:\n      bucketnames: platform-loki-chunks-e2e\n" +
		"      s3: https://us-ord-10.linodeobjects.com\n      s3forcepathstyle: true\n" +
		"schema_config:\n  configs:\n  - object_store: s3\n    store: tsdb\n"
	ep, bucket, err := parseObjConfig(cfg)
	if err != nil {
		t.Fatalf("parseObjConfig on the real Loki config: %v", err)
	}
	if ep != "us-ord-10.linodeobjects.com" {
		t.Errorf("endpoint = %q, want the host from the `s3:` key", ep)
	}
	if bucket != "platform-loki-chunks-e2e" {
		t.Errorf("bucket = %q", bucket)
	}
}

// `object_store: s3` and `s3forcepathstyle: true` must not be mistaken for the
// endpoint — matching either would send the round trip at a host named "s3" or
// "true" and report a broken consumer.
// `object_store: s3` and `s3forcepathstyle: true` must not be mistaken for the
// endpoint — matching either would send the round trip at a host named "s3" or
// "true" and report a broken consumer.
func TestParseObjConfigIgnoresS3LookalikeKeys(t *testing.T) {
	cfg := "schema_config:\n  configs:\n  - object_store: s3\n" +
		"storage:\n  s3forcepathstyle: true\n  bucket: b\n"
	if _, _, err := parseObjConfig(cfg); err == nil {
		t.Error("a config with no real endpoint must error, not match object_store/s3forcepathstyle")
	}
}

// Harbor's registry keeps using `regionendpoint:` — the other spelling must
// still work.
// Harbor's registry keeps using `regionendpoint:` — the other spelling must
// still work.
func TestParseObjConfigStillReadsHarborRegionEndpoint(t *testing.T) {
	cfg := "storage:\n  s3:\n    region: us-ord-10\n    bucket: platform-harbor-registry-e2e\n" +
		"    regionendpoint: https://us-ord-10.linodeobjects.com\n"
	ep, bucket, err := parseObjConfig(cfg)
	if err != nil || ep != "us-ord-10.linodeobjects.com" || bucket != "platform-harbor-registry-e2e" {
		t.Errorf("parseObjConfig = (%q, %q, %v)", ep, bucket, err)
	}
}

// The consumer table must name the Secrets the workloads MOUNT, not the OpenBao
// paths that happen to describe the same credential. Conflating them is what made
// this lane report both consumers unable to write on a cluster where both were
// writing fine.
// The consumer table must name the Secrets the workloads MOUNT, not the OpenBao
// paths that happen to describe the same credential. Conflating them is what made
// this lane report both consumers unable to write on a cluster where both were
// writing fine.
func TestObjConsumersNameMountedSecretsNotOpenBaoPaths(t *testing.T) {
	want := map[string]string{
		"loki":   "monitoring/loki-s3-linode-credentials",
		"harbor": "harbor/registry-storage-credentials",
	}
	for _, c := range objConsumers {
		if got := want[c.Name]; got != "" && c.SecretRef != got {
			t.Errorf("%s SecretRef = %q, want %q — the OpenBao path name is not a k8s Secret name",
				c.Name, c.SecretRef, got)
		}
	}
}

// "Secret X is absent" is true and nearly useless on its own — the reader's next
// question is always "then what IS in that namespace", and answering it needed a
// live cluster when this gate shipped pointing at two Secrets that had not
// existed since 52465691.
// "Secret X is absent" is true and nearly useless on its own — the reader's next
// question is always "then what IS in that namespace", and answering it needed a
// live cluster when this gate shipped pointing at two Secrets that had not
// existed since 52465691.
func TestProbeObjConsumerNamesTheSecretsThatDoExist(t *testing.T) {
	oS, oL := readObjSecret, listSecretsIn
	t.Cleanup(func() { readObjSecret, listSecretsIn = oS, oL })
	readObjSecret = func(string) ([]byte, error) { return nil, errors.New("NotFound") }
	listSecretsIn = func(ns string) ([]string, error) {
		return []string{"loki-s3-linode-credentials", "alertmanager-config"}, nil
	}

	v := probeObjConsumer(objConsumer{Name: "loki", SecretRef: "monitoring/loki-object-store"}, "p", time.Now())
	if v.FailWhy == "" {
		t.Fatal("an absent credential Secret must fail")
	}
	if !strings.Contains(v.FailWhy, "loki-s3-linode-credentials") {
		t.Errorf("the failure must name the Secrets that DO exist, or diagnosing it needs a cluster: %s", v.FailWhy)
	}
}

// The hint is best-effort: if it cannot be gathered, the verdict must be
// unchanged rather than swallowing the real failure.
// The hint is best-effort: if it cannot be gathered, the verdict must be
// unchanged rather than swallowing the real failure.
func TestProbeObjConsumerSurvivesAnUnlistableNamespace(t *testing.T) {
	oS, oL := readObjSecret, listSecretsIn
	t.Cleanup(func() { readObjSecret, listSecretsIn = oS, oL })
	readObjSecret = func(string) ([]byte, error) { return nil, errors.New("NotFound") }
	listSecretsIn = func(string) ([]string, error) { return nil, errors.New("forbidden") }

	v := probeObjConsumer(objConsumer{Name: "loki", SecretRef: "monitoring/loki-object-store"}, "p", time.Now())
	if v.FailWhy == "" || !strings.Contains(v.FailWhy, "is absent") {
		t.Errorf("the underlying failure must survive a hint that could not be gathered: %s", v.FailWhy)
	}
}

func objSecretJSON(access, secret string) []byte {
	b, _ := json.Marshal(map[string]any{"data": map[string]string{
		"AWS_ACCESS_KEY_ID":     base64.StdEncoding.EncodeToString([]byte(access)),
		"AWS_SECRET_ACCESS_KEY": base64.StdEncoding.EncodeToString([]byte(secret)),
	}})
	return b
}

// specRoot writes a minimal instance whose prod deployment declares the given
// managedApps, so the narrowing is exercised against a real LoadSplit rather than
// a hand-built struct — the parse is part of what can break.
func specRoot(t *testing.T, managed string, apl bool) string {
	t.Helper()
	root := t.TempDir()
	lz := `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata:
  name: inst
spec:
  instance:
    upstreamOrg: akamai-consulting
    repo: acme/inst
    forge: github
  teams:
    - name: platform
      openbaoSubtree: secret/platform
`
	if err := os.WriteFile(filepath.Join(root, "landingzone.yaml"), []byte(lz), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "environments"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := fmt.Sprintf(`apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata:
  name: prod
spec:
  cluster:
    clusterLabel: inst-prod
    region: us-ord
    k8sVersion: v1.33.6+lke7
    nodePool: {type: g8-dedicated-8-4, count: 5}
    bootstrap:
      name: inst-prod
      managedAppPlatform: %t
      managedApps: [%s]
    objectStorage:
      cluster: us-ord-1
`, apl, managed)
	if err := os.WriteFile(filepath.Join(root, "environments", "prod.yaml"), []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func names(cs []objConsumer) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// THE REASON THIS CHECK CAN BE SCHEDULED AT ALL. Harbor is optional; an instance
// that never enabled it has no registry-storage-credentials Secret, and
// probeObjConsumer reports an absent Secret as a failure. Without the narrowing
// the weekly lane is permanently red on those instances, and a gate nobody can
// turn green is one that gets switched off.
func TestAnUndeclaredAppIsNotHeldToItsWritePath(t *testing.T) {
	sel, skipped := enabledConsumers(objConsumers, specRoot(t, "loki", true), "prod")
	if got := names(sel); len(got) != 1 || got[0] != "loki" {
		t.Errorf("selected %v, want just loki", got)
	}
	if len(skipped) != 1 || skipped[0] != "harbor" {
		t.Errorf("skipped %v, want [harbor] — a silent narrowing reads like full coverage", skipped)
	}
}

// The narrowing must not become an excuse. An instance that DOES declare the app
// is held to it, which is the case that caught a 42-day outage.
func TestADeclaredAppIsChecked(t *testing.T) {
	sel, skipped := enabledConsumers(objConsumers, specRoot(t, "loki, harbor", true), "prod")
	if len(sel) != 2 {
		t.Errorf("selected %v, want both consumers", names(sel))
	}
	if len(skipped) != 0 {
		t.Errorf("skipped %v with both declared", skipped)
	}
}

// FAIL-OPEN, and only in this direction. An unreadable spec must widen to every
// consumer rather than narrow: a false red is recoverable, a check that quietly
// stopped looking is the failure this whole lane exists to refuse.
func TestAnUnreadableSpecChecksEverything(t *testing.T) {
	for name, root := range map[string]string{
		"no spec at all": t.TempDir(),
		"unknown env":    specRoot(t, "loki", true),
		"self-install":   specRoot(t, "loki", false),
	} {
		env := "prod"
		if name == "unknown env" {
			env = "staging"
		}
		sel, skipped := enabledConsumers(objConsumers, root, env)
		if len(sel) != len(objConsumers) {
			t.Errorf("%s: selected %v, want every consumer — narrowing on evidence we do not "+
				"have is how a gate stops checking the thing it exists for", name, names(sel))
		}
		if len(skipped) != 0 {
			t.Errorf("%s: reported skips %v while checking everything", name, skipped)
		}
	}
}

// managedApps is documented as IGNORED on a self-install cluster (component
// toggles drive those), so reading it there would narrow on a field that means
// nothing. Pinned separately from the unreadable cases because it is a different
// reason for the same answer.
func TestSelfInstallClustersAreNotNarrowedByManagedApps(t *testing.T) {
	sel, _ := enabledConsumers(objConsumers, specRoot(t, "loki", false), "prod")
	if len(sel) != len(objConsumers) {
		t.Errorf("a self-install cluster was narrowed by managedApps: %v", names(sel))
	}
}
