package main

// Gap-closing tests for ci_bootstrap_cluster.go surfaced by mutation testing.
// The existing suite drives the managed bridge down its happy path, where every
// `if err != nil { return err }` is a no-op. That leaves the error handling
// itself unasserted — and an inverted guard here does not fail the bootstrap, it
// makes it EXIT 0 having applied half (or none) of the Argo bridge, which is the
// convergence contract's worst outcome: green while inert.

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// managedArgoUpKubectl answers the waitManagedArgoReady probes so a test reaches
// the bridge applies, delegating everything else to extra.
func managedArgoUpKubectl(extra func(line string) (string, bool)) func(args ...string) (string, bool) {
	return func(args ...string) (string, bool) {
		line := strings.Join(args, " ")
		switch {
		case strings.Contains(line, "crd applications.argoproj.io"):
			return "applications.argoproj.io", true
		case strings.Contains(line, "deploy argocd-server"):
			return "1", true // availableReplicas
		}
		if extra != nil {
			return extra(line)
		}
		return "", true
	}
}

// A failed managed apl-core configuration must ABORT bootstrap and say why.
//
// This test previously asserted the opposite — warn-and-continue — which was the
// contract when it was written. Upstream deliberately made it fatal: on a managed
// cluster that never publishes apl-git-config there is no apl-core, so every
// gitops-* Application would sit in ComparisonError and converge would hard-fail
// ~20 minutes later naming none of it. Failing here cannot turn a passing run red;
// it fails earlier and names the cause. The assertion is inverted to match, and
// the error text is asserted so a silent abort is still a failure.
func TestBootstrapClusterAbortsWhenManagedAplConfigFails(t *testing.T) {
	o := bootstrapClusterOpts{
		// clusterID is required since the block-storage-retain StorageClass gained
		// its lke<id> ownership tag: without it bootstrap aborts before the bridge,
		// which is what this test is about.
		env: "primary", clusterID: "393244", instanceRepo: "acme/instance",
		upstreamOrg: "akamai-consulting", templateRef: "ref", appsRepoRevision: "main",
		instanceRepoToken: "tok",
	}
	// The apl-git-config read answers empty, so readAplGitConfig rejects it
	// ("no repoUrl") and configureManagedApl returns an error.
	d := bootstrapDeps{
		kubectl: managedArgoUpKubectl(nil),
		apply:   func(string, string, bool) (string, bool) { return "", true },
		now:     time.Now,
		sleep:   func(time.Duration) {},
	}
	var err error
	captureStdout(t, func() { err = bootstrapCluster(o, d) })
	if err == nil {
		t.Fatal("a failed managed-apl config must abort: apl-core has no values branch, so every gitops-* " +
			"Application would sit in ComparisonError and converge would fail 20 minutes later naming none of it")
	}
	for _, want := range []string{"managed apl-core BYO-Git", "no values branch to reconcile"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("abort error must name the cause (%q), got: %v", want, err)
		}
	}
}

// The instance-repo credential is applied BEFORE the bridge so it is present on
// the bridge's first reconcile — it is a step in the sequence, not the end of it.
// A bootstrap that stops there exits 0 having created no Application at all.
func TestBootstrapClusterAppliesTheBridgeAfterTheInstanceRepoSecret(t *testing.T) {
	o := bootstrapClusterOpts{
		// clusterID is required since the block-storage-retain StorageClass gained
		// its lke<id> ownership tag: without it bootstrap aborts before the bridge,
		// which is what this test is about.
		env: "primary", clusterID: "393244", instanceRepo: "acme/instance",
		upstreamOrg: "akamai-consulting", templateRef: "ref", appsRepoRevision: "main",
		instanceRepoToken: "tok",
	}
	var applied []string
	d := bootstrapDeps{
		// A populated apl-git-config: configureManagedApl is fatal on failure now,
		// so this test — which is about the ORDER of the bridge steps — must not
		// trip it. Answering the read keeps the run on the happy path.
		kubectl: managedArgoUpKubectl(func(line string) (string, bool) {
			// configureManagedApl is fatal on failure now, so this test — which is
			// about the ORDER of the bridge steps — has to keep it on its happy
			// path: answer the apl-git-config read, then report the migration Job
			// succeeded so the wait returns instead of burning its 13m budget.
			for k, v := range fullAplGitSecret() {
				if strings.Contains(line, "jsonpath={.data."+k+"}") {
					return base64.StdEncoding.EncodeToString([]byte(v)), true
				}
			}
			if strings.Contains(line, "jsonpath={.status.succeeded}") {
				return "1", true
			}
			return "", true
		}),
		apply: func(y, _ string, _ bool) (string, bool) {
			applied = append(applied, y)
			return "", true
		},
		now:   time.Now,
		sleep: func(time.Duration) {},
	}
	captureStdout(t, func() {
		if err := bootstrapCluster(o, d); err != nil {
			t.Fatalf("bootstrapCluster: %v", err)
		}
	})
	joined := strings.Join(applied, "\n---\n")
	if !strings.Contains(joined, "secret-type: repository") {
		t.Fatal("expected the instance-repo repository Secret to be applied")
	}
	for _, want := range []string{"kind: AppProject", "name: platform-bootstrap", "name: llz-secret-store"} {
		if !strings.Contains(joined, want) {
			t.Errorf("bridge manifest %q was never applied — the run stopped at the repo Secret and still exited 0:\n%s", want, joined)
		}
	}
}

