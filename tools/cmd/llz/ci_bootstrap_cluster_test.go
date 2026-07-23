package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
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
			// configureManagedApl runs when a token is set: ls-remote reports the branch
			// present (skip the seed dance) and helm succeeds.
			git: func(args ...string) (string, bool) {
				if strings.Contains(strings.Join(args, " "), "ls-remote") {
					return "abc123\trefs/heads/apl-primary", true
				}
				return "", true
			},
			helm: func(...string) (string, bool) { return "", true },
			now:  time.Now, sleep: func(time.Duration) {},
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

// TestConfigureManagedApl: seeds the apl-<env> branch when absent and helm-upgrades
// the managed apl release pointing otomi.git at github + enabling the default apps.
func TestConfigureManagedApl(t *testing.T) {
	o := bootstrapClusterOpts{env: "primary", instanceRepo: "acme/instance", instanceRepoToken: "test-repo-token"}
	var helmArgs []string
	var pushed bool
	d := bootstrapDeps{
		git: func(args ...string) (string, bool) {
			line := strings.Join(args, " ")
			if strings.Contains(line, "ls-remote") {
				return "", true // branch absent → seed it
			}
			if strings.Contains(line, "push") {
				pushed = true
			}
			return "", true
		},
		helm: func(args ...string) (string, bool) { helmArgs = args; return "", true },
	}
	if err := configureManagedApl(o, d); err != nil {
		t.Fatalf("configureManagedApl: %v", err)
	}
	if !pushed {
		t.Error("absent apl-<env> branch must be seeded (git push)")
	}
	joined := strings.Join(helmArgs, " ")
	for _, want := range []string{
		"upgrade apl apl", "--reuse-values",
		"otomi.git.repoUrl=https://github.com/acme/instance.git",
		"otomi.git.branch=apl-primary", "otomi.git.password=test-repo-token",
		"apps.harbor.enabled=true", "apps.loki.enabled=true",
		"apps.grafana.enabled=true", "apps.kyverno.enabled=true",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("helm upgrade missing %q; got: %s", want, joined)
		}
	}
	// No token → skip entirely (no helm/git).
	var ran bool
	d2 := bootstrapDeps{
		git:  func(...string) (string, bool) { ran = true; return "", true },
		helm: func(...string) (string, bool) { ran = true; return "", true },
	}
	if err := configureManagedApl(bootstrapClusterOpts{env: "primary", instanceRepo: "acme/instance"}, d2); err != nil {
		t.Fatalf("configureManagedApl (no token): %v", err)
	}
	if ran {
		t.Error("no APL_VALUES_REPO_TOKEN → configureManagedApl must skip (no git/helm)")
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

// TestManagedBlockStorageClassYAML: the managed variant keeps the class name +
// Linode-CSI provisioner but drops the is-default annotation (managed apl-core
// owns the cluster-default; a second default would race it).
func TestManagedBlockStorageClassYAML(t *testing.T) {
	out, err := managedBlockStorageClassYAML()
	if err != nil {
		t.Fatalf("managedBlockStorageClassYAML: %v", err)
	}
	if !strings.Contains(out, "name: block-storage-retain") {
		t.Errorf("lost the class name; got:\n%s", out)
	}
	if !strings.Contains(out, "linodebs.csi.linode.com") {
		t.Errorf("lost the CSI provisioner; got:\n%s", out)
	}
	if strings.Contains(out, "is-default-class") {
		t.Errorf("managed SC must NOT be marked default; got:\n%s", out)
	}
	// Retain policy (the whole point of the -retain class) must survive.
	if !strings.Contains(out, "Retain") {
		t.Errorf("lost Retain reclaim policy; got:\n%s", out)
	}
}

// TestBootstrapCluster_AppliesStorageClass: the managed path applies the
// non-default block-storage-retain SC + the llz-openbao namespace before the bridge.
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
				if strings.Contains(y, "is-default-class") {
					t.Errorf("managed SC applied AS DEFAULT — must be non-default")
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
