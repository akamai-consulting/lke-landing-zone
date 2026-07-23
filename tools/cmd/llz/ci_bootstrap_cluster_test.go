package main

import (
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// ── runCombined (the production exec seam) ───────────────────────────────────

// Regression: `return buf.String(), cmd.Run() == nil` evaluates left-to-right,
// snapshotting the buffer BEFORE the command runs — every kubectl call
// returned "" on the e2e bootstrap (misread as an empty kubeconfig). runCombined
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

// TestDefaultAplChartVersion pins the platform baseline other tooling asserts
// against (ci_assert_apl_version.go). On a managed cluster Linode owns the
// apl-core version, so bootstrap does not consume this — but the constant is still
// the single baseline; bump it deliberately, in lockstep with the platform.
func TestDefaultAplChartVersion(t *testing.T) {
	if defaultAplChartVersion != "6.0.0" {
		t.Errorf("defaultAplChartVersion = %q, want \"6.0.0\" — bump deliberately, in lockstep with the platform baseline", defaultAplChartVersion)
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

// TestLlzOpenbaoNamespaceManifest — the managed bootstrap pre-creates the
// llz-openbao namespace (managed apl-core does not) with the restricted PSS +
// monitoring labels and the bootstrap marker, so the OpenBao seal-key seed lands
// without waiting on a namespace that would otherwise never be created.
func TestLlzOpenbaoNamespaceManifest(t *testing.T) {
	m := llzOpenbaoNamespaceManifest()
	if m["kind"] != "Namespace" {
		t.Fatalf("kind = %v, want Namespace", m["kind"])
	}
	meta := m["metadata"].(map[string]any)
	if meta["name"] != "llz-openbao" {
		t.Errorf("name = %v, want llz-openbao", meta["name"])
	}
	labels := meta["labels"].(map[string]any)
	for k, want := range map[string]string{
		"pod-security.kubernetes.io/enforce": "restricted",
		"monitoring":                         "enabled",
		managedByBootstrapLabel:              "true",
	} {
		if labels[k] != want {
			t.Errorf("label %s = %v, want %s", k, labels[k], want)
		}
	}
}

// TestBootstrapCluster_AppliesOnlyBridge asserts the managed path layers EXACTLY
// the three Argo bridge manifests (plus the SC + namespace) onto the managed
// ArgoCD — and, structurally, has no helm/git seam to self-install with, since
// Linode owns apl-core. See ADR 0005 option A.
func TestBootstrapCluster_AppliesOnlyBridge(t *testing.T) {
	o := bootstrapClusterOpts{
		env: "primary", instanceRepo: "acme/instance",
		upstreamOrg: "akamai-consulting", templateRef: "abc123",
		appsRepoRevision: "main",
	}
	var applied, mgrs []string
	d := bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			line := strings.Join(args, " ")
			if strings.Contains(line, "crd applications.argoproj.io") {
				return "applications.argoproj.io", true
			}
			if strings.Contains(line, "deploy argocd-server") {
				return "1", true // availableReplicas
			}
			return "", true
		},
		apply: func(y, mgr string, _ bool) (string, bool) {
			applied = append(applied, y)
			mgrs = append(mgrs, mgr)
			return "", true
		},
		now:   time.Now,
		sleep: func(time.Duration) {},
	}
	if err := bootstrapCluster(o, d); err != nil {
		t.Fatalf("bootstrapCluster: %v", err)
	}
	// Separate the bridge manifests from the block-storage-retain SC + namespace apply.
	var bridge []string
	for _, y := range applied {
		if strings.Contains(y, "kind: StorageClass") || strings.Contains(y, "kind: Namespace") {
			continue
		}
		bridge = append(bridge, y)
	}
	if len(bridge) != 3 {
		t.Fatalf("want 3 bridge applies (AppProject + 2 Applications), got %d", len(bridge))
	}
	joined := strings.Join(bridge, "\n---\n")
	for _, want := range []string{"kind: AppProject", "platform-bootstrap", "llz-secret-store", "apl-values/primary/manifest"} {
		if !strings.Contains(joined, want) {
			t.Errorf("bridge manifests missing %q; got:\n%s", want, joined)
		}
	}
	for _, m := range mgrs {
		if m != "llz-managed-bridge" {
			t.Errorf("field manager = %q, want llz-managed-bridge", m)
		}
	}
}

// TestBootstrapCluster_AppliesInstanceRepoSecret: with APL_VALUES_REPO_TOKEN set,
// the managed path applies an ArgoCD repository Secret for the private instance repo
// (breaking the platform-bootstrap "authentication required" deadlock on managed);
// with no token it applies none (public-repo path).
func TestBootstrapCluster_AppliesInstanceRepoSecret(t *testing.T) {
	run := func(token string) []string {
		o := bootstrapClusterOpts{
			env: "primary", instanceRepo: "acme/instance",
			upstreamOrg: "akamai-consulting", templateRef: "ref", appsRepoRevision: "main",
			instanceRepoToken: token,
		}
		var applied []string
		d := bootstrapDeps{
			kubectl: func(args ...string) (string, bool) {
				line := strings.Join(args, " ")
				if strings.Contains(line, "crd applications.argoproj.io") {
					return "applications.argoproj.io", true
				}
				if strings.Contains(line, "deploy argocd-server") {
					return "1", true
				}
				return "", true
			},
			apply: func(y, _ string, _ bool) (string, bool) { applied = append(applied, y); return "", true },
			// configureManagedApl runs when a token is set; the kubectl stub above
			// returns no apl-git-config repoUrl, so it warns-and-continues (best-effort)
			// without reaching the migration Job — this test only asserts the bridge.
			now: time.Now, sleep: func(time.Duration) {},
		}
		if err := bootstrapCluster(o, d); err != nil {
			t.Fatalf("bootstrapCluster: %v", err)
		}
		return applied
	}
	repoSecret := func(applied []string) string {
		for _, y := range applied {
			if strings.Contains(y, "secret-type: repository") && strings.Contains(y, "acme/instance") {
				return y
			}
		}
		return ""
	}
	// With a token: the repository Secret is applied with the token as password.
	sec := repoSecret(run("test-repo-token"))
	if sec == "" {
		t.Fatal("managed path with APL_VALUES_REPO_TOKEN must apply an instance-repo repository Secret")
	}
	for _, want := range []string{"type: git", "url: https://github.com/acme/instance.git", "password: test-repo-token"} {
		if !strings.Contains(sec, want) {
			t.Errorf("instance-repo Secret missing %q:\n%s", want, sec)
		}
	}
	// Without a token (public repo): no repository Secret for the instance repo.
	if s := repoSecret(run("")); s != "" {
		t.Errorf("no token → must apply no instance-repo Secret; got:\n%s", s)
	}
}

// TestConfigureManagedApl: reads apl-core's current (in-cluster Gitea) BYO-Git Secret,
// applies an in-cluster migration Job (clone → AplApp enable files → force-push the full
// tree to the github apl-<env> branch), waits for it, then patches apl-secrets/apl-git-config
// to repoint apl-core at github. No helm, no runner-side git.
func TestConfigureManagedApl(t *testing.T) {
	o := bootstrapClusterOpts{env: "primary", instanceRepo: "acme/instance", instanceRepoToken: "test-repo-token"}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	var applied []string
	var patched string
	d := bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			line := strings.Join(args, " ")
			switch {
			case strings.Contains(line, "get secret apl-git-config") && strings.Contains(line, "data.repoUrl"):
				return b64("http://git-server.git-server.svc.cluster.local/otomi/values.git"), true
			case strings.Contains(line, "get secret apl-git-config") && strings.Contains(line, "data.branch"):
				return b64("main"), true
			case strings.Contains(line, "get secret apl-git-config") && strings.Contains(line, "data.username"):
				return b64("otomi-admin"), true
			case strings.Contains(line, "get secret apl-git-config") && strings.Contains(line, "data.password"):
				return b64("gitea-pw"), true
			case strings.Contains(line, "get job") && strings.Contains(line, "status.succeeded"):
				return "1", true // migration Job completed
			case strings.Contains(line, "patch secret apl-git-config"):
				patched = line
				return "", true
			}
			return "", true
		},
		apply: func(y, _ string, _ bool) (string, bool) { applied = append(applied, y); return "", true },
		now:   time.Now, sleep: func(time.Duration) {},
	}
	if err := configureManagedApl(o, d); err != nil {
		t.Fatalf("configureManagedApl: %v", err)
	}
	all := strings.Join(applied, "\n---\n")
	// A migration Secret (credential-bearing clone/push URLs) + a Job (enable + push).
	for _, want := range []string{
		"kind: Secret", "SRC_URL:", "DST_URL:",
		"kind: Job", "llz-apl-values-migrate", "alpine/git",
		"name: APPS", "harbor loki grafana kyverno", "name: DST_BRANCH", "'apl-primary'",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("migration manifests missing %q", want)
		}
	}
	// SRC_URL must embed the in-cluster gitea creds (http:// — git can't prompt in a Job).
	if !strings.Contains(all, "http://otomi-admin:gitea-pw@git-server.git-server.svc") {
		t.Errorf("SRC_URL must embed http creds for the in-cluster values repo; manifests:\n%s", all)
	}
	if !strings.Contains(all, "x-access-token:test-repo-token@github.com/acme/instance.git") {
		t.Error("DST_URL must embed the github token")
	}
	// After the Job, the BYO-Git Secret is repointed at the github branch.
	for _, want := range []string{`"repoUrl":"https://github.com/acme/instance.git"`, `"branch":"apl-primary"`, `"username":"x-access-token"`} {
		if !strings.Contains(patched, want) {
			t.Errorf("apl-git-config patch missing %q; got %q", want, patched)
		}
	}
	// No token → skip entirely (no kubectl/apply).
	var ran bool
	d2 := bootstrapDeps{
		kubectl: func(...string) (string, bool) { ran = true; return "", true },
		apply:   func(_, _ string, _ bool) (string, bool) { ran = true; return "", true },
	}
	if err := configureManagedApl(bootstrapClusterOpts{env: "primary", instanceRepo: "acme/instance"}, d2); err != nil {
		t.Fatalf("configureManagedApl (no token): %v", err)
	}
	if ran {
		t.Error("no APL_VALUES_REPO_TOKEN → configureManagedApl must skip (no kubectl/apply)")
	}
}

