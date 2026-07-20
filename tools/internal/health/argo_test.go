package health

import (
	"strings"
	"testing"
)

func TestMatchPrefix(t *testing.T) {
	items := []string{"openbao/platform-openbao", "harbor/harbor-core"}
	if !MatchPrefix("harbor/harbor-core", items) {
		t.Error("exact match should hit")
	}
	if !MatchPrefix("harbor/harbor-core-7d9f", items) {
		t.Error("name-<suffix> should hit")
	}
	if MatchPrefix("harbor/harbor-corex", items) {
		t.Error("a non-hyphen continuation must NOT hit")
	}
	if MatchPrefix("other/thing", items) {
		t.Error("unrelated name must not hit")
	}
}

func TestMatchExternalDep(t *testing.T) {
	entries := ExternalDepWorkloads()
	// Exact + generated-suffix both match the suffix-tolerant ^(p)(-.*)?$ form.
	if r, ok := MatchExternalDep("kube-system/linode-internal-cidr-firewall", entries); !ok || r == "" {
		t.Errorf("exact workload should match (%q,%v)", r, ok)
	}
	if _, ok := MatchExternalDep("kube-system/linode-internal-cidr-firewall-abc123-xyz", entries); !ok {
		t.Error("generated ReplicaSet/Pod suffix should match")
	}
	if _, ok := MatchExternalDep("kube-system/something-else", entries); ok {
		t.Error("unrelated workload must not match")
	}
	// otomi-api (apl-core-internal, deferred): the real pod carries a ReplicaSet
	// suffix — the suffix-tolerant form must still match so its CrashLoopBackOff
	// is deferred, not hard-failed.
	if _, ok := MatchExternalDep("otomi/otomi-api-54c87866fb-jf65w", entries); !ok {
		t.Error("otomi-api pod (with ReplicaSet suffix) should be deferred")
	}
	// A bad regex pattern is skipped, not panicked.
	if _, ok := MatchExternalDep("x", []DepEntry{{"(", "broken"}}); ok {
		t.Error("malformed pattern should not match")
	}
}

