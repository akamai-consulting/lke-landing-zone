package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── runCapture (the production exec seam) ────────────────────────────────────

// Regression: `return buf.String(), cmd.Run() == nil` evaluates left-to-right,
// snapshotting the buffer BEFORE the command runs — every kubectl/helm call
// returned "" on the e2e bootstrap (misread as an empty kubeconfig). runCapture
// must return the output the run itself produced, on success AND failure.
func TestRunCombined_OutputAfterRun(t *testing.T) {
	out, ok := runCombined(exec.Command("sh", "-c", "echo to-stdout; echo to-stderr >&2"))
	if !ok {
		t.Fatalf("runCombined(exit 0) reported failure (out=%q)", out)
	}
	if !strings.Contains(out, "to-stdout") || !strings.Contains(out, "to-stderr") {
		t.Fatalf("runCombined returned output snapshotted before the run (eval-order regression): %q", out)
	}

	out, ok = runCombined(exec.Command("sh", "-c", "echo boom >&2; exit 3"))
	if ok {
		t.Fatal("runCombined(exit 3) reported success")
	}
	if !strings.Contains(out, "boom") {
		t.Fatalf("runCombined must capture output of a FAILING run too (diagnostics depend on it): %q", out)
	}
}

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

// dnsServicesJSON mimics `kubectl get services -n kube-system -o json` on LKE-E:
// the DNS Service (port 53) plus the sibling metrics Service (port 9153).
const dnsServicesJSON = `{"items":[
  {"metadata":{"name":"coredns"},"spec":{"clusterIP":"10.3.192.10","ports":[{"port":53},{"port":53}]}},
  {"metadata":{"name":"workload-coredns-metrics"},"spec":{"clusterIP":"10.3.200.6","ports":[{"port":9153}]}}
]}`

func TestDnsClusterIPFromServicesJSON(t *testing.T) {
	if got := dnsClusterIPFromServicesJSON(dnsServicesJSON); got != "10.3.192.10" {
		t.Errorf("port-53 Service ClusterIP = %q, want 10.3.192.10 (must exclude the :9153 metrics svc)", got)
	}
	if got := dnsClusterIPFromServicesJSON(`{"items":[]}`); got != "" {
		t.Errorf("no services: got %q want empty", got)
	}
	if got := dnsClusterIPFromServicesJSON("not json"); got != "" {
		t.Errorf("garbage: got %q want empty", got)
	}
	// Headless DNS (clusterIP None) is not usable.
	if got := dnsClusterIPFromServicesJSON(`{"items":[{"spec":{"clusterIP":"None","ports":[{"port":53}]}}]}`); got != "" {
		t.Errorf("headless: got %q want empty", got)
	}
}