// TestAplMigrateManifestsValidYAML: the migration Secret + Job render as valid YAML
// (guards the Job's block-scalar script indentation + single-quote escaping).
func TestAplMigrateManifestsValidYAML(t *testing.T) {
	manifests := map[string]string{
		"secret": aplMigrateSecretManifest(
			"http://otomi-admin:pw@git-server.git-server.svc.cluster.local/otomi/values.git",
			"https://x-access-token:tok@github.com/acme/instance.git"),
		"job": aplMigrateJobManifest("main", "apl-primary", []string{"harbor", "loki", "grafana", "kyverno"}),
	}
	for name, m := range manifests {
		var obj map[string]any
		if err := yaml.Unmarshal([]byte(m), &obj); err != nil {
			t.Errorf("%s manifest is not valid YAML: %v\n%s", name, err, m)
		}
	}
}

// TestBasicAuthGitURL: creds embed for BOTH http (in-cluster git-server) and https
// (github); empty secret or non-http(s) is unchanged.
func TestBasicAuthGitURL(t *testing.T) {
	cases := []struct{ raw, user, secret, want string }{
		{"http://git-server.git-server.svc/otomi/values.git", "otomi-admin", "pw", "http://otomi-admin:pw@git-server.git-server.svc/otomi/values.git"},
		// a generated password with reserved chars must be percent-encoded, not corrupt the URL.
		{"http://git-server.git-server.svc/otomi/values.git", "otomi-admin", "k2qaZ3gPS&PPlRnrnES6z", "http://otomi-admin:k2qaZ3gPS%26PPlRnrnES6z@git-server.git-server.svc/otomi/values.git"},
		{"https://github.com/acme/instance.git", "x-access-token", "tok", "https://x-access-token:tok@github.com/acme/instance.git"},
		{"https://github.com/acme/instance.git", "", "tok", "https://x-access-token:tok@github.com/acme/instance.git"},
		{"http://git-server/x.git", "u", "", "http://git-server/x.git"},
		{"git@github.com:acme/x.git", "u", "s", "git@github.com:acme/x.git"},
	}
	for _, c := range cases {
		if got := basicAuthGitURL(c.raw, c.user, c.secret); got != c.want {
			t.Errorf("basicAuthGitURL(%q,%q,secret)=%q want %q", c.raw, c.user, got, c.want)
		}
	}
}