func TestClassifyArgoApp(t *testing.T) {
	cases := []struct {
		name   string
		app    ArgoApp
		phase1 bool
		want   Category
	}{
		{"synced+healthy", ArgoApp{Name: "platform-harbor", Sync: "Synced", Health: "Healthy", Automated: true}, false, CatOK},
		{"deferred beats spec-err", ArgoApp{Name: "external-dns-external-dns", Sync: "Unknown", Health: "Healthy", SpecErr: "ComparisonError: token not seeded", Automated: true}, false, CatDeferred},
		{"otomi team values-gitops no longer deferred — a genuinely Unknown apl-core gitops app now fails the gate (was CatDeferred; verified Synced/Healthy in reality)", ArgoApp{Name: "team-admin-values-gitops", Sync: "Unknown", Health: "Healthy", SpecErr: "ComparisonError: env/teams/admin/sealedsecrets: app path does not exist", Automated: true}, false, CatFail},
		{"spec error fails", ArgoApp{Name: "platform-foo", Sync: "OutOfSync", Health: "Healthy", SpecErr: "InvalidSpecError: bad path", Automated: true}, false, CatFail},
		{"no automated -> pending", ArgoApp{Name: "platform-manual", Sync: "OutOfSync", Health: "Missing", Automated: false}, false, CatPending},
		{"phase1 cascade -> pending", ArgoApp{Name: "platform-openbao", Sync: "OutOfSync", Health: "Missing", Automated: true}, true, CatPending},
		{"phase1 cascade ignored when not phase1 -> fail", ArgoApp{Name: "platform-openbao", Sync: "OutOfSync", Health: "Degraded", Automated: true}, false, CatFail},
		{"outofsync but healthy -> drift", ArgoApp{Name: "platform-eso", Sync: "OutOfSync", Health: "Healthy", Automated: true}, false, CatDrift},
		{"synced+progressing -> pending", ArgoApp{Name: "monitoring-loki", Sync: "Synced", Health: "Progressing", Automated: true}, false, CatPending},
		{"outofsync+progressing -> pending", ArgoApp{Name: "platform-rollout", Sync: "OutOfSync", Health: "Progressing", Automated: true}, false, CatPending},
		{"unhealthy -> fail", ArgoApp{Name: "platform-bad", Sync: "OutOfSync", Health: "Degraded", Automated: true}, false, CatFail},
		{"redis WRONGPASS cache auth -> pending (transient, poll)", ArgoApp{Name: "llz-harbor", Sync: "Unknown", Health: "Healthy", SpecErr: "ComparisonError: Failed to load target state: failed to generate manifest for source 1 of 1: rpc error: code = Unknown desc = failed to list refs: WRONGPASS invalid username-password pair or user is disabled.", Automated: true}, false, CatPending},
		{"redis NOAUTH cache auth -> pending (transient, poll)", ArgoApp{Name: "monitoring-loki", Sync: "Unknown", Health: "Healthy", SpecErr: "ComparisonError: failed to list refs: NOAUTH Authentication required.", Automated: true}, false, CatPending},
		{"deferred still wins over redis cache auth", ArgoApp{Name: "external-dns-external-dns", Sync: "Unknown", Health: "Healthy", SpecErr: "ComparisonError: failed to list refs: WRONGPASS", Automated: true}, false, CatDeferred},
		{"real comparison error still fails (no redis code)", ArgoApp{Name: "platform-foo", Sync: "Unknown", Health: "Healthy", SpecErr: "ComparisonError: rpc error: repository not found", Automated: true}, false, CatFail},
		{"annotation-limit sync error -> pending (transient, self-heal)", ArgoApp{Name: "kyverno-kyverno", Sync: "OutOfSync", Health: "Degraded", OpErr: `error when patching "/dev/shm/x": CustomResourceDefinition.apiextensions.k8s.io "clusterpolicies.kyverno.io" is invalid: metadata.annotations: Too long: may not be more than 262144 bytes`, Automated: true}, false, CatPending},
		{"deferred still wins over annotation-limit", ArgoApp{Name: "external-dns-external-dns", Sync: "OutOfSync", Health: "Degraded", OpErr: "metadata.annotations: Too long", Automated: true}, false, CatDeferred},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := ClassifyArgoApp(c.app, c.phase1)
			if got != c.want {
				t.Errorf("ClassifyArgoApp = %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseArgoApp(t *testing.T) {
	const raw = `{
      "metadata": {"name": "platform-openbao"},
      "spec": {"syncPolicy": {"automated": {"prune": true}}},
      "status": {
        "sync": {"status": "OutOfSync"},
        "health": {"status": "Missing"},
        "conditions": [
          {"type": "ComparisonError", "message": "no matches for kind"},
          {"type": "OrphanedResourceWarning", "message": "ignore me"},
          {"type": "InvalidSpecError", "message": ""}
        ]
      }
    }`
	a, err := ParseArgoApp([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "platform-openbao" || a.Sync != "OutOfSync" || a.Health != "Missing" {
		t.Errorf("parsed core fields wrong: %+v", a)
	}
	if !a.Automated {
		t.Error("automated syncPolicy should resolve true")
	}
	// Only the two spec-error conditions are joined; the empty message -> (no message).
	want := "ComparisonError: no matches for kind | InvalidSpecError: (no message)"
	if a.SpecErr != want {
		t.Errorf("SpecErr = %q, want %q", a.SpecErr, want)
	}

	// Absent syncPolicy.automated => not automated.
	noAuto, _ := ParseArgoApp([]byte(`{"metadata":{"name":"x"},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}}`))
	if noAuto.Automated || noAuto.SpecErr != "" {
		t.Errorf("absent automated/conditions: %+v", noAuto)
	}

	// A FAILED operationState carries its apply-time message into OpErr...
	failed, _ := ParseArgoApp([]byte(`{"metadata":{"name":"kyverno-kyverno"},"status":{"sync":{"status":"OutOfSync"},"health":{"status":"Degraded"},"operationState":{"phase":"Failed","message":"metadata.annotations: Too long"}}}`))
	if failed.OpErr != "metadata.annotations: Too long" {
		t.Errorf("failed-sync OpErr = %q, want the annotation-limit message", failed.OpErr)
	}
	// ...but a Succeeded/Running phase must NOT be treated as an error.
	ok, _ := ParseArgoApp([]byte(`{"metadata":{"name":"x"},"status":{"operationState":{"phase":"Succeeded","message":"successfully synced"}}}`))
	if ok.OpErr != "" {
		t.Errorf("succeeded sync must leave OpErr empty, got %q", ok.OpErr)
	}
}

func TestIsAnnotationLimitError(t *testing.T) {
	if !IsAnnotationLimitError(`CustomResourceDefinition "clusterpolicies.kyverno.io" is invalid: metadata.annotations: Too long: may not be more than 262144 bytes`) {
		t.Error("should match the 256KB annotation-limit sync error")
	}
	if IsAnnotationLimitError("some unrelated sync failure") {
		t.Error("must not match an unrelated error")
	}
	if IsAnnotationLimitError("") {
		t.Error("empty message must not match")
	}
}

func TestReportAddRouting(t *testing.T) {
	var r Report
	r.Add(CatOK, "ignored")
	r.Add(CatFail, "f")
	r.Add(CatPending, "p")
	r.Add(CatDeferred, "d")
	r.Add(CatDrift, "dr")
	if len(r.Failed) != 1 || len(r.Pending) != 1 || len(r.Deferred) != 1 || len(r.Drift) != 1 {
		t.Errorf("routing wrong: %+v", r)
	}
}

// TestIsGitAuthError separates the three ways an Argo ComparisonError can mention
// a failed fetch. Only one of them is terminal, and conflating them is expensive
// in both directions: calling a flake terminal aborts a recoverable run, calling
// a credential refusal transient polls until the budget dies.
func TestIsGitAuthError(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
		want bool
	}{
		// The verbatim message from gsap-apl run 29709276389.
		{"argo 401 on ref discovery",
			"Failed to load target state: failed to generate manifest for source 1 of 1: rpc error: code = Unknown desc = failed to list refs: authentication required: Unauthorized", true},
		{"basic-auth rejection", "failed to list refs: invalid username or password", true},
		{"no credential at all", "could not read Username for 'https://github.com': terminal prompts disabled", true},
		{"ssh key refused", "ssh: handshake failed: ssh: unable to authenticate", true},
		{"token without write", "write access to repository not granted", true},

		// Redis's NOAUTH contains "authentication required" verbatim but is the
		// transient cache split, self-healed by restarting argocd-redis. Claiming it
		// as terminal would abort a run that repairs itself.
		{"redis NOAUTH is not git auth", "failed to list refs: NOAUTH Authentication required.", false},
		{"redis WRONGPASS is not git auth", "failed to list refs: WRONGPASS invalid username-password pair or user is disabled.", false},

		// Genuine flakes and real manifest faults stay out.
		{"network flake", "failed to list refs: dial tcp: i/o timeout", false},
		{"repo not found is ambiguous", "failed to list refs: repository not found", false},
		{"manifest fault", "Failed to compare desired state: is missing required field kind", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsGitAuthError(tc.msg); got != tc.want {
				t.Errorf("IsGitAuthError(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

// TestClassifyArgoApp_GitAuthIsTerminal pins the classification and its guidance.
// The message must tell an operator where to look, because the failure surfaces on
// gitops-global while the actual fault is a credential three hops upstream.
func TestClassifyArgoApp_GitAuthIsTerminal(t *testing.T) {
	a := ArgoApp{
		Name: "gitops-global", Sync: "Unknown", Health: "Healthy", Automated: true,
		SpecErr: "failed to list refs: authentication required: Unauthorized",
	}
	// phase1 must not soften it — the phase says nothing about credentials.
	for _, phase1 := range []bool{false, true} {
		cat, msg := ClassifyArgoApp(a, phase1)
		if cat != CatFail {
			t.Errorf("phase1=%v: category = %v, want CatFail", phase1, cat)
		}
		if !strings.Contains(msg, "TERMINAL") {
			t.Errorf("phase1=%v: message must mark the failure terminal; got %q", phase1, msg)
		}
		if !strings.Contains(msg, "APL_VALUES_REPO_TOKEN") {
			t.Errorf("phase1=%v: message must name the credential to check; got %q", phase1, msg)
		}
	}
}

// TestClassifyArgoApp_RedisSplitStillPolls guards the ordering: the Redis case is
// tested before the git case, so a WRONGPASS/NOAUTH split keeps its self-healing
// pending verdict rather than being reclassified as a terminal git refusal.
func TestClassifyArgoApp_RedisSplitStillPolls(t *testing.T) {
	a := ArgoApp{
		Name: "llz-harbor", Sync: "Unknown", Health: "Healthy", Automated: true,
		SpecErr: "failed to list refs: NOAUTH Authentication required.",
	}
	if cat, msg := ClassifyArgoApp(a, false); cat != CatPending {
		t.Errorf("category = %v (%s), want CatPending — the redis split self-heals", cat, msg)
	}
}
