package kyverno

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
)

func TestKyvernoOptsFromEnv(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		env := map[string]string{
			"KUBECONFIG_RAW":  "kc",
			"POLICY_MANIFEST": "p.yaml",
		}
		o, err := kyvernoOptsFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		if !o.waitForKyverno {
			t.Error("waitForKyverno should default true")
		}
		if o.fieldManager != "cluster-bootstrap-tf" {
			t.Errorf("fieldManager = %q", o.fieldManager)
		}
		if o.waitTimeout != 900*time.Second {
			t.Errorf("waitTimeout = %v", o.waitTimeout)
		}
		if o.retrofitNamespace != "monitoring" {
			t.Errorf("retrofitNamespace = %q", o.retrofitNamespace)
		}
		if o.retrofitWait != 60*time.Second {
			t.Errorf("retrofitWait = %v", o.retrofitWait)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		env := map[string]string{
			"KUBECONFIG_RAW":        "kc",
			"POLICY_MANIFEST":       "p.yaml",
			"WAIT_FOR_KYVERNO":      "false",
			"FIELD_MANAGER":         "fm",
			"WAIT_TIMEOUT_SECONDS":  "30",
			"RETROFIT_CONFIGMAP":    "loki-gateway",
			"RETROFIT_NAMESPACE":    "obs",
			"RETROFIT_ROLLOUT":      "loki-gateway",
			"RETROFIT_WAIT_SECONDS": "10",
		}
		o, err := kyvernoOptsFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		if o.waitForKyverno {
			t.Error("waitForKyverno should be false")
		}
		if o.fieldManager != "fm" || o.waitTimeout != 30*time.Second {
			t.Errorf("unexpected: %+v", o)
		}
		if o.retrofitConfigMap != "loki-gateway" || o.retrofitNamespace != "obs" || o.retrofitWait != 10*time.Second {
			t.Errorf("retrofit fields wrong: %+v", o)
		}
	})

	t.Run("required missing", func(t *testing.T) {
		for _, miss := range []string{"KUBECONFIG_RAW", "POLICY_MANIFEST"} {
			env := map[string]string{"KUBECONFIG_RAW": "kc", "POLICY_MANIFEST": "p.yaml"}
			delete(env, miss)
			if _, err := kyvernoOptsFromEnv(func(k string) string { return env[k] }); err == nil {
				t.Errorf("expected error when %s missing", miss)
			}
		}
	})

	t.Run("bad timeout", func(t *testing.T) {
		env := map[string]string{"KUBECONFIG_RAW": "kc", "POLICY_MANIFEST": "p.yaml", "WAIT_TIMEOUT_SECONDS": "soon"}
		if _, err := kyvernoOptsFromEnv(func(k string) string { return env[k] }); err == nil {
			t.Error("expected error on non-integer WAIT_TIMEOUT_SECONDS")
		}
	})
}

// fakeKubectl scripts kubectl responses keyed by a substring of the joined argv,
// and records the calls made.
type fakeKubectl struct {
	responses []kubectlRule
	calls     []string
}

type kubectlRule struct {
	match string // substring that must appear in the joined args
	out   string
	ok    bool
}

func (f *fakeKubectl) run(args ...string) (string, bool) {
	joined := strings.Join(args, " ")
	f.calls = append(f.calls, joined)
	for _, r := range f.responses {
		if strings.Contains(joined, r.match) {
			return r.out, r.ok
		}
	}
	// DEFAULT: kyverno-svc HAS a ready endpoint. Apply's readiness poll requires
	// one — the Deployment being Available is not the webhook being reachable —
	// and every fixture here models a cluster that finished starting. A test that
	// wants the RACE sets an explicit response for `endpointslice`.
	if strings.Contains(joined, "endpointslice") {
		return "true", true
	}
	return "", true // default: success, no output
}

