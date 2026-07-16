package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── injectRuntimeValues ──────────────────────────────────────────────────────

func TestInjectRuntimeValues_FillsAllFour(t *testing.T) {
	raw := "repo: ${apl_values_repo_password}\n" +
		"dns: ${linode_dns_token}\n" +
		"resolver: \"${coredns_cluster_ip}\"\n" +
		"adminPassword: ${loki_admin_password}\n"
	got, err := injectRuntimeValues(raw, map[string]string{
		"apl_values_repo_password": "PAT",
		"linode_dns_token":         "DNS",
		"coredns_cluster_ip":       "10.0.0.10",
		"loki_admin_password":      "pw20",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"repo: PAT", "dns: DNS", `resolver: "10.0.0.10"`, "adminPassword: pw20"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "${") {
		t.Errorf("a placeholder survived:\n%s", got)
	}
}

func TestInjectRuntimeValues_ErrorsOnUnknownPlaceholder(t *testing.T) {
	// The apl_values_repo_url class of stale placeholder — must hard-fail.
	raw := "url: ${apl_values_repo_url}\npw: ${loki_admin_password}\n"
	_, err := injectRuntimeValues(raw, map[string]string{
		"apl_values_repo_password": "x",
		"linode_dns_token":         "x",
		"coredns_cluster_ip":       "x",
		"loki_admin_password":      "x",
	})
	if err == nil {
		t.Fatal("expected an error for the unknown ${apl_values_repo_url} placeholder")
	}
	if !strings.Contains(err.Error(), "apl_values_repo_url") {
		t.Errorf("error should name the offending placeholder, got: %v", err)
	}
}

func TestInjectRuntimeValues_DeEscapesDollarDollar(t *testing.T) {
	// The values file names its placeholders as $${x} in comments; those are
	// literals that must de-escape to ${x} and NOT trigger the unknown-var guard.
	raw := "# fills $${loki_admin_password}, $${apl_values_repo_password}\n" +
		"adminPassword: ${loki_admin_password}\n"
	got, err := injectRuntimeValues(raw, map[string]string{
		"apl_values_repo_password": "PAT",
		"linode_dns_token":         "DNS",
		"coredns_cluster_ip":       "IP",
		"loki_admin_password":      "pw",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "# fills ${loki_admin_password}, ${apl_values_repo_password}") {
		t.Errorf("escaped comment placeholders not de-escaped to literals:\n%s", got)
	}
	if !strings.Contains(got, "adminPassword: pw") {
		t.Errorf("real placeholder not filled:\n%s", got)
	}
}

// ── assertEnvRevision ────────────────────────────────────────────────────────

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestAssertEnvRevision(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
		wantErr bool
	}{
		{"match-plain", "data:\n  revision: main\n", "main", false},
		{"match-quoted", "data:\n  revision: \"feat/x\"\n", "feat/x", false},
		{"match-single-quoted", "data:\n  revision: 'feat/x'\n", "feat/x", false},
		{"match-whitespace", "data:\n  revision:    main   \n", "main", false},
		{"mismatch", "data:\n  revision: main\n", "feat/x", true},
		{"unparseable", "data:\n  nope: here\n", "main", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := writeTemp(t, "env-revision-configmap.yaml", c.content)
			err := assertEnvRevision(p, c.want)
			if c.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestAssertEnvRevision_MissingFile(t *testing.T) {
	if err := assertEnvRevision(filepath.Join(t.TempDir(), "nope.yaml"), "main"); err == nil {
		t.Fatal("expected an error for a missing configmap file")
	}
}

// ── readCoreDNSClusterIP ─────────────────────────────────────────────────────

func TestReadCoreDNSClusterIP(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		ok      bool
		want    string
		wantErr bool
	}{
		{"ip", "10.128.0.10", true, "10.128.0.10", false},
		{"trims", "  10.0.0.5\n", true, "10.0.0.5", false},
		{"empty", "", true, "", true},
		{"nonzero-exit", "Error from server (NotFound)", false, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := bootstrapDeps{kubectl: func(_ ...string) (string, bool) { return c.out, c.ok }}
			got, err := readCoreDNSClusterIP(d)
			if c.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

// ── existingLokiPassword (first-install vs upgrade) ───────────────────────────

func TestExistingLokiPassword(t *testing.T) {
	cases := []struct {
		name string
		out  string
		ok   bool
		want string
	}{
		{"first-install-no-release", "", false, ""},
		{"upgrade-reuses", "apps:\n  loki:\n    adminPassword: abc123XYZ\n", true, "abc123XYZ"},
		{"ignores-null", "apps:\n  loki:\n    adminPassword: null\n", true, ""},
		{"ignores-unfilled-placeholder", "apps:\n  loki:\n    adminPassword: ${loki_admin_password}\n", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := bootstrapDeps{helm: func(_ ...string) (string, bool) { return c.out, c.ok }}
			if got := existingLokiPassword(d); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestGenLokiPassword(t *testing.T) {
	pw := genLokiPassword()
	if len(pw) != 20 {
		t.Fatalf("want 20 chars, got %d (%q)", len(pw), pw)
	}
	for _, r := range pw {
		if !strings.ContainsRune(lokiPasswordAlphabet, r) {
			t.Fatalf("non-alphanumeric char %q in %q", r, pw)
		}
	}
}

// ── manifest builders (spot checks) ──────────────────────────────────────────

func TestManifestBuilders(t *testing.T) {
	o := bootstrapClusterOpts{
		env:              "primary",
		appsRepoRevision: "feat/x",
		instanceRepo:     "acme/inst",
		upstreamOrg:      "akamai-consulting",
		templateRef:      "v1.2.3",
	}
	app := platformBootstrapApplicationManifest(o)
	src := app["spec"].(map[string]any)["source"].(map[string]any)
	if src["repoURL"] != "https://github.com/acme/inst.git" {
		t.Errorf("bootstrap app repoURL = %v", src["repoURL"])
	}
	if src["targetRevision"] != "feat/x" {
		t.Errorf("bootstrap app targetRevision = %v", src["targetRevision"])
	}
	if src["path"] != "apl-values/primary/manifest" {
		t.Errorf("bootstrap app path = %v", src["path"])
	}
	// The load-bearing retry budget.
	retry := app["spec"].(map[string]any)["syncPolicy"].(map[string]any)["retry"].(map[string]any)
	if retry["limit"] != 40 {
		t.Errorf("retry limit = %v want 40", retry["limit"])
	}

	ss := secretStoreApplicationManifest(o)
	sssrc := ss["spec"].(map[string]any)["source"].(map[string]any)
	if sssrc["repoURL"] != "https://github.com/akamai-consulting/lke-landing-zone.git" {
		t.Errorf("secret-store repoURL = %v", sssrc["repoURL"])
	}
	if sssrc["targetRevision"] != "v1.2.3" {
		t.Errorf("secret-store targetRevision = %v", sssrc["targetRevision"])
	}
	if sssrc["path"] != "platform-apl/manifest-secret-store" {
		t.Errorf("secret-store path = %v", sssrc["path"])
	}

	proj := platformBootstrapAppProjectManifest(o)
	repos := proj["spec"].(map[string]any)["sourceRepos"].([]any)
	if repos[0] != "https://github.com/acme/inst.git" {
		t.Errorf("appproject sourceRepos[0] = %v", repos[0])
	}
}

// ── bootstrapCluster: ordering + happy path ──────────────────────────────────

// recorder is a concurrency-safe ordered call log for the bootstrap flow.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.calls = append(r.calls, s)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recorder) indexOf(substr string) int {
	for i, c := range r.snapshot() {
		if strings.Contains(c, substr) {
			return i
		}
	}
	return -1
}

// bootstrapTestOpts writes the two on-disk inputs (values + env-revision) and
// returns opts pointing at them.
func bootstrapTestOpts(t *testing.T, revision string) bootstrapClusterOpts {
	t.Helper()
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(valuesPath, []byte(
		"apps:\n  loki:\n    adminPassword: ${loki_admin_password}\n    gateway:\n      resolver: \"${coredns_cluster_ip}\"\n"+
			"otomi:\n  git:\n    password: ${apl_values_repo_password}\n"+
			"dns:\n  provider:\n    linode:\n      apiToken: ${linode_dns_token}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	revPath := filepath.Join(dir, "env-revision-configmap.yaml")
	if err := os.WriteFile(revPath, []byte("data:\n  revision: "+revision+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bootstrapClusterOpts{
		env:                "primary",
		aplChartVersion:    "6.1.2",
		appsRepoRevision:   revision,
		instanceRepo:       "acme/inst",
		upstreamOrg:        "akamai-consulting",
		templateRef:        "main",
		valuesPath:         valuesPath,
		envRevisionPath:    revPath,
		aplValuesRepoToken: "PAT",
		linodeDNSToken:     "DNS",
	}
}

func TestBootstrapCluster_HappyPathOrdering(t *testing.T) {
	o := bootstrapTestOpts(t, "main")
	rec := &recorder{}

	d := bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			line := strings.Join(args, " ")
			rec.add("kubectl " + line)
			if strings.Contains(line, "service coredns") {
				return "10.0.0.10", true
			}
			// kyverno policy apply (applyKyvernoPolicy uses kubectl apply -f <path>)
			if len(args) > 0 && args[0] == "apply" {
				return "", true
			}
			// gate existence + waits, kyverno readiness → all succeed immediately.
			return "", true
		},
		apply: func(_ string, fieldManager string, _ bool) (string, bool) {
			rec.add("apply-inline")
			return "", true
		},
		helm: func(args ...string) (string, bool) {
			line := strings.Join(args, " ")
			rec.add("helm " + line)
			if len(args) > 1 && args[0] == "get" {
				return "", false // no existing release → first install
			}
			return "", true
		},
		now:         time.Now,
		sleep:       func(time.Duration) {},
		genPassword: func() string { return "generated-pw-20chars" },
	}

	if err := bootstrapCluster(o, d); err != nil {
		t.Fatalf("bootstrapCluster: %v", err)
	}

	calls := rec.snapshot()
	// coredns read is first; helm upgrade uses the pinned version; a helm install
	// happened after the namespace/SC applies; the bridge applies happen last.
	if rec.indexOf("service coredns") != 0 {
		t.Errorf("coredns read should be first, got calls:\n%s", strings.Join(calls, "\n"))
	}
	if rec.indexOf("upgrade --install apl") < 0 {
		t.Errorf("expected a helm upgrade --install; calls:\n%s", strings.Join(calls, "\n"))
	}
	// The two inline SSA namespaces + SC come before helm; the 3 bridge applies come after the gate.
	firstApply := rec.indexOf("apply-inline")
	helmIdx := rec.indexOf("upgrade --install apl")
	if firstApply < 0 || helmIdx < 0 || firstApply > helmIdx {
		t.Errorf("expected namespace/SC applies before helm install (firstApply=%d helm=%d)", firstApply, helmIdx)
	}
	// helm upgrade must carry the pinned version.
	if rec.indexOf("--version 6.1.2") < 0 {
		t.Errorf("helm upgrade missing --version 6.1.2; calls:\n%s", strings.Join(calls, "\n"))
	}
}

// TestBootstrapCluster_KyvernoRacesAheadOfGate is the key regression guard: the
// Kyverno policy applies must be dispatched CONCURRENTLY with the readiness gate,
// not serialized after it. The fake blocks the gate's first stage (argo CRD
// existence) until a Kyverno policy has been applied; if the flow ever serializes
// the gate before the policies, the gate blocks forever and this test times out.
func TestBootstrapCluster_KyvernoRacesAheadOfGate(t *testing.T) {
	o := bootstrapTestOpts(t, "main")
	rec := &recorder{}

	kyvernoApplied := make(chan struct{})
	var once sync.Once
	markApplied := func() { once.Do(func() { close(kyvernoApplied) }) }

	d := bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			line := strings.Join(args, " ")
			if strings.Contains(line, "service coredns") {
				return "10.0.0.10", true
			}
			if len(args) > 0 && args[0] == "apply" {
				// kyverno policy apply
				rec.add("kyverno-apply")
				markApplied()
				return "", true
			}
			// The gate's first existence check is on applications.argoproj.io and is
			// unique to the gate (Kyverno never references it). Block it until a
			// Kyverno policy applies — proving the two run concurrently.
			if strings.Contains(line, "applications.argoproj.io") {
				<-kyvernoApplied
			}
			return "", true
		},
		apply: func(_ string, _ string, _ bool) (string, bool) { return "", true },
		helm: func(args ...string) (string, bool) {
			if len(args) > 1 && args[0] == "get" {
				return "", false
			}
			return "", true
		},
		now:         time.Now,
		sleep:       func(time.Duration) {},
		genPassword: func() string { return "pw" },
	}

	done := make(chan error, 1)
	go func() { done <- bootstrapCluster(o, d) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bootstrapCluster: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bootstrapCluster deadlocked — the Kyverno policies were serialized AFTER the readiness gate instead of racing ahead of it (fidelity regression)")
	}
	if rec.indexOf("kyverno-apply") < 0 {
		t.Fatal("no Kyverno policy was applied")
	}
}

// TestBootstrapCluster_GHCRSecretsGatedOnToken asserts the optional GHCR secrets
// are applied only when a token is present (the count-guard port).
func TestBootstrapCluster_GHCRSecretsGatedOnToken(t *testing.T) {
	countApplies := func(o bootstrapClusterOpts) int {
		applies := 0
		var mu sync.Mutex
		d := bootstrapDeps{
			kubectl: func(args ...string) (string, bool) {
				if strings.Contains(strings.Join(args, " "), "service coredns") {
					return "10.0.0.10", true
				}
				return "", true
			},
			apply: func(stdinYAML, _ string, _ bool) (string, bool) {
				if strings.Contains(stdinYAML, "ghcr") {
					mu.Lock()
					applies++
					mu.Unlock()
				}
				return "", true
			},
			helm:        func(_ ...string) (string, bool) { return "", true },
			now:         time.Now,
			sleep:       func(time.Duration) {},
			genPassword: func() string { return "pw" },
		}
		if err := bootstrapCluster(o, d); err != nil {
			t.Fatalf("bootstrapCluster: %v", err)
		}
		return applies
	}

	noToken := bootstrapTestOpts(t, "main")
	if got := countApplies(noToken); got != 0 {
		t.Errorf("no GHCR token: want 0 ghcr applies, got %d", got)
	}
	withToken := bootstrapTestOpts(t, "main")
	withToken.ghcrToken = "ghp_x"
	withToken.ghcrUsername = "bot"
	if got := countApplies(withToken); got != 2 {
		t.Errorf("with GHCR token: want 2 ghcr applies (repo + pull secret), got %d", got)
	}
}