// TestAplAppEnableManifest: the enable file is a valid AplApp whose spec enables the app.
func TestAplAppEnableManifest(t *testing.T) {
	got := aplAppEnableManifest("harbor")
	for _, want := range []string{"kind: AplApp", "name: harbor", "enabled: true"} {
		if !strings.Contains(got, want) {
			t.Errorf("aplAppEnableManifest missing %q:\n%s", want, got)
		}
	}
}

// TestWaitManagedArgoReady_Timeout: when managed ArgoCD never comes up, the wait
// returns a diagnostic error rather than hanging (budget enforced via the clock seam).
func TestWaitManagedArgoReady_Timeout(t *testing.T) {
	base := time.Unix(0, 0)
	d := bootstrapDeps{
		kubectl: func(_ ...string) (string, bool) { return "", false }, // never ready
		now: func() time.Time {
			cur := base
			base = base.Add(20 * time.Minute) // second read is past the 15m budget
			return cur
		},
		sleep: func(time.Duration) {},
	}
	if err := waitManagedArgoReady(d); err == nil {
		t.Fatal("expected a timeout error when ArgoCD never becomes ready")
	}
}

// TestBootstrapCluster_AppliesStorageClass: the managed path applies the DEFAULT
// block-storage-retain SC + the llz-openbao namespace before the bridge. It is the
// cluster default (llzReconciler sc-demote keeps LKE's class non-default); managed
// leaves no default of its own, so without this PVCs without a class stay Pending.
func TestBootstrapCluster_AppliesStorageClass(t *testing.T) {
	o := bootstrapClusterOpts{env: "primary", instanceRepo: "acme/instance", upstreamOrg: "akamai-consulting", templateRef: "ref", appsRepoRevision: "main"}
	var sawSC, sawOpenbaoNS bool
	d := bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			line := strings.Join(args, " ")
			if strings.Contains(line, "crd applications.argoproj.io") {
				return "applications.argoproj.io", true
			}
			if strings.Contains(line, "deploy argocd-server") {
				return "1", true
			}
			return "", true
		},
		apply: func(y, _ string, _ bool) (string, bool) {
			if strings.Contains(y, "kind: Namespace") && strings.Contains(y, "llz-openbao") {
				sawOpenbaoNS = true
			}
			if strings.Contains(y, "block-storage-retain") && strings.Contains(y, "StorageClass") {
				sawSC = true
				if !strings.Contains(y, `is-default-class: "true"`) {
					t.Errorf("managed block-storage-retain SC must be applied AS the cluster default:\n%s", y)
				}
			}
			return "", true
		},
		now: time.Now, sleep: func(time.Duration) {},
	}
	if err := bootstrapCluster(o, d); err != nil {
		t.Fatalf("bootstrapCluster: %v", err)
	}
	if !sawOpenbaoNS {
		t.Error("managed path did not apply the llz-openbao Namespace — the OpenBao extra (CreateNamespace=false) would never sync")
	}
	if !sawSC {
		t.Error("managed path did not apply the block-storage-retain StorageClass")
	}
}