func (f *fakeKubectl) called(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// fakeClock advances a fixed step each time now() is read so deadline loops
// terminate without real sleeping.
func fakeClock(step time.Duration) (func() time.Time, *time.Duration) {
	base := time.Unix(1_700_000_000, 0)
	elapsed := new(time.Duration)
	now := func() time.Time {
		t := base.Add(*elapsed)
		*elapsed += step
		return t
	}
	return now, elapsed
}

func testDeps(f *fakeKubectl, step time.Duration) cigate.Deps {
	now, _ := fakeClock(step)
	return cigate.Deps{
		Kubectl: f.run,
		// Granted from this extension's own binding and routed to the same fake:
		// kyverno's policy install is a real cluster-write and the fixture has to
		// model that, without being able to grant itself more than the declaration.
		Writer: capability.WithExec(Extension().Bindings[0],
			func(_ string, args ...string) ([]byte, error) {
				out, ok := f.run(args...)
				if !ok {
					return []byte(out), errKubectlFailed
				}
				return []byte(out), nil
			},
			func(_ string, args ...string) string { out, _ := f.run(args...); return out }).Writer,
		Now: now, Sleep: func(time.Duration) {},
	}
}

func TestApplyKyvernoPolicy(t *testing.T) {
	base := Opts{
		policyManifest: "manifests/kyverno-pvc.yaml",
		fieldManager:   "fm",
		waitForKyverno: true,
		waitTimeout:    20 * time.Second,
	}

	t.Run("ready then apply succeeds", func(t *testing.T) {
		f := &fakeKubectl{} // everything succeeds
		if err := Apply(base, testDeps(f, time.Second)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !f.called("apply --server-side") {
			t.Error("expected a server-side apply")
		}
		if !f.called("--field-manager=fm") {
			t.Error("apply should pass the field manager")
		}
		// Post-apply: confirm the policy reached Ready.
		if !f.called("wait --for=condition=Ready clusterpolicy/kyverno-pvc") {
			t.Error("expected a post-apply ClusterPolicy Ready confirmation")
		}
	})

	t.Run("policy never Ready -> warn, still nil err", func(t *testing.T) {
		f := &fakeKubectl{responses: []kubectlRule{
			{match: "wait --for=condition=Ready clusterpolicy", out: "timed out", ok: false},
		}}
		if err := Apply(base, testDeps(f, time.Second)); err != nil {
			t.Fatalf("a not-Ready policy must soft-fail (nil err), got %v", err)
		}
		if !f.called("apply --server-side") {
			t.Error("apply should still have run")
		}
	})

	t.Run("readiness times out -> warn, no apply, nil err", func(t *testing.T) {
		f := &fakeKubectl{responses: []kubectlRule{
			{match: "get crd clusterpolicies", out: "", ok: false},
		}}
		// 30s timeout, 20s/now-step → deadline passes after a couple polls.
		o := base
		o.waitTimeout = 30 * time.Second
		if err := Apply(o, testDeps(f, 20*time.Second)); err != nil {
			t.Fatalf("timeout must soft-fail (nil err), got %v", err)
		}
		if f.called("apply --server-side") {
			t.Error("must NOT apply after readiness timeout")
		}
	})

	t.Run("webhook race -> soft-fail nil", func(t *testing.T) {
		f := &fakeKubectl{responses: []kubectlRule{
			{match: "apply --server-side", out: `failed calling webhook "mutate-policy.kyverno.svc"`, ok: false},
		}}
		if err := Apply(base, testDeps(f, time.Second)); err != nil {
			t.Fatalf("webhook race must soft-fail (nil err), got %v", err)
		}
	})

	t.Run("hard apply error -> non-nil err", func(t *testing.T) {
		f := &fakeKubectl{responses: []kubectlRule{
			{match: "apply --server-side", out: `error validating "p.yaml": schema invalid`, ok: false},
		}}
		if err := Apply(base, testDeps(f, time.Second)); err == nil {
			t.Fatal("a non-race apply failure must return an error")
		}
	})

	t.Run("no-wait mode, CRD absent -> warn, no apply", func(t *testing.T) {
		o := base
		o.waitForKyverno = false
		f := &fakeKubectl{responses: []kubectlRule{
			{match: "get crd clusterpolicies", out: "", ok: false},
		}}
		if err := Apply(o, testDeps(f, time.Second)); err != nil {
			t.Fatalf("missing CRD must soft-fail, got %v", err)
		}
		if f.called("apply --server-side") {
			t.Error("must not apply when CRD is absent in no-wait mode")
		}
	})

	t.Run("no-wait mode, CRD present -> applies without polling deployment", func(t *testing.T) {
		o := base
		o.waitForKyverno = false
		f := &fakeKubectl{}
		if err := Apply(o, testDeps(f, time.Second)); err != nil {
			t.Fatal(err)
		}
		if f.called("wait --for=condition=Available") {
			t.Error("no-wait mode must not poll the admission controller")
		}
		if !f.called("apply --server-side") {
			t.Error("expected apply")
		}
	})
}

func TestRetrofitKyvernoConfigMap(t *testing.T) {
	base := Opts{
		policyManifest:    "manifests/kyverno-loki-gateway-resolver.yaml",
		fieldManager:      "fm",
		waitForKyverno:    true,
		waitTimeout:       20 * time.Second,
		retrofitConfigMap: "loki-gateway",
		retrofitNamespace: "monitoring",
		retrofitRollout:   "loki-gateway",
		retrofitWait:      20 * time.Second,
	}

	t.Run("configmap present -> annotate + rollout", func(t *testing.T) {
		f := &fakeKubectl{} // apply ok, get cm ok, annotate ok, rollout ok
		if err := Apply(base, testDeps(f, time.Second)); err != nil {
			t.Fatal(err)
		}
		if !f.called("annotate configmap loki-gateway") {
			t.Error("expected the retrofit annotate")
		}
		if !f.called("rollout restart deploy/loki-gateway") {
			t.Error("expected the retrofit rollout")
		}
	})

	t.Run("configmap absent -> notice, no annotate", func(t *testing.T) {
		f := &fakeKubectl{responses: []kubectlRule{
			{match: "get configmap loki-gateway", out: "", ok: false},
		}}
		o := base
		o.retrofitWait = 30 * time.Second
		if err := Apply(o, testDeps(f, 20*time.Second)); err != nil {
			t.Fatal(err)
		}
		if f.called("annotate configmap") {
			t.Error("must not annotate a ConfigMap that never appeared")
		}
	})

	t.Run("no rollout configured -> annotate only", func(t *testing.T) {
		o := base
		o.retrofitRollout = ""
		f := &fakeKubectl{}
		if err := Apply(o, testDeps(f, time.Second)); err != nil {
			t.Fatal(err)
		}
		if !f.called("annotate configmap loki-gateway") {
			t.Error("expected annotate")
		}
		if f.called("rollout restart") {
			t.Error("must not roll when RETROFIT_ROLLOUT is unset")
		}
	})
}

// TestPolicyName pins the fix for a check that could never pass. policyName feeds
// `kubectl wait clusterpolicy/<name>`, but returned the manifest's FILENAME — and
// no manifest's filename equals the name it declares. Every readiness wait ever
// made addressed a nonexistent object and degraded to "applied but did not report
// Ready", so the confirmation that a policy is actually ENFORCING never ran.
//
// Reads the real manifests rather than fixtures on purpose: the bug was precisely
// that these two spellings drift, so the test has to compare against the shipped
// files or it re-encodes the assumption instead of checking it.
//
// THE PATH REACHES BACK INTO cmd/llz, AND THAT IS NOT AN OVERSIGHT. The manifests
// directory cannot follow this package: ci_bootstrap_cluster.go //go:embed-s three
// of its files, and Go's embed cannot reach outside the embedding package's own
// directory. Moving only the kyverno-* subset would split one directory of related
// policy assets across two packages for the convenience of one test, which is
// worse than a relative path that says what it means. manifestDir is the single
// place to fix if either side ever moves.
// manifestDir is the SHIPPED Kyverno policies, relative to this package.
//
// IT USED TO POINT AT bootstrapcluster/manifests, and the fixtures there were four
// PVC/StorageClass policies that had not been applied since LLZ went managed-only
// — their //go:embed lines went with the self-install flow and were never
// restored. This test kept passing against them, which is the shape worth noticing:
// a filename-vs-metadata.name regression test does not care whether the manifest
// is live, so it went on proving a property of assets nobody deployed.
//
// It now reads the two policies that actually ship. Both still have a filename
// differing from their metadata.name, so the regression it exists to catch is
// still catchable — and now on files a cluster receives.
const manifestDir = "../../../../../platform-apl/components"

func TestPolicyName(t *testing.T) {
	for manifest, want := range map[string]string{
		manifestDir + "/objProxy/obj-proxy/kyverno-harbor-ca.yaml":              "harbor-obj-proxy-ca",
		manifestDir + "/imageSignature/kyverno-verify-llz-image-signature.yaml": "verify-llz-image-signature",
	} {
		got := policyName(manifest)
		if got != want {
			t.Errorf("policyName(%s) = %q, want the manifest's metadata.name %q — a filename here makes `kubectl wait clusterpolicy/<name>` address nothing",
				manifest, got, want)
		}
		if got == strings.TrimSuffix(filepath.Base(manifest), ".yaml") {
			t.Errorf("policyName(%s) returned the basename — this manifest can no longer catch the regression; pick one whose filename differs from its metadata.name", manifest)
		}
	}

	// Unreadable manifests fall back to the basename: no worse than the old
	// behaviour, and never a panic on a path the operator typo'd.
	if got := policyName("manifests/does-not-exist.yaml"); got != "does-not-exist" {
		t.Errorf("missing manifest should fall back to the basename, got %q", got)
	}
}

// errKubectlFailed marks a non-zero exit for the Writer fakes above.
var errKubectlFailed = errors.New("kubectl exited non-zero")

// ── The webhook race: prevent it, and fail closed when it cannot be prevented ──
//
// The bug: `helm install --wait` returns when the DEPLOYMENT is Available, which
// is not the webhook being reachable. Kyverno registers its policy webhooks
// separately, so kyverno-svc can still have no ready endpoint — and an apply
// landing there is refused by the apiserver with a message NAMING A WEBHOOK, so
// it reads as a policy failure. It reddened a pull request that touched no
// Kubernetes at all (2026-08-24).

// The readiness poll must not pass on a Service with no ready endpoint, or the
// apply goes straight back into the race this exists to close.
func TestApplyWaitsForAReadyWebhookEndpoint(t *testing.T) {
	f := &fakeKubectl{responses: []kubectlRule{
		// Deployment Available, but kyverno-svc has NO ready endpoint yet.
		{match: "endpointslice", out: "", ok: true},
	}}
	base := Opts{policyManifest: "m.yaml", fieldManager: "fm", waitForKyverno: true, waitTimeout: 20 * time.Second}

	if err := Apply(base, testDeps(f, time.Second)); err != nil {
		t.Fatalf("without WEBHOOK_RACE_FATAL this skips rather than errors: %v", err)
	}
	if f.called("apply") {
		t.Error("applied while kyverno-svc had no ready endpoint — that is the race, not a fix for it")
	}
}

// A GATE must not skip its own subject. Bootstrap can warn and re-run; a check
// that passes having applied nothing is the vacuous pass docs/e2e-gates.md
// forbids, so WEBHOOK_RACE_FATAL turns both skip paths into failures.
func TestWebhookRaceFatalRefusesToSkip(t *testing.T) {
	t.Run("never becomes ready", func(t *testing.T) {
		f := &fakeKubectl{responses: []kubectlRule{{match: "endpointslice", out: "", ok: true}}}
		o := Opts{policyManifest: "m.yaml", fieldManager: "fm", waitForKyverno: true,
			waitTimeout: 20 * time.Second, webhookRaceFatal: true}
		if err := Apply(o, testDeps(f, time.Second)); err == nil {
			t.Fatal("a gate must fail when kyverno never becomes answerable, not pass having applied nothing")
		}
	})

	t.Run("apply itself hits the race", func(t *testing.T) {
		f := &fakeKubectl{responses: []kubectlRule{
			{match: "apply", out: `failed calling webhook "mutate-policy.kyverno.svc": ` +
				`dial tcp 10.96.75.24:443: connect: connection refused`, ok: false},
		}}
		o := Opts{policyManifest: "m.yaml", fieldManager: "fm", waitForKyverno: true,
			waitTimeout: 20 * time.Second, webhookRaceFatal: true}
		if err := Apply(o, testDeps(f, time.Second)); err == nil {
			t.Fatal("a gate must fail on the webhook race rather than soft-skipping it")
		}
	})
}

// The default stays a SOFT skip, because cluster-bootstrap's null_resource
// legitimately re-runs. Flipping that for everyone would turn a re-runnable
// bootstrap step into a hard provisioning failure.
func TestWebhookRaceStaysSoftByDefault(t *testing.T) {
	f := &fakeKubectl{responses: []kubectlRule{
		{match: "apply", out: `failed calling webhook "mutate-policy.kyverno.svc": connection refused`, ok: false},
	}}
	o := Opts{policyManifest: "m.yaml", fieldManager: "fm", waitForKyverno: true, waitTimeout: 20 * time.Second}
	if err := Apply(o, testDeps(f, time.Second)); err != nil {
		t.Fatalf("default must soft-skip the race for bootstrap: %v", err)
	}
}

// WEBHOOK_RACE_FATAL must not swallow a REAL rejection into the race path — a
// CEL error or a schema violation is the finding the gate exists to surface.
func TestFatalModeStillDistinguishesARealRejection(t *testing.T) {
	f := &fakeKubectl{responses: []kubectlRule{
		{match: "apply", out: "admission webhook denied the request: policy is invalid: CEL compile error", ok: false},
	}}
	o := Opts{policyManifest: "m.yaml", fieldManager: "fm", waitForKyverno: true,
		waitTimeout: 20 * time.Second, webhookRaceFatal: true}
	err := Apply(o, testDeps(f, time.Second))
	if err == nil {
		t.Fatal("a real rejection must fail")
	}
	if strings.Contains(err.Error(), "unreachable") {
		t.Errorf("a policy rejection was reported as a startup race: %v", err)
	}
}

func TestWebhookRaceFatalReadsItsEnvVar(t *testing.T) {
	get := func(k string) string {
		switch k {
		case "KUBECONFIG_RAW":
			return "kc"
		case "POLICY_MANIFEST":
			return "m.yaml"
		case "WEBHOOK_RACE_FATAL":
			return "true"
		}
		return ""
	}
	o, err := kyvernoOptsFromEnv(get)
	if err != nil {
		t.Fatalf("kyvernoOptsFromEnv: %v", err)
	}
	if !o.webhookRaceFatal {
		t.Error("WEBHOOK_RACE_FATAL=true did not reach Opts")
	}
}
