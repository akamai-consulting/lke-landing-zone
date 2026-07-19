package main

import (
	"strings"
	"testing"
	"time"
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

func TestIsKyvernoWebhookRace(t *testing.T) {
	races := []string{
		`Error from server (InternalError): failed calling webhook "mutate-policy.kyverno.svc"`,
		`dial tcp 10.0.0.1:443: connect: operation not permitted`,
		`connection refused`,
		`no endpoints available for service "kyverno-svc"`,
	}
	for _, s := range races {
		if !isKyvernoWebhookRace(s) {
			t.Errorf("should classify as race: %q", s)
		}
	}
	notRace := []string{
		`error validating "p.yaml": ClusterPolicy in version "v1" cannot be handled`,
		`the server could not find the requested resource`,
		``,
	}
	for _, s := range notRace {
		if isKyvernoWebhookRace(s) {
			t.Errorf("should NOT classify as race: %q", s)
		}
	}
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

func testDeps(f *fakeKubectl, step time.Duration) aplGateDeps {
	now, _ := fakeClock(step)
	return aplGateDeps{kubectl: f.run, now: now, sleep: func(time.Duration) {}}
}

func TestApplyKyvernoPolicy(t *testing.T) {
	base := kyvernoPolicyOpts{
		policyManifest: "manifests/kyverno-pvc.yaml",
		fieldManager:   "fm",
		waitForKyverno: true,
		waitTimeout:    20 * time.Second,
	}

	t.Run("ready then apply succeeds", func(t *testing.T) {
		f := &fakeKubectl{} // everything succeeds
		if err := applyKyvernoPolicy(base, testDeps(f, time.Second)); err != nil {
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
		if err := applyKyvernoPolicy(base, testDeps(f, time.Second)); err != nil {
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
		if err := applyKyvernoPolicy(o, testDeps(f, 20*time.Second)); err != nil {
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
		if err := applyKyvernoPolicy(base, testDeps(f, time.Second)); err != nil {
			t.Fatalf("webhook race must soft-fail (nil err), got %v", err)
		}
	})

	t.Run("hard apply error -> non-nil err", func(t *testing.T) {
		f := &fakeKubectl{responses: []kubectlRule{
			{match: "apply --server-side", out: `error validating "p.yaml": schema invalid`, ok: false},
		}}
		if err := applyKyvernoPolicy(base, testDeps(f, time.Second)); err == nil {
			t.Fatal("a non-race apply failure must return an error")
		}
	})

	t.Run("no-wait mode, CRD absent -> warn, no apply", func(t *testing.T) {
		o := base
		o.waitForKyverno = false
		f := &fakeKubectl{responses: []kubectlRule{
			{match: "get crd clusterpolicies", out: "", ok: false},
		}}
		if err := applyKyvernoPolicy(o, testDeps(f, time.Second)); err != nil {
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
		if err := applyKyvernoPolicy(o, testDeps(f, time.Second)); err != nil {
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
	base := kyvernoPolicyOpts{
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
		if err := applyKyvernoPolicy(base, testDeps(f, time.Second)); err != nil {
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
		if err := applyKyvernoPolicy(o, testDeps(f, 20*time.Second)); err != nil {
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
		if err := applyKyvernoPolicy(o, testDeps(f, time.Second)); err != nil {
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

func TestPolicyName(t *testing.T) {
	if got := policyName("manifests/kyverno-pvc-encrypted-storage-class.yaml"); got != "kyverno-pvc-encrypted-storage-class" {
		t.Errorf("policyName = %q", got)
	}
}