func TestReadCoreDNSClusterIP(t *testing.T) {
	// Zero the budget so the miss cases try once then give up (no real waiting).
	orig := coreDNSReadBudget
	coreDNSReadBudget = 0
	t.Cleanup(func() { coreDNSReadBudget = orig })

	cases := []struct {
		name string
		out  string
		ok   bool
		want string
	}{
		{"resolves-from-json", dnsServicesJSON, true, "10.3.192.10"},
		{"empty-list-non-fatal", `{"items":[]}`, true, ""}, // NON-FATAL: warns + returns ""
		{"kubectl-fails-non-fatal", "", false, ""},         // NON-FATAL
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := bootstrapDeps{
				kubectl: func(_ ...string) (string, bool) { return c.out, c.ok },
				now:     time.Now,
				sleep:   func(time.Duration) {},
			}
			if got := readCoreDNSClusterIP(d); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

// The Flux-managed CoreDNS can lag: the first list has no port-53 Service (or no
// ClusterIP), so the loop must retry until it appears.
func TestReadCoreDNSClusterIP_RetriesUntilAssigned(t *testing.T) {
	orig := coreDNSReadBudget
	coreDNSReadBudget = time.Minute
	t.Cleanup(func() { coreDNSReadBudget = orig })

	calls := 0
	d := bootstrapDeps{
		kubectl: func(_ ...string) (string, bool) {
			calls++
			if calls < 2 {
				return `{"items":[]}`, true // no DNS Service yet
			}
			return dnsServicesJSON, true
		},
		now:   time.Now,
		sleep: func(time.Duration) {},
	}
	if got := readCoreDNSClusterIP(d); got != "10.3.192.10" {
		t.Errorf("got %q want 10.3.192.10", got)
	}
	if calls < 2 {
		t.Errorf("expected a retry (>=2 calls), got %d", calls)
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

// TestDefaultAplChartVersion guards the e2e-critical fallback: spec.cluster.
// bootstrap.aplChartVersion is OPTIONAL, and the release-e2e instance (env add
// --region --obj-cluster only) never sets it. The retired cluster-bootstrap
// terraform.tfvars.example pinned apl_chart_version = "6.0.0" as the default, so
// bootstrap-cluster must fall back to that same value or the whole e2e fails at
// the helm install with "apl chart version unresolved".
func TestDefaultAplChartVersion(t *testing.T) {
	if defaultAplChartVersion != "6.0.0" {
		t.Errorf("defaultAplChartVersion = %q, want \"6.0.0\" (the retired tfvars.example default) — bump deliberately, in lockstep with the platform baseline", defaultAplChartVersion)
	}
	// The final resolution is firstNonEmpty(flag, spec, default): an unset flag +
	// unset spec must resolve to the baked default, never "".
	if got := firstNonEmpty("", "", defaultAplChartVersion); got == "" {
		t.Fatal("chart-version resolution must never be empty when the default is set")
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
			"otomi:\n  git:\n    password: ${apl_values_repo_password}\n    repoUrl: https://github.com/acme/inst.git\n    branch: apl-primary\n"+
			"dns:\n  provider:\n    linode:\n      apiToken: ${linode_dns_token}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	revPath := filepath.Join(dir, "env-revision-configmap.yaml")
	if err := os.WriteFile(revPath, []byte("data:\n  revision: "+revision+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bootstrapClusterOpts{
		env:                "primary",
		clusterID:          "393244",
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
			if strings.Contains(line, "get services") && strings.Contains(line, "json") {
				return dnsServicesJSON, true
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
		git: func(args ...string) (string, bool) {
			rec.add("git " + args[0])
			return "deadbeefsha\trefs/heads/apl-primary", true
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
	if rec.indexOf("get services") != 0 {
		t.Errorf("coredns read should be first, got calls:\n%s", strings.Join(calls, "\n"))
	}
	if rec.indexOf("upgrade --install apl") < 0 {
		t.Errorf("expected a helm upgrade --install; calls:\n%s", strings.Join(calls, "\n"))
	}
	// The values-branch ensure MUST run BEFORE the helm install: apl-operator's
	// installer phase (started by the chart) is the only phase that bootstraps the
	// full env values into the branch — install-before-branch wedges the cluster
	// (installation completed, branch empty, every reconcile crashing).
	if gi, hi := rec.indexOf("git ls-remote"), rec.indexOf("upgrade --install apl"); gi < 0 || gi > hi {
		t.Errorf("values-branch ensure (git ls-remote, idx %d) must precede the helm install (idx %d); calls:\n%s", gi, hi, strings.Join(calls, "\n"))
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

// TestBootstrapCluster_BridgeAppliesForceConflicts guards the reused-/migrated-
// cluster SSA fix: the Argo bridge (AppProject + platform-bootstrap Application +
// llz-secret-store Application) MUST apply with --force-conflicts. The old
// cluster-bootstrap TF applied these with the kubectl provider's default field
// manager "kubectl"; our manager is "cluster-bootstrap-tf", and llz-secret-store's
// targetRevision is the template-ref SHA (changes every push) — so a plain SSA
// conflicts on .spec.source.targetRevision and the bootstrap dies at the last step
// (observed on e2e cluster 632033). Every bridge apply carrying an argoproj.io
// object must set force=true.
func TestBootstrapCluster_BridgeAppliesForceConflicts(t *testing.T) {
	o := bootstrapTestOpts(t, "main")

	type applyCall struct {
		yaml  string
		force bool
	}
	var mu sync.Mutex
	var applies []applyCall

	d := bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			line := strings.Join(args, " ")
			if strings.Contains(line, "get services") && strings.Contains(line, "json") {
				return dnsServicesJSON, true
			}
			return "", true
		},
		apply: func(yaml, _ string, force bool) (string, bool) {
			mu.Lock()
			applies = append(applies, applyCall{yaml, force})
			mu.Unlock()
			return "", true
		},
		helm: func(args ...string) (string, bool) {
			if len(args) > 1 && args[0] == "get" {
				return "", false // first install
			}
			return "", true
		},
		git:         func(_ ...string) (string, bool) { return "deadbeefsha\trefs/heads/apl-primary", true },
		now:         time.Now,
		sleep:       func(time.Duration) {},
		genPassword: func() string { return "generated-pw-20chars" },
	}

	if err := bootstrapCluster(o, d); err != nil {
		t.Fatalf("bootstrapCluster: %v", err)
	}

	// Every apply of an argoproj.io object (the bridge AppProject + Applications)
	// must have forced conflicts.
	sawBridge := false
	for _, c := range applies {
		if !strings.Contains(c.yaml, "argoproj.io") {
			continue
		}
		sawBridge = true
		if !c.force {
			t.Errorf("bridge apply did not force conflicts (would die on a reused/migrated cluster):\n%s", c.yaml)
		}
	}
	if !sawBridge {
		t.Fatal("no argoproj.io bridge object was applied — the Argo bridge never ran")
	}
}

// TestHelmInstallApl_SkipsWhenAlreadyAtTargetVersion is the reused-cluster safety
// guard: when apl is already `deployed` at the target chart version, helmInstallApl
// must NOT run `helm upgrade`. Re-asserting the release rolls apl-operator, resets
// its 10-15m helmfile clock, and (under branch-isolation) delays the apl-<env>
// branch push the gitops-* Apps need — timing out the convergence gate on a reused
// cluster.
func TestHelmInstallApl_SkipsWhenAlreadyAtTargetVersion(t *testing.T) {
	o := bootstrapClusterOpts{aplChartVersion: "6.0.0"}
	upgraded := false
	d := bootstrapDeps{
		helm: func(args ...string) (string, bool) {
			switch args[0] {
			case "list":
				return `[{"name":"apl","chart":"apl-6.0.0","status":"deployed"}]`, true
			case "get": // get values -o yaml — same values, different formatting/order
				return "b: 2\na: 1\n", true
			case "upgrade":
				upgraded = true
			}
			return "", true
		},
	}
	if _, err := helmInstallApl(d, o, "a: 1\nb: 2\n"); err != nil {
		t.Fatalf("helmInstallApl: %v", err)
	}
	if upgraded {
		t.Error("must NOT helm upgrade when apl is already deployed at the target version with identical values (rolls apl-operator on a reused cluster)")
	}

	// Same version but CHANGED values → must upgrade (a values-only fix like the
	// loki memberlist publishNotReadyAddresses change has to reach the operator).
	upgraded = false
	d.helm = func(args ...string) (string, bool) {
		switch args[0] {
		case "list":
			return `[{"name":"apl","chart":"apl-6.0.0","status":"deployed"}]`, true
		case "get":
			return "a: 1\n", true
		case "upgrade":
			upgraded = true
		}
		return "", true
	}
	if _, err := helmInstallApl(d, o, "a: 1\nb: 2\n"); err != nil {
		t.Fatalf("helmInstallApl(values changed): %v", err)
	}
	if !upgraded {
		t.Error("same version + CHANGED values must upgrade — a values-only change would otherwise never reach apl-operator on a reused cluster")
	}

	// Same version, values unreadable → bias to skip (don't roll on a blip).
	upgraded = false
	d.helm = func(args ...string) (string, bool) {
		switch args[0] {
		case "list":
			return `[{"name":"apl","chart":"apl-6.0.0","status":"deployed"}]`, true
		case "get":
			return "", false
		case "upgrade":
			upgraded = true
		}
		return "", true
	}
	if _, err := helmInstallApl(d, o, "a: 1\n"); err != nil {
		t.Fatalf("helmInstallApl(values unreadable): %v", err)
	}
	if upgraded {
		t.Error("unreadable current values must bias to SKIP, not roll apl-operator")
	}
}

// TestHelmInstallApl_UpgradesWhenNeeded covers the cases that MUST still install/
// upgrade: no release, a different deployed version (the spec-driven upgrade path),
// and a non-`deployed` status (a half-applied prior run that must self-heal).
func TestHelmInstallApl_UpgradesWhenNeeded(t *testing.T) {
	cases := []struct{ name, list string }{
		{"absent", `[]`},
		{"version mismatch (spec bump)", `[{"name":"apl","chart":"apl-5.9.0","status":"deployed"}]`},
		{"pending state self-heals", `[{"name":"apl","chart":"apl-6.0.0","status":"pending-upgrade"}]`},
		{"unparseable list output", `not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := bootstrapClusterOpts{aplChartVersion: "6.0.0"}
			upgraded := false
			d := bootstrapDeps{
				helm: func(args ...string) (string, bool) {
					switch args[0] {
					case "list":
						return tc.list, true
					case "upgrade":
						upgraded = true
					}
					return "", true
				},
			}
			if _, err := helmInstallApl(d, o, "rendered-values"); err != nil {
				t.Fatalf("helmInstallApl: %v", err)
			}
			if !upgraded {
				t.Errorf("%s: expected helm upgrade to run", tc.name)
			}
		})
	}
}

// ── apl-values branch-readiness wait ─────────────────────────────────────────

func TestAplValuesGitCoords(t *testing.T) {
	rendered := "otomi:\n  git:\n    repoUrl: https://github.com/acme/inst.git\n    branch: apl-e2e\n    password: secret\n"
	repo, branch := aplValuesGitCoords(rendered)
	if repo != "https://github.com/acme/inst.git" || branch != "apl-e2e" {
		t.Fatalf("coords = (%q,%q)", repo, branch)
	}
	// Absent otomi.git → empty (caller skips the wait), never a panic.
	if r, b := aplValuesGitCoords("apps:\n  loki: {}\n"); r != "" || b != "" {
		t.Errorf("absent coords = (%q,%q), want empty", r, b)
	}
	if r, b := aplValuesGitCoords("not: [valid"); r != "" || b != "" {
		t.Errorf("unparseable coords = (%q,%q), want empty", r, b)
	}
}

func TestAuthedGitURL(t *testing.T) {
	if got := authedGitURL("https://github.com/acme/inst.git", "PAT"); got != "https://x-access-token:PAT@github.com/acme/inst.git" {
		t.Errorf("authedGitURL = %q", got)
	}
	// No token / non-https → unchanged.
	if got := authedGitURL("https://github.com/acme/inst.git", ""); got != "https://github.com/acme/inst.git" {
		t.Errorf("no-token URL changed: %q", got)
	}
	if got := authedGitURL("git@github.com:acme/inst.git", "PAT"); got != "git@github.com:acme/inst.git" {
		t.Errorf("non-https URL changed: %q", got)
	}
}

func TestEnsureAplValuesBranch(t *testing.T) {
	o := bootstrapClusterOpts{aplValuesRepoToken: "PAT"}

	// No coords → clean skip, never touches git.
	called := false
	d := bootstrapDeps{git: func(_ ...string) (string, bool) { called = true; return "", true }}
	if seedSHA, err := ensureAplValuesBranch(d, o, "", ""); err != nil || called || seedSHA != "" {
		t.Fatalf("empty coords should skip: err=%v called=%v", err, called)
	}

	// git subcommand, skipping global flag pairs (`-C <dir>`, `-c <kv>`).
	gitSub := func(args []string) string {
		for i := 0; i < len(args); {
			switch args[i] {
			case "-C", "-c":
				i += 2
			default:
				return args[i]
			}
		}
		return ""
	}

	// Branch already exists (ls-remote returns a SHA) → no clone/push, authed URL used.
	var lsURL, lsRef string
	created := false
	d.git = func(args ...string) (string, bool) {
		switch gitSub(args) {
		case "ls-remote":
			lsURL, lsRef = args[1], args[2]
			return "abc123\trefs/heads/apl-e2e", true // present
		case "clone", "checkout", "push":
			created = true
		}
		return "", true
	}
	if seedSHA, err := ensureAplValuesBranch(d, o, "https://github.com/acme/inst.git", "apl-e2e"); err != nil || seedSHA != "" {
		t.Fatalf("existing branch should be a no-op (seedSHA=%q): %v", seedSHA, err)
	}
	if created {
		t.Error("must NOT create a branch that already exists")
	}
	if lsURL != "https://x-access-token:PAT@github.com/acme/inst.git" || lsRef != "refs/heads/apl-e2e" {
		t.Errorf("ls-remote called with (%q,%q)", lsURL, lsRef)
	}

	// Branch absent → seed an EMPTY orphan branch: init → empty commit → push.
	// (NOT a clone of the default branch: apl-operator applies the branch content
	// as its otomi env repo, and an instance-repo copy crashes its derived-values
	// template — the customRootCA e2e failure.)
	var seq []string
	var pushArgs []string
	d.git = func(args ...string) (string, bool) {
		sub := gitSub(args)
		seq = append(seq, sub)
		if sub == "push" {
			pushArgs = args
		}
		if sub == "commit" && !strings.Contains(strings.Join(args, " "), "--allow-empty") {
			t.Errorf("seed commit must be --allow-empty (no content!), got %v", args)
		}
		return "", true // ls-remote empty = absent; rest succeed
	}
	if seedSHA, err := ensureAplValuesBranch(d, o, "https://github.com/acme/inst.git", "apl-e2e"); err != nil || seedSHA == "" {
		t.Fatalf("absent branch should be seeded (seedSHA=%q): %v", seedSHA, err)
	}
	if strings.Join(seq, ",") != "ls-remote,init,commit,push,rev-parse" {
		t.Errorf("seed sequence = %v, want ls-remote,init,commit,push,rev-parse (EMPTY orphan seed + sha capture)", seq)
	}
	if !strings.Contains(strings.Join(pushArgs, " "), "HEAD:refs/heads/apl-e2e") {
		t.Errorf("push must target refs/heads/apl-e2e, got %v", pushArgs)
	}

	// A push failure surfaces LOUD, names the token, and does NOT leak the secret.
	d.git = func(args ...string) (string, bool) {
		switch gitSub(args) {
		case "ls-remote":
			return "", true // absent
		case "push":
			return "remote: Permission denied to https://x-access-token:PAT@github.com/acme/inst.git", false
		}
		return "", true
	}
	_, err := ensureAplValuesBranch(d, o, "https://github.com/acme/inst.git", "apl-e2e")
	if err == nil || !strings.Contains(err.Error(), "Contents:write") {
		t.Errorf("expected a loud push error naming the token, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "PAT") {
		t.Errorf("push error leaked the token: %v", err)
	}
}

// TestBootstrapCluster_ReseededBranchReArmsInstaller guards the reused-cluster
// composition: when the values branch had to be RE-seeded (a fresh instantiate
// deleted it) AND the helm install was skipped (apl already deployed), the
// running operator is reconcile-only (apl-installation-status: completed) and
// can never repopulate the empty branch — bootstrap-cluster must re-arm the
// installer (patch status→pending + rollout restart). Without both conditions
// the reset must NOT run.
func TestBootstrapCluster_ReseededBranchReArmsInstaller(t *testing.T) {
	run := func(branchExists, deployed, valuesChanged bool) (patched, restarted bool) {
		o := bootstrapTestOpts(t, "main")
		gitPushed := false
		d := bootstrapDeps{
			kubectl: func(args ...string) (string, bool) {
				line := strings.Join(args, " ")
				if strings.Contains(line, "get services") && strings.Contains(line, "json") {
					return dnsServicesJSON, true
				}
				if strings.Contains(line, "get configmap apl-installation-status") {
					if deployed {
						return "completed", true // reused cluster: installer done → reconcile-only
					}
					return "", false // fresh cluster: no status cm yet
				}
				if strings.Contains(line, "patch configmap apl-installation-status") {
					patched = true
				}
				if strings.Contains(line, "rollout restart deployment/apl-operator") {
					restarted = true
				}
				return "", true
			},
			apply: func(_, _ string, _ bool) (string, bool) { return "", true },
			helm: func(args ...string) (string, bool) {
				if args[0] == "list" && deployed {
					return `[{"name":"apl","chart":"apl-6.1.2","status":"deployed"}]`, true
				}
				if args[0] == "list" {
					return `[]`, true
				}
				if args[0] == "get" && strings.Contains(strings.Join(args, " "), "-o yaml") {
					if valuesChanged {
						return "stale: true\n", true // differs from the render → upgrade
					}
					return "", false // unreadable → bias to skip (values treated as equal)
				}
				if args[0] == "get" {
					return "", false // existingLokiPassword probe
				}
				return "", true
			},
			git: func(args ...string) (string, bool) {
				sub := args[0]
				if sub == "-C" { // git -C <dir> [-c k=v]... <subcmd>
					for j := 0; j < len(args); {
						if args[j] == "-C" || args[j] == "-c" {
							j += 2
							continue
						}
						sub = args[j]
						break
					}
				}
				switch sub {
				case "ls-remote":
					if branchExists {
						return "abc\trefs/heads/apl-primary", true
					}
					if gitPushed {
						// post-seed: the ref resolves and (fake) moves past the seed sha,
						// standing in for the operator's env push — the populated-wait exits.
						return "operatorsha\trefs/heads/apl-primary", true
					}
					return "", true // absent → seed path
				case "push":
					gitPushed = true
				case "rev-parse":
					return "seedsha", true
				}
				return "", true
			},
			now:         time.Now,
			sleep:       func(time.Duration) {},
			genPassword: func() string { return "pw" },
		}
		if err := bootstrapCluster(o, d); err != nil {
			t.Fatalf("bootstrapCluster(branchExists=%v deployed=%v): %v", branchExists, deployed, err)
		}
		return patched, restarted
	}

	// Re-seeded + install skipped → MUST re-arm.
	if p, r := run(false, true, false); !p || !r {
		t.Errorf("reseeded+skipped: patched=%v restarted=%v, want both true", p, r)
	}
	// Fresh install (seeded but helm installed) → installer status probe answers
	// not-found → the reset no-ops on its own.
	if p, r := run(false, false, false); p || r {
		t.Errorf("seeded+fresh-install: patched=%v restarted=%v, want both false", p, r)
	}
	// Reused cluster, branch intact, values unchanged → nothing to fix, no reset.
	if p, r := run(true, true, false); p || r {
		t.Errorf("existing-branch+skipped: patched=%v restarted=%v, want both false", p, r)
	}
	// Reused cluster, branch intact, VALUES CHANGED (case 3: downstream instance
	// re-apply with a rotated token / new shared values) → the upgrade alone does
	// not make reconcile-mode ingest VALUES_INPUT — the installer must be re-armed
	// so the change propagates deterministically.
	if p, r := run(true, true, true); !p || !r {
		t.Errorf("existing-branch+values-upgrade: patched=%v restarted=%v, want both true", p, r)
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
			if strings.Contains(line, "get services") && strings.Contains(line, "json") {
				return dnsServicesJSON, true
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
		git:         func(_ ...string) (string, bool) { return "deadbeefsha\trefs/heads/apl-primary", true },
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
				if strings.Contains(strings.Join(args, " "), "get services") && strings.Contains(strings.Join(args, " "), "json") {
					return dnsServicesJSON, true
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
			git:         func(_ ...string) (string, bool) { return "deadbeefsha\trefs/heads/apl-primary", true },
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

// ── renderBlockStorageClass (rebase adaptation: TF templatefile → Go render) ──

// The lke<id> ownership tag must be rendered into the class's volumeTags from the
// explicit cluster id (--cluster-id / $LKE_CLUSTER_ID, threaded from the cluster
// workspace's cluster_id output); the CSI CreateVolume call then carries it, which
// is the whole basis for reap's cluster-liveness attribution.
func TestRenderBlockStorageClass_InjectsLKETag(t *testing.T) {
	got, err := renderBlockStorageClass("393244")
	if err != nil {
		t.Fatal(err)
	}
	const want = `linodebs.csi.linode.com/volumeTags: "block-storage,platform-support-services,lke393244"`
	if !strings.Contains(got, want) {
		t.Errorf("rendered class missing %q\n---\n%s", want, got)
	}
	if strings.Contains(got, "${") {
		t.Errorf("rendered class still has an unrendered ${...} placeholder:\n%s", got)
	}
}

// An already-prefixed id is normalized (not doubled): lke393244 -> lke393244, not
// lkelke393244.
func TestRenderBlockStorageClass_StripsOptionalLKEPrefix(t *testing.T) {
	got, err := renderBlockStorageClass("lke393244")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `,lke393244"`) || strings.Contains(got, "lkelke") {
		t.Errorf("prefixed id not normalized:\n%s", got)
	}
}

// HARD-FAIL on an empty id: a StorageClass without the lke<id> tag provisions
// un-reapable Volumes, and its params are immutable — so the bootstrap must refuse
// rather than ship a silently-untagged class.
func TestRenderBlockStorageClass_EmptyIDHardFails(t *testing.T) {
	if _, err := renderBlockStorageClass("   "); err == nil {
		t.Fatal("expected an error for an empty cluster id, got nil")
	}
}

// A non-numeric id would render a malformed lke<id> tag that reap's parser
// (`^lke-?[0-9]+$`) can't attribute — reject it up front.
func TestRenderBlockStorageClass_MalformedIDHardFails(t *testing.T) {
	if _, err := renderBlockStorageClass("us-ord-1"); err == nil {
		t.Fatal("expected an error for a non-numeric cluster id, got nil")
	}
}
