package bootstrapcluster

import (
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"sigs.k8s.io/yaml"
)

// ── runCombined (the production exec seam) ───────────────────────────────────

// Regression: `return buf.String(), cmd.Run() == nil` evaluates left-to-right,
// snapshotting the buffer BEFORE the command runs — every kubectl call
// returned "" on the e2e bootstrap (misread as an empty kubeconfig). runCombined
// must return the output the run itself produced, on success AND failure.
func TestRunCombined_OutputAfterRun(t *testing.T) {
	out, ok := cigate.RunCombined(exec.Command("sh", "-c", "echo to-stdout; echo to-stderr >&2"))
	if !ok {
		t.Fatalf("runCombined(exit 0) reported failure (out=%q)", out)
	}
	if !strings.Contains(out, "to-stdout") || !strings.Contains(out, "to-stderr") {
		t.Fatalf("runCombined returned output snapshotted before the run (eval-order regression): %q", out)
	}

	out, ok = cigate.RunCombined(exec.Command("sh", "-c", "echo boom >&2; exit 3"))
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
// the baseline; bump it deliberately, in lockstep with the platform.
//
// The literal is the EXACT published chart version, "v" and all: apl-core changed
// its published Chart.yaml from `version: 6.0.0` to `version: v6.1.0` with the
// 6.1.0 release automation and kept the convention at v6.2.0, and
// `helm --version 6.2.0` only resolves it via a fallback warning. Dropping the
// prefix on the next bump is a silent regression, so assert it here.
func TestDefaultAplChartVersion(t *testing.T) {
	if clusterspec.BaselineAplChartVersion != "v6.2.0" {
		t.Errorf("clusterspec.BaselineAplChartVersion = %q, want \"v6.2.0\" — bump deliberately, in lockstep with the platform baseline", clusterspec.BaselineAplChartVersion)
	}
	// The skew check that used to sit here is GONE, and getting what it wanted is
	// why. It compared package main's `defaultAplChartVersion` against
	// clusterspec.BaselineAplChartVersion — two copies of one fact. The alias was
	// deleted during the extraction, so there is one constant and nothing left to
	// skew; the comparison had quietly become value-against-itself, which
	// staticcheck (SA4000) named the first time it ran on this branch.
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
	// Same compare-options the carved Apps render: this App's own tree carries the
	// verify-llz-image-signature ClusterPolicy, whose Kyverno-defaulted spec fields
	// were the ONE thing keeping platform-bootstrap OutOfSync — and selfHeal
	// re-applying it in a loop (#394).
	ann, _ := app["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann["argocd.argoproj.io/compare-options"] != clusterspec.CompareOptions {
		t.Errorf("bootstrap app compare-options = %v, want %q", ann["argocd.argoproj.io/compare-options"], clusterspec.CompareOptions)
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

// TestLlzNamespaceManifest — the managed bootstrap pre-creates the
// LLZ namespaces (managed apl-core does not) with the restricted PSS + monitoring
// labels and the bootstrap marker, so the carved apps (CreateNamespace=false) sync
// without waiting on a namespace that would otherwise never be created. llz-observability
// is included so its dashboards + loki-object-store ExternalSecret can land.
func TestLlzNamespaceManifest(t *testing.T) {
	if want := []string{"llz-openbao", "llz-observability"}; strings.Join(managedLLZNamespaces, ",") != strings.Join(want, ",") {
		t.Fatalf("managedLLZNamespaces = %v, want %v", managedLLZNamespaces, want)
	}
	m := llzNamespaceManifest("llz-observability")
	if m["kind"] != "Namespace" {
		t.Fatalf("kind = %v, want Namespace", m["kind"])
	}
	meta := m["metadata"].(map[string]any)
	if meta["name"] != "llz-observability" {
		t.Errorf("name = %v, want llz-observability", meta["name"])
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
		env: "primary", clusterID: "393244", instanceRepo: "acme/instance",
		upstreamOrg: "akamai-consulting", templateRef: "abc123",
		appsRepoRevision: "main",
	}
	var applied, mgrs []string
	// A FAKE clock whose sleep advances it: with a frozen (or real) clock, a
	// waitManagedArgoReady that stops recognising "ready" spins for its whole
	// 15-minute budget instead of failing this test. See advancingClock.
	now, sleep := advancingClock()
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
		now:   now,
		sleep: sleep,
	}
	if err := bootstrapCluster(o, d); err != nil {
		t.Fatalf("bootstrapCluster: %v", err)
	}
	// Separate the bridge manifests from the cluster-scoped pieces bootstrap also
	// applies: the block-storage-retain SC, the LLZ namespaces, and the
	// pvc-deny-untaggable-clone ValidatingAdmissionPolicy + its binding.
	var bridge []string
	for _, y := range applied {
		if strings.Contains(y, "kind: StorageClass") || strings.Contains(y, "kind: Namespace") ||
			strings.Contains(y, "kind: ValidatingAdmissionPolicy") {
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
			env: "primary", clusterID: "393244", instanceRepo: "acme/instance",
			upstreamOrg: "akamai-consulting", templateRef: "ref", appsRepoRevision: "main",
			instanceRepoToken: token,
		}
		var applied []string
		now, sleep := advancingClock()
		d := bootstrapDeps{
			kubectl: func(args ...string) (string, bool) {
				line := strings.Join(args, " ")
				if strings.Contains(line, "crd applications.argoproj.io") {
					return "applications.argoproj.io", true
				}
				if strings.Contains(line, "deploy argocd-server") {
					return "1", true
				}
				// apl-git-config, already published. This test is about the BRIDGE, so
				// apl-core has to look ready: a missing repoUrl is now TERMINAL rather
				// than a warn-and-continue, and would abort before the bridge is applied.
				if strings.Contains(line, "secret apl-git-config") {
					switch {
					case strings.Contains(line, "data.repoUrl"):
						return b64("http://git-server.git-server.svc.cluster.local/otomi/values.git"), true
					case strings.Contains(line, "data.branch"):
						return b64("main"), true
					case strings.Contains(line, "data.username"):
						return b64("otomi-admin"), true
					case strings.Contains(line, "data.password"):
						return b64("pw"), true
					}
					return "", true
				}
				// Migration Job: report success immediately.
				if strings.Contains(line, "jsonpath={.status.succeeded}") {
					return "1", true
				}
				return "", true
			},
			apply: func(y, _ string, _ bool) (string, bool) { applied = append(applied, y); return "", true },
			// configureManagedApl runs when a token is set; the kubectl stub above
			// returns no apl-git-config repoUrl, so it warns-and-continues (best-effort)
			// without reaching the migration Job — this test only asserts the bridge.
			// The advancing fake clock keeps the wait loops it DOES enter bounded in
			// fake time rather than in 15 real minutes.
			now: now, sleep: sleep,
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
	now, sleep := advancingClock()
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
		now:   now, sleep: sleep,
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
		"job": aplMigrateJobManifest("main", "apl-primary", []string{"harbor", "loki", "grafana", "kyverno"}, false),
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
	now, sleep := advancingClock()
	d := bootstrapDeps{
		kubectl: func(_ ...string) (string, bool) { return "", false }, // never ready
		now:     now,
		sleep:   sleep, // advances the fake clock, so the budget is actually reached
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
	o := bootstrapClusterOpts{env: "primary", clusterID: "393244", instanceRepo: "acme/instance", upstreamOrg: "akamai-consulting", templateRef: "ref", appsRepoRevision: "main"}
	var sawSC, sawOpenbaoNS bool
	now, sleep := advancingClock()
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
		now: now, sleep: sleep,
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

// THE RE-BOOTSTRAP BUG. The first migration force-pushes apl-core's Gitea tree to
// the github apl-<env> branch, then PATCHES apl-git-config to point at it. From
// then on cur.repoURL IS the github URL and cur.branch IS apl-<env> — so the
// second bootstrap set SRC = DST and tried to clone the destination in order to
// rebuild the destination. That only works while the destination exists, and can
// never recreate it.
//
// Observed on cluster 637367: with apl-e2e missing, the Job waited 48x10s for a
// branch it was itself meant to create, exited 1, and (being best-effort) let
// "Bootstrap cluster" report SUCCESS. apl-operator then logged `couldn't find
// remote ref apl-e2e` every 15s and converge hard-failed 20 minutes later naming
// none of it.
func TestAplMigrationSourceSurvivesReBootstrap(t *testing.T) {
	const gh = "https://github.com/o/r.git"
	const dst = "apl-e2e"
	gitea := "https://otomi-admin:pw@git.internal/otomi/values.git"

	t.Run("first bootstrap clones the current (Gitea) repo", func(t *testing.T) {
		cur := aplGitConfig{repoURL: "https://git.internal/otomi/values.git", branch: "main",
			username: "u", password: "p", cloneCmd: gitea}
		src, br, skip, err := aplMigrationSource(cur, gh, dst)
		if err != nil || br != "main" {
			t.Fatalf("want (Gitea, main, nil), got (%q, %q, %v)", src, br, err)
		}
		// A first migration MUST still force-push, even over a partial branch left by
		// an earlier failed attempt — skipping there would leave apl-core on a tree
		// that has no env/apps/* enables.
		if skip {
			t.Error("first bootstrap must not skip on an existing destination")
		}
		// Credentials must be INJECTED for this source, or the clone is anonymous.
		if !strings.Contains(src, "u:p@") {
			t.Errorf("cur.repoURL source must carry injected basic auth, got %q", src)
		}
	})

	t.Run("re-bootstrap re-seeds from Gitea, NOT from its own destination", func(t *testing.T) {
		cur := aplGitConfig{repoURL: gh, branch: dst, username: "x-access-token",
			password: "tok", cloneCmd: gitea}
		src, br, skip, err := aplMigrationSource(cur, gh, dst)
		if err != nil {
			t.Fatalf("re-bootstrap must resolve a source, got %v", err)
		}
		// The repair must be conditional: apl-core has been reconciling the github
		// branch since the first migration, so force-pushing the abandoned Gitea tree
		// over a HEALTHY branch would revert every values change made since.
		if !skip {
			t.Error("re-bootstrap must skip when the destination branch is intact")
		}
		// The public git.<domain> host has no route from inside the cluster
		// (httproute.enabled=false); only the git-server Service DNS name is proven.
		if !strings.Contains(src, "git-server.git-server.svc.cluster.local") {
			t.Errorf("re-seed source must use the in-cluster git-server address, got %q", src)
		}
		// It must still carry the Gitea credentials recovered from gitCloneCmd —
		// username/password now hold the github coords.
		if !strings.Contains(src, "otomi-admin:") {
			t.Errorf("re-seed source lost the Gitea credentials from gitCloneCmd, got %q", src)
		}
		if src == gh || strings.Contains(src, "github.com") {
			t.Errorf("source is the DESTINATION (%q) — this is the bug: the migration cannot clone the "+
				"branch it exists to create", src)
		}
		if br == dst {
			t.Errorf("source branch is the destination branch %q — it would wait forever for a branch "+
				"nothing has pushed", br)
		}
		if br != "main" {
			t.Errorf("want apl-core's own default branch main, got %q", br)
		}
		// cloneCmd already embeds credentials; re-injecting would corrupt the URL.
		if strings.Count(src, "@") != 1 {
			t.Errorf("credentials must appear exactly once, got %q", src)
		}
	})

	t.Run("re-bootstrap with no cloneCmd defers the refusal to the Job", func(t *testing.T) {
		// The common re-bootstrap is "destination is fine, nothing to do", which needs
		// NO source. Erroring here would fail that normal case to prepare for the rare
		// one — and since patchAplGitConfig overwrites username/password with the github
		// coords, gitCloneCmd is the only surviving Gitea credential, so any cluster
		// whose apl-core omits that field would fail its bootstrap outright.
		cur := aplGitConfig{repoURL: gh, branch: dst, username: "x-access-token", password: "tok"}
		src, br, skip, err := aplMigrationSource(cur, gh, dst)
		if err != nil {
			t.Fatalf("a missing source must not fail the resolve — the Job decides: %v", err)
		}
		if !skip {
			t.Error("must still skip when the destination is intact")
		}
		if br != "main" {
			t.Errorf("source branch should be apl-core's default main, got %q", br)
		}
		// The safety property is unchanged: it must NEVER invent a source. Force-pushing
		// a wrong or incomplete tree wipes apl-core's tracked config.
		if src != "" {
			t.Errorf("guessed a source %q with nothing to recover it from", src)
		}
		// And the Job must refuse rather than push an empty tree when it genuinely
		// needs the source it does not have.
		y := aplMigrateJobManifest(br, dst, []string{"harbor"}, skip)
		if !strings.Contains(y, `if [ -z "$SRC_URL" ]`) {
			t.Error("the Job does not guard on an empty SRC_URL — with the branch missing it " +
				"would clone nothing and force-push an empty tree over apl-core's config")
		}
	})
}

// TestAplMigrateJobSkipsHealthyDestination covers the in-cluster half of the
// re-bootstrap repair. The decision has to live in the Job, not the runner: the
// runner cannot reach either git remote (the Gitea Service is cluster-internal, and
// checking github from CI would be a second, differently-authenticated code path).
func TestAplMigrateJobSkipsHealthyDestination(t *testing.T) {
	t.Run("repair mode guards on the destination branch", func(t *testing.T) {
		y := aplMigrateJobManifest("main", "apl-e2e", []string{"harbor"}, true)
		if !strings.Contains(y, `SKIP_IF_DST_EXISTS`) || !strings.Contains(y, `value: "true"`) {
			t.Fatal("SKIP_IF_DST_EXISTS=true not rendered into the Job env")
		}
		// The guard must probe the DESTINATION. Probing SRC would answer the wrong
		// question and re-push over a healthy branch anyway.
		//
		// Through branch_state now, not a bare `ls-remote | grep -q`: that pipeline
		// read a FAILED ls-remote as "the branch is gone" and force-pushed over a
		// healthy one. TestAplBranchStateSeparatesAbsentFromUnreachable runs the
		// function; this only checks it is the thing the guard calls.
		if !strings.Contains(y, `branch_state "$DST_URL" "$DST_BRANCH"`) {
			t.Error("no branch_state guard against $DST_URL/$DST_BRANCH")
		}
		if !strings.Contains(y, "exit 0") {
			t.Error("the skip path must exit 0 — a non-zero exit is now TERMINAL for the bootstrap")
		}
	})

	t.Run("first migration does not guard", func(t *testing.T) {
		y := aplMigrateJobManifest("main", "apl-e2e", []string{"harbor"}, false)
		if !strings.Contains(y, `value: "false"`) {
			t.Fatal("SKIP_IF_DST_EXISTS=false not rendered")
		}
		// Belt and braces: a first migration must force-push even when a partial
		// branch survives from an earlier failed attempt.
		if !strings.Contains(y, "git push --force") {
			t.Error("the migration must force-push the complete tree")
		}
	})
}

// TestGiteaSourceFromCloneCmd pins the two things that make the recovered source
// usable: the credentials come from gitCloneCmd (the only place the Gitea password
// survives the github repoint) and the HOST does not.
func TestGiteaSourceFromCloneCmd(t *testing.T) {
	got, err := giteaSourceFromCloneCmd("https://otomi-admin:s3cr3t@git.lke1.akamai-apl.net/otomi/values.git")
	if err != nil {
		t.Fatal(err)
	}
	// The public host has no in-cluster route (git-server httproute.enabled=false).
	if strings.Contains(got, "akamai-apl.net") {
		t.Errorf("kept the PUBLIC host, which has no route from a pod: %q", got)
	}
	if !strings.Contains(got, aplGiteaInClusterURL[len("http://"):len("http://")+len("git-server")]) {
		t.Errorf("did not rebuild against the in-cluster git-server address: %q", got)
	}
	if !strings.Contains(got, "otomi-admin") || !strings.Contains(got, "s3cr3t") {
		t.Errorf("dropped the recovered credentials: %q", got)
	}

	// No embedded credentials => there is nothing to authenticate with, and guessing
	// would force-push an incomplete tree over apl-core's config.
	if _, err := giteaSourceFromCloneCmd("https://git.lke1.akamai-apl.net/otomi/values.git"); err == nil {
		t.Error("a credential-less gitCloneCmd must ERROR, not yield an anonymous URL")
	}
	if _, err := giteaSourceFromCloneCmd(""); err == nil {
		t.Error("empty gitCloneCmd must ERROR")
	}
}

// b64 is the wire form kubectl -o jsonpath={.data.<k>} returns.
func b64(v string) string { return base64.StdEncoding.EncodeToString([]byte(v)) }

// TestWaitAplGitConfigIsBoundedAndTerminal covers the wait that replaced the old
// warn-and-skip. Two properties matter, and the first is not about correctness of
// the happy path at all:
//
//  1. It must be bounded by ATTEMPTS. The deps expose now and sleep as separate
//     seams, and every fake in this file pairs a real time.Now with a no-op sleep.
//     A deadline-driven loop spins at full speed for a real ten minutes under that
//     combination — which is exactly what happened when this was first written.
//  2. Exhausting the budget must ERROR, not skip. Skipping leaves apl-core with no
//     values branch and defers the failure to converge ~20 minutes later.
func TestWaitAplGitConfigIsBoundedAndTerminal(t *testing.T) {
	t.Run("gives up after a bounded number of attempts", func(t *testing.T) {
		calls := 0
		d := bootstrapDeps{
			kubectl: func(args ...string) (string, bool) { calls++; return "", true }, // never publishes
			now:     time.Now,                                                         // real clock…
			sleep:   func(time.Duration) {},                                           // …and a no-op sleep
		}
		start := time.Now()
		if _, err := waitAplGitConfig(d); err == nil {
			t.Fatal("want an error when apl-core never publishes its git config")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("took %s — the loop is not attempt-bounded, it is spinning on a clock that never advances", elapsed)
		}
		if want := aplGitConfigAttempts(); calls < want {
			t.Errorf("only %d kubectl calls for %d attempts — did it give up early?", calls, want)
		}
	})

	t.Run("returns as soon as apl-core publishes", func(t *testing.T) {
		n := 0
		d := bootstrapDeps{
			kubectl: func(args ...string) (string, bool) {
				line := strings.Join(args, " ")
				if !strings.Contains(line, "data.repoUrl") {
					return b64("main"), true
				}
				n++
				if n < 3 { // absent for the first two polls
					return "", true
				}
				return b64("http://git-server.git-server.svc.cluster.local/otomi/values.git"), true
			},
			now:   time.Now,
			sleep: func(time.Duration) {},
		}
		cur, err := waitAplGitConfig(d)
		if err != nil {
			t.Fatalf("want success once published, got %v", err)
		}
		if cur.repoURL == "" {
			t.Error("returned an empty config on success")
		}
	})

	t.Run("a non-transient failure is not retried", func(t *testing.T) {
		// Undecodable data will never fix itself; waiting 10 minutes on it is waste.
		calls := 0
		d := bootstrapDeps{
			kubectl: func(args ...string) (string, bool) {
				calls++
				return "!!!not-base64!!!", true
			},
			now:   time.Now,
			sleep: func(time.Duration) {},
		}
		if _, err := waitAplGitConfig(d); err == nil {
			t.Fatal("want an error on undecodable data")
		}
		if calls > 2 {
			t.Errorf("retried a non-transient failure %d times", calls)
		}
	})
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

// ── SPIKE: encrypting the LKE stock StorageClasses ───────────────────────────

const stockSCJSON = `{
  "apiVersion":"storage.k8s.io/v1","kind":"StorageClass",
  "metadata":{"name":"linode-block-storage",
    "annotations":{"meta.helm.sh/release-name":"workload","storageclass.kubernetes.io/is-default-class":"true"},
    "labels":{"helm.toolkit.fluxcd.io/name":"workload"}},
  "provisioner":"linodebs.csi.linode.com",
  "reclaimPolicy":"Delete","volumeBindingMode":"Immediate","allowVolumeExpansion":true,
  "parameters":{"linodebs.csi.linode.com/someOtherOption":"keepme"}
}`

// stockSCKubectl fakes the reads encryptStockStorageClasses makes and records the
// deletes; `present` maps class name → JSON (absent names report failure).
func stockSCKubectl(present map[string]string, deleteFails bool) (func(...string) (string, bool), *[]string) {
	var deleted []string
	return func(args ...string) (string, bool) {
		line := strings.Join(args, " ")
		for name, body := range present {
			if strings.HasPrefix(line, "get storageclass "+name+" ") {
				return body, true
			}
		}
		if strings.HasPrefix(line, "get storageclass ") {
			return "", false // absent
		}
		if strings.HasPrefix(line, "delete storageclass ") {
			deleted = append(deleted, strings.Fields(line)[2])
			return "", !deleteFails
		}
		return "", true
	}, &deleted
}

// TestEncryptStockStorageClasses_RecreatesEncrypted is the core of the spike: the
// stock class must come back encrypted and ownership-tagged, because on managed
// apl-core names it EXPLICITLY and neither the cluster default nor a Kyverno
// mutation can reach those PVCs in time.
func TestEncryptStockStorageClasses_RecreatesEncrypted(t *testing.T) {
	kubectl, deleted := stockSCKubectl(map[string]string{"linode-block-storage": stockSCJSON}, false)
	var applied []string
	d := bootstrapDeps{
		kubectl: kubectl,
		apply:   func(y, _ string, _ bool) (string, bool) { applied = append(applied, y); return "", true },
		now:     time.Now, sleep: func(time.Duration) {},
	}
	if err := encryptStockStorageClasses(bootstrapClusterOpts{clusterID: "637888"}, d); err != nil {
		t.Fatalf("encryptStockStorageClasses: %v", err)
	}
	if len(*deleted) != 1 || (*deleted)[0] != "linode-block-storage" {
		t.Fatalf("expected exactly the one present class to be deleted, got %v", *deleted)
	}
	if len(applied) != 1 {
		t.Fatalf("expected one recreate, got %d", len(applied))
	}

	var got map[string]any
	if err := yaml.Unmarshal([]byte(applied[0]), &got); err != nil {
		t.Fatal(err)
	}
	params, _ := got["parameters"].(map[string]any)
	if params["linodebs.csi.linode.com/encrypted"] != "true" {
		t.Errorf("recreated class is not encrypted: %v", params)
	}
	if params["linodebs.csi.linode.com/volumeTags"] != "block-storage,platform-support-services,lke637888" {
		t.Errorf("volumeTags = %v — reap keys on the lke<id> tag", params["linodebs.csi.linode.com/volumeTags"])
	}
	// Unknown parameters must survive: the class may carry Linode options this
	// code does not model, and silently dropping one changes provisioning.
	if params["linodebs.csi.linode.com/someOtherOption"] != "keepme" {
		t.Errorf("pre-existing parameter dropped: %v", params)
	}
	// Identity + lifecycle must be preserved or apl-core's PVCs stop resolving /
	// change reclaim semantics.
	if got["reclaimPolicy"] != "Delete" || got["volumeBindingMode"] != "Immediate" || got["allowVolumeExpansion"] != true {
		t.Errorf("lifecycle fields not preserved: %v", got)
	}
	meta, _ := got["metadata"].(map[string]any)
	if meta["name"] != "linode-block-storage" {
		t.Errorf("name changed: %v", meta)
	}
	// Two defaults wedge the cluster (the race sc-demote exists to prevent), and
	// the recreated object is LLZ's — Helm/Flux ownership must not be copied over.
	if anns, ok := meta["annotations"]; ok {
		t.Errorf("recreated class must carry NO annotations (no is-default-class, no Helm ownership), got %v", anns)
	}
	if _, ok := meta["labels"]; ok {
		t.Errorf("recreated class must not inherit LKE's Helm/Flux labels, got %v", meta["labels"])
	}
}

// TestEncryptStockStorageClasses_Idempotent: a second bootstrap must not churn a
// class that is already correct — a needless delete+recreate reopens the window
// where a PVC can be created with no class to bind to.
func TestEncryptStockStorageClasses_Idempotent(t *testing.T) {
	already := `{"metadata":{"name":"linode-block-storage"},"provisioner":"linodebs.csi.linode.com",
	  "parameters":{"linodebs.csi.linode.com/encrypted":"true",
	                "linodebs.csi.linode.com/volumeTags":"block-storage,platform-support-services,lke637888"}}`
	kubectl, deleted := stockSCKubectl(map[string]string{"linode-block-storage": already}, false)
	var applied int
	d := bootstrapDeps{
		kubectl: kubectl,
		apply:   func(string, string, bool) (string, bool) { applied++; return "", true },
		now:     time.Now, sleep: func(time.Duration) {},
	}
	if err := encryptStockStorageClasses(bootstrapClusterOpts{clusterID: "637888"}, d); err != nil {
		t.Fatal(err)
	}
	if len(*deleted) != 0 || applied != 0 {
		t.Errorf("an already-correct class must not be touched: deleted=%v applied=%d", *deleted, applied)
	}
}

// TestEncryptStockStorageClasses_SkipsAbsentAndForeign: absent classes are normal,
// and a non-Linode provisioner must be left alone — deleting someone else's
// StorageClass to add parameters its driver ignores would be pure damage.
func TestEncryptStockStorageClasses_SkipsAbsentAndForeign(t *testing.T) {
	foreign := `{"metadata":{"name":"linode-block-storage"},"provisioner":"ebs.csi.aws.com","parameters":{}}`
	kubectl, deleted := stockSCKubectl(map[string]string{"linode-block-storage": foreign}, false)
	var applied int
	d := bootstrapDeps{
		kubectl: kubectl,
		apply:   func(string, string, bool) (string, bool) { applied++; return "", true },
		now:     time.Now, sleep: func(time.Duration) {},
	}
	// linode-block-storage-retain is absent in this fake; both classes must no-op.
	if err := encryptStockStorageClasses(bootstrapClusterOpts{clusterID: "637888"}, d); err != nil {
		t.Fatal(err)
	}
	if len(*deleted) != 0 || applied != 0 {
		t.Errorf("foreign/absent classes must be untouched: deleted=%v applied=%d", *deleted, applied)
	}
}

// TestEncryptStockStorageClasses_RecreateFailureIsLoud pins the one genuinely
// dangerous path. The class is deleted before it can be recreated, so a failed
// recreate leaves the cluster with NO stock class and every PVC naming it Pending.
// That must surface as a hard error naming the class, never a warning.
func TestEncryptStockStorageClasses_RecreateFailureIsLoud(t *testing.T) {
	kubectl, _ := stockSCKubectl(map[string]string{"linode-block-storage": stockSCJSON}, false)
	d := bootstrapDeps{
		kubectl: kubectl,
		apply:   func(string, string, bool) (string, bool) { return "apiserver said no", false },
		now:     time.Now, sleep: func(time.Duration) {},
	}
	err := encryptStockStorageClasses(bootstrapClusterOpts{clusterID: "637888"}, d)
	if err == nil {
		t.Fatal("a failed recreate leaves the cluster with no stock StorageClass — it must hard-fail")
	}
	if !strings.Contains(err.Error(), "linode-block-storage") || !strings.Contains(err.Error(), "Pending") {
		t.Errorf("error must name the class and the consequence, got: %v", err)
	}
}

// TestEncryptStockStorageClasses_RejectsBadClusterID: without a numeric id the
// volumeTags render without lke<id>, and StorageClass parameters are immutable —
// so an untagged class can never be fixed in place. Fail before deleting anything.
func TestEncryptStockStorageClasses_RejectsBadClusterID(t *testing.T) {
	kubectl, deleted := stockSCKubectl(map[string]string{"linode-block-storage": stockSCJSON}, false)
	d := bootstrapDeps{
		kubectl: kubectl,
		apply:   func(string, string, bool) (string, bool) { return "", true },
		now:     time.Now, sleep: func(time.Duration) {},
	}
	if err := encryptStockStorageClasses(bootstrapClusterOpts{clusterID: ""}, d); err == nil {
		t.Fatal("an empty cluster id must fail before any class is deleted")
	}
	if len(*deleted) != 0 {
		t.Errorf("nothing may be deleted when the id is unusable, got %v", *deleted)
	}
}

// ── LLZ's OWN block-storage-retain class: immutable-parameter drift ───────────

// oldKeysBlockStorageRetainJSON is a block-storage-retain class as an LLZ older
// than the CSI key correction created it: `/encryption: enabled` and
// `/volume-tags`, the two keys the Linode driver silently ignores. Every live
// cluster bootstrapped before that fix carries exactly this.
const oldKeysBlockStorageRetainJSON = `{
  "apiVersion":"storage.k8s.io/v1","kind":"StorageClass",
  "metadata":{"name":"block-storage-retain",
    "annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},
  "provisioner":"linodebs.csi.linode.com",
  "reclaimPolicy":"Retain","volumeBindingMode":"Immediate","allowVolumeExpansion":true,
  "parameters":{"linodebs.csi.linode.com/encryption":"enabled",
                "linodebs.csi.linode.com/volume-tags":"block-storage,platform-support-services,lke637888"}
}`

// kubectlSCNotFound is what kubectl actually prints when the class is simply
// absent — the greenfield case. The seam is cigate.RunCombined (stdout+stderr
// merged), so this body is ALL the code gets to tell "there is no class here"
// apart from "I could not ask".
const kubectlSCNotFound = `Error from server (NotFound): storageclasses.storage.k8s.io "block-storage-retain" not found`

// kubectlSCForbidden is a get that failed for a reason that is NOT absence. It
// must never be read as greenfield: falling through to the plain apply on this
// is what re-arms the immutable-parameters wedge.
const kubectlSCForbidden = `Error from server (Forbidden): storageclasses.storage.k8s.io "block-storage-retain" is forbidden: User "system:serviceaccount:default:llz" cannot get resource "storageclasses"`

// foreignBlockStorageRetainJSON is a `block-storage-retain` that is NOT ours — an
// adopted cluster's own class that happens to share the name. LLZ must refuse it,
// not recycle it.
const foreignBlockStorageRetainJSON = `{
  "apiVersion":"storage.k8s.io/v1","kind":"StorageClass",
  "metadata":{"name":"block-storage-retain"},
  "provisioner":"ebs.csi.aws.com",
  "reclaimPolicy":"Retain","volumeBindingMode":"Immediate",
  "parameters":{"type":"gp3","encrypted":"true"}
}`

// managedSCFake drives the get/delete/apply seam applyManagedBlockStorageClass
// uses. Every field is an answer kubectl gives back through cigate.RunCombined.
type managedSCFake struct {
	// live is the class the apiserver returns as JSON. Empty means the get fails
	// the way kubectl fails on an ABSENT class, unless getErr overrides it.
	live string
	// getErr, when set, is the combined body of a get that failed for some other
	// reason — not the same thing as absent.
	getErr      string
	deleteFails bool
	applyFails  bool
}

// deps records every mutating operation IN ORDER, so a test can assert that the
// delete preceded the recreate rather than merely that both happened.
func (f managedSCFake) deps() (bootstrapDeps, *[]string, *[]string) {
	var ops, applied []string
	d := bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			line := strings.Join(args, " ")
			switch {
			case strings.HasPrefix(line, "get storageclass "):
				switch {
				case f.getErr != "":
					return f.getErr, false
				case f.live == "":
					return kubectlSCNotFound, false
				}
				return f.live, true
			case strings.HasPrefix(line, "delete storageclass "):
				ops = append(ops, "delete")
				if f.deleteFails {
					return "apiserver refused the delete", false
				}
				return "", true
			}
			return "", true
		},
		apply: func(y, _ string, _ bool) (string, bool) {
			ops = append(ops, "apply")
			applied = append(applied, y)
			return "apiserver said no", !f.applyFails
		},
		now: time.Now, sleep: func(time.Duration) {},
	}
	return d, &ops, &applied
}

// TestApplyManagedBlockStorageClass_RecreatesOnImmutableParamDrift is THE
// regression. StorageClass parameters are immutable, and the CSI parameter keys
// this class ships changed (`/encryption`+`/volume-tags` → `/encrypted`+
// `/volumeTags`). A plain apply over the old class is refused by the API server
// with "parameters: Forbidden: updates to parameters are forbidden", and
// bootstrap-cluster dies before the Argo bridge is placed — so on a live
// deployment NO LLZ extra, OpenBao included, ever syncs. The class must be
// deleted and recreated, in that order.
func TestApplyManagedBlockStorageClass_RecreatesOnImmutableParamDrift(t *testing.T) {
	d, ops, applied := managedSCFake{live: oldKeysBlockStorageRetainJSON}.deps()
	stderr := captureStderr(t, func() {
		if err := applyManagedBlockStorageClass(bootstrapClusterOpts{clusterID: "637888"}, d); err != nil {
			t.Fatalf("applyManagedBlockStorageClass: %v", err)
		}
	})
	// The recreate is the only way this class's volumeTags can ever change, and
	// `llz reap` keys on the lke<id> inside them — so it prints what it stamped.
	if !strings.Contains(stderr, "volumeTags=") || !strings.Contains(stderr, "lke637888") {
		t.Errorf("the recreate must print the volumeTags it stamped, stderr was:\n%s", stderr)
	}
	if len(*ops) != 2 || (*ops)[0] != "delete" || (*ops)[1] != "apply" {
		t.Fatalf("a parameters change is immutable — expected delete then apply, got %v", *ops)
	}
	if len(*applied) != 1 {
		t.Fatalf("expected exactly one recreate, got %d", len(*applied))
	}

	var got map[string]any
	if err := yaml.Unmarshal([]byte((*applied)[0]), &got); err != nil {
		t.Fatal(err)
	}
	params, _ := got["parameters"].(map[string]any)
	// The corrected keys — the whole point of the recreate. The old spellings are
	// accepted by the API server and ignored by the driver, so asserting only that
	// "a class was applied" would pass while every Volume stayed unencrypted.
	if params[csiEncryptedParam] != "true" {
		t.Errorf("recreated class is not encrypted under %s: %v", csiEncryptedParam, params)
	}
	if params[csiVolumeTagsParam] != "block-storage,platform-support-services,lke637888" {
		t.Errorf("%s = %v — reap keys on the lke<id> tag", csiVolumeTagsParam, params[csiVolumeTagsParam])
	}
	if _, stale := params["linodebs.csi.linode.com/encryption"]; stale {
		t.Errorf("recreated class still carries the driver-ignored /encryption key: %v", params)
	}
	if _, stale := params["linodebs.csi.linode.com/volume-tags"]; stale {
		t.Errorf("recreated class still carries the driver-ignored /volume-tags key: %v", params)
	}
}

// TestApplyManagedBlockStorageClass_GreenfieldAppliesWithoutDelete: with no live
// class there is nothing to collide with. Deleting here would be a pointless
// window in which a PVC can be created with no class to bind to.
func TestApplyManagedBlockStorageClass_GreenfieldAppliesWithoutDelete(t *testing.T) {
	d, ops, applied := managedSCFake{}.deps()
	if err := applyManagedBlockStorageClass(bootstrapClusterOpts{clusterID: "637888"}, d); err != nil {
		t.Fatalf("applyManagedBlockStorageClass: %v", err)
	}
	if len(*ops) != 1 || (*ops)[0] != "apply" {
		t.Fatalf("greenfield must apply without deleting, got %v", *ops)
	}
	if !strings.Contains((*applied)[0], "block-storage-retain") {
		t.Errorf("applied manifest is not the block-storage-retain class:\n%s", (*applied)[0])
	}
}

// TestApplyManagedBlockStorageClass_IdempotentWhenAlreadyCorrect: a re-bootstrap
// of a correct cluster must not churn the class. The live fixture is built by
// running the REAL renderer and converting it — restating the parameters here
// would let the test agree with itself while the shipped comparison drifts.
func TestApplyManagedBlockStorageClass_IdempotentWhenAlreadyCorrect(t *testing.T) {
	y, err := renderBlockStorageClass("637888")
	if err != nil {
		t.Fatal(err)
	}
	liveJSON, err := yaml.YAMLToJSON([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	d, ops, _ := managedSCFake{live: string(liveJSON)}.deps()
	if err := applyManagedBlockStorageClass(bootstrapClusterOpts{clusterID: "637888"}, d); err != nil {
		t.Fatalf("applyManagedBlockStorageClass: %v", err)
	}
	// One plain apply, no delete: mutable drift (annotations) still converges.
	if len(*ops) != 1 || (*ops)[0] != "apply" {
		t.Fatalf("an already-correct class must be applied in place, never deleted; got %v", *ops)
	}
}

// TestApplyManagedBlockStorageClass_RecreateFailureIsLoud pins the one genuinely
// dangerous path: the class is deleted before it can be recreated, so a failed
// recreate leaves the cluster with NO default class and every PVC naming it
// Pending. That must surface as a hard error naming the class and the consequence.
func TestApplyManagedBlockStorageClass_RecreateFailureIsLoud(t *testing.T) {
	d, _, _ := managedSCFake{live: oldKeysBlockStorageRetainJSON, applyFails: true}.deps()
	err := applyManagedBlockStorageClass(bootstrapClusterOpts{clusterID: "637888"}, d)
	if err == nil {
		t.Fatal("a failed recreate leaves the cluster with no block-storage-retain class — it must hard-fail")
	}
	if !strings.Contains(err.Error(), "block-storage-retain") || !strings.Contains(err.Error(), "Pending") {
		t.Errorf("error must name the class and the consequence, got: %v", err)
	}
}

// TestImmutableStorageClassDiff_IgnoresMutableFields: allowVolumeExpansion and
// metadata ARE mutable. Treating them as drift would delete+recreate the cluster
// default on every bootstrap that so much as re-labels the class.
func TestImmutableStorageClassDiff_IgnoresMutableFields(t *testing.T) {
	expand := true
	base := stockStorageClass{
		Name: "block-storage-retain", Provisioner: linodeCSIProvisioner,
		ReclaimPolicy: "Retain", VolumeBindingMode: "Immediate",
		Parameters: map[string]string{csiEncryptedParam: "true"},
	}
	live := base
	live.AllowVolumeExpansion = &expand
	live.IsDefault = false
	want := base
	want.IsDefault = true
	if diff := immutableStorageClassDiff(live, want); len(diff) != 0 {
		t.Errorf("allowVolumeExpansion/metadata are mutable and must not force a recreate, got %v", diff)
	}
	// ...but a real immutable field must still be caught.
	want.ReclaimPolicy = "Delete"
	if diff := immutableStorageClassDiff(live, want); len(diff) != 1 || diff[0] != "reclaimPolicy" {
		t.Errorf("reclaimPolicy is immutable and must be reported, got %v", diff)
	}
}

// TestApplyManagedBlockStorageClass_RefusesToRecycleAForeignClass: an adopted
// cluster can already carry its OWN class named block-storage-retain. Every
// immutable field disagrees with ours, so the drift check alone would delete it —
// destroying an adopter's storage configuration to fix a problem they do not have.
// The sibling encryptStockStorageClasses refuses a non-Linode-CSI class for the
// same reason; this must too, and nothing may be deleted or applied over it.
func TestApplyManagedBlockStorageClass_RefusesToRecycleAForeignClass(t *testing.T) {
	d, ops, _ := managedSCFake{live: foreignBlockStorageRetainJSON}.deps()
	err := applyManagedBlockStorageClass(bootstrapClusterOpts{clusterID: "637888"}, d)
	if err == nil {
		t.Fatal("a same-named class owned by someone else must be reported, not recycled")
	}
	if !strings.Contains(err.Error(), "ebs.csi.aws.com") {
		t.Errorf("error must name the foreign provisioner so the collision is diagnosable, got: %v", err)
	}
	if len(*ops) != 0 {
		t.Fatalf("a class LLZ did not create must never be deleted or overwritten, got %v", *ops)
	}
}

// TestApplyManagedBlockStorageClass_UnreadableGetIsNotSilentlyGreenfield: the get
// seam reports failure for BOTH "no such class" and "I could not ask" (RBAC, an
// apiserver blip). Only the first is greenfield. We still fall through to the
// plain apply — a blip must not fail a bootstrap that would otherwise succeed —
// but on a brownfield cluster that apply dies on "parameters: Forbidden", and
// without this warning the operator has no way to tell why the recreate never ran.
func TestApplyManagedBlockStorageClass_UnreadableGetIsNotSilentlyGreenfield(t *testing.T) {
	d, ops, _ := managedSCFake{getErr: kubectlSCForbidden}.deps()
	var err error
	stderr := captureStderr(t, func() {
		err = applyManagedBlockStorageClass(bootstrapClusterOpts{clusterID: "637888"}, d)
	})
	if err != nil {
		t.Fatalf("a get blip must not fail the bootstrap on its own: %v", err)
	}
	if len(*ops) != 1 || (*ops)[0] != "apply" {
		t.Fatalf("nothing may be deleted on a class we could not read, got %v", *ops)
	}
	if !strings.Contains(stderr, "NOT the same as it being absent") {
		t.Errorf("an unreadable get must not pass silently as greenfield, stderr was:\n%s", stderr)
	}
	// ...and a genuine absence is NOT worth a warning, or every greenfield bootstrap
	// cries wolf.
	d2, _, _ := managedSCFake{}.deps()
	quiet := captureStderr(t, func() {
		if err := applyManagedBlockStorageClass(bootstrapClusterOpts{clusterID: "637888"}, d2); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(quiet, "WARNING") {
		t.Errorf("an absent class is the greenfield case and must warn about nothing, got:\n%s", quiet)
	}
}

// TestApplyManagedBlockStorageClass_DeleteFailureStopsBeforeApplying: if the
// delete fails the old class is STILL THERE, and the apply that follows would die
// on the same "parameters: Forbidden" the delete existed to get past. Fail on the
// delete, naming it, rather than reporting the apply's confusing symptom.
func TestApplyManagedBlockStorageClass_DeleteFailureStopsBeforeApplying(t *testing.T) {
	d, ops, _ := managedSCFake{live: oldKeysBlockStorageRetainJSON, deleteFails: true}.deps()
	err := applyManagedBlockStorageClass(bootstrapClusterOpts{clusterID: "637888"}, d)
	if err == nil {
		t.Fatal("a failed delete leaves the un-updatable class in place — it must hard-fail")
	}
	if !strings.Contains(err.Error(), "delete StorageClass block-storage-retain") {
		t.Errorf("error must name the delete as what failed, got: %v", err)
	}
	if len(*ops) != 1 || (*ops)[0] != "delete" {
		t.Fatalf("no apply may follow a delete that did not happen, got %v", *ops)
	}
}

// TestApplyManagedBlockStorageClass_RefusesAClassOwnedByAnotherCluster: a live
// class that ALREADY carries the corrected keys and an `lke<id>` tag for some
// other cluster cannot be a stale copy of ours — cluster_id is fixed for a
// cluster's life, so it means --cluster-id does not describe the cluster this
// kubeconfig reaches. Before this fix, parameter immutability made re-stamping
// that tag impossible; the recreate removes that stop, and `llz reap` deletes
// Volumes whose tagged cluster is gone. Getting this wrong reaps live Volumes.
func TestApplyManagedBlockStorageClass_RefusesAClassOwnedByAnotherCluster(t *testing.T) {
	y, err := renderBlockStorageClass("637888")
	if err != nil {
		t.Fatal(err)
	}
	liveJSON, err := yaml.YAMLToJSON([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	d, ops, _ := managedSCFake{live: string(liveJSON)}.deps()
	err = applyManagedBlockStorageClass(bootstrapClusterOpts{clusterID: "999111"}, d)
	if err == nil {
		t.Fatal("re-tagging this class for a cluster it does not belong to makes `llz reap` delete live Volumes — it must hard-fail")
	}
	if !strings.Contains(err.Error(), "637888") || !strings.Contains(err.Error(), "999111") {
		t.Errorf("error must name BOTH ids so the operator can tell which is wrong, got: %v", err)
	}
	if len(*ops) != 0 {
		t.Fatalf("nothing may be deleted or applied when the ids disagree, got %v", *ops)
	}

	// ...but the class this whole function exists to repair — pre-correction keys,
	// so NO lke<id> token under the new volumeTags key — must still be recreated.
	d2, ops2, _ := managedSCFake{live: oldKeysBlockStorageRetainJSON}.deps()
	if err := applyManagedBlockStorageClass(bootstrapClusterOpts{clusterID: "999111"}, d2); err != nil {
		t.Fatalf("a pre-correction class carries no tag under the new key and must still be repaired: %v", err)
	}
	if len(*ops2) != 2 || (*ops2)[0] != "delete" {
		t.Fatalf("expected delete then apply, got %v", *ops2)
	}
}