// The LAST bridge apply is still a bridge apply. A failure there must fail the
// command, not fall through to the "✓ Argo bridge applied" line: llz-secret-store
// is the ClusterSecretStore tree every extra's ExternalSecret resolves against.
func TestBootstrapClusterFailsWhenABridgeApplyFails(t *testing.T) {
	o := bootstrapClusterOpts{
		env: "primary", instanceRepo: "acme/instance",
		upstreamOrg: "akamai-consulting", templateRef: "ref", appsRepoRevision: "main",
	}
	for _, failOn := range []string{"name: llz-secret-store", "kind: AppProject"} {
		t.Run(failOn, func(t *testing.T) {
			d := bootstrapDeps{
				kubectl: managedArgoUpKubectl(nil),
				apply: func(y, _ string, _ bool) (string, bool) {
					if strings.Contains(y, failOn) {
						return "error: admission webhook denied the request", false
					}
					return "", true
				},
				now:   time.Now,
				sleep: func(time.Duration) {},
			}
			var err error
			out := captureStderr(t, func() { err = bootstrapCluster(o, d) })
			if err == nil {
				t.Fatalf("a rejected %q apply must fail the bootstrap, not report the bridge as applied", failOn)
			}
			if strings.Contains(out, "✓ Argo bridge applied") {
				t.Errorf("the success banner was printed despite a rejected apply:\n%s", out)
			}
		})
	}
}

// aplGitStubKubectl answers a complete, healthy managed apl-core BYO-Git flow;
// override lets a test break exactly one step of it.
func aplGitStubKubectl(branch string, override func(line string) (string, bool, bool)) func(args ...string) (string, bool) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	return func(args ...string) (string, bool) {
		line := strings.Join(args, " ")
		if override != nil {
			if out, ok, handled := override(line); handled {
				return out, ok
			}
		}
		switch {
		case strings.Contains(line, "get secret apl-git-config") && strings.Contains(line, "data.repoUrl"):
			return b64("http://git-server.git-server.svc.cluster.local/otomi/values.git"), true
		case strings.Contains(line, "get secret apl-git-config") && strings.Contains(line, "data.branch"):
			return b64(branch), true
		case strings.Contains(line, "get secret apl-git-config") && strings.Contains(line, "data.username"):
			return b64("otomi-admin"), true
		case strings.Contains(line, "get secret apl-git-config") && strings.Contains(line, "data.password"):
			return b64("gitea-pw"), true
		case strings.Contains(line, "get job") && strings.Contains(line, "status.succeeded"):
			return "1", true
		}
		return "", true
	}
}

// Repointing apl-core is the LAST and load-bearing step: the migration Job has
// already force-pushed the complete tree to the github branch, and if the Secret
// patch is rejected apl-core keeps reconciling gitea. Reporting that as success
// leaves the operator with a green bootstrap and an unchanged cluster.
func TestConfigureManagedAplFailsWhenTheGitConfigPatchIsRejected(t *testing.T) {
	o := bootstrapClusterOpts{env: "primary", instanceRepo: "acme/instance", instanceRepoToken: "tok"}
	d := bootstrapDeps{
		kubectl: aplGitStubKubectl("main", func(line string) (string, bool, bool) {
			if strings.Contains(line, "patch secret apl-git-config") {
				return "Error from server (Forbidden): secrets \"apl-git-config\" is forbidden", false, true
			}
			return "", false, false
		}),
		apply: func(string, string, bool) (string, bool) { return "", true },
		now:   time.Now,
		sleep: func(time.Duration) {},
	}
	var err error
	out := captureStderr(t, func() { err = configureManagedApl(o, d) })
	if err == nil {
		t.Fatal("a rejected apl-git-config patch must be reported — apl-core would keep reconciling gitea")
	}
	if !strings.Contains(err.Error(), "apl-git-config") {
		t.Errorf("err = %v, want it to name the Secret that could not be patched", err)
	}
	if strings.Contains(out, "repointed at") {
		t.Errorf("the repoint success line was printed despite a rejected patch:\n%s", out)
	}
}

// readAplGitConfig's branch fallback applies ONLY to an absent value. apl-core's
// current branch is what the migration Job clones, so overwriting a real branch
// with "main" clones the wrong tree — and the orphan force-push then supersedes
// the github branch with it.
func TestReadAplGitConfigBranchFallback(t *testing.T) {
	read := func(t *testing.T, branch string) aplGitConfig {
		t.Helper()
		d := bootstrapDeps{kubectl: aplGitStubKubectl(branch, nil), now: time.Now, sleep: func(time.Duration) {}}
		c, err := readAplGitConfig(d)
		if err != nil {
			t.Fatalf("readAplGitConfig: %v", err)
		}
		return c
	}
	if got := read(t, "otomi-values").branch; got != "otomi-values" {
		t.Errorf("branch = %q, want otomi-values — apl-core's configured branch must be preserved", got)
	}
	if got := read(t, "").branch; got != "main" {
		t.Errorf("branch = %q, want the main fallback when apl-core reports none", got)
	}
	if got := read(t, "main").repoURL; !strings.HasPrefix(got, "http://git-server") {
		t.Errorf("repoURL = %q, want the decoded in-cluster values repo", got)
	}
}

// ...and a config with no repoUrl at all is rejected rather than migrated from
// nowhere.
func TestReadAplGitConfigRejectsAnUnconfiguredSecret(t *testing.T) {
	d := bootstrapDeps{
		kubectl: func(...string) (string, bool) { return "", true },
		now:     time.Now, sleep: func(time.Duration) {},
	}
	if _, err := readAplGitConfig(d); err == nil {
		t.Fatal("an apl-git-config with no repoUrl must be rejected")
	}
}
