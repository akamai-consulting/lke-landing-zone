package health

import "testing"

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
		{"otomi team values-gitops deferred (nonexistent sealedsecrets path)", ArgoApp{Name: "team-admin-values-gitops", Sync: "Unknown", Health: "Healthy", SpecErr: "ComparisonError: env/teams/admin/sealedsecrets: app path does not exist", Automated: true}, false, CatDeferred},
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
